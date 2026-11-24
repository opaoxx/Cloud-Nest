package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.content.res.AssetFileDescriptor
import android.net.Uri
import android.system.ErrnoException
import android.system.Os
import android.system.OsConstants
import java.io.File
import java.io.FileOutputStream
import java.io.IOException
import java.io.InputStream
import java.io.InterruptedIOException
import java.util.UUID

private const val STAGING_BUDGET_BYTES = 512L * 1024 * 1024
private const val FREE_RESERVE_BYTES = 128L * 1024 * 1024
private val stageFilePattern = Regex("([0-9a-f-]{36})\\.(source|partial)")

internal class UploadSourceChanged : IOException()
internal class UploadSourcePaused(val reason: String) : IOException()

internal data class UploadSourceReference(
    val uri: Uri? = null,
    val stageId: String? = null,
    val summary: SourceSummary? = null,
) {
    fun view(): Map<String, Any> {
        val prepared = checkNotNull(summary)
        return mapOf(
            "name" to prepared.name,
            "sizeBytes" to prepared.sizeBytes,
            "sha256" to prepared.sha256,
            "sourceMode" to if (stageId == null) "direct" else "staged",
        )
    }
}

internal fun isStageId(text: String): Boolean =
    runCatching { UUID.fromString(text).toString() == text }.getOrDefault(false)

internal fun checkStagingCapacity(bytes: Long, remainingBudget: Long, usableSpace: Long) {
    if (bytes > remainingBudget) throw UploadSourcePaused("document_staging_budget")
    if (usableSpace < FREE_RESERVE_BYTES || bytes > usableSpace - FREE_RESERVE_BYTES) {
        throw UploadSourcePaused("document_staging_space")
    }
}

internal class PreparedUploadSources(
    private val resolver: ContentResolver,
    private val directory: File,
) {
    fun prepare(uri: Uri, persistDirectRead: () -> Boolean): UploadSourceReference {
        // 1. 使用同一个描述符判断读取方式。管道和分段资源走暂存。
        val details = readDocumentDetails(resolver, uri)
        val descriptor = resolver.openAssetFileDescriptor(uri, "r")
            ?: throw IOException("Document descriptor unavailable")
        descriptor.use {
            val directSize = wholeFileSize(descriptor)
            val direct = directSize != null && persistDirectRead()
            val sizeHint = descriptor.length.takeIf { it >= 0 } ?: details.sizeHint
            val verifyEnd: (SourceSummary) -> Unit = { summary ->
                val fd = descriptor.parcelFileDescriptor
                if (fd.canDetectErrors()) fd.checkError()
                if (descriptor.declaredLength >= 0 && summary.sizeBytes != descriptor.declaredLength) {
                    throw UploadSourceChanged()
                }
            }
            descriptor.createInputStream().use { input ->
                if (direct) {
                    val summary = summarizeSource(input, details.name)
                    verifyEnd(summary)
                    if (summary.sizeBytes != directSize) throw UploadSourceChanged()
                    return UploadSourceReference(uri = uri, summary = summary)
                }
                return stage(input, details.name, sizeHint, verifyEnd)
            }
        }
    }

    fun verifyStage(reference: UploadSourceReference) {
        val expected = checkNotNull(reference.summary)
        val file = readyFile(checkNotNull(reference.stageId))
        if (!file.isFile || file.length() != expected.sizeBytes) throw UploadSourceChanged()
        val actual = file.inputStream().use { summarizeSource(it, expected.name) }
        if (!expected.matches(actual)) throw UploadSourceChanged()
    }

    // 只回收本功能命名的半成品及未被任何账号引用的完整副本。
    fun prune(usedIds: Set<String>) {
        directory.listFiles()?.forEach { file ->
            val match = stageFilePattern.matchEntire(file.name) ?: return@forEach
            val (id, extension) = match.destructured
            if (file.isFile && isStageId(id) && (extension == "partial" || id !in usedIds)) file.delete()
        }
    }

    private fun stage(
        input: InputStream,
        name: String,
        sizeHint: Long?,
        verifyEnd: (SourceSummary) -> Unit,
    ): UploadSourceReference {
        if (!directory.isDirectory) {
            val parent = directory.parentFile ?: throw IOException("Staging directory unavailable")
            checkStagingCapacity(sizeHint ?: 0, STAGING_BUDGET_BYTES, parent.usableSpace)
            if (!directory.mkdirs() && !directory.isDirectory) {
                checkStagingCapacity(0, STAGING_BUDGET_BYTES, parent.usableSpace)
                throw IOException("Staging directory unavailable")
            }
        }
        val files = directory.listFiles() ?: throw IOException("Could not list staged sources")
        val used = files
            .filter { it.isFile && it.name.endsWith(".source") && isStageId(it.name.removeSuffix(".source")) }
            .sumOf { it.length() }
        val budget = (STAGING_BUDGET_BYTES - used).coerceAtLeast(0)
        checkStagingCapacity(sizeHint ?: 0, budget, directory.usableSpace)

        // 2. 流式检查每块预算。只在完整写入并同步后发布副本。
        val id = UUID.randomUUID().toString()
        val partial = File(directory, "$id.partial")
        val ready = readyFile(id)
        try {
            val summary = FileOutputStream(partial).use { output ->
                val measured = summarizeSource(input, name) { buffer, count, before ->
                    checkStagingCapacity(count.toLong(), budget - before, directory.usableSpace)
                    output.write(buffer, 0, count)
                }
                verifyEnd(measured)
                output.fd.sync()
                measured
            }
            if (!partial.renameTo(ready)) throw IOException("Could not publish staged source")
            return UploadSourceReference(stageId = id, summary = summary)
        } catch (failure: IOException) {
            if (failure is UploadSourcePaused || failure is InterruptedIOException) throw failure
            if (noSpace(failure) || directory.usableSpace < FREE_RESERVE_BYTES) {
                throw UploadSourcePaused("document_staging_space")
            }
            throw failure
        } finally {
            partial.delete()
        }
    }

    private fun readyFile(id: String): File {
        if (!isStageId(id)) throw IOException("Invalid staged source identifier")
        return File(directory, "$id.source")
    }
}

private fun wholeFileSize(descriptor: AssetFileDescriptor): Long? {
    if (descriptor.startOffset != 0L) return null
    val fd = descriptor.fileDescriptor
    return try {
        val stat = Os.fstat(fd)
        if (!OsConstants.S_ISREG(stat.st_mode) || stat.st_size < 0) return null
        if (descriptor.declaredLength >= 0 && descriptor.declaredLength != stat.st_size) return null
        stat.st_size.takeIf { Os.lseek(fd, 0, OsConstants.SEEK_CUR) == 0L }
    } catch (_: ErrnoException) {
        null
    }
}

private fun noSpace(failure: IOException): Boolean {
    var cause: Throwable? = failure
    while (cause != null) {
        if (cause is ErrnoException && (cause.errno == OsConstants.ENOSPC || cause.errno == OsConstants.EDQUOT)) {
            return true
        }
        cause = cause.cause
    }
    return false
}
