package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.util.AtomicFile
import org.json.JSONObject
import java.io.File
import java.io.FileNotFoundException
import java.io.IOException

internal class DocumentAccessLost : IOException()

private const val GUEST_SCOPE = "guest"
private val rememberedDocumentLock = Any()

internal class RememberedDocument(context: Context) {
    private val resolver = context.contentResolver
    private val savedDocument = AtomicFile(File(context.noBackupFilesDir, "remembered_document"))

    fun readSelection(scope: String, uri: Uri, intentFlags: Int): Map<String, Any> {
        val summary = readSelectedDocument(resolver, uri)
        return summary + ("persisted" to remember(scope, uri, intentFlags))
    }

    fun restore(scope: String): Map<String, Any>? {
        // 1. 按本次打开的账号范围查找文档，只有有效授权才能读正文。
        val uri = synchronized(rememberedDocumentLock) { storedDocuments()[scope] } ?: return null
        if (uri.scheme != ContentResolver.SCHEME_CONTENT || !hasReadGrant(uri)) {
            throw DocumentAccessLost()
        }
        return try {
            readSelectedDocument(resolver, uri) + ("persisted" to true)
        } catch (_: SecurityException) {
            throw DocumentAccessLost()
        }
    }

    private fun remember(scope: String, uri: Uri, intentFlags: Int): Boolean = synchronized(rememberedDocumentLock) {
        // 1. 使用选择结果提供的授权，只延长读取权限。
        val documents = storedDocuments()
        val previous = documents[scope]
        val alreadyGranted = hasReadGrant(uri)
        val persistent = alreadyGranted || takeReadGrant(uri, intentFlags)
        val replacement = uri.takeIf { persistent }
        if (previous == replacement) return@synchronized persistent

        // 2. 各范围分别保留一条引用，原子更新当前范围的记录。
        if (replacement == null) documents.remove(scope) else documents[scope] = replacement
        val serialized = JSONObject()
        documents.forEach { (owner, reference) -> serialized.put(owner, reference.toString()) }
        try {
            val output = savedDocument.startWrite()
            try {
                output.write(serialized.toString().toByteArray(Charsets.UTF_8))
                savedDocument.finishWrite(output)
            } catch (failure: IOException) {
                savedDocument.failWrite(output)
                throw failure
            }
            if (storedDocuments() != documents) throw IOException("Could not save the document reference")
        } catch (failure: Exception) {
            if (persistent && !alreadyGranted) releaseReadGrant(uri)
            throw failure
        }
        // 3. URI 授权属于应用。若其他范围仍引用旧 URI，就保留授权。
        if (previous != null && previous !in documents.values) releaseReadGrant(previous)
        persistent
    }

    private fun storedDocuments(): MutableMap<String, Uri> {
        val data = try {
            String(savedDocument.readFully(), Charsets.UTF_8)
        } catch (_: FileNotFoundException) {
            return mutableMapOf()
        }
        if (data.isEmpty()) return mutableMapOf()
        // 旧版的单一引用缺少账号信息，只允许在访客范围读取。
        if (!data.startsWith('{')) return mutableMapOf(GUEST_SCOPE to Uri.parse(data))
        val stored = JSONObject(data)
        return stored.keys().asSequence().associateWithTo(mutableMapOf()) { key ->
            val value = stored.get(key) as? String ?: throw IOException("Invalid document reference")
            Uri.parse(value)
        }
    }

    private fun hasReadGrant(uri: Uri): Boolean =
        resolver.persistedUriPermissions.any { it.isReadPermission && it.uri == uri }

    private fun takeReadGrant(uri: Uri, intentFlags: Int): Boolean {
        val readFlag = Intent.FLAG_GRANT_READ_URI_PERMISSION
        val requiredFlags = readFlag or Intent.FLAG_GRANT_PERSISTABLE_URI_PERMISSION
        if ((intentFlags and requiredFlags) != requiredFlags) return false
        return try {
            resolver.takePersistableUriPermission(uri, readFlag)
            hasReadGrant(uri)
        } catch (_: SecurityException) {
            false
        }
    }

    private fun releaseReadGrant(uri: Uri) {
        try {
            resolver.releasePersistableUriPermission(uri, Intent.FLAG_GRANT_READ_URI_PERMISSION)
        } catch (_: SecurityException) {
            // 授权也可能已经被系统或文档提供方撤回。
        }
    }
}
