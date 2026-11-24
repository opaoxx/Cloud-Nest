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
    5,
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
    }

    override fun onUpgrade(database: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
        if (oldVersion !in 1..4 || newVersion != 5) {
            throw SQLiteException("Unsupported task database migration: $oldVersion to $newVersion")
        }
        if (oldVersion == 1) addSingleUploadFields(database)
        if (oldVersion <= 2) addMultipartUploadFields(database)
        if (oldVersion <= 3) createPartReceipts(database)
        addUploadOutcomeFields(database)
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
                arrayOf("local_task_id", "remote_task_id", "part_size_bytes", "part_count", "expires_at"),
                "$OWNER_FILTER AND kind = 'UPLOAD' AND state = 'PENDING' AND parent_id = ? AND name = ? AND size_bytes = ? AND sha256 = ?",
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
                }
                database.insertOrThrow(TASKS, null, values)
                mapOf("id" to id, "remoteId" to null)
            }
            database.setTransactionSuccessful()
            return intent
        } finally {
            database.endTransaction()
        }
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
            put("state", state)
            put("result_entry_id", entryId)
        }
        val remoteFilter = if (remoteId == null) "remote_task_id IS NULL"
            else "(remote_task_id IS NULL OR remote_task_id = ?)"
        val arguments = owner.selectionArgs() + id + listOfNotNull(remoteId)
        val updated = writableDatabase.update(
            TASKS,
            values,
            "$OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND state = 'PENDING' AND $remoteFilter AND size_bytes <= ?",
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
            $OWNER_FILTER AND local_task_id = ? AND kind = 'UPLOAD' AND state = 'PENDING' AND size_bytes = ?
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

    fun saveMultipartOutcome(
        owner: TaskOwner, id: String, remoteId: String, state: String, entryId: String?, errorCode: String?,
    ) {
        require(isStageId(id) && validTaskNumber(remoteId))
        require(state in setOf("COMPLETED", "FAILED", "EXPIRED", "CANCELLED"))
        if (state == "COMPLETED") {
            require(entryId != null && validTaskNumber(entryId) && errorCode == null)
        } else {
            require(entryId == null && (errorCode == null || taskErrorCodePattern.matches(errorCode)))
        }
        // 终态和结果在同一次更新写入。按账号和任务 ID 限定到分片上传记录。
        val values = ContentValues().apply {
            put("state", state)
            put("result_entry_id", entryId)
            put("error_code", errorCode)
        }
        val updated = writableDatabase.update(
            TASKS,
            values,
            """
            $OWNER_FILTER AND local_task_id = ? AND remote_task_id = ? AND kind = 'UPLOAD'
            AND part_count IS NOT NULL AND (state = 'PENDING' OR
                (state = ? AND COALESCE(result_entry_id, '') = ? AND COALESCE(error_code, '') = ?))
            """.trimIndent(),
            owner.selectionArgs() + arrayOf(id, remoteId, state, entryId.orEmpty(), errorCode.orEmpty()),
        )
        if (updated != 1) throw SQLiteException("Upload outcome did not match its multipart task")
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

    fun list(owner: TaskOwner): List<Map<String, Any?>> = readTasks(readableDatabase, owner)

    fun recordDownload(owner: TaskOwner, actualName: String, sizeBytes: Long, sha256: String) {
        require(actualName.isNotBlank() && sizeBytes >= 0 && Regex("[0-9a-f]{64}").matches(sha256))
        // 外部文件写入完成后，按所属账号记录提供方实际返回的名称。
        val values = ContentValues().apply {
            put("local_task_id", UUID.randomUUID().toString())
            put("server_origin", owner.server)
            put("user_id", owner.userId)
            put("kind", "DOWNLOAD")
            put("name", actualName)
            put("state", "COMPLETED")
            put("created_at_ms", System.currentTimeMillis())
            put("size_bytes", sizeBytes)
            put("sha256", sha256)
        }
        writableDatabase.insertOrThrow(TASKS, null, values)
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

    private fun readTasks(database: SQLiteDatabase, owner: TaskOwner): List<Map<String, Any?>> =
        database.query(
            TASKS,
            arrayOf(
                "local_task_id", "name", "kind", "state", "created_at_ms",
                "size_bytes", "remote_task_id", "part_size_bytes", "part_count", "expires_at", "error_code",
                "(SELECT count(*) FROM multipart_part_receipts p WHERE p.local_task_id = transfer_tasks.local_task_id) AS confirmed_parts",
            ),
            OWNER_FILTER,
            owner.selectionArgs(),
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
                        ),
                    )
                }
            }
        }
}
