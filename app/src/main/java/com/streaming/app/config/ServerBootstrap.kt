package com.streaming.app.config

import android.content.Context
import android.content.SharedPreferences
import android.util.Base64
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import org.json.JSONObject
import java.io.File
import java.security.SecureRandom

class ServerBootstrap(context: Context) {

    data class LaunchConfig(
        val configFile: File,
        val runtimeFile: File,
        val encryptionKey: String,
        val importConfig: String?,
        val initialHttpPort: Int,
    )

    private val appContext = context.applicationContext
    private val prefs = createPrefs(appContext)
    private val serverDir = File(appContext.filesDir, "server")
    private val configFile = File(serverDir, "config.bin")
    private val runtimeFile = File(serverDir, "runtime.json")

    @Synchronized
    fun prepare(): LaunchConfig {
        serverDir.mkdirs()
        val key = encryptionKey()
        val legacyPort = prefs.getInt(KEY_HTTP_PORT, DEFAULT_HTTP_PORT)
        val import = if (!configFile.exists()) legacyConfig() else null
        return LaunchConfig(
            configFile = configFile,
            runtimeFile = runtimeFile,
            encryptionKey = key,
            importConfig = import,
            initialHttpPort = currentHttpPort(legacyPort),
        )
    }

    fun currentHttpPort(fallback: Int = DEFAULT_HTTP_PORT): Int {
        return try {
            if (!runtimeFile.exists()) {
                prefs.getInt(KEY_HTTP_PORT, fallback)
            } else {
                JSONObject(runtimeFile.readText()).optInt("httpPort", fallback)
                    .takeIf { it in 1024..65535 } ?: fallback
            }
        } catch (_: Exception) {
            fallback
        }
    }

    @Synchronized
    fun completeLegacyMigration() {
        if (!configFile.exists()) return
        prefs.edit().apply {
            LEGACY_KEYS.forEach { remove(it) }
            putBoolean(KEY_MIGRATION_COMPLETE, true)
        }.apply()
    }

    private fun encryptionKey(): String {
        prefs.getString(KEY_SERVER_KEY, null)?.let { return it }
        val bytes = ByteArray(32)
        SecureRandom().nextBytes(bytes)
        val encoded = Base64.encodeToString(bytes, Base64.NO_WRAP)
        check(prefs.edit().putString(KEY_SERVER_KEY, encoded).commit()) {
            "Could not persist the Go server configuration key"
        }
        return encoded
    }

    private fun legacyConfig(): String? {
        if (prefs.getBoolean(KEY_MIGRATION_COMPLETE, false)) return null
        val hasLegacyConfig = LEGACY_KEYS.any(prefs::contains)
        if (!hasLegacyConfig) return null

        var englishPreset = prefs.getInt(KEY_ENGLISH_PRESET, DEFAULT_ENGLISH_PRESET)
        var mandarinPreset = prefs.getInt(KEY_MANDARIN_PRESET, DEFAULT_MANDARIN_PRESET)
        if (
            prefs.getInt(KEY_CONFIG_VERSION, 1) < CURRENT_SCHEMA_VERSION &&
            prefs.contains(KEY_ENGLISH_PRESET) &&
            prefs.contains(KEY_MANDARIN_PRESET) &&
            englishPreset == 1 &&
            mandarinPreset == 2
        ) {
            englishPreset = DEFAULT_ENGLISH_PRESET
            mandarinPreset = DEFAULT_MANDARIN_PRESET
        }
        val config = JSONObject()
            .put("schemaVersion", CURRENT_SCHEMA_VERSION)
            .put("smpHost", prefs.getString(KEY_SMP_HOST, "") ?: "")
            .put("smpSshPort", prefs.getInt(KEY_SMP_SSH_PORT, DEFAULT_SMP_SSH_PORT))
            .put("smpUsername", prefs.getString(KEY_SMP_USERNAME, "") ?: "")
            .put("smpPassword", prefs.getString(KEY_SMP_PASSWORD, "") ?: "")
            .put("httpPort", prefs.getInt(KEY_HTTP_PORT, DEFAULT_HTTP_PORT))
            .put("streamIndex", 1)
            .put("englishPreset", englishPreset)
            .put("mandarinPreset", mandarinPreset)
            .put(
                "pollIntervalSeconds",
                prefs.getInt(KEY_POLL_INTERVAL_SECONDS, DEFAULT_POLL_INTERVAL_SECONDS),
            )
        return Base64.encodeToString(config.toString().toByteArray(Charsets.UTF_8), Base64.NO_WRAP)
    }

    companion object {
        const val DEFAULT_HTTP_PORT = 8080

        private const val PREFS_FILE = "streaming_secure_prefs"
        private const val KEY_SERVER_KEY = "go_server_config_key"
        private const val KEY_MIGRATION_COMPLETE = "go_server_migration_complete"
        private const val KEY_CONFIG_VERSION = "config_version"
        private const val KEY_SMP_HOST = "smp_host"
        private const val KEY_SMP_SSH_PORT = "smp_ssh_port"
        private const val KEY_SMP_USERNAME = "smp_username"
        private const val KEY_SMP_PASSWORD = "smp_password"
        private const val KEY_HTTP_PORT = "http_port"
        private const val KEY_STREAM_INDEX = "stream_index"
        private const val KEY_ENGLISH_PRESET = "english_preset"
        private const val KEY_MANDARIN_PRESET = "mandarin_preset"
        private const val KEY_POLL_INTERVAL_SECONDS = "poll_interval_seconds"

        private const val CURRENT_SCHEMA_VERSION = 2
        private const val DEFAULT_SMP_SSH_PORT = 22023
        private const val DEFAULT_ENGLISH_PRESET = 2
        private const val DEFAULT_MANDARIN_PRESET = 1
        private const val DEFAULT_POLL_INTERVAL_SECONDS = 3

        private val LEGACY_KEYS = listOf(
            KEY_CONFIG_VERSION,
            KEY_SMP_HOST,
            KEY_SMP_SSH_PORT,
            KEY_SMP_USERNAME,
            KEY_SMP_PASSWORD,
            KEY_HTTP_PORT,
            KEY_STREAM_INDEX,
            KEY_ENGLISH_PRESET,
            KEY_MANDARIN_PRESET,
            KEY_POLL_INTERVAL_SECONDS,
        )

        private fun createPrefs(context: Context): SharedPreferences {
            val masterKey = MasterKey.Builder(context)
                .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
                .build()
            return EncryptedSharedPreferences.create(
                context,
                PREFS_FILE,
                masterKey,
                EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
                EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
            )
        }
    }
}
