package com.streaming.app.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import android.os.Handler
import android.os.Looper
import android.util.Log
import androidx.core.app.NotificationCompat
import com.streaming.app.MainActivity
import com.streaming.app.R
import com.streaming.app.StreamingApplication
import java.io.File
import java.net.HttpURLConnection
import java.net.URL

class StreamingForegroundService : Service() {

    private val mainHandler = Handler(Looper.getMainLooper())
    @Volatile
    private var serverProcess: Process? = null
    @Volatile
    private var stopping = false
    private var notificationPort = 0

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForeground(NOTIFICATION_ID, buildNotification())
        startGoServer()
        return START_STICKY
    }

    override fun onDestroy() {
        stopping = true
        mainHandler.removeCallbacksAndMessages(null)
        val process = serverProcess
        serverProcess = null
        process?.destroy()
        if (process != null && process.isAlive) {
            process.destroyForcibly()
        }
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    @Synchronized
    private fun startGoServer() {
        if (stopping || serverProcess?.isAlive == true) return

        val app = application as StreamingApplication
        val launch = app.serverBootstrap.prepare()
        val binary = File(applicationInfo.nativeLibraryDir, GO_SERVER_LIBRARY)
        try {
            check(binary.isFile && binary.canExecute()) {
                "Go server is missing or not executable: ${binary.absolutePath}"
            }
            val go2rtc = File(applicationInfo.nativeLibraryDir, GO2RTC_LIBRARY)
            val builder = ProcessBuilder(binary.absolutePath)
                .redirectErrorStream(true)
            builder.environment().apply {
                put("STREAMING_CONFIG", launch.configFile.absolutePath)
                put("STREAMING_RUNTIME", launch.runtimeFile.absolutePath)
                put("STREAMING_CONFIG_KEY", launch.encryptionKey)
                launch.importConfig?.let { put("STREAMING_IMPORT_CONFIG", it) }
                if (go2rtc.isFile) {
                    put("STREAMING_GO2RTC", go2rtc.absolutePath)
                    put("STREAMING_GO2RTC_HOME", File(launch.configFile.parentFile, "go2rtc").absolutePath)
                }
            }
            val process = builder.start()
            serverProcess = process
            updateNotification(launch.initialHttpPort)
            captureOutput(process)
            monitorServer(process, launch.initialHttpPort)
            monitorExit(process)
            Log.i(TAG, "Started Go control server")
        } catch (error: Exception) {
            Log.e(TAG, "Failed to start Go control server", error)
            scheduleRestart()
        }
    }

    private fun captureOutput(process: Process) {
        Thread({
            try {
                process.inputStream.bufferedReader().useLines { lines ->
                    lines.forEach { Log.i(GO_SERVER_TAG, it) }
                }
            } catch (error: Exception) {
                if (!stopping) Log.w(TAG, "Failed reading Go server output", error)
            }
        }, "go-server-output").start()
    }

    private fun monitorServer(process: Process, initialPort: Int) {
        Thread({
            var migrated = false
            var lastPort = initialPort
            while (!stopping && process.isAlive && serverProcess === process) {
                val app = application as StreamingApplication
                val port = app.serverBootstrap.currentHttpPort(lastPort)
                if (serverIsReady(port)) {
                    if (!migrated) {
                        app.serverBootstrap.completeLegacyMigration()
                        migrated = true
                    }
                    if (port != lastPort || notificationPort != port) {
                        lastPort = port
                        mainHandler.post { updateNotification(port) }
                    }
                }
                try {
                    Thread.sleep(HEALTH_CHECK_INTERVAL_MS)
                } catch (_: InterruptedException) {
                    return@Thread
                }
            }
        }, "go-server-health").start()
    }

    private fun monitorExit(process: Process) {
        Thread({
            val exitCode = process.waitFor()
            if (serverProcess === process) {
                serverProcess = null
            }
            if (!stopping) {
                Log.w(TAG, "Go control server exited with code $exitCode")
                scheduleRestart()
            }
        }, "go-server-exit").start()
    }

    private fun serverIsReady(port: Int): Boolean {
        return try {
            val connection = URL("http://127.0.0.1:$port/api/status")
                .openConnection() as HttpURLConnection
            connection.connectTimeout = HEALTH_CHECK_TIMEOUT_MS
            connection.readTimeout = HEALTH_CHECK_TIMEOUT_MS
            connection.useCaches = false
            try {
                connection.responseCode == HttpURLConnection.HTTP_OK
            } finally {
                connection.disconnect()
            }
        } catch (_: Exception) {
            false
        }
    }

    private fun scheduleRestart() {
        mainHandler.removeCallbacks(restartRunnable)
        mainHandler.postDelayed(restartRunnable, RESTART_DELAY_MS)
    }

    private val restartRunnable = Runnable { startGoServer() }

    private fun buildNotification(
        port: Int = (application as StreamingApplication).serverBootstrap.currentHttpPort(),
    ): Notification {
        val openIntent = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle(getString(R.string.service_notification_title))
            .setContentText("http://127.0.0.1:$port/")
            .setSmallIcon(R.drawable.ic_notification)
            .setContentIntent(openIntent)
            .setOngoing(true)
            .build()
    }

    private fun updateNotification(port: Int) {
        notificationPort = port
        val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        manager.notify(NOTIFICATION_ID, buildNotification(port))
    }

    private fun createNotificationChannel() {
        val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        val channel = NotificationChannel(
            CHANNEL_ID,
            "Streaming server",
            NotificationManager.IMPORTANCE_LOW,
        )
        manager.createNotificationChannel(channel)
    }

    companion object {
        private const val TAG = "StreamingForegroundSvc"
        private const val GO_SERVER_TAG = "StreamingGoServer"
        private const val GO_SERVER_LIBRARY = "libstreaming.so"
        private const val GO2RTC_LIBRARY = "libgo2rtc.so"
        private const val CHANNEL_ID = "streaming_server"
        private const val NOTIFICATION_ID = 1001
        private const val RESTART_DELAY_MS = 2_000L
        private const val HEALTH_CHECK_INTERVAL_MS = 1_000L
        private const val HEALTH_CHECK_TIMEOUT_MS = 750

        fun start(context: Context) {
            val intent = Intent(context, StreamingForegroundService::class.java)
            context.startForegroundService(intent)
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, StreamingForegroundService::class.java))
        }
    }
}
