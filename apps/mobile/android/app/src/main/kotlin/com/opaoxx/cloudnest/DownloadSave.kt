package com.opaoxx.cloudnest

import android.app.Activity
import android.content.ContentResolver
import android.content.Intent
import android.webkit.MimeTypeMap
import androidx.activity.result.ActivityResult
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.util.concurrent.Executors

internal class DownloadSave(
    private val activity: Activity,
    private val launch: (Intent) -> Unit,
) {
    private data class Request(val result: MethodChannel.Result, var staged: StagedDownload? = null)

    private val worker = Executors.newSingleThreadExecutor()
    private val files by lazy { VerifiedDownload(activity.cacheDir, activity.contentResolver) }
    private var active: Request? = null

    fun handle(call: MethodCall, result: MethodChannel.Result) {
        if (call.method != "downloadAndSave") {
            result.notImplemented()
            return
        }
        if (active != null) {
            result.error("download_busy", null, null)
            return
        }
        val url = call.argument<String>("url")
        val name = call.argument<String>("name")
        val size = call.argument<Number>("sizeBytes")?.toLong()
        val digest = call.argument<String>("sha256")
        if (url.isNullOrEmpty() || name.isNullOrBlank() || size == null ||
            size !in 0..9_007_199_254_740_991L || digest == null || !digest.matches(Regex("[0-9a-f]{64}"))
        ) {
            result.error("download_network", null, null)
            return
        }

        val request = Request(result)
        active = request
        // 下载和校验在工作线程中完成，系统选择器只接收已验证的文件。
        worker.execute {
            val staged = try {
                files.stage(url, SourceSummary(name, size, digest))
            } catch (failure: Exception) {
                replyFailure(request, failure, "download_network")
                return@execute
            }
            activity.runOnUiThread {
                if (active !== request) {
                    staged.file.delete()
                    return@runOnUiThread
                }
                request.staged = staged
                try {
                    launch(Intent(Intent.ACTION_CREATE_DOCUMENT).apply {
                        addCategory(Intent.CATEGORY_OPENABLE)
                        type = MimeTypeMap.getSingleton().getMimeTypeFromExtension(name.substringAfterLast('.', "").lowercase())
                            ?: "application/octet-stream"
                        putExtra(Intent.EXTRA_TITLE, name)
                        addFlags(Intent.FLAG_GRANT_WRITE_URI_PERMISSION)
                    })
                } catch (_: RuntimeException) {
                    finish(request) { error("download_picker_unavailable", null, null) }
                }
            }
        }
    }

    fun onDestination(selection: ActivityResult) {
        val request = active ?: return
        val staged = request.staged ?: return
        if (selection.resultCode != Activity.RESULT_OK) {
            finish(request) { success(null) }
            return
        }
        val uri = selection.data?.data
        if (uri == null || uri.scheme != ContentResolver.SCHEME_CONTENT) {
            finish(request) { error("download_save_failed", null, null) }
            return
        }
        worker.execute {
            val name = try {
                files.save(staged, uri)
            } catch (failure: Exception) {
                replyFailure(request, failure, "download_save_incomplete")
                return@execute
            }
            activity.runOnUiThread { finish(request) { success(mapOf("name" to name)) } }
        }
    }

    private fun replyFailure(request: Request, failure: Exception, fallback: String) {
        val code = (failure as? DownloadFailure)?.code ?: fallback
        activity.runOnUiThread { finish(request) { error(code, null, null) } }
    }

    private fun finish(request: Request, respond: MethodChannel.Result.() -> Unit) {
        if (active !== request) return
        active = null
        request.staged?.file?.delete()
        request.result.respond()
    }

    fun detach() {
        active?.let { request -> finish(request) { error("download_closed", null, null) } }
    }

    fun close() {
        detach()
        worker.shutdownNow()
    }
}
