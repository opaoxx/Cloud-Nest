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
private val sha256Pattern = Regex("[0-9a-f]{64}")

internal class RememberedDocument(context: Context) {
    private val resolver = context.contentResolver
    private val savedDocument = AtomicFile(File(context.noBackupFilesDir, "remembered_document"))
    private val sources = PreparedUploadSources(resolver, File(context.noBackupFilesDir, "upload_sources"))

    fun readSelection(scope: String, uri: Uri, intentFlags: Int): Map<String, Any> = synchronized(rememberedDocumentLock) {
        val documents = storedDocuments()
        pruneStages(documents)
        var tookReadGrant = false
        try {
            val prepared = sources.prepare(uri) {
                hasReadGrant(uri) || takeReadGrant(uri, intentFlags).also { tookReadGrant = it }
            }
            save(scope, documents, prepared)
            prepared.view()
        } finally {
            // 文件与元数据分开落盘。按实际保存成功的引用回收孤立副本和新申请的授权。
            val saved = cleanupUnreferenced()
            if (tookReadGrant && saved != null && saved.values.none { it.uri == uri }) releaseReadGrant(uri)
        }
    }

    fun restore(scope: String): Map<String, Any>? = synchronized(rememberedDocumentLock) {
        // 1. 先清理未提交的副本，再按账号恢复它自己的来源。
        val documents = storedDocuments()
        pruneStages(documents)
        val previous = documents[scope] ?: return@synchronized null
        if (previous.stageId != null) {
            sources.verifyStage(previous)
            return@synchronized previous.view()
        }
        val uri = checkNotNull(previous.uri)
        if (uri.scheme != ContentResolver.SCHEME_CONTENT || !hasReadGrant(uri)) {
            throw DocumentAccessLost()
        }
        try {
            // 2. 重新读取直接来源，比较上次确认的字节数和摘要。
            val prepared = sources.prepare(uri) { true }
            if (previous.summary?.matches(checkNotNull(prepared.summary)) == false) throw UploadSourceChanged()
            save(scope, documents, prepared)
            prepared.view()
        } catch (_: SecurityException) {
            throw DocumentAccessLost()
        } finally {
            cleanupUnreferenced()
        }
    }

    private fun save(
        scope: String,
        documents: MutableMap<String, UploadSourceReference>,
        replacement: UploadSourceReference,
    ) {
        val previous = documents[scope]
        if (previous == replacement) return

        // 3. 各账号分别保存来源。原子记录写入后才释放旧引用。
        documents[scope] = replacement
        val serialized = JSONObject()
        documents.forEach { (owner, reference) -> serialized.put(owner, encodeReference(reference)) }
        val output = savedDocument.startWrite()
        try {
            output.write(serialized.toString().toByteArray(Charsets.UTF_8))
            savedDocument.finishWrite(output)
        } catch (failure: IOException) {
            savedDocument.failWrite(output)
            throw failure
        }
        if (storedDocuments() != documents) throw IOException("Could not save the document reference")
        val oldUri = previous?.uri
        if (oldUri != null && documents.values.none { it.uri == oldUri }) releaseReadGrant(oldUri)
    }

    private fun cleanupUnreferenced(): Map<String, UploadSourceReference>? {
        val documents = runCatching { storedDocuments() }.getOrNull() ?: return null
        pruneStages(documents)
        return documents
    }

    private fun pruneStages(documents: Map<String, UploadSourceReference>) {
        sources.prune(documents.values.mapNotNull { it.stageId }.toSet())
    }

    private fun storedDocuments(): MutableMap<String, UploadSourceReference> {
        val data = try {
            String(savedDocument.readFully(), Charsets.UTF_8)
        } catch (missing: FileNotFoundException) {
            if (savedDocument.baseFile.exists()) throw missing
            return mutableMapOf()
        }
        if (data.isEmpty()) return mutableMapOf()
        // 旧版的单一引用缺少账号信息，只允许在访客范围读取。
        if (!data.startsWith('{')) return mutableMapOf(GUEST_SCOPE to UploadSourceReference(uri = Uri.parse(data)))
        val stored = JSONObject(data)
        return stored.keys().asSequence().associateWithTo(mutableMapOf()) { key ->
            when (val value = stored.get(key)) {
                is String -> UploadSourceReference(uri = Uri.parse(value))
                is JSONObject -> decodeReference(value)
                else -> throw IOException("Invalid document reference")
            }
        }
    }

    private fun encodeReference(reference: UploadSourceReference): Any {
        val summary = reference.summary ?: return checkNotNull(reference.uri).toString()
        return JSONObject().apply {
            put("name", summary.name)
            put("sizeBytes", summary.sizeBytes)
            put("sha256", summary.sha256)
            if (reference.stageId != null) put("stageId", reference.stageId)
            else put("uri", checkNotNull(reference.uri).toString())
        }
    }

    private fun decodeReference(value: JSONObject): UploadSourceReference {
        val name = value.getString("name")
        val size = value.getLong("sizeBytes")
        val digest = value.getString("sha256")
        val uri = (value.opt("uri") as? String)?.let(Uri::parse)
        val stageId = value.opt("stageId") as? String
        if (name.isBlank() || size < 0 || !sha256Pattern.matches(digest) ||
            (uri == null) == (stageId == null) ||
            (stageId != null && !isStageId(stageId))
        ) {
            throw IOException("Invalid prepared source reference")
        }
        return UploadSourceReference(uri, stageId, SourceSummary(name, size, digest))
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
