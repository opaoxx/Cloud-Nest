package com.opaoxx.cloudnest

import android.content.Context
import android.util.AtomicFile
import java.io.File
import java.io.FileNotFoundException
import java.io.IOException
import java.util.UUID

private val installationLock = Any()

internal class InstallationIdentity(context: Context) {
    private val record = AtomicFile(File(context.noBackupFilesDir, "installation_id"))

    fun getOrCreate(): String = synchronized(installationLock) {
        // 1. 复用当前安装已落盘的小写 UUID v4。
        val existing = try {
            record.readFully().toString(Charsets.UTF_8)
        } catch (missing: FileNotFoundException) {
            if (record.baseFile.exists()) throw missing
            null
        }
        if (existing != null) {
            val parsed = UUID.fromString(existing)
            if (parsed.toString() != existing || parsed.version() != 4 || parsed.variant() != 2) {
                throw IOException("Invalid installation identity")
            }
            return@synchronized existing
        }

        // 2. 首次生成标识，确认写入应用私有目录后再用于登录。
        val created = UUID.randomUUID().toString()
        val output = record.startWrite()
        try {
            output.write(created.toByteArray(Charsets.UTF_8))
            record.finishWrite(output)
        } catch (failure: IOException) {
            record.failWrite(output)
            throw failure
        }
        if (record.readFully().toString(Charsets.UTF_8) != created) {
            throw IOException("Installation identity was not saved")
        }
        created
    }
}
