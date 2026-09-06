package com.streaming.app.server

import android.util.Log
import com.streaming.app.config.AppConfig
import com.streaming.app.config.ConfigStore
import com.streaming.app.smp.SmpClient
import com.streaming.app.smp.StreamState
import fi.iki.elonen.NanoHTTPD
import org.json.JSONObject
import java.util.concurrent.Callable
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.TimeoutException

class StreamingHttpServer(
    port: Int,
    private val configStore: ConfigStore,
    private val smpClient: SmpClient,
    private val pages: WebPages,
    private val onConfigSaved: (AppConfig) -> Unit,
    initialState: StreamState,
) : NanoHTTPD(port) {

    private val worker: ExecutorService = Executors.newSingleThreadExecutor()
    private val currentState = AtomicReference(initialState)

    fun updateState(state: StreamState) {
        currentState.set(state)
    }

    fun currentState(): StreamState = currentState.get()

    override fun serve(session: IHTTPSession): Response {
        val uri = session.uri
        val method = session.method

        return try {
            when {
                method == Method.GET && uri == "/" -> htmlResponse(pages.controlPage())
                method == Method.GET && uri == "/config" -> htmlResponse(pages.configPage())
                method == Method.GET && uri == "/api/status" -> jsonResponse(currentState.get().toJson())
                method == Method.GET && uri == "/api/config" -> {
                    val config = configStore.load()
                    jsonResponse(configStore.toPublicJson(config))
                }
                method == Method.POST && uri == "/api/stream/english" -> {
                    runSmpAction { config -> smpClient.startEnglish(config) }
                }
                method == Method.POST && uri == "/api/stream/mandarin" -> {
                    runSmpAction { config -> smpClient.startMandarin(config) }
                }
                method == Method.POST && uri == "/api/stream/stop" -> {
                    runSmpAction { config ->
                        val current = currentState.get()
                        if (!current.streamEnabled) {
                            current
                        } else {
                            smpClient.stop(config)
                        }
                    }
                }
                method == Method.POST && uri == "/api/config" -> handleConfigSave(session)
                else -> newFixedLengthResponse(Response.Status.NOT_FOUND, MIME_PLAINTEXT, "Not found")
            }
        } catch (error: Exception) {
            Log.w(TAG, "HTTP request failed for $method $uri", error)
            jsonResponse(
                """{"error":"${escapeJson(error.message ?: "Request failed")}"}""",
                Response.Status.INTERNAL_ERROR,
            )
        }
    }

    private fun handleConfigSave(session: IHTTPSession): Response {
        val body = readBody(session)
        val json = JSONObject(body)
        val existing = configStore.load()
        val password = json.optString("smpPassword", "")
        val candidate = AppConfig(
            smpHost = json.optString("smpHost", existing.smpHost),
            smpSshPort = json.optInt("smpSshPort", existing.smpSshPort),
            smpUsername = json.optString("smpUsername", existing.smpUsername),
            smpPassword = if (password.isNotEmpty()) password else existing.smpPassword,
            httpPort = json.optInt("httpPort", existing.httpPort),
            streamIndex = AppConfig.DEFAULT_STREAM_INDEX,
            englishPreset = json.optInt("englishPreset", existing.englishPreset),
            mandarinPreset = json.optInt("mandarinPreset", existing.mandarinPreset),
            pollIntervalSeconds = existing.pollIntervalSeconds,
        )
        val (config, error) = candidate.validated()
        if (error != null) {
            return jsonResponse(
                """{"error":"${escapeJson(error)}"}""",
                Response.Status.BAD_REQUEST,
            )
        }

        configStore.save(config)
        onConfigSaved(config)
        return jsonResponse("""{"ok":true,"httpPort":${config.httpPort}}""")
    }

    private fun runSmpAction(action: (AppConfig) -> StreamState): Response {
        val config = configStore.load()
        val state = try {
            worker.submit(Callable { action(config) }).get(20, TimeUnit.SECONDS)
        } catch (error: TimeoutException) {
            StreamState(
                statusMessage = "SMP is not reachable",
                lastError = "timed out waiting for SMP",
            )
        }
        currentState.set(state)
        return jsonResponse(state.toJson())
    }

    private fun readBody(session: IHTTPSession): String {
        val files = HashMap<String, String>()
        session.parseBody(files)
        return files["postData"] ?: ""
    }

    private fun htmlResponse(content: String): Response {
        return newFixedLengthResponse(Response.Status.OK, "text/html; charset=utf-8", content)
    }

    private fun jsonResponse(
        content: String,
        status: Response.Status = Response.Status.OK,
    ): Response {
        return newFixedLengthResponse(status, "application/json; charset=utf-8", content)
    }

    private fun escapeJson(value: String): String {
        return value
            .replace("\\", "\\\\")
            .replace("\"", "\\\"")
            .replace("\n", "\\n")
            .replace("\r", "\\r")
    }

    fun shutdownServer() {
        worker.shutdownNow()
        stop()
    }

    companion object {
        private const val TAG = "StreamingHttpServer"
    }
}
