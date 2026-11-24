package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.net.Uri
import android.provider.OpenableColumns
import java.io.InputStream
import java.io.InterruptedIOException
import java.security.MessageDigest

internal data class SelectedDocument(val name: String, val sizeHint: Long?)

internal data class SourceSummary(val name: String, val sizeBytes: Long, val sha256: String) {
    fun matches(other: SourceSummary) = sizeBytes == other.sizeBytes && sha256 == other.sha256
}

internal fun readDocumentDetails(resolver: ContentResolver, uri: Uri): SelectedDocument {
    // 提供方的长度可以缺失。它只用于暂存预检，最终大小以读到的正文为准。
    val projection = arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE)
    return resolver.query(uri, projection, null, null, null)?.use { cursor ->
        if (!cursor.moveToFirst()) return@use null
        val nameIndex = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
        val sizeIndex = cursor.getColumnIndex(OpenableColumns.SIZE)
        val name = if (nameIndex >= 0) cursor.getString(nameIndex) else null
        val size = if (sizeIndex >= 0 && !cursor.isNull(sizeIndex)) cursor.getLong(sizeIndex) else null
        SelectedDocument(name?.takeIf { it.isNotBlank() } ?: "未命名文件", size?.takeIf { it >= 0 })
    } ?: SelectedDocument("未命名文件", null)
}

internal fun summarizeSource(
    input: InputStream,
    name: String,
    writeChunk: ((ByteArray, Int, Long) -> Unit)? = null,
): SourceSummary {
    // 循环复用 64 KiB 缓冲区，同一次读取完成摘要和可选的暂存写入。
    val digest = MessageDigest.getInstance("SHA-256")
    var sizeBytes = 0L
    val buffer = ByteArray(64 * 1024)
    while (true) {
        if (Thread.currentThread().isInterrupted) throw InterruptedIOException()
        val count = input.read(buffer)
        if (count == -1) break
        writeChunk?.invoke(buffer, count, sizeBytes)
        digest.update(buffer, 0, count)
        sizeBytes += count
    }
    val sha256 = digest.digest().joinToString("") { "%02x".format(it.toInt() and 0xff) }
    return SourceSummary(name, sizeBytes, sha256)
}
