package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.net.Uri
import android.provider.DocumentsContract
import android.provider.OpenableColumns
import java.io.File
import java.io.IOException
import java.io.InputStream
import java.io.InterruptedIOException
import java.io.RandomAccessFile
import java.net.HttpURLConnection
import java.net.URI
import java.net.URL

internal class DownloadFailure(val code: String) : IOException()

internal data class StagedDownload(val checkpoint: DownloadCheckpoint, val summary: SourceSummary)

private const val DOWNLOAD_CHECKPOINT_INTERVAL = 1024L * 1024
private val legacyDownloadPattern = Regex("cloudnest-download-[0-9a-f-]{36}-[0-9]+\\.part")

internal class VerifiedDownload(
    private val directory: File,
    private val legacyCache: File,
    private val resolver: ContentResolver,
) {
    fun stage(signedUrl: String, identity: DownloadIdentity, shouldPause: () -> Boolean): StagedDownload {
        val url = URI(signedUrl).toURL()
        if (url.protocol !in setOf("http", "https") || url.host.isEmpty() ||
            url.userInfo != null || url.ref != null
        ) throw DownloadFailure("download_network")

        val expected = identity.source
        val checkpoint = DownloadCheckpoint.open(directory, identity)
        var complete = false
        var discard = false
        try {
            legacyCache.listFiles()?.forEach { file ->
                if (file.isFile && legacyDownloadPattern.matches(file.name)) file.delete()
            }
            RandomAccessFile(checkpoint.file, "rw").use { output ->
                // 1. 恢复同一对象已落盘的偏移，丢弃尚未确认的尾部。
                checkpoint.restore(output)
                checkPause(shouldPause)
                if (checkpoint.confirmed == 0L || checkpoint.confirmed < expected.sizeBytes) {
                    receive(url, checkpoint, output, expected.sizeBytes, shouldPause)
                }
            }
            // 3. 核对整文件大小和 SHA-256，交给系统另存为。
            val actual = checkpoint.file.inputStream().use { input ->
                summarizeSource(input, expected.name) { _, _, _ -> checkPause(shouldPause) }
            }
            if (!expected.matches(actual)) {
                discard = true
                throw DownloadFailure("download_integrity")
            }
            complete = true
            return StagedDownload(checkpoint, expected)
        } catch (failure: IOException) {
            if (failure !is DownloadFailure && directory.usableSpace == 0L) {
                throw DownloadFailure("download_space")
            }
            throw failure
        } finally {
            if (!complete) checkpoint.release(discard)
        }
    }

    private fun receive(
        url: URL, checkpoint: DownloadCheckpoint, output: RandomAccessFile, size: Long, shouldPause: () -> Boolean,
    ) {
        // 2. 检查存储返回的范围，边下载边保存断点。存储 GET 不携带业务令牌。
        val requested = checkpoint.confirmed
        val connection = url.openConnection() as HttpURLConnection
        try {
            connection.instanceFollowRedirects = false
            connection.connectTimeout = 10_000
            connection.readTimeout = 20_000
            connection.useCaches = false
            connection.setRequestProperty("Accept-Encoding", "identity")
            if (requested > 0) connection.setRequestProperty("Range", "bytes=$requested-")
            val start = responseOffset(connection, requested, size)
            if (start != requested) checkpoint.restart(output)
            checkSpace(size - start)
            connection.inputStream.use { input -> writeBody(input, checkpoint, output, start, size, shouldPause) }
        } finally {
            connection.disconnect()
        }
    }

    private fun responseOffset(connection: HttpURLConnection, requested: Long, size: Long): Long {
        val status = connection.responseCode
        val ranges = connection.responseHeaders("Content-Range")
        val start = when {
            status == HttpURLConnection.HTTP_OK && ranges.isEmpty() -> 0L
            status == HttpURLConnection.HTTP_PARTIAL && requested > 0 &&
                ranges == listOf("bytes $requested-${size - 1}/$size") -> requested
            status == HttpURLConnection.HTTP_OK || status == HttpURLConnection.HTTP_PARTIAL || status == 416 ->
                throw DownloadFailure("download_range")
            else -> throw DownloadFailure("download_network")
        }
        val lengths = connection.responseHeaders("Content-Length")
        if (connection.responseHeaders("Content-Encoding").any { !it.equals("identity", ignoreCase = true) } ||
            (lengths.isNotEmpty() && lengths.singleOrNull()?.toLongOrNull() != size - start)
        ) throw DownloadFailure("download_range")
        return start
    }

    private fun HttpURLConnection.responseHeaders(name: String): List<String> =
        headerFields.entries.filter { it.key?.equals(name, ignoreCase = true) == true }.flatMap { it.value }

    private fun writeBody(
        input: InputStream, checkpoint: DownloadCheckpoint, output: RandomAccessFile, start: Long, size: Long,
        shouldPause: () -> Boolean,
    ) {
        val buffer = ByteArray(64 * 1024)
        var offset = start
        try {
            while (offset < size) {
                checkPause(shouldPause)
                if (Thread.currentThread().isInterrupted) throw InterruptedIOException()
                val length = minOf(buffer.size.toLong(), size - offset).toInt()
                val count = readBodyChunk { input.read(buffer, 0, length) }
                if (count <= 0) throw DownloadFailure("download_interrupted")
                checkSpace(count.toLong())
                output.write(buffer, 0, count)
                offset += count
                if (offset < size && offset - checkpoint.confirmed >= DOWNLOAD_CHECKPOINT_INTERVAL) {
                    checkpoint.confirm(output, offset)
                }
            }
            checkPause(shouldPause)
            if (readBodyChunk { input.read() } != -1) throw DownloadFailure("download_integrity")
            checkpoint.confirm(output, offset)
        } catch (failure: DownloadFailure) {
            if ((failure.code == "download_interrupted" || failure.code == "download_paused") &&
                offset > checkpoint.confirmed && offset < size
            ) {
                checkpoint.confirm(output, offset)
            }
            throw failure
        }
    }

    private fun checkPause(shouldPause: () -> Boolean) {
        if (shouldPause()) throw DownloadFailure("download_paused")
    }

    private inline fun readBodyChunk(read: () -> Int): Int = try {
        read()
    } catch (_: IOException) {
        throw DownloadFailure("download_interrupted")
    }

    fun save(download: StagedDownload, destination: Uri): String? {
        // 3. 只向系统新建的目标 URI 写入。出错时尝试回收空壳或残留。
        val expected = download.summary
        try {
            download.checkpoint.file.inputStream().use { input ->
                val output = resolver.openOutputStream(destination, "wt")
                    ?: throw IOException("No output stream")
                output.use {
                    val written = summarizeSource(input, expected.name) { buffer, count, before ->
                        if (count.toLong() > expected.sizeBytes - before) throw IOException("Longer staged file")
                        it.write(buffer, 0, count)
                    }
                    if (!expected.matches(written)) throw IOException("Staged file changed")
                }
            }
        } catch (_: Exception) {
            val removed = runCatching { DocumentsContract.deleteDocument(resolver, destination) }.getOrDefault(false)
            throw DownloadFailure(if (removed) "download_save_failed" else "download_save_incomplete")
        }
        // 提供方可能因重名或类型调整文件名。以实际落盘后的查询结果为准。
        return runCatching {
            resolver.query(destination, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { cursor ->
                val nameColumn = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
                if (nameColumn >= 0 && cursor.moveToFirst()) cursor.getString(nameColumn)?.takeIf { it.isNotBlank() }
                else null
            }
        }.getOrNull()
    }

    private fun checkSpace(bytes: Long) {
        if (directory.usableSpace < bytes) throw DownloadFailure("download_space")
    }
}
