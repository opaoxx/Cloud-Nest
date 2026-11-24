package com.opaoxx.cloudnest

import android.app.Activity
import android.content.ContentResolver
import android.content.Intent
import android.webkit.MimeTypeMap
import androidx.activity.result.ActivityResult
import io.flutter.plugin.common.MethodCall
import io.flutter.plugin.common.MethodChannel
import java.io.File
import java.util.concurrent.Executors

internal class DownloadSave(
    private val activity: Activity,
    private val launch: (Intent) -> Unit,
) {
    private data class Request(
        val result: MethodChannel.Result,
        var staged: StagedDownload? = null,
        var saving: Boolean = false,
        @Volatile var pauseRequested: Boolean = false,
    )

    private val worker = Executors.newSingleThreadExecutor()
    private val directory by lazy { File(activity.noBackupFilesDir, "downloads") }
    private val files by lazy {
        VerifiedDownload(directory, activity.cacheDir, activity.contentResolver)
    }
    private var active: Request? = null

    fun handle(call: MethodCall, result: MethodChannel.Result) {
        if (call.method == "pauseDownload") {
            active?.takeIf { it.staged == null }?.pauseRequested = true
            result.success(null)
            return
        }
        if (call.method != "downloadAndSave" && call.method != "discardDownload") {
            result.notImplemented()
            return
        }
        if (active != null) {
            result.error("download_busy", null, null)
            return
        }
        val url = call.argument<String>("url")
        val identity = runCatching {
            call.downloadIdentity(
                TaskOwner(requireNotNull(call.argument<String>("server")), requireNotNull(call.argument<String>("userId"))),
            )
        }.getOrNull()
        if (identity == null) {
            result.error("download_network", null, null)
            return
        }
        if (call.method == "discardDownload") {
            worker.execute {
                val discarded = runCatching {
                    if (directory.isDirectory) {
                        val checkpoint = DownloadCheckpoint.open(directory, identity)
                        try {
                            checkpoint.discardIfCurrent()
                        } finally {
                            checkpoint.release()
                        }
                    }
                }
                activity.runOnUiThread {
                    if (discarded.isSuccess) result.success(null)
                    else result.error("download_discard_failed", null, null)
                }
            }
            return
        }
        if (url.isNullOrEmpty()) {
            result.error("download_network", null, null)
            return
        }
        val name = identity.source.name

        val request = Request(result)
        active = request
        // 下载和校验在工作线程中完成，系统选择器只接收已验证的文件。
        worker.execute {
            val staged = try {
                files.stage(url, identity) { request.pauseRequested }
            } catch (failure: Exception) {
                replyFailure(request, failure, "download_network")
                return@execute
            }
            activity.runOnUiThread {
                if (active !== request) {
                    release(staged)
                    return@runOnUiThread
                }
                request.staged = staged
                if (request.pauseRequested) {
                    finish(request) { error("download_paused", null, null) }
                    return@runOnUiThread
                }
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
            finish(request, discard = true) { success(null) }
            return
        }
        val uri = selection.data?.data
        if (uri == null || uri.scheme != ContentResolver.SCHEME_CONTENT) {
            finish(request) { error("download_save_failed", null, null) }
            return
        }
        request.saving = true
        worker.execute {
            val saved = runCatching { files.save(staged, uri) }
            activity.runOnUiThread {
                request.saving = false
                if (active !== request) {
                    release(staged, discard = saved.isSuccess)
                } else {
                    finish(request, discard = saved.isSuccess) {
                        saved.fold(
                            onSuccess = { name -> success(mapOf("name" to name)) },
                            onFailure = { failure ->
                                error((failure as? DownloadFailure)?.code ?: "download_save_incomplete", null, null)
                            },
                        )
                    }
                }
            }
        }
    }

    private fun replyFailure(request: Request, failure: Exception, fallback: String) {
        val code = (failure as? DownloadFailure)?.code ?: fallback
        activity.runOnUiThread { finish(request) { error(code, null, null) } }
    }

    private fun finish(request: Request, discard: Boolean = false, respond: MethodChannel.Result.() -> Unit) {
        if (active !== request) return
        active = null
        if (!request.saving) request.staged?.let { release(it, discard) }
        request.result.respond()
    }

    private fun release(staged: StagedDownload, discard: Boolean = false) {
        // 选择器取消或导出成功后回收断点，导出失败可复用已验文件。
        runCatching { staged.checkpoint.release(discard) }
    }

    fun detach() {
        active?.let { request -> finish(request) { error("download_closed", null, null) } }
    }

    fun close() {
        detach()
        worker.shutdown()
    }
}

internal fun MethodCall.downloadIdentity(owner: TaskOwner): DownloadIdentity = DownloadIdentity(
    owner,
    requireNotNull(argument<String>("entryId")),
    requireNotNull(argument<String>("revision")),
    requireNotNull(argument<String>("objectId")),
    SourceSummary(
        requireNotNull(argument<String>("name")),
        requireNotNull(argument<Number>("sizeBytes")).toLong(),
        requireNotNull(argument<String>("sha256")),
    ),
)
