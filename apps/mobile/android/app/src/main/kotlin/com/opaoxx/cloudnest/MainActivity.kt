package com.opaoxx.cloudnest

import android.app.Activity
import android.content.ContentResolver
import android.content.Intent
import androidx.activity.result.ActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import io.flutter.embedding.android.FlutterFragmentActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import java.util.concurrent.Executors

class MainActivity : FlutterFragmentActivity() {
    private val documentReader = Executors.newSingleThreadExecutor()
    private var documentChannel: MethodChannel? = null
    private var pendingSelection: MethodChannel.Result? = null
    private val openDocument = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
        ::onDocumentSelected,
    )

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)
        documentChannel = MethodChannel(
            flutterEngine.dartExecutor.binaryMessenger,
            "cloudnest/android_document",
        ).apply {
            setMethodCallHandler { call, result ->
                if (call.method == "pickAndSummarize") {
                    selectDocument(result)
                } else {
                    result.notImplemented()
                }
            }
        }
    }

    private fun selectDocument(result: MethodChannel.Result) {
        // 1. 让系统只展示可作为文件流打开的文档。
        if (pendingSelection != null) {
            result.error("selection_in_progress", null, null)
            return
        }
        pendingSelection = result
        val intent = Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
            type = "*/*"
        }
        try {
            openDocument.launch(intent)
        } catch (_: RuntimeException) {
            finishSelection(result) { error("picker_unavailable", null, null) }
        }
    }

    private fun onDocumentSelected(selection: ActivityResult) {
        val result = pendingSelection ?: return

        // 2. 取消直接返回。选中后通过 Android 授权的内容 URI 读取。
        if (selection.resultCode != Activity.RESULT_OK) {
            finishSelection(result) { success(null) }
            return
        }
        val uri = selection.data?.data
        if (uri == null || uri.scheme != ContentResolver.SCHEME_CONTENT) {
            finishSelection(result) { error("invalid_document", null, null) }
            return
        }

        // 3. 让后台线程读取，回到主线程交付单次摘要。
        val resolver = contentResolver
        documentReader.execute {
            val summary = try {
                readSelectedDocument(resolver, uri)
            } catch (_: Exception) {
                null
            }
            runOnUiThread {
                finishSelection(result) {
                    if (summary == null) error("document_read_failed", null, null)
                    else success(summary)
                }
            }
        }
    }

    private fun finishSelection(
        result: MethodChannel.Result,
        reply: MethodChannel.Result.() -> Unit,
    ) {
        if (pendingSelection !== result) return
        pendingSelection = null
        result.reply()
    }

    override fun cleanUpFlutterEngine(flutterEngine: FlutterEngine) {
        documentChannel?.setMethodCallHandler(null)
        documentChannel = null
        pendingSelection?.error("document_picker_closed", null, null)
        pendingSelection = null
        super.cleanUpFlutterEngine(flutterEngine)
    }

    override fun onDestroy() {
        documentReader.shutdownNow()
        super.onDestroy()
    }
}
