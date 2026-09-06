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

    fun validated(): Pair<AppConfig, String?> {
        val parsed = parseHostPort(smpHost, smpSshPort) ?: return this to "invalid SMP address"
        val next = copy(
            smpHost = parsed.first,
            smpSshPort = parsed.second,
            smpUsername = smpUsername.trim(),
            streamIndex = DEFAULT_STREAM_INDEX,
        )
        return next to next.validationError()
    }

    private fun validationError(): String? {
        if (smpHost.isBlank() || smpUsername.isBlank()) {
            return "SMP host and username are required"
        }
        if (smpUsername.length > 64 || smpUsername.any { it.isISOControl() }) {
            return "invalid SSH username"
        }
        if (smpSshPort !in 1..65535) {
            return "SSH port must be between 1 and 65535"
        }
        if (httpPort !in 1024..65535) {
            return "HTTP port must be between 1024 and 65535"
        }
        if (englishPreset !in 1..32 || mandarinPreset !in 1..32) {
            return "preset numbers must be between 1 and 32"
        }
        if (englishPreset == mandarinPreset) {
            return "English and Mandarin presets must be different"
        }
        return null
    }

    companion object {
        const val DEFAULT_SMP_SSH_PORT = 22023
        const val DEFAULT_HTTP_PORT = 8080
        const val DEFAULT_STREAM_INDEX = 1
        const val DEFAULT_ENGLISH_PRESET = 2
        const val DEFAULT_MANDARIN_PRESET = 1
        const val DEFAULT_POLL_INTERVAL_SECONDS = 3

        fun parseHostPort(address: String, explicitPort: Int): Pair<String, Int>? {
            val raw = address.trim()
            if (raw.isEmpty() || raw.any { it.isWhitespace() } || "://" in raw.lowercase()) {
                return null
            }
            var host = raw
            var port = if (explicitPort == 0) DEFAULT_SMP_SSH_PORT else explicitPort
            val colon = raw.lastIndexOf(':')
            if (raw.startsWith("[") && raw.contains("]")) {
                val end = raw.indexOf(']')
                host = raw.substring(1, end)
                val remainder = raw.substring(end + 1)
                if (remainder.startsWith(":")) {
                    port = remainder.substring(1).toIntOrNull() ?: return null
                }
            } else if (colon > 0 && raw.indexOf(':') == colon) {
                val parsed = raw.substring(colon + 1).toIntOrNull() ?: return null
                host = raw.substring(0, colon).trim()
                port = parsed
            }
            if (host.isBlank() || host.length > 253 || port !in 1..65535) {
                return null
            }
            return host to port
        }
    }
}
