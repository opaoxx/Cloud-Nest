package com.opaoxx.cloudnest

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.AtomicFile
import java.io.File
import java.io.FileNotFoundException
import java.io.IOException
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

private val sessionStoreLock = Any()
private const val KEY_ALIAS = "cloudnest_mobile_session_v1"
private const val KEYSTORE = "AndroidKeyStore"
private const val TRANSFORMATION = "AES/GCM/NoPadding"
private const val ENCRYPTED_RECORD: Byte = 1
private const val CLEARED_RECORD: Byte = 0
private const val IV_BYTES = 12
private const val TAG_BYTES = 16
private const val MAX_PLAINTEXT_BYTES = 16 * 1024

internal class SessionCredentialStore(context: Context) {
    private val record = AtomicFile(File(context.noBackupFilesDir, "mobile_session"))

    fun read(): String? = synchronized(sessionStoreLock) {
        // 1. 空记录代表没有可恢复的凭证。其余记录必须通过 GCM 认证。
        val encrypted = try {
            record.readFully()
        } catch (missing: FileNotFoundException) {
            if (record.baseFile.exists()) throw missing
            return@synchronized null
        }
        if (encrypted.contentEquals(byteArrayOf(CLEARED_RECORD))) return@synchronized null
        val headerBytes = 1 + IV_BYTES
        val overhead = headerBytes + TAG_BYTES
        if (encrypted.size !in (overhead + 1)..(overhead + MAX_PLAINTEXT_BYTES) ||
            encrypted[0] != ENCRYPTED_RECORD
        ) {
            throw IOException("Invalid session record")
        }
        val key = keyStore().getKey(KEY_ALIAS, null) as? SecretKey
            ?: throw IOException("Missing session key")
        val cipher = Cipher.getInstance(TRANSFORMATION)
        val iv = encrypted.copyOfRange(1, headerBytes)
        cipher.init(Cipher.DECRYPT_MODE, key, GCMParameterSpec(TAG_BYTES * 8, iv))
        cipher.doFinal(encrypted, headerBytes, encrypted.size - headerBytes).toString(Charsets.UTF_8)
    }

    fun write(credential: String) = synchronized(sessionStoreLock) {
        // 2. 密钥留在 Keystore。每次加密使用 Cipher 新生成的 IV。
        val plaintext = credential.toByteArray(Charsets.UTF_8)
        if (plaintext.size !in 1..MAX_PLAINTEXT_BYTES) throw IOException("Invalid session size")
        val key = keyStore().getKey(KEY_ALIAS, null) as? SecretKey ?: createKey()
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(Cipher.ENCRYPT_MODE, key)
        val iv = cipher.iv
        if (iv.size != IV_BYTES) throw IOException("Invalid session IV")
        writeRecord(byteArrayOf(ENCRYPTED_RECORD) + iv + cipher.doFinal(plaintext))
    }

    fun clear() = synchronized(sessionStoreLock) {
        // 3. 轮换前原子写入空记录，进程中断后也不会自动使用旧凭证。
        writeRecord(byteArrayOf(CLEARED_RECORD))
    }

    private fun keyStore(): KeyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }

    private fun createKey(): SecretKey {
        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE)
        generator.init(
            KeyGenParameterSpec.Builder(
                KEY_ALIAS,
                KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
            )
                .setKeySize(256)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .build(),
        )
        return generator.generateKey()
    }

    private fun writeRecord(contents: ByteArray) {
        val output = record.startWrite()
        try {
            output.write(contents)
            record.finishWrite(output)
        } catch (failure: IOException) {
            record.failWrite(output)
            throw failure
        }
        if (!record.readFully().contentEquals(contents)) {
            throw IOException("Session record was not saved")
        }
    }
}
