package com.streaming.app.smp

import android.util.Log
import com.jcraft.jsch.ChannelShell
import com.jcraft.jsch.JSch
import com.jcraft.jsch.Session
import com.streaming.app.config.AppConfig
import java.io.ByteArrayOutputStream
import java.io.InputStream
import java.io.OutputStream
import java.nio.charset.StandardCharsets
import java.util.Properties
import java.util.concurrent.TimeUnit

class SmpClient {

    fun recallStreamingPreset(config: AppConfig, preset: Int): String {
        val command = "3*${config.streamIndex}*$preset."
        return sendCommand(config, command)
    }

    fun setStreamEnabled(config: AppConfig, enabled: Boolean): String {
        val value = if (enabled) 1 else 0
        val command = "E ${config.streamIndex}*$value STRC}"
        return sendCommand(config, command)
    }

    fun queryStreamEnabled(config: AppConfig): Boolean {
        val response = sendCommand(config, "E ${config.streamIndex})STRC}")
        return parseEnabledFlag(response)
    }

    fun queryActiveStreamingPreset(config: AppConfig): Int? {
        val response = sendCommand(config, streamingPresetQueryCommand(config.streamIndex))
        return parseSelectedStreamingPreset(response)
    }

    fun queryState(config: AppConfig): StreamState {
        if (!config.isSmpConfigured) {
            return StreamState(
                statusMessage = "SMP not configured",
                lastError = "Configure SMP host and credentials on the configuration page.",
            )
        }

        return try {
            val streamEnabled = queryStreamEnabled(config)
            val activePreset = queryActiveStreamingPreset(config)
            val activeStream = when {
                streamEnabled && activePreset == config.englishPreset -> ActiveStream.ENGLISH
                streamEnabled && activePreset == config.mandarinPreset -> ActiveStream.MANDARIN
                streamEnabled -> ActiveStream.NONE
                else -> ActiveStream.NONE
            }
            val statusMessage = when (activeStream) {
                ActiveStream.ENGLISH -> "Streaming English (preset ${config.englishPreset})"
                ActiveStream.MANDARIN -> "Streaming Mandarin (preset ${config.mandarinPreset})"
                ActiveStream.NONE -> if (streamEnabled) {
                    "Streaming (preset ${activePreset ?: "unknown"})"
                } else {
                    "Idle"
                }
            }
            StreamState(
                activeStream = activeStream,
                streamEnabled = streamEnabled,
                activePreset = activePreset,
                statusMessage = statusMessage,
                smpReachable = true,
            )
        } catch (error: Exception) {
            Log.w(TAG, "Failed to query SMP state", error)
            StreamState(
                statusMessage = "SMP is not reachable",
                lastError = error.message ?: error.javaClass.simpleName,
            )
        }
    }

    fun startEnglish(config: AppConfig): StreamState {
        return startPreset(config, config.englishPreset, ActiveStream.ENGLISH)
    }

    fun startMandarin(config: AppConfig): StreamState {
        return startPreset(config, config.mandarinPreset, ActiveStream.MANDARIN)
    }

    fun stop(config: AppConfig): StreamState {
        if (!config.isSmpConfigured) {
            return StreamState(
                statusMessage = "SMP not configured",
                lastError = "Configure SMP host and credentials on the configuration page.",
            )
        }

        return try {
            setStreamEnabled(config, false)
            queryState(config)
        } catch (error: Exception) {
            Log.w(TAG, "Failed to stop SMP stream", error)
            StreamState(
                statusMessage = "Stop failed",
                lastError = error.message ?: error.javaClass.simpleName,
            )
        }
    }

