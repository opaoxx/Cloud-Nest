package com.opaoxx.cloudnest

import android.content.ContentValues
import android.content.Context
import android.database.sqlite.SQLiteDatabase
import android.database.sqlite.SQLiteException
import android.database.sqlite.SQLiteOpenHelper
import java.io.File
import java.util.UUID

private const val TASKS = "transfer_tasks"
private const val OWNER_FILTER = "server_origin = ? AND user_id = ?"
private const val TEST_KIND = "TEST"
private const val UPLOAD_KIND = "UPLOAD"
private const val MAX_UPLOAD_PROTOCOL_BYTES = 9007199254740991L
private val taskErrorCodePattern = Regex("[A-Z][A-Z_0-9]{0,63}")
private val pauseReasons = setOf("manual", "source", "target", "authorization", "interrupted", "space")
private const val OPEN_STATES = "state IN ('PENDING', 'CANCEL_PENDING', 'CANCELLING')"
private const val CANCELLATION_STATES = "state IN ('CANCEL_PENDING', 'CANCELLING')"

internal data class TaskOwner(val server: String, val userId: String) {
    init {
        require(server.startsWith("https://") || server.startsWith("http://"))
        require(userId != "0" && userId.toULongOrNull()?.toString() == userId)
    }

    fun selectionArgs() = arrayOf(server, userId)
}

internal data class UploadDeclaration(
    val parentId: String,
    val name: String,
    val sizeBytes: Long,
    val sha256: String,
) {
    init {
        require(validTaskNumber(parentId) && name.isNotBlank())
        require(sizeBytes in 0L..MAX_UPLOAD_PROTOCOL_BYTES)
        require(Regex("[0-9a-f]{64}").matches(sha256))
    }
}

internal data class MultipartUploadCheckpoint(
    val remoteId: String,
    val sizeBytes: Long,
    val partSizeBytes: Long,
    val partCount: Int,
    val expiresAt: String,
) {
    init {
        require(validTaskNumber(remoteId) && expiresAt.isNotBlank())
        require(sizeBytes in (MAX_SINGLE_UPLOAD_BYTES + 1)..MAX_UPLOAD_PROTOCOL_BYTES)
        require(partSizeBytes in MAX_SINGLE_UPLOAD_BYTES..MAX_UPLOAD_PROTOCOL_BYTES)
        require(partSizeBytes % (1024 * 1024) == 0L)
        require(partCount in 1..10000 && (sizeBytes - 1) / partSizeBytes + 1 == partCount.toLong())
    }
}

internal data class MultipartPartReceipt(
    val number: Int,
    val sizeBytes: Long,
    val sha256: String,
    val etag: String,
) {
    init {
        require(number in 1..10000 && sizeBytes in 1L..MAX_UPLOAD_PROTOCOL_BYTES)
        require(multipartDigestPattern.matches(sha256) && multipartETagPattern.matches(etag))
    }
}

private fun validTaskNumber(value: String): Boolean =
    value != "0" && value.toULongOrNull()?.toString() == value

