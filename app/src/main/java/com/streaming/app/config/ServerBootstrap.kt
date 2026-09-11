package com.streaming.app.config

import android.content.Context
import android.content.SharedPreferences
import android.os.UserManager
import android.util.Base64
import android.util.Log
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

    /**
     * The server state lives in device-protected storage, which the hardware
     * key unlocks at power-on. Credential-protected storage would tie the SMP
     * settings to the screen lock, at the cost of leaving the tablet without a
     * control server until somebody signs in.
     */
    private val deviceContext = appContext.createDeviceProtectedStorageContext()
    private val prefs = deviceContext.getSharedPreferences(PREFS_FILE, Context.MODE_PRIVATE)
    private val serverDir = File(deviceContext.filesDir, "server")
    private val configFile = File(serverDir, "config.bin")
    private val runtimeFile = File(serverDir, "runtime.json")

    private val credentialServerDir = File(appContext.filesDir, "server")
    private var cachedCredentialPrefs: SharedPreferences? = null

    @Synchronized
    fun prepare(): LaunchConfig {
        serverDir.mkdirs()
        importCredentialStorage()
        val key = encryptionKey()
        val legacyPort = credentialPrefs()?.getInt(KEY_HTTP_PORT, DEFAULT_HTTP_PORT)
            ?: DEFAULT_HTTP_PORT
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
                fallback
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
        val credential = credentialPrefs() ?: return
        credential.edit().apply {
            LEGACY_KEYS.forEach { remove(it) }
            putBoolean(KEY_MIGRATION_COMPLETE, true)
        }.apply()
    }

    /**
     * Moves an existing configuration out of credential-protected storage the
     * first time the tablet is unlocked after the update. The key and the
     * configuration file are copied together, because either one alone leaves
     * the server with a file it cannot decrypt.
     */
    private fun importCredentialStorage() {
        if (prefs.getBoolean(KEY_IMPORT_COMPLETE, false)) return
        val credential = credentialPrefs() ?: return

        val credentialConfig = File(credentialServerDir, configFile.name)
        val credentialKey = credential.getString(KEY_SERVER_KEY, null)
        if (credentialConfig.exists() && credentialKey != null) {
            try {
                check(prefs.edit().putString(KEY_SERVER_KEY, credentialKey).commit()) {
                    "Could not persist the imported configuration key"
                }
                credentialConfig.copyTo(configFile, overwrite = true)
                File(credentialServerDir, runtimeFile.name)
                    .takeIf(File::exists)
                    ?.copyTo(runtimeFile, overwrite = true)
                Log.i(TAG, "Imported the configuration into device-protected storage")
            } catch (error: Exception) {
                // A key and a configuration file that do not match stop the
                // server from starting at all, so drop the file and let it
                // begin from defaults instead. Retried on the next start.
                Log.w(TAG, "Could not import the existing configuration", error)
                configFile.delete()
                return
            }
        }
        prefs.edit().putBoolean(KEY_IMPORT_COMPLETE, true).apply()
    }

    /**
     * Opens the old credential-protected preferences, which only exist on
     * tablets updated from an earlier build and can only be read once the
     * user has signed in. Never created from scratch, so a fresh install
     * neither needs a Keystore key nor waits for an unlock.
     */
    private fun credentialPrefs(): SharedPreferences? {
        cachedCredentialPrefs?.let { return it }
        val userManager = appContext.getSystemService(UserManager::class.java)
        if (userManager != null && !userManager.isUserUnlocked) return null
        val file = File(File(appContext.dataDir, "shared_prefs"), "$CREDENTIAL_PREFS_FILE.xml")
        if (!file.exists()) return null
        return try {
            createCredentialPrefs(appContext).also { cachedCredentialPrefs = it }
        } catch (error: Exception) {
            Log.w(TAG, "Could not open the credential-protected preferences", error)
            null
        }
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
        val legacy = credentialPrefs() ?: return null
        if (legacy.getBoolean(KEY_MIGRATION_COMPLETE, false)) return null
        val hasLegacyConfig = LEGACY_KEYS.any(legacy::contains)
        if (!hasLegacyConfig) return null

        var englishPreset = legacy.getInt(KEY_ENGLISH_PRESET, DEFAULT_ENGLISH_PRESET)
        var mandarinPreset = legacy.getInt(KEY_MANDARIN_PRESET, DEFAULT_MANDARIN_PRESET)
        if (
            legacy.getInt(KEY_CONFIG_VERSION, 1) < CURRENT_SCHEMA_VERSION &&
            legacy.contains(KEY_ENGLISH_PRESET) &&
            legacy.contains(KEY_MANDARIN_PRESET) &&
            englishPreset == 1 &&
            mandarinPreset == 2
        ) {
            englishPreset = DEFAULT_ENGLISH_PRESET
            mandarinPreset = DEFAULT_MANDARIN_PRESET
        }
        val config = JSONObject()
            .put("schemaVersion", CURRENT_SCHEMA_VERSION)
            .put("smpHost", legacy.getString(KEY_SMP_HOST, "") ?: "")
            .put("smpSshPort", legacy.getInt(KEY_SMP_SSH_PORT, DEFAULT_SMP_SSH_PORT))
            .put("smpUsername", legacy.getString(KEY_SMP_USERNAME, "") ?: "")
            .put("smpPassword", legacy.getString(KEY_SMP_PASSWORD, "") ?: "")
            .put("httpPort", legacy.getInt(KEY_HTTP_PORT, DEFAULT_HTTP_PORT))
            .put("streamIndex", 1)
            .put("englishPreset", englishPreset)
            .put("mandarinPreset", mandarinPreset)
            .put(
                "pollIntervalSeconds",
                legacy.getInt(KEY_POLL_INTERVAL_SECONDS, DEFAULT_POLL_INTERVAL_SECONDS),
            )
        return Base64.encodeToString(config.toString().toByteArray(Charsets.UTF_8), Base64.NO_WRAP)
    }

    companion object {
        const val DEFAULT_HTTP_PORT = 8080

        private const val TAG = "StreamingBootstrap"
        private const val PREFS_FILE = "streaming_server_prefs"
        private const val CREDENTIAL_PREFS_FILE = "streaming_secure_prefs"
        private const val KEY_SERVER_KEY = "go_server_config_key"
        private const val KEY_IMPORT_COMPLETE = "device_storage_import_complete"
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

        private fun createCredentialPrefs(context: Context): SharedPreferences {
            val masterKey = MasterKey.Builder(context)
                .setKeyScheme(MasterKey.KeyScheme.AES256_GCM)
                .build()
            return EncryptedSharedPreferences.create(
                context,
                CREDENTIAL_PREFS_FILE,
                masterKey,
                EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
                EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
            )
        }
    }
}
