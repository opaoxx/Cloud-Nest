package com.opaoxx.cloudnest

import android.app.Activity
import android.content.ContentResolver
import android.content.Intent
import androidx.activity.result.ActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import io.flutter.embedding.android.FlutterFragmentActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import io.flutter.plugin.common.StandardMethodCodec
import java.util.concurrent.Executors

class MainActivity : FlutterFragmentActivity() {
    private val documentReader = Executors.newSingleThreadExecutor()
    private val rememberedDocument by lazy { RememberedDocument(applicationContext) }
    private var installationChannel: MethodChannel? = null
    private var documentChannel: MethodChannel? = null
    private var pendingDocumentRequest: MethodChannel.Result? = null
    private val openDocument = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
        ::onDocumentSelected,
    )

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
        documentChannel = MethodChannel(
            messenger,
            "cloudnest/android_document",
        ).apply {
            setMethodCallHandler { call, result ->
                when (call.method) {
                    "pickAndSummarize" -> selectDocument(result)
                    "restoreDocument" -> restoreDocument(result)
                    else -> result.notImplemented()
                }
            }
        }
    }

    private fun selectDocument(result: MethodChannel.Result) {
        // 1. 让系统只展示可作为文件流打开的文档。
        if (!beginRequest(result)) return
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            type = "*/*"
        }
        try {
            openDocument.launch(intent)
        } catch (_: RuntimeException) {
            finishRequest(result) { error("picker_unavailable", null, null) }
        }
    }

    private fun onDocumentSelected(selection: ActivityResult) {
        val result = pendingDocumentRequest ?: return

        // 2. 取消直接返回。选中后通过 Android 授权的内容 URI 读取。
        if (selection.resultCode != Activity.RESULT_OK) {
            finishRequest(result) { success(null) }
            return
        }
        val selectedIntent = selection.data
        val uri = selectedIntent?.data
        if (uri == null || uri.scheme != ContentResolver.SCHEME_CONTENT) {
            finishRequest(result) { error("invalid_document", null, null) }
            return
        }
        val intentFlags = selectedIntent?.flags ?: 0

        readOnWorker(result, "document_read_failed") {
            rememberedDocument.readSelection(uri, intentFlags)
        }
    }

    private fun restoreDocument(result: MethodChannel.Result) {
        if (!beginRequest(result)) return
        readOnWorker(result, "document_restore_failed") { rememberedDocument.restore() }
    }

    private fun beginRequest(result: MethodChannel.Result): Boolean {
        if (pendingDocumentRequest != null) {
            result.error("selection_in_progress", null, null)
            return false
        }
        pendingDocumentRequest = result
        return true
    }

    private fun readOnWorker(
        result: MethodChannel.Result,
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
            } catch (_: Exception) {
                defaultErrorCode
            }
            runOnUiThread {
                finishRequest(result) {
                    if (errorCode == null) success(summary)
                    else error(errorCode, null, null)
                }
            }
        }
    }

    private fun finishRequest(
        result: MethodChannel.Result,
        reply: MethodChannel.Result.() -> Unit,
    ) {
        if (pendingDocumentRequest !== result) return
        pendingDocumentRequest = null
        result.reply()
    }

    override fun cleanUpFlutterEngine(flutterEngine: FlutterEngine) {
        installationChannel?.setMethodCallHandler(null)
        installationChannel = null
        documentChannel?.setMethodCallHandler(null)
        documentChannel = null
        pendingDocumentRequest?.error("document_picker_closed", null, null)
        pendingDocumentRequest = null
        super.cleanUpFlutterEngine(flutterEngine)
    }

    override fun onDestroy() {
        documentReader.shutdownNow()
        super.onDestroy()
    }
}
