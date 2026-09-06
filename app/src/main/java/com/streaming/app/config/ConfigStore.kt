package com.streaming.app.config

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

class ConfigStore(context: Context) {

    private val appContext = context.applicationContext
    private val prefs: SharedPreferences = createPrefs(appContext)

    init {
        migratePresetMapping()
    }

    fun load(): AppConfig {
        return AppConfig(
            smpHost = prefs.getString(KEY_SMP_HOST, "") ?: "",
            smpSshPort = prefs.getInt(KEY_SMP_SSH_PORT, AppConfig.DEFAULT_SMP_SSH_PORT),
            smpUsername = prefs.getString(KEY_SMP_USERNAME, "") ?: "",
            smpPassword = prefs.getString(KEY_SMP_PASSWORD, "") ?: "",
            httpPort = prefs.getInt(KEY_HTTP_PORT, AppConfig.DEFAULT_HTTP_PORT),
            streamIndex = prefs.getInt(KEY_STREAM_INDEX, AppConfig.DEFAULT_STREAM_INDEX),
            englishPreset = prefs.getInt(KEY_ENGLISH_PRESET, AppConfig.DEFAULT_ENGLISH_PRESET),
            mandarinPreset = prefs.getInt(KEY_MANDARIN_PRESET, AppConfig.DEFAULT_MANDARIN_PRESET),
            pollIntervalSeconds = prefs.getInt(
                KEY_POLL_INTERVAL_SECONDS,
                AppConfig.DEFAULT_POLL_INTERVAL_SECONDS,
            ),
        )
    }

    fun save(config: AppConfig) {
        prefs.edit()
            .putString(KEY_SMP_HOST, config.smpHost.trim())
            .putInt(KEY_SMP_SSH_PORT, config.smpSshPort)
            .putString(KEY_SMP_USERNAME, config.smpUsername.trim())
            .putString(KEY_SMP_PASSWORD, config.smpPassword)
            .putInt(KEY_HTTP_PORT, config.httpPort)
            .putInt(KEY_STREAM_INDEX, config.streamIndex)
            .putInt(KEY_ENGLISH_PRESET, config.englishPreset)
            .putInt(KEY_MANDARIN_PRESET, config.mandarinPreset)
            .putInt(KEY_POLL_INTERVAL_SECONDS, config.pollIntervalSeconds)
            .putInt(KEY_CONFIG_VERSION, CURRENT_CONFIG_VERSION)
            .apply()
    }

    fun toPublicJson(config: AppConfig): String {
        return """
            {
              "smpHost": "${escapeJson(config.smpHost)}",
              "smpSshPort": ${config.smpSshPort},
              "smpUsername": "${escapeJson(config.smpUsername)}",
              "hasPassword": ${config.smpPassword.isNotEmpty()},
              "httpPort": ${config.httpPort},
              "streamIndex": ${config.streamIndex},
              "englishPreset": ${config.englishPreset},
              "mandarinPreset": ${config.mandarinPreset},
              "pollIntervalSeconds": ${config.pollIntervalSeconds},
              "isSmpConfigured": ${config.isSmpConfigured}
            }
        """.trimIndent()
    }

    private fun escapeJson(value: String): String {
        return value
            .replace("\\", "\\\\")
            .replace("\"", "\\\"")
            .replace("\n", "\\n")
            .replace("\r", "\\r")
    }

    private fun migratePresetMapping() {
        if (prefs.getInt(KEY_CONFIG_VERSION, 1) >= CURRENT_CONFIG_VERSION) {
            return
        }

        val editor = prefs.edit()
        val englishPreset = prefs.getInt(KEY_ENGLISH_PRESET, OLD_DEFAULT_ENGLISH_PRESET)
        val mandarinPreset = prefs.getInt(KEY_MANDARIN_PRESET, OLD_DEFAULT_MANDARIN_PRESET)
        if (
            prefs.contains(KEY_ENGLISH_PRESET) &&
            prefs.contains(KEY_MANDARIN_PRESET) &&
            englishPreset == OLD_DEFAULT_ENGLISH_PRESET &&
            mandarinPreset == OLD_DEFAULT_MANDARIN_PRESET
        ) {
            editor
                .putInt(KEY_ENGLISH_PRESET, AppConfig.DEFAULT_ENGLISH_PRESET)
                .putInt(KEY_MANDARIN_PRESET, AppConfig.DEFAULT_MANDARIN_PRESET)
        }
        editor.putInt(KEY_CONFIG_VERSION, CURRENT_CONFIG_VERSION).commit()
    }

    companion object {
        private const val PREFS_FILE = "streaming_secure_prefs"
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
        private const val CURRENT_CONFIG_VERSION = 2
        private const val OLD_DEFAULT_ENGLISH_PRESET = 1
        private const val OLD_DEFAULT_MANDARIN_PRESET = 2

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
