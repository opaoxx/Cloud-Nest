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

internal data class TaskOwner(val server: String, val userId: String) {
    init {
        require(server.startsWith("https://") || server.startsWith("http://"))
        require(userId != "0" && userId.toULongOrNull()?.toString() == userId)
    }

    fun selectionArgs() = arrayOf(server, userId)
}

internal class MobileTaskStore(context: Context) : SQLiteOpenHelper(
    context,
    File(context.noBackupFilesDir, "mobile_tasks.sqlite3").absolutePath,
    null,
    1,
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
    }

    override fun onUpgrade(database: SQLiteDatabase, oldVersion: Int, newVersion: Int) {
        throw SQLiteException("Unsupported task database migration: $oldVersion to $newVersion")
    }

    fun list(owner: TaskOwner): List<Map<String, Any>> = readTasks(readableDatabase, owner)

    fun createTest(owner: TaskOwner): List<Map<String, Any>> {
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

    private fun readTasks(database: SQLiteDatabase, owner: TaskOwner): List<Map<String, Any>> =
        database.query(
            TASKS,
            arrayOf("local_task_id", "name", "kind", "state", "created_at_ms"),
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
            buildList {
                while (cursor.moveToNext()) {
                    add(
                        mapOf(
                            "id" to cursor.getString(id),
                            "name" to cursor.getString(name),
                            "kind" to cursor.getString(kind),
                            "state" to cursor.getString(state),
                            "createdAtMs" to cursor.getLong(createdAt),
                        ),
                    )
                }
            }
        }
}
