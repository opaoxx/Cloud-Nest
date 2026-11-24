package com.opaoxx.cloudnest

import android.app.Activity
import android.content.ContentResolver
import android.content.Intent
import androidx.activity.result.ActivityResult
import androidx.activity.result.ActivityResultLauncher
import androidx.activity.result.contract.ActivityResultContracts
import io.flutter.embedding.android.FlutterFragmentActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.StandardMethodCodec
import java.util.concurrent.Executors

class MainActivity : FlutterFragmentActivity() {
    private val documentReader = Executors.newSingleThreadExecutor()
    private val rememberedDocument by lazy { RememberedDocument(applicationContext) }
    private var installationChannel: MethodChannel? = null
    private var sessionChannel: MethodChannel? = null
    private var taskChannel: MethodChannel? = null
    private var downloadChannel: MethodChannel? = null
    private var documentChannel: MethodChannel? = null
    private val downloadSave: DownloadSave by lazy { DownloadSave(this) { saveDestination.launch(it) } }
    private data class DocumentRequest(
        val scope: String,
        val result: MethodChannel.Result,
        val expected: SourceSummary? = null,
    )
    private var pendingDocumentRequest: DocumentRequest? = null
    private val openDocument = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
        ::onDocumentSelected,
    )
    private val saveDestination: ActivityResultLauncher<Intent> = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { downloadSave.onDestination(it) }

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        val messenger = flutterEngine.dartExecutor.binaryMessenger
        // 安装标识需要访盘，由 Flutter 的后台消息队列处理。
        installationChannel = MethodChannel(
            messenger,
            "cloudnest/android_installation",
            StandardMethodCodec.INSTANCE,
            messenger.makeBackgroundTaskQueue(),
        ).apply {
            val identity = InstallationIdentity(applicationContext)
            setMethodCallHandler { call, result ->
                when (call.method) {
                    "getInstallationId" -> try {
                        result.success(identity.getOrCreate())
                    } catch (_: Exception) {
                        result.error("installation_id_unavailable", null, null)
                    }
                    else -> result.notImplemented()
                }
            }
        }
        sessionChannel = MethodChannel(
            messenger,
            "cloudnest/android_session",
            StandardMethodCodec.INSTANCE,
            messenger.makeBackgroundTaskQueue(),
        ).apply {
            val credentials by lazy { SessionCredentialStore(applicationContext) }
            setMethodCallHandler { call, result ->
                try {
                    when (call.method) {
                        "read" -> result.success(credentials.read())
                        "write" -> {
                            val credential = call.arguments as? String
                                ?: throw IllegalArgumentException("Invalid session record")
                            credentials.write(credential)
                            result.success(null)
                        }
                        "clear" -> {
                            credentials.clear()
                            result.success(null)
                        }
                        else -> result.notImplemented()
                    }
                } catch (_: Exception) {
                    result.error("session_store_unavailable", null, null)
                }
            }
        }
        taskChannel = MethodChannel(
            messenger,
            "cloudnest/android_tasks",
            StandardMethodCodec.INSTANCE,
            messenger.makeBackgroundTaskQueue(),
        ).apply {
            setMethodCallHandler { call, result ->
                if (call.method !in setOf(
                        "list", "createTest", "reserveUpload", "checkpointSingle", "checkpointMultipart",
                        "readMultipartParts", "recordMultipartPart", "reconcileMultipartParts",
                        "saveUploadOutcome", "pendingMultipart", "reserveDownload", "pendingDownload",
                        "finishDownload", "setPauseReason", "cancellationTask", "recordUploadCancelling",
                        "finishDownloadCancellation",
                    )
                ) {
                    result.notImplemented()
                    return@setMethodCallHandler
                }
                try {
                    val owner = TaskOwner(
                        requireNotNull(call.argument<String>("server")),
                        requireNotNull(call.argument<String>("userId")),
                    )
                    val store = MobileTaskStore(applicationContext)
                    val rows = try {
                        when (call.method) {
                            "createTest" -> store.createTest(owner)
                            "pendingMultipart" -> store.pendingMultipart(owner, requireNotNull(call.argument<String>("id")))
                            "pendingDownload" -> store.pendingDownload(owner, requireNotNull(call.argument<String>("id")))
                            "cancellationTask" -> store.cancellationTask(
                                owner, requireNotNull(call.argument<String>("id")), call.argument<Boolean>("submit") == true,
                            )
                            "recordUploadCancelling" -> {
                                store.recordUploadCancelling(
                                    owner, requireNotNull(call.argument<String>("id")),
                                    requireNotNull(call.argument<String>("remoteId")),
                                )
                                null
                            }
                            "finishDownloadCancellation" -> {
                                store.finishDownloadCancellation(owner, requireNotNull(call.argument<String>("id")))
                                null
                            }
                            "reserveDownload" -> store.reserveDownload(call.downloadIdentity(owner))
                            "setPauseReason" -> {
                                store.setPauseReason(
                                    owner, requireNotNull(call.argument<String>("id")), call.argument<String>("reason"),
                                )
                                null
                            }
                            "reserveUpload" -> store.reserveUpload(
                                owner,
                                UploadDeclaration(
                                    requireNotNull(call.argument<String>("parentId")),
                                    requireNotNull(call.argument<String>("name")),
                                    requireNotNull(call.argument<Number>("sizeBytes")).toLong(),
                                    requireNotNull(call.argument<String>("sha256")),
                                ),
                            )
                            "checkpointSingle" -> {
                                store.checkpointSingle(
                                    owner,
                                    requireNotNull(call.argument<String>("id")),
                                    call.argument<String>("remoteId"),
                                    requireNotNull(call.argument<String>("state")),
                                    call.argument<String>("entryId"),
                                )
                                null
                            }
                            "checkpointMultipart" -> {
                                store.checkpointMultipart(
                                    owner,
                                    requireNotNull(call.argument<String>("id")),
                                    MultipartUploadCheckpoint(
                                        requireNotNull(call.argument<String>("remoteId")),
                                        requireNotNull(call.argument<Number>("sizeBytes")).toLong(),
                                        requireNotNull(call.argument<Number>("partSizeBytes")).toLong(),
                                        requireNotNull(call.argument<Int>("partCount")),
                                        requireNotNull(call.argument<String>("expiresAt")),
                                    ),
                                )
                                null
                            }
                            "readMultipartParts" -> store.readMultipartParts(
                                owner,
                                requireNotNull(call.argument<String>("id")),
                                requireNotNull(call.argument<String>("remoteId")),
                            )
                            "recordMultipartPart" -> {
                                store.recordMultipartPart(
                                    owner,
                                    requireNotNull(call.argument<String>("id")),
                                    requireNotNull(call.argument<String>("remoteId")),
                                    parseMultipartReceipt(call.arguments as Map<*, *>),
                                )
                                null
                            }
                            "reconcileMultipartParts" -> {
                                val rows = requireNotNull(call.argument<List<Map<*, *>>>("receipts"))
                                store.reconcileMultipartParts(
                                    owner,
                                    requireNotNull(call.argument<String>("id")),
                                    requireNotNull(call.argument<String>("remoteId")),
                                    rows.map(::parseMultipartReceipt),
                                )
                                null
                            }
                            "saveUploadOutcome" -> {
                                store.saveUploadOutcome(
                                    owner,
                                    requireNotNull(call.argument<String>("id")),
                                    requireNotNull(call.argument<String>("remoteId")),
                                    requireNotNull(call.argument<String>("state")),
                                    call.argument<String>("entryId"),
                                    call.argument<String>("errorCode"),
                                )
                                null
                            }
                            "finishDownload" -> {
                                store.finishDownload(
                                    owner,
                                    requireNotNull(call.argument<String>("id")),
                                    call.argument<String>("name"),
                                )
                                null
                            }
                            else -> store.list(owner)
                        }
                    } finally {
                        store.close()
                    }
                    result.success(rows)
                } catch (_: Exception) {
                    result.error("task_store_unavailable", null, null)
                }
            }
        }
        downloadChannel = MethodChannel(messenger, "cloudnest/android_download").apply {
            setMethodCallHandler(downloadSave::handle)
        }
        documentChannel = MethodChannel(
            messenger,
            "cloudnest/android_document",
        ).apply {
            setMethodCallHandler { call, result ->
                val scope = if (call.arguments is Map<*, *>) call.argument<String>("scope")
                    else call.arguments as? String
                if (scope.isNullOrEmpty()) {
                    result.error("document_scope_required", null, null)
                    return@setMethodCallHandler
                }
                when (call.method) {
                    "pickAndSummarize" -> selectDocument(DocumentRequest(scope, result))
                    "restoreDocument" -> restoreDocument(DocumentRequest(scope, result))
                    "clearTaskSource" -> {
                        val request = DocumentRequest(scope, result)
                        if (beginRequest(request)) readOnWorker(request, "document_read_failed") {
                            rememberedDocument.clearTaskSource(scope)
                            null
                        }
                    }
                    "pickTaskSource", "bindTaskSource", "snapshotForSingle", "inspectMultipart", "putMultipartPart" -> {
                        val name = call.argument<String>("name")
                        val size = call.argument<Number>("sizeBytes")?.toLong()
                        val sha = call.argument<String>("sha256")
                        if (name == null || size == null || size < 0 || sha == null ||
                            (call.method == "snapshotForSingle" && size > MAX_SINGLE_UPLOAD_BYTES)
                        ) {
                            result.error("document_source_changed", null, null)
                            return@setMethodCallHandler
                        }
                        val source = SourceSummary(name, size, sha)
                        val request = DocumentRequest(scope, result, source)
                        if (call.method == "pickTaskSource") {
                            selectDocument(request)
                            return@setMethodCallHandler
                        }
                        val error = if (call.method == "putMultipartPart") "multipart_put_unconfirmed"
                            else "document_read_failed"
                        if (beginRequest(request)) readOnWorker(request, error) {
                            readUploadSource(call, scope, source)
                        }
                    }
                    else -> result.notImplemented()
                }
            }
        }
    }

    private fun parseMultipartReceipt(arguments: Map<*, *>): MultipartPartReceipt = MultipartPartReceipt(
        arguments["number"] as Int,
        (arguments["sizeBytes"] as Number).toLong(),
        arguments["sha256"] as String,
        arguments["etag"] as String,
    )

    private fun readUploadSource(call: MethodCall, scope: String, source: SourceSummary): Map<String, Any> =
        when (call.method) {
            "bindTaskSource" -> {
                rememberedDocument.bindTaskSource(scope, requireNotNull(call.argument<String>("selectedScope")), source)
                emptyMap()
            }
            "snapshotForSingle" -> rememberedDocument.snapshotForSingle(scope, source)
            "inspectMultipart" -> rememberedDocument.openMultipartBody(scope, source).use { input ->
                mapOf("partDigests" to multipartDigests(
                    input, source,
                    requireNotNull(call.argument<Number>("partSizeBytes")).toLong(),
                    requireNotNull(call.argument<Int>("partCount")),
                ))
            }
            else -> rememberedDocument.openMultipartBody(scope, source).use { input ->
                mapOf("etag" to putMultipartBody(
                    input, source.sizeBytes,
                    requireNotNull(call.argument<Number>("offsetBytes")).toLong(),
                    requireNotNull(call.argument<Number>("lengthBytes")).toLong(),
                    requireNotNull(call.argument<String>("url")),
                    requireNotNull(call.argument<String>("checksum")),
                ))
            }
        }

    private fun selectDocument(request: DocumentRequest) {
        // 1. 让系统只展示可作为文件流打开的文档。
        if (!beginRequest(request)) return
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            type = "*/*"
        }
        try {
            openDocument.launch(intent)
        } catch (_: RuntimeException) {
            finishRequest(request) { error("picker_unavailable", null, null) }
        }
    }

    private fun onDocumentSelected(selection: ActivityResult) {
        val request = pendingDocumentRequest ?: return

        // 2. 取消直接返回。选中后通过 Android 授权的内容 URI 读取。
        if (selection.resultCode != Activity.RESULT_OK) {
            finishRequest(request) { success(null) }
            return
        }
        val selectedIntent = selection.data
        val uri = selectedIntent?.data
        if (uri == null || uri.scheme != ContentResolver.SCHEME_CONTENT) {
            finishRequest(request) { error("invalid_document", null, null) }
            return
        }
        val intentFlags = selectedIntent?.flags ?: 0

        readOnWorker(request, "document_read_failed") {
            rememberedDocument.readSelection(request.scope, uri, intentFlags, request.expected)
        }
    }

    private fun restoreDocument(request: DocumentRequest) {
        if (!beginRequest(request)) return
        readOnWorker(request, "document_restore_failed") { rememberedDocument.restore(request.scope) }
    }

    private fun beginRequest(request: DocumentRequest): Boolean {
        if (pendingDocumentRequest != null) {
            request.result.error("selection_in_progress", null, null)
            return false
        }
        pendingDocumentRequest = request
        return true
    }

    private fun readOnWorker(
        request: DocumentRequest,
        defaultErrorCode: String,
        read: () -> Map<String, Any>?,
    ) {
        // 后台线程处理正文和授权记录，在主线程交付单次结果。
        documentReader.execute {
            var summary: Map<String, Any>? = null
            val errorCode = try {
                summary = read()
                null
            } catch (_: DocumentAccessLost) {
                "document_access_lost"
            } catch (_: UploadSourceChanged) {
                "document_source_changed"
            } catch (paused: UploadSourcePaused) {
                paused.reason
            } catch (_: MultipartSignatureExpired) {
                "multipart_signature_expired"
            } catch (_: Exception) {
                defaultErrorCode
            }
            runOnUiThread {
                finishRequest(request) {
                    if (errorCode == null) success(summary)
                    else error(errorCode, null, null)
                }
            }
        }
    }

    private fun finishRequest(
        request: DocumentRequest,
        reply: MethodChannel.Result.() -> Unit,
    ) {
        if (pendingDocumentRequest !== request) return
        pendingDocumentRequest = null
        request.result.reply()
    }

    override fun cleanUpFlutterEngine(flutterEngine: FlutterEngine) {
        installationChannel?.setMethodCallHandler(null)
        installationChannel = null
        sessionChannel?.setMethodCallHandler(null)
        sessionChannel = null
        taskChannel?.setMethodCallHandler(null)
        taskChannel = null
        downloadChannel?.setMethodCallHandler(null)
        downloadChannel = null
        downloadSave.detach()
        documentChannel?.setMethodCallHandler(null)
        documentChannel = null
        pendingDocumentRequest?.result?.error("document_picker_closed", null, null)
        pendingDocumentRequest = null
        super.cleanUpFlutterEngine(flutterEngine)
    }

    override fun onDestroy() {
        documentReader.shutdownNow()
        downloadSave.close()
        super.onDestroy()
    }
}