internal class MobileTaskStore(context: Context) : SQLiteOpenHelper(
    context,
    File(context.noBackupFilesDir, "mobile_tasks.sqlite3").absolutePath,
    null,
    8,
    // 损坏时报告错误并保留原始文件，避免自动删除任务记录。
    { _ -> },
) {
    override fun onCreate(database: SQLiteDatabase) {
        database.execSQL(
            """
            CREATE TABLE transfer_tasks (
                local_task_id TEXT NOT NULL PRIMARY KEY,
                server_origin TEXT NOT NULL,
                user_id TEXT NOT NULL,
                kind TEXT NOT NULL,
                name TEXT NOT NULL,
                state TEXT NOT NULL,
                created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0)
            )
            """.trimIndent(),
        )
        database.execSQL(
            "CREATE INDEX transfer_tasks_by_owner ON transfer_tasks(server_origin, user_id, created_at_ms DESC)",
        )
        database.execSQL(
            "CREATE UNIQUE INDEX transfer_tasks_one_test ON transfer_tasks(server_origin, user_id) WHERE kind = 'TEST'",
        )
        addSingleUploadFields(database)
        addMultipartUploadFields(database)
        createPartReceipts(database)
        addUploadOutcomeFields(database)
        addPauseFields(database)
        extendOpenTaskIndexes(database)
        addSubmissionStage(database)
    }

    override fun onUpgrade(database: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
        if (oldVersion !in 1..7 || newVersion != 8) {
            throw SQLiteException("Unsupported task database migration: $oldVersion to $newVersion")
        }
        if (oldVersion == 1) addSingleUploadFields(database)
        if (oldVersion <= 2) addMultipartUploadFields(database)
        if (oldVersion <= 3) createPartReceipts(database)
        if (oldVersion <= 4) addUploadOutcomeFields(database)
        if (oldVersion <= 5) addPauseFields(database)
        if (oldVersion <= 6) extendOpenTaskIndexes(database)
        addSubmissionStage(database)
    }

    private fun addSingleUploadFields(database: SQLiteDatabase) {
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN parent_id TEXT")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN size_bytes INTEGER")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN sha256 TEXT")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN remote_task_id TEXT")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN result_entry_id TEXT")
        database.execSQL(
            """
            CREATE UNIQUE INDEX transfer_tasks_pending_single
            ON transfer_tasks(server_origin, user_id, parent_id, name, size_bytes, sha256)
            WHERE kind = 'UPLOAD' AND state = 'PENDING'
            """.trimIndent(),
        )
    }

    private fun addMultipartUploadFields(database: SQLiteDatabase) {
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN part_size_bytes INTEGER")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN part_count INTEGER")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN expires_at TEXT")
    }

    private fun createPartReceipts(database: SQLiteDatabase) {
        database.execSQL(
            """
            CREATE TABLE multipart_part_receipts (
                local_task_id TEXT NOT NULL REFERENCES transfer_tasks(local_task_id) ON DELETE CASCADE,
                part_number INTEGER NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
                size_bytes INTEGER NOT NULL CHECK (size_bytes > 0),
                sha256 TEXT NOT NULL,
                etag TEXT NOT NULL,
                PRIMARY KEY (local_task_id, part_number)
            )
            """.trimIndent(),
        )
    }

    private fun addUploadOutcomeFields(database: SQLiteDatabase) {
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN error_code TEXT")
    }

    private fun addPauseFields(database: SQLiteDatabase) {
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN pause_reason TEXT")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN download_entry_id TEXT")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN download_revision TEXT")
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN download_object_id TEXT")
        database.execSQL(
            """
            CREATE UNIQUE INDEX transfer_tasks_pending_download
            ON transfer_tasks(server_origin, user_id, download_entry_id)
            WHERE kind = 'DOWNLOAD' AND state = 'PENDING'
            """.trimIndent(),
        )
    }

    private fun extendOpenTaskIndexes(database: SQLiteDatabase) {
        database.execSQL("DROP INDEX transfer_tasks_pending_single")
        database.execSQL("DROP INDEX transfer_tasks_pending_download")
        database.execSQL(
            """
            CREATE UNIQUE INDEX transfer_tasks_open_upload
            ON transfer_tasks(server_origin, user_id, parent_id, name, size_bytes, sha256)
            WHERE kind = 'UPLOAD' AND $OPEN_STATES
            """.trimIndent(),
        )
        database.execSQL(
            """
            CREATE UNIQUE INDEX transfer_tasks_open_download
            ON transfer_tasks(server_origin, user_id, download_entry_id)
            WHERE kind = 'DOWNLOAD' AND $OPEN_STATES
            """.trimIndent(),
        )
    }

    private fun addSubmissionStage(database: SQLiteDatabase) {
        // 旧待确认意图可能已普通创建，升级后沿用原路径和创建键。
        database.execSQL("ALTER TABLE transfer_tasks ADD COLUMN submission_stage TEXT NOT NULL DEFAULT 'CREATE'")
    }

    override fun onConfigure(database: SQLiteDatabase) {
        database.setForeignKeyConstraintsEnabled(true)
    }

    fun reserveUpload(owner: TaskOwner, declaration: UploadDeclaration): Map<String, Any?> {
        val database = writableDatabase
        database.beginTransaction()
        try {
            // 1. 同一账号、目标和文件指纹只能有一条待确认意图。
            val previous = database.query(
                TASKS,
                arrayOf("local_task_id", "remote_task_id", "part_size_bytes", "part_count", "expires_at", "state", "submission_stage"),
                "$OWNER_FILTER AND kind = 'UPLOAD' AND $OPEN_STATES AND parent_id = ? AND name = ? AND size_bytes = ? AND sha256 = ?",
                owner.selectionArgs() + arrayOf(declaration.parentId, declaration.name, declaration.sizeBytes.toString(), declaration.sha256),
                null,
                null,
                null,
            ).use { cursor ->
                if (cursor.moveToFirst()) {
                    mapOf(
                        "id" to cursor.getString(0),
                        "remoteId" to cursor.getString(1),
                        "partSizeBytes" to if (cursor.isNull(2)) null else cursor.getLong(2),
                        "partCount" to if (cursor.isNull(3)) null else cursor.getInt(3),
                        "expiresAt" to cursor.getString(4),
                        "state" to cursor.getString(5),
                        "submissionStage" to cursor.getString(6),
                    )
                } else null
            }
            // 2. 本地 UUID 同时用作创建键。事务提交后，调用方才可以访问服务端。
            val intent = previous ?: run {
                val id = UUID.randomUUID().toString()
                val values = ContentValues().apply {
                    put("local_task_id", id)
                    put("server_origin", owner.server)
                    put("user_id", owner.userId)
                    put("kind", UPLOAD_KIND)
                    put("name", declaration.name)
                    put("state", "PENDING")
                    put("created_at_ms", System.currentTimeMillis())
                    put("parent_id", declaration.parentId)
                    put("size_bytes", declaration.sizeBytes)
                    put("sha256", declaration.sha256)
                    put("submission_stage", "REUSE_READY")
                }
                database.insertOrThrow(TASKS, null, values)
                mapOf("id" to id, "remoteId" to null, "state" to "PENDING", "submissionStage" to "REUSE_READY")
            }
            database.setTransactionSuccessful()
            return intent
        } finally {
            database.endTransaction()
        }
    }

    fun setUploadSubmissionStage(owner: TaskOwner, id: String, stage: String) {
        require(isStageId(id))
        val previous = when (stage) {
            "REUSE_PENDING" -> "REUSE_READY"
            "CREATE" -> "REUSE_PENDING"
            else -> throw IllegalArgumentException("Invalid upload submission stage")
        }
        val values = ContentValues().apply { put("submission_stage", stage) }
        val updated = writableDatabase.update(
            TASKS, values,
            "$OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND state = 'PENDING' AND remote_task_id IS NULL AND submission_stage = ?",
            owner.selectionArgs() + arrayOf(id, previous),
        )
        if (updated != 1) throw SQLiteException("Upload submission stage changed")
    }

    fun saveUploadReuse(owner: TaskOwner, id: String, entryId: String, actualName: String) {
        require(isStageId(id) && validTaskNumber(entryId) && actualName.isNotBlank())
        // 同一次更新保存真实条目、实际名称和终态。
        val values = ContentValues().apply {
            put("submission_stage", "REUSED")
            put("state", "COMPLETED")
            put("result_entry_id", entryId)
            put("name", actualName)
            putNull("pause_reason")
        }
        val updated = writableDatabase.update(
            TASKS, values,
            "$OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND state IN ('PENDING', 'CANCEL_PENDING') AND remote_task_id IS NULL AND submission_stage = 'REUSE_PENDING'",
            owner.selectionArgs() + id,
        )
        if (updated != 1) throw SQLiteException("Reuse receipt did not match its upload")
    }

    fun cancelUncreatedUpload(owner: TaskOwner, id: String) {
        require(isStageId(id))
        val values = ContentValues().apply { put("state", "CANCELLED") }
        val updated = writableDatabase.update(
            TASKS, values,
            "$OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND state = 'CANCEL_PENDING' AND remote_task_id IS NULL AND submission_stage IN ('REUSE_READY', 'REUSE_PENDING')",
            owner.selectionArgs() + id,
        )
        if (updated != 1) throw SQLiteException("Reuse cancellation did not match its upload")
    }

    fun checkpointSingle(owner: TaskOwner, id: String, remoteId: String?, state: String, entryId: String?) {
        require(isStageId(id))
        require(remoteId == null || validTaskNumber(remoteId))
        require(state == "PENDING" || state == "COMPLETED" || state == "FAILED")
        require(if (state == "COMPLETED") remoteId != null && entryId != null && validTaskNumber(entryId) else entryId == null)
        require(state != "PENDING" || remoteId != null)

        // 回执只能更新所属账号的待确认意图，已有远端 ID 时要求一致。
        val values = ContentValues().apply {
            put("remote_task_id", remoteId)
            if (state != "PENDING") put("state", state)
            put("result_entry_id", entryId)
        }
        val remoteFilter = if (remoteId == null) "remote_task_id IS NULL"
            else "(remote_task_id IS NULL OR remote_task_id = ?)"
        val arguments = owner.selectionArgs() + id + listOfNotNull(remoteId)
        val updated = writableDatabase.update(
            TASKS,
            values,
            "$OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND $OPEN_STATES AND $remoteFilter AND size_bytes <= ? AND submission_stage = 'CREATE'",
            arguments + MAX_SINGLE_UPLOAD_BYTES.toString(),
        )
        if (updated != 1) throw SQLiteException("Upload checkpoint did not match a pending intent")
    }

    fun checkpointMultipart(owner: TaskOwner, id: String, checkpoint: MultipartUploadCheckpoint) {
        require(isStageId(id))
        val values = ContentValues().apply {
            put("remote_task_id", checkpoint.remoteId)
            put("part_size_bytes", checkpoint.partSizeBytes)
            put("part_count", checkpoint.partCount)
            put("expires_at", checkpoint.expiresAt)
        }
        // 一次写入整个回执。同一意图可以确认原回执，不能换成另一份计划。
        val updated = writableDatabase.update(
            TASKS,
            values,
            """
            $OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND $OPEN_STATES AND size_bytes = ? AND submission_stage = 'CREATE'
            AND (remote_task_id IS NULL OR
                (remote_task_id = ? AND part_size_bytes = ? AND part_count = ? AND expires_at = ?))
            """.trimIndent(),
            owner.selectionArgs() + arrayOf(
                id, checkpoint.sizeBytes.toString(), checkpoint.remoteId,
                checkpoint.partSizeBytes.toString(), checkpoint.partCount.toString(), checkpoint.expiresAt,
            ),
        )
        if (updated != 1) throw SQLiteException("Multipart checkpoint did not match its upload intent")
    }

    fun saveUploadOutcome(
        owner: TaskOwner, id: String, remoteId: String, state: String, entryId: String?, errorCode: String?,
    ) {
        require(isStageId(id) && validTaskNumber(remoteId))
        require(state in setOf("COMPLETED", "FAILED", "EXPIRED", "CANCELLED"))
        if (state == "COMPLETED") {
            require(entryId != null && validTaskNumber(entryId) && errorCode == null)
        } else {
            require(entryId == null && (errorCode == null || taskErrorCodePattern.matches(errorCode)))
        }
        // 终态和发布结果在同一次更新写入，范围限定为当前账号的原上传。
        val values = ContentValues().apply {
            put("state", state)
            put("result_entry_id", entryId)
            put("error_code", errorCode)
            putNull("pause_reason")
        }
        val updated = writableDatabase.update(
            TASKS,
            values,
            """
            $OWNER_FILTER AND local_task_id = ? AND remote_task_id = ? AND kind = 'UPLOAD'
            AND ($OPEN_STATES OR
                (state = ? AND COALESCE(result_entry_id, '') = ? AND COALESCE(error_code, '') = ?))
            """.trimIndent(),
            owner.selectionArgs() + arrayOf(id, remoteId, state, entryId.orEmpty(), errorCode.orEmpty()),
        )
        if (updated != 1) throw SQLiteException("Upload outcome did not match its original task")
    }

    fun readMultipartParts(owner: TaskOwner, id: String, remoteId: String): List<Map<String, Any>> {
        require(isStageId(id) && validTaskNumber(remoteId))
        return readableDatabase.rawQuery(
            """
            SELECT p.part_number, p.size_bytes, p.sha256, p.etag
            FROM multipart_part_receipts p JOIN transfer_tasks t USING (local_task_id)
            WHERE t.server_origin = ? AND t.user_id = ? AND t.local_task_id = ? AND t.remote_task_id = ?
            ORDER BY p.part_number
            """.trimIndent(),
            owner.selectionArgs() + arrayOf(id, remoteId),
        ).use { cursor ->
            buildList {
                while (cursor.moveToNext()) {
                    add(mapOf(
                        "number" to cursor.getInt(0), "sizeBytes" to cursor.getLong(1),
                        "sha256" to cursor.getString(2), "etag" to cursor.getString(3),
                    ))
                }
            }
        }
    }

    fun recordMultipartPart(owner: TaskOwner, id: String, remoteId: String, receipt: MultipartPartReceipt) {
        require(isStageId(id) && validTaskNumber(remoteId))
        val database = writableDatabase
        database.beginTransaction()
        try {
            // 1. 片号必须属于当前账号的原任务，长度由已保存的计划决定。
            database.execSQL(
                """
                INSERT OR IGNORE INTO multipart_part_receipts (local_task_id, part_number, size_bytes, sha256, etag)
                SELECT local_task_id, ?, ?, ?, ? FROM transfer_tasks
                WHERE $OWNER_FILTER AND local_task_id = ? AND remote_task_id = ?
                AND kind = 'UPLOAD' AND state = 'PENDING' AND ? BETWEEN 1 AND part_count
                AND ? = min(part_size_bytes, size_bytes - (? - 1) * part_size_bytes)
                """.trimIndent(),
                arrayOf<Any>(
                    receipt.number, receipt.sizeBytes, receipt.sha256, receipt.etag,
                    owner.server, owner.userId, id, remoteId,
                    receipt.number, receipt.sizeBytes, receipt.number,
                ),
            )

            // 2. 同一片重复确认时保留原回执。回读含归属校验，可以区分冲突与有效重试。
            val matched = database.rawQuery(
                """
                SELECT 1 FROM multipart_part_receipts p JOIN transfer_tasks t USING (local_task_id)
                WHERE t.server_origin = ? AND t.user_id = ? AND t.local_task_id = ? AND t.remote_task_id = ?
                AND p.part_number = ? AND p.size_bytes = ? AND p.sha256 = ? AND p.etag = ?
                """.trimIndent(),
                owner.selectionArgs() + arrayOf(
                    id, remoteId, receipt.number.toString(), receipt.sizeBytes.toString(), receipt.sha256, receipt.etag,
                ),
            ).use { cursor -> cursor.moveToFirst() }
            if (!matched) throw SQLiteException("Part receipt did not match its upload intent")
            database.setTransactionSuccessful()
        } finally {
            database.endTransaction()
        }
    }

    fun reconcileMultipartParts(owner: TaskOwner, id: String, remoteId: String, receipts: List<MultipartPartReceipt>) {
        require(isStageId(id) && validTaskNumber(remoteId))
        val database = writableDatabase
        database.beginTransaction()
        try {
            // 1. 在写事务内核对归属、阶段和计划，避免替换已经结束的任务。
            database.query(
                TASKS,
                arrayOf("size_bytes", "part_size_bytes", "part_count"),
                "$OWNER_FILTER AND local_task_id = ? AND remote_task_id = ? AND kind = 'UPLOAD' AND state = 'PENDING' AND part_count IS NOT NULL",
                owner.selectionArgs() + arrayOf(id, remoteId),
                null, null, null,
            ).use { cursor ->
                if (!cursor.moveToFirst()) throw SQLiteException("Multipart task not available for reconciliation")
                val size = cursor.getLong(0)
                val partSize = cursor.getLong(1)
                val partCount = cursor.getInt(2)
                require(receipts.size <= partCount && receipts.map { it.number }.toSet().size == receipts.size)
                for (receipt in receipts) {
                    require(receipt.number in 1..partCount)
                    require(receipt.sizeBytes == minOf(partSize, size - (receipt.number - 1L) * partSize))
                }
            }

            // 2. 一次提交删除旧回执并保存已核对的远端 ETag，失败时全部回滚。
            database.delete("multipart_part_receipts", "local_task_id = ?", arrayOf(id))
            for (receipt in receipts) {
                val values = ContentValues().apply {
                    put("local_task_id", id)
                    put("part_number", receipt.number)
                    put("size_bytes", receipt.sizeBytes)
                    put("sha256", receipt.sha256)
                    put("etag", receipt.etag)
                }
                database.insertOrThrow("multipart_part_receipts", null, values)
            }
            database.setTransactionSuccessful()
        } finally {
            database.endTransaction()
        }
    }

    fun list(owner: TaskOwner): List<Map<String, Any?>> = readTasks(readableDatabase, owner)

    fun pendingMultipart(owner: TaskOwner, id: String): Map<String, Any?>? {
        require(isStageId(id))
        return readTasks(readableDatabase, owner, "local_task_id = ? AND kind = 'UPLOAD' AND state = 'PENDING' AND size_bytes > ?",
            arrayOf(id, MAX_SINGLE_UPLOAD_BYTES.toString())).singleOrNull()
    }

    fun pendingDownload(owner: TaskOwner, id: String): Map<String, Any?>? {
        require(isStageId(id))
        return readTasks(readableDatabase, owner, "local_task_id = ? AND kind = 'DOWNLOAD' AND state = 'PENDING'",
            arrayOf(id)).singleOrNull()
    }

    fun cancellationTask(owner: TaskOwner, id: String, submit: Boolean): Map<String, Any?>? {
        require(isStageId(id))
        val database = writableDatabase
        database.beginTransaction()
        try {
            // 先写入取消意图。后续打开原任务只能确认取消，直到取得终态。
            if (submit) {
                val values = ContentValues().apply {
                    put("state", "CANCEL_PENDING")
                    putNull("pause_reason")
                }
                database.update(
                    TASKS, values,
                    "$OWNER_FILTER AND local_task_id = ? AND state = 'PENDING' AND kind IN ('UPLOAD', 'DOWNLOAD')",
                    owner.selectionArgs() + id,
                )
            }
            val task = readTasks(
                database, owner, "local_task_id = ? AND $CANCELLATION_STATES", arrayOf(id),
            ).singleOrNull()
            database.setTransactionSuccessful()
            return task
        } finally {
            database.endTransaction()
        }
    }

    fun recordUploadCancelling(owner: TaskOwner, id: String, remoteId: String) {
        require(isStageId(id) && validTaskNumber(remoteId))
        val values = ContentValues().apply { put("state", "CANCELLING") }
        val updated = writableDatabase.update(
            TASKS, values,
            "$OWNER_FILTER AND local_task_id = ? AND remote_task_id = ? AND kind = 'UPLOAD' AND $CANCELLATION_STATES",
            owner.selectionArgs() + arrayOf(id, remoteId),
        )
        if (updated != 1) throw SQLiteException("Upload cancellation did not match its task")
    }

    fun finishDownloadCancellation(owner: TaskOwner, id: String) {
        require(isStageId(id))
        val values = ContentValues().apply { put("state", "CANCELLED") }
        val updated = writableDatabase.update(
            TASKS, values,
            "$OWNER_FILTER AND local_task_id = ? AND kind = 'DOWNLOAD' AND state IN ('CANCEL_PENDING', 'CANCELLED')",
            owner.selectionArgs() + id,
        )
        if (updated != 1) throw SQLiteException("Download cancellation did not match its task")
    }

    fun setPauseReason(owner: TaskOwner, id: String, reason: String?) {
        require(isStageId(id) && (reason == null || reason in pauseReasons))
        val values = ContentValues().apply { put("pause_reason", reason) }
        writableDatabase.update(
            TASKS, values, "$OWNER_FILTER AND local_task_id = ? AND state = 'PENDING'",
            owner.selectionArgs() + id,
        )
    }

    fun pendingMultipartIds(owner: TaskOwner): Set<String> = readableDatabase.query(
        TASKS,
        arrayOf("local_task_id"),
        "$OWNER_FILTER AND kind = 'UPLOAD' AND $OPEN_STATES AND size_bytes > ?",
        owner.selectionArgs() + MAX_SINGLE_UPLOAD_BYTES.toString(),
        null, null, null,
    ).use { cursor ->
        buildSet { while (cursor.moveToNext()) add(cursor.getString(0)) }
    }

    fun reserveDownload(identity: DownloadIdentity): Map<String, String> {
        val database = writableDatabase
        val filter = "$OWNER_FILTER AND kind = 'DOWNLOAD' AND $OPEN_STATES AND download_entry_id = ?"
        val scope = identity.owner.selectionArgs() + identity.entryId
        database.beginTransaction()
        try {
            // 1. 相同对象沿用原下载。本次明确选了新版本时替换该条目的旧待下载记录。
            val existing = database.query(
                TASKS, arrayOf("local_task_id", "state", "download_revision", "download_object_id", "size_bytes", "sha256"),
                filter, scope, null, null, null,
            ).use { cursor ->
                if (cursor.moveToFirst() && (cursor.getString(1) != "PENDING" ||
                    (cursor.getString(2) == identity.revision && cursor.getString(3) == identity.objectId &&
                        cursor.getLong(4) == identity.source.sizeBytes && cursor.getString(5) == identity.source.sha256))
                ) mapOf("id" to cursor.getString(0), "state" to cursor.getString(1)) else null
            }
            if (existing != null) {
                database.setTransactionSuccessful()
                return existing
            }
            database.delete(TASKS, filter, scope)
            val id = UUID.randomUUID().toString()
            val values = ContentValues().apply {
                put("local_task_id", id)
                put("server_origin", identity.owner.server)
                put("user_id", identity.owner.userId)
                put("kind", "DOWNLOAD")
                put("name", identity.source.name)
                put("state", "PENDING")
                put("created_at_ms", System.currentTimeMillis())
                put("size_bytes", identity.source.sizeBytes)
                put("sha256", identity.source.sha256)
                put("download_entry_id", identity.entryId)
                put("download_revision", identity.revision)
                put("download_object_id", identity.objectId)
            }
            database.insertOrThrow(TASKS, null, values)
            database.setTransactionSuccessful()
            return mapOf("id" to id, "state" to "PENDING")
        } finally {
            database.endTransaction()
        }
    }

    fun finishDownload(owner: TaskOwner, id: String, actualName: String?) {
        require(isStageId(id) && (actualName == null || actualName.isNotBlank()))
        val filter = "$OWNER_FILTER AND local_task_id = ? AND kind = 'DOWNLOAD' AND state = 'PENDING'"
        val scope = owner.selectionArgs() + id
        if (actualName == null) {
            writableDatabase.delete(TASKS, filter, scope)
        } else {
            val values = ContentValues().apply {
                put("name", actualName)
                put("state", "COMPLETED")
                putNull("pause_reason")
            }
            if (writableDatabase.update(TASKS, values, filter, scope) != 1) {
                throw SQLiteException("Download task changed before saving")
            }
        }
    }

    fun createTest(owner: TaskOwner): List<Map<String, Any?>> {
        val database = writableDatabase
        database.beginTransaction()
        try {
            // 1. 在写事务内检查当前归属是否已有测试记录。
            val exists = database.query(
                TASKS,
                arrayOf("local_task_id"),
                "$OWNER_FILTER AND kind = ?",
                owner.selectionArgs() + TEST_KIND,
                null,
                null,
                null,
            ).use { cursor -> cursor.moveToFirst() }

            // 2. 第一次创建时生成本地任务 ID，测试状态与实际传输执行分开。
            if (!exists) {
                val values = ContentValues().apply {
                    put("local_task_id", UUID.randomUUID().toString())
                    put("server_origin", owner.server)
                    put("user_id", owner.userId)
                    put("kind", TEST_KIND)
                    put("name", "本地测试任务")
                    put("state", "TEST_RECORD")
                    put("created_at_ms", System.currentTimeMillis())
                }
                database.insertOrThrow(TASKS, null, values)
            }

            // 3. 回读同一账号的行，事务提交后再交付给界面。
            val rows = readTasks(database, owner)
            database.setTransactionSuccessful()
            return rows
        } finally {
            database.endTransaction()
        }
    }

    private fun readTasks(
        database: SQLiteDatabase, owner: TaskOwner,
        filter: String? = null, arguments: Array<String> = emptyArray(),
    ): List<Map<String, Any?>> =
        database.query(
            TASKS,
            arrayOf(
                "local_task_id", "name", "kind", "state", "created_at_ms",
                "size_bytes", "remote_task_id", "part_size_bytes", "part_count", "expires_at", "error_code",
                "parent_id", "sha256",
                "pause_reason", "download_entry_id", "download_revision", "download_object_id",
                "submission_stage",
                "(SELECT count(*) FROM multipart_part_receipts p WHERE p.local_task_id = transfer_tasks.local_task_id) AS confirmed_parts",
            ),
            if (filter == null) OWNER_FILTER else "$OWNER_FILTER AND $filter",
            owner.selectionArgs() + arguments,
            null,
            null,
            "created_at_ms DESC, local_task_id",
        ).use { cursor ->
            val id = cursor.getColumnIndexOrThrow("local_task_id")
            val name = cursor.getColumnIndexOrThrow("name")
            val kind = cursor.getColumnIndexOrThrow("kind")
            val state = cursor.getColumnIndexOrThrow("state")
            val createdAt = cursor.getColumnIndexOrThrow("created_at_ms")
            val size = cursor.getColumnIndexOrThrow("size_bytes")
            val remoteId = cursor.getColumnIndexOrThrow("remote_task_id")
            val partSize = cursor.getColumnIndexOrThrow("part_size_bytes")
            val partCount = cursor.getColumnIndexOrThrow("part_count")
            val expiresAt = cursor.getColumnIndexOrThrow("expires_at")
            val confirmedParts = cursor.getColumnIndexOrThrow("confirmed_parts")
            val errorCode = cursor.getColumnIndexOrThrow("error_code")
            val parentId = cursor.getColumnIndexOrThrow("parent_id")
            val sha256 = cursor.getColumnIndexOrThrow("sha256")
            val pause = cursor.getColumnIndexOrThrow("pause_reason")
            val entry = cursor.getColumnIndexOrThrow("download_entry_id")
            val revision = cursor.getColumnIndexOrThrow("download_revision")
            val objectId = cursor.getColumnIndexOrThrow("download_object_id")
            val submissionStage = cursor.getColumnIndexOrThrow("submission_stage")
            buildList {
                while (cursor.moveToNext()) {
                    add(
                        mapOf(
                            "id" to cursor.getString(id),
                            "name" to cursor.getString(name),
                            "kind" to cursor.getString(kind),
                            "state" to cursor.getString(state),
                            "createdAtMs" to cursor.getLong(createdAt),
                            "sizeBytes" to if (cursor.isNull(size)) null else cursor.getLong(size),
                            "remoteId" to cursor.getString(remoteId),
                            "partSizeBytes" to if (cursor.isNull(partSize)) null else cursor.getLong(partSize),
                            "partCount" to if (cursor.isNull(partCount)) null else cursor.getInt(partCount),
                            "expiresAt" to cursor.getString(expiresAt),
                            "confirmedParts" to cursor.getInt(confirmedParts),
                            "errorCode" to cursor.getString(errorCode),
                            "parentId" to cursor.getString(parentId),
                            "sha256" to cursor.getString(sha256),
                            "pauseReason" to cursor.getString(pause),
                            "entryId" to cursor.getString(entry),
                            "revision" to cursor.getString(revision),
                            "objectId" to cursor.getString(objectId),
                            "submissionStage" to cursor.getString(submissionStage),
                        ),
                    )
                }
            }
        }
}
