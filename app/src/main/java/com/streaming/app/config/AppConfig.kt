package com.streaming.app.config

data class AppConfig(
    val smpHost: String = "",
    val smpSshPort: Int = DEFAULT_SMP_SSH_PORT,
    val smpUsername: String = "",
    val smpPassword: String = "",
    val httpPort: Int = DEFAULT_HTTP_PORT,
    val streamIndex: Int = DEFAULT_STREAM_INDEX,
    val englishPreset: Int = DEFAULT_ENGLISH_PRESET,
    val mandarinPreset: Int = DEFAULT_MANDARIN_PRESET,
    val pollIntervalSeconds: Int = DEFAULT_POLL_INTERVAL_SECONDS,
) {
    val isSmpConfigured: Boolean
        get() = smpHost.isNotBlank() && smpUsername.isNotBlank()

    companion object {
        const val DEFAULT_SMP_SSH_PORT = 22023
        const val DEFAULT_HTTP_PORT = 8080
        const val DEFAULT_STREAM_INDEX = 1
        const val DEFAULT_ENGLISH_PRESET = 1
        const val DEFAULT_MANDARIN_PRESET = 2
        const val DEFAULT_POLL_INTERVAL_SECONDS = 3
    }
}
