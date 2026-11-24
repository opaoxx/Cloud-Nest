package com.opaoxx.cloudnest

import android.util.Xml
import org.xmlpull.v1.XmlPullParser
import java.io.ByteArrayInputStream
import java.io.FileInputStream
import java.io.IOException
import java.io.InterruptedIOException
import java.net.HttpURLConnection
import java.net.URI
import java.security.MessageDigest

internal val multipartDigestPattern = Regex("[0-9a-f]{64}")
internal val multipartETagPattern = Regex("[A-Za-z0-9._:-]{1,64}")
private const val CHECKSUM_HEADER = "x-amz-checksum-sha256"
private const val BUFFER_SIZE = 64 * 1024
private const val MAX_STORAGE_ERROR_BYTES = 8 * 1024

internal class MultipartSignatureExpired : IOException()

internal fun multipartDigests(
    input: FileInputStream,
    expected: SourceSummary,
    partSize: Long,
    partCount: Int,
): List<String> {
    require(partSize >= MAX_SINGLE_UPLOAD_BYTES && partCount in 1..10000)
    require(expected.sizeBytes > MAX_SINGLE_UPLOAD_BYTES && (expected.sizeBytes - 1) / partSize + 1 == partCount.toLong())
    val whole = MessageDigest.getInstance("SHA-256")
    val part = MessageDigest.getInstance("SHA-256")
    val buffer = ByteArray(BUFFER_SIZE)

    // 1. 同一遍顺序扫描计算整个文件和各片摘要，短末片只读剩余字节。
    val digests = List(partCount) { index ->
        val length = minOf(partSize, expected.sizeBytes - index.toLong() * partSize)
        readRange(input, buffer, length) { bytes, count ->
            whole.update(bytes, 0, count)
            part.update(bytes, 0, count)
        }
        part.digest().hexDigest()
    }
    // 2. 原文件发生变化时拒绝申请任何签名。
    if (input.read() != -1 || whole.digest().hexDigest() != expected.sha256) {
        throw UploadSourceChanged()
    }
    return digests
}

internal fun putMultipartBody(
    input: FileInputStream,
    sourceSize: Long,
    offset: Long,
    length: Long,
    signedUrl: String,
    checksum: String,
): String {
    require(offset >= 0 && length > 0 && offset <= sourceSize - length)
    val url = URI(signedUrl).toURL()
    require(
        url.protocol in setOf("https", "http") &&
            url.host.isNotEmpty() && url.userInfo == null && url.ref == null,
    )
    input.channel.position(offset)

    // 1. 用固定长度流式写入这一片。API 凭证和重定向都不参与正文请求。
    val connection = url.openConnection() as HttpURLConnection
    try {
        connection.instanceFollowRedirects = false
        connection.connectTimeout = 10_000
        connection.readTimeout = 30_000
        connection.useCaches = false
        connection.requestMethod = "PUT"
        connection.doOutput = true
        connection.setFixedLengthStreamingMode(length)
        connection.setRequestProperty(CHECKSUM_HEADER, checksum)
        connection.outputStream.use { output ->
            readRange(input, ByteArray(BUFFER_SIZE), length) { buffer, count ->
                output.write(buffer, 0, count)
            }
        }

        // 2. 只有存储明确返回成功和唯一 ETag 才交付回执。
        val status = connection.responseCode
        if (status != HttpURLConnection.HTTP_OK) {
            if (isExpiredSignature(connection, status)) throw MultipartSignatureExpired()
            throw IOException("Storage did not accept part")
        }
        val tags = connection.headerFields.entries
            .filter { it.key?.equals("ETag", ignoreCase = true) == true }
            .flatMap { it.value }
        val tag = tags.singleOrNull()?.removeSurrounding("\"")
        if (tag == null || !multipartETagPattern.matches(tag)) throw IOException("Invalid part ETag")
        return tag
    } finally {
        connection.disconnect()
    }
}

private fun isExpiredSignature(connection: HttpURLConnection, status: Int): Boolean {
    if (status != HttpURLConnection.HTTP_BAD_REQUEST && status != HttpURLConnection.HTTP_FORBIDDEN) return false
    val error = connection.errorStream ?: return false
    return runCatching {
        error.use { input ->
            // 1. 只读取有界的错误正文，避免把普通 403 当成过期。
            val buffer = ByteArray(MAX_STORAGE_ERROR_BYTES + 1)
            var size = 0
            while (size < buffer.size) {
                val count = input.read(buffer, size, buffer.size - size)
                if (count <= 0) break
                size += count
            }
            if (size > MAX_STORAGE_ERROR_BYTES) return@use false
            val parser = Xml.newPullParser()
            parser.setFeature(XmlPullParser.FEATURE_PROCESS_DOCDECL, false)
            parser.setInput(ByteArrayInputStream(buffer, 0, size), "UTF-8")
            parser.nextTag()
            parser.require(XmlPullParser.START_TAG, null, "Error")
            val fields = mutableMapOf<String, String>()
            while (parser.nextTag() == XmlPullParser.START_TAG) {
                val name = parser.name
                if (fields.put(name, parser.nextText()) != null) return@use false
            }
            parser.require(XmlPullParser.END_TAG, null, "Error")

            // 2. 与存储端的明确过期响应一致时才允许上层重签。
            fields["Code"] == "RequestExpired" ||
                (status == HttpURLConnection.HTTP_FORBIDDEN && fields["Code"] == "AccessDenied" &&
                    fields["Message"] == "Request has expired")
        }
    }.getOrDefault(false)
}

private inline fun readRange(
    input: FileInputStream,
    buffer: ByteArray,
    length: Long,
    consume: (ByteArray, Int) -> Unit,
) {
    var remaining = length
    while (remaining > 0) {
        if (Thread.currentThread().isInterrupted) throw InterruptedIOException()
        val count = input.read(buffer, 0, minOf(remaining, buffer.size.toLong()).toInt())
        if (count <= 0) throw UploadSourceChanged()
        consume(buffer, count)
        remaining -= count
    }
}

private fun ByteArray.hexDigest() = joinToString("") { "%02x".format(it.toInt() and 0xff) }