    private fun startPreset(
        config: AppConfig,
        preset: Int,
        expectedStream: ActiveStream,
    ): StreamState {
        if (!config.isSmpConfigured) {
            return StreamState(
                statusMessage = "SMP not configured",
                lastError = "Configure SMP host and credentials on the configuration page.",
            )
        }

        return try {
            recallStreamingPreset(config, preset)
            setStreamEnabled(config, true)
            val state = queryState(config)
            if (state.streamEnabled) {
                state.copy(
                    activeStream = expectedStream,
                    statusMessage = when (expectedStream) {
                        ActiveStream.ENGLISH -> "Streaming English (preset $preset)"
                        ActiveStream.MANDARIN -> "Streaming Mandarin (preset $preset)"
                        ActiveStream.NONE -> state.statusMessage
                    },
                )
            } else {
                state
            }
        } catch (error: Exception) {
            Log.w(TAG, "Failed to start SMP stream preset $preset", error)
            StreamState(
                statusMessage = "Start failed",
                lastError = error.message ?: error.javaClass.simpleName,
            )
        }
    }

    private fun sendCommand(config: AppConfig, command: String): String {
        val session = openSession(config)
        try {
            val channel = session.openChannel("shell") as ChannelShell
            channel.setPty(false)
            val inputStream = channel.inputStream
            val outputStream = channel.outputStream
            channel.connect(CONNECT_TIMEOUT_MS)

            writeCommand(outputStream, command)
            val response = readResponse(inputStream)
            channel.disconnect()
            return response
        } finally {
            session.disconnect()
        }
    }

    private fun openSession(config: AppConfig): Session {
        val jsch = JSch()
        val session = jsch.getSession(config.smpUsername, config.smpHost, config.smpSshPort)
        session.setPassword(config.smpPassword)
        val properties = Properties()
        properties["StrictHostKeyChecking"] = "no"
        session.setConfig(properties)
        session.timeout = CONNECT_TIMEOUT_MS
        session.connect(CONNECT_TIMEOUT_MS)
        return session
    }

    private fun writeCommand(outputStream: OutputStream, command: String) {
        val payload = "$command\r\n"
        outputStream.write(payload.toByteArray(StandardCharsets.US_ASCII))
        outputStream.flush()
    }

    private fun readResponse(inputStream: InputStream): String {
        val buffer = ByteArrayOutputStream()
        val chunk = ByteArray(1024)
        val deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(READ_TIMEOUT_MS.toLong())

        while (System.nanoTime() < deadline) {
            while (inputStream.available() > 0) {
                val read = inputStream.read(chunk)
                if (read <= 0) {
                    break
                }
                buffer.write(chunk, 0, read)
                if (buffer.toString(StandardCharsets.US_ASCII.name()).contains("]")) {
                    return buffer.toString(StandardCharsets.US_ASCII.name()).trim()
                }
            }
            Thread.sleep(50)
        }

        return buffer.toString(StandardCharsets.US_ASCII.name()).trim()
    }

    private fun parseEnabledFlag(response: String): Boolean {
        val match = STREAM_ENABLED_REGEX.find(response)
        return match?.groupValues?.getOrNull(1) == "1"
    }

    private fun streamingPresetQueryCommand(streamIndex: Int): String {
        return when (streamIndex) {
            1 -> "46I"
            2 -> "47I"
            3 -> "48I"
            else -> "46I"
        }
    }

    private fun parseSelectedStreamingPreset(response: String): Int? {
        val selectedMatch = SELECTED_PRESET_REGEX.find(response)
        if (selectedMatch != null) {
            return selectedMatch.groupValues[1].toIntOrNull()
        }
        val singleMatch = SINGLE_PRESET_REGEX.find(response.trim())
        return singleMatch?.groupValues?.getOrNull(1)?.toIntOrNull()
    }

    companion object {
        private const val TAG = "SmpClient"
        private const val CONNECT_TIMEOUT_MS = 10_000
        private const val READ_TIMEOUT_MS = 5_000
        private val STREAM_ENABLED_REGEX = Regex("""Strc\d+\*(\d+)""", RegexOption.IGNORE_CASE)
        private val SELECTED_PRESET_REGEX = Regex(""",\s*(\d+)\*""")
        private val SINGLE_PRESET_REGEX = Regex("""^(\d+)\*""")
    }
}
