package com.opaoxx.cloudnest

import android.content.ContentResolver
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.util.AtomicFile
import java.io.File
import java.io.FileNotFoundException
import java.io.IOException

internal class DocumentAccessLost : IOException()

private val rememberedDocumentLock = Any()

internal class RememberedDocument(context: Context) {
    private val resolver = context.contentResolver
    private val savedDocument = AtomicFile(File(context.noBackupFilesDir, "remembered_document"))

    fun readSelection(uri: Uri, intentFlags: Int): Map<String, Any> {
        val summary = readSelectedDocument(resolver, uri)
        return summary + ("persisted" to remember(uri, intentFlags))
    }

    fun restore(): Map<String, Any>? {
        // 1. 只在系统仍登记了持久读取授权时重新打开原文档。
        val uri = synchronized(rememberedDocumentLock) { storedUri() } ?: return null
        if (uri.scheme != ContentResolver.SCHEME_CONTENT || !hasReadGrant(uri)) {
            throw DocumentAccessLost()
        }
        return try {
            readSelectedDocument(resolver, uri) + ("persisted" to true)
        } catch (_: SecurityException) {
            throw DocumentAccessLost()
        }
    }

    private fun remember(uri: Uri, intentFlags: Int): Boolean = synchronized(rememberedDocumentLock) {
        // 1. 使用选择结果提供的授权，只延长读取权限。
        val previous = storedUri()
        val alreadyGranted = hasReadGrant(uri)
        val persistent = alreadyGranted || takeReadGrant(uri, intentFlags)
        val replacement = uri.takeIf { persistent }
        if (previous == replacement) return@synchronized persistent

        // 2. 本机只留一条引用。新记录落盘后释放上一条的读取授权。
        try {
            val output = savedDocument.startWrite()
            try {
                output.write(replacement?.toString().orEmpty().toByteArray(Charsets.UTF_8))
                savedDocument.finishWrite(output)
            } catch (failure: IOException) {
                savedDocument.failWrite(output)
                throw failure
            }
            if (storedUri() != replacement) throw IOException("Could not save the document reference")
        } catch (failure: IOException) {
            if (persistent && !alreadyGranted) releaseReadGrant(uri)
            throw failure
        }
        if (previous != null) releaseReadGrant(previous)
        persistent
    }

    private fun storedUri(): Uri? {
        val uri = try {
            String(savedDocument.readFully(), Charsets.UTF_8)
        } catch (_: FileNotFoundException) {
            return null
        }
        return uri.takeIf { it.isNotEmpty() }?.let(Uri::parse)
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
