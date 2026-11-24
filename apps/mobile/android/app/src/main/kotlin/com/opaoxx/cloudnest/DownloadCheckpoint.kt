package com.opaoxx.cloudnest

import android.util.AtomicFile
import org.json.JSONArray
import java.io.File
import java.io.FileNotFoundException
import java.io.IOException
import java.io.RandomAccessFile
import java.nio.channels.OverlappingFileLockException
import java.security.MessageDigest

internal data class DownloadIdentity(
    val owner: TaskOwner,
    val entryId: String,
    val revision: String,
    val objectId: String,
    val source: SourceSummary,
) {
    init {
        require(listOf(entryId, revision, objectId).all { id ->
            id != "0" && id.toULongOrNull()?.toString() == id
        })
        require(source.name.isNotBlank() && source.sizeBytes in 0..9_007_199_254_740_991L)
        require(multipartDigestPattern.matches(source.sha256))
    }

    fun fileKey(): String = downloadKey(owner.server, owner.userId, entryId)

    fun versionKey(): String = downloadKey(revision, objectId, source.sizeBytes, source.sha256)
}

private fun downloadKey(vararg values: Any): String =
    MessageDigest.getInstance("SHA-256")
        .digest(JSONArray(values.toList()).toString().toByteArray(Charsets.UTF_8))
        .joinToString("") { "%02x".format(it.toInt() and 0xff) }

internal class DownloadCheckpoint private constructor(
    val file: File,
    private val marker: AtomicFile,
    private val lockFile: RandomAccessFile,
    private val versionKey: String,
    private val sizeBytes: Long,
) {
    var confirmed = 0L
        private set

    fun restore(output: RandomAccessFile) {
        // 1. 只承认属于同一对象、且文件实际还保有的已同步字节。
        val lines = try {
            marker.readFully().decodeToString().lineSequence().toList()
        } catch (_: FileNotFoundException) {
            emptyList()
        }
        val offset = lines.getOrNull(1)?.toLongOrNull()
        if (lines.firstOrNull() != versionKey || offset == null ||
            offset !in 0..sizeBytes || output.length() < offset
        ) {
            restart(output)
            return
        }
        confirmed = offset
        if (output.length() > offset) output.setLength(offset)
        output.seek(offset)
    }

    fun restart(output: RandomAccessFile) {
        // 2. 先降低偏移再清空正文，进程中断时也能安全从零恢复。
        store(0)
        output.setLength(0)
        output.seek(0)
    }

    fun confirm(output: RandomAccessFile, offset: Long) {
        // 3. 确认点总是晚于正文同步，原子文件提供跨进程恢复边界。
        output.fd.sync()
        store(offset)
    }

    private fun store(offset: Long) {
        val record = "$versionKey\n$offset"
        val output = marker.startWrite()
        try {
            output.write(record.toByteArray(Charsets.UTF_8))
            marker.finishWrite(output)
        } catch (error: IOException) {
            marker.failWrite(output)
            throw error
        }
        if (marker.readFully().decodeToString() != record) throw IOException("Download checkpoint was not saved")
        confirmed = offset
    }

    fun release(discard: Boolean = false) {
        try {
            if (discard) {
                marker.delete()
                file.delete()
            }
        } finally {
            lockFile.close()
        }
    }

    companion object {
        fun open(directory: File, identity: DownloadIdentity): DownloadCheckpoint {
            if (!directory.isDirectory && !directory.mkdirs() && !directory.isDirectory) {
                throw DownloadFailure("download_space")
            }
            val lock = RandomAccessFile(File(directory, "download.lock"), "rw")
            try {
                // 下载、系统选择器和导出共用此锁，防止另一个 Activity 改写待保存文件。
                val acquired = try {
                    lock.channel.tryLock()
                } catch (_: OverlappingFileLockException) {
                    null
                }
                if (acquired == null) throw DownloadFailure("download_busy")
                val key = identity.fileKey()
                return DownloadCheckpoint(
                    File(directory, "$key.part"), AtomicFile(File(directory, "$key.checkpoint")),
                    lock, identity.versionKey(), identity.source.sizeBytes,
                )
            } catch (failure: Exception) {
                lock.close()
                throw failure
            }
        }
    }
}
