package com.streaming.app.smp

enum class ActiveStream {
    NONE,
    ENGLISH,
    MANDARIN,
}

data class StreamState(
    val activeStream: ActiveStream = ActiveStream.NONE,
    val streamEnabled: Boolean = false,
    val activePreset: Int? = null,
    val statusMessage: String = "Idle",
    val lastError: String? = null,
    val smpReachable: Boolean = false,
    val queriedAtEpochMs: Long = System.currentTimeMillis(),
) {
    fun toJson(): String {
        val presetValue = activePreset?.toString() ?: "null"
        val errorValue = lastError?.let { "\"${escapeJson(it)}\"" } ?: "null"
        return """
            {
              "activeStream": "${activeStream.name.lowercase()}",
              "streamEnabled": $streamEnabled,
              "activePreset": $presetValue,
              "statusMessage": "${escapeJson(statusMessage)}",
              "lastError": $errorValue,
              "smpReachable": $smpReachable,
              "queriedAtEpochMs": $queriedAtEpochMs
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
}
