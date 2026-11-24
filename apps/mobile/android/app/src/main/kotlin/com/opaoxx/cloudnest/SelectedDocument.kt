package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.net.Uri
import android.provider.OpenableColumns
import java.io.IOException
import java.io.InterruptedIOException
import java.security.MessageDigest

internal fun readSelectedDocument(resolver: ContentResolver, uri: Uri): Map<String, Any> {
    // 1. 由文档提供方返回显示名称。文档不必对应本地文件路径。
    val name = resolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
        ?.use { cursor ->
            val index = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
            if (index >= 0 && cursor.moveToFirst()) cursor.getString(index) else null
        }
        ?.takeIf { it.isNotBlank() }
        ?: "未命名文件"

    // 2. 重复使用 64 KiB 缓冲区，按实际读到的内容计数和计算 SHA-256。
    val digest = MessageDigest.getInstance("SHA-256")
    var sizeBytes = 0L
    val stream = resolver.openInputStream(uri) ?: throw IOException("Document stream unavailable")
    stream.use {
        val buffer = ByteArray(64 * 1024)
        while (true) {
            if (Thread.currentThread().isInterrupted) throw InterruptedIOException()
            val count = it.read(buffer)
            if (count == -1) break
            digest.update(buffer, 0, count)
            sizeBytes += count
        }
    }

    // 3. 关闭流后只返回 Flutter 显示所需的摘要。
    val sha256 = digest.digest().joinToString("") { "%02x".format(it.toInt() and 0xff) }
    return mapOf("name" to name, "sizeBytes" to sizeBytes, "sha256" to sha256)
}
