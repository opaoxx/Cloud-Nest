package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.net.Uri
import android.provider.DocumentsContract
import android.provider.OpenableColumns
import java.io.File
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URI
import java.util.UUID

internal class DownloadFailure(val code: String) : IOException()

internal data class StagedDownload(val file: File, val summary: SourceSummary)

private val downloadPrefix = "cloudnest-download-${UUID.randomUUID()}-"
private val downloadFilePattern = Regex("cloudnest-download-[0-9a-f-]{36}-[0-9]+\\.part")

internal class VerifiedDownload(private val cache: File, private val resolver: ContentResolver) {
    fun stage(signedUrl: String, expected: SourceSummary): StagedDownload {
        // 1. 直接访问原签名地址，关闭跳转，不带 API 的访问令牌。
        val url = URI(signedUrl).toURL()
        if (url.protocol !in setOf("http", "https") || url.host.isEmpty() ||
            url.userInfo != null || url.ref != null
        ) throw DownloadFailure("download_network")

        pruneOldDownloads()
        checkSpace(expected.sizeBytes)
        val target = try {
            File.createTempFile(downloadPrefix, ".part", cache)
        } catch (failure: IOException) {
            checkSpace(1)
            throw failure
        }
        var complete = false
        try {
            val connection = url.openConnection() as HttpURLConnection
            try {
                connection.instanceFollowRedirects = false
                connection.connectTimeout = 10_000
                connection.readTimeout = 20_000
                connection.useCaches = false
                connection.setRequestProperty("Accept-Encoding", "identity")
                if (connection.responseCode != HttpURLConnection.HTTP_OK) {
                    throw DownloadFailure("download_network")
                }
                val encoding = connection.contentEncoding
                val length = connection.getHeaderField("Content-Length")
                if (connection.getHeaderField("Content-Range") != null ||
                    (encoding != null && !encoding.equals("identity", ignoreCase = true)) ||
                    (length != null && length.toLongOrNull() != expected.sizeBytes)
                ) throw DownloadFailure("download_integrity")

                // 2. 流式写入私有临时文件，校验成功后才交给另存为。
                val actual = connection.inputStream.use { input ->
                    target.outputStream().use { output ->
                        summarizeSource(input, expected.name) { buffer, count, before ->
                            if (count.toLong() > expected.sizeBytes - before) {
                                throw DownloadFailure("download_integrity")
                            }
                            checkSpace(count.toLong())
                            output.write(buffer, 0, count)
                        }
                    }
                }
                if (!expected.matches(actual)) throw DownloadFailure("download_integrity")
            } finally {
                connection.disconnect()
            }
            complete = true
            return StagedDownload(target, expected)
        } catch (failure: IOException) {
            if (failure !is DownloadFailure && cache.usableSpace == 0L) {
                throw DownloadFailure("download_space")
            }
            throw failure
        } finally {
            if (!complete) target.delete()
        }
    }

    fun save(download: StagedDownload, destination: Uri): String? {
        // 3. 只向系统新建的目标 URI 写入。出错时尝试回收空壳或残留。
        val expected = download.summary
        try {
            download.file.inputStream().use { input ->
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
        if (cache.usableSpace < bytes) throw DownloadFailure("download_space")
    }

    private fun pruneOldDownloads() {
        cache.listFiles()?.forEach { file ->
            if (file.isFile && downloadFilePattern.matches(file.name) && !file.name.startsWith(downloadPrefix)) {
                file.delete()
            }
        }
    }
}
