package com.streaming.app.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import android.util.Log
import androidx.core.app.NotificationCompat
import com.streaming.app.MainActivity
import com.streaming.app.R
import com.streaming.app.StreamingApplication
import com.streaming.app.config.AppConfig
import com.streaming.app.server.StreamingHttpServer
import com.streaming.app.smp.StreamState
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class StreamingForegroundService : Service() {

    private val serviceScope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var pollJob: Job? = null
    private var httpServer: StreamingHttpServer? = null

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForeground(NOTIFICATION_ID, buildNotification())
        startServer()
        return START_STICKY
    }

    override fun onDestroy() {
        pollJob?.cancel()
        serviceScope.cancel()
        httpServer?.shutdownServer()
        httpServer = null
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun startServer() {
        val app = application as StreamingApplication
        val config = app.configStore.load()
        restartHttpServer(app, config)
        startPolling(app, config)
    }

    private fun restartHttpServer(app: StreamingApplication, config: AppConfig) {
        httpServer?.shutdownServer()
        val server = StreamingHttpServer(
            port = config.httpPort,
            configStore = app.configStore,
            smpClient = app.smpClient,
            onConfigSaved = { updatedConfig ->
                serviceScope.launch {
                    restartHttpServer(app, updatedConfig)
                    startPolling(app, updatedConfig)
                }
            },
            initialState = httpServer?.currentState() ?: StreamState(),
        )
        try {
            server.start(NanoTimeout.SOCKET_READ_TIMEOUT, false)
            httpServer = server
            updateNotification(config.httpPort)
            Log.i(TAG, "HTTP server listening on port ${config.httpPort}")
        } catch (error: Exception) {
            Log.e(TAG, "Failed to start HTTP server on port ${config.httpPort}", error)
        }
    }

    private fun startPolling(app: StreamingApplication, config: AppConfig) {
        pollJob?.cancel()
        pollJob = serviceScope.launch {
            while (isActive) {
                val latestConfig = app.configStore.load()
                val state = withContext(Dispatchers.IO) {
                    app.smpClient.queryState(latestConfig)
                }
                httpServer?.updateState(state)
                delay(latestConfig.pollIntervalSeconds * 1000L)
            }
        }
    }

    private fun buildNotification(port: Int = currentHttpPort()): Notification {
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
        val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        manager.notify(NOTIFICATION_ID, buildNotification(port))
    }

    private fun currentHttpPort(): Int {
        return (application as StreamingApplication).configStore.load().httpPort
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
        private const val CHANNEL_ID = "streaming_server"
        private const val NOTIFICATION_ID = 1001

        fun start(context: Context) {
            val intent = Intent(context, StreamingForegroundService::class.java)
            context.startForegroundService(intent)
        }

        fun stop(context: Context) {
            context.stopService(Intent(context, StreamingForegroundService::class.java))
        }
    }

    private object NanoTimeout {
        const val SOCKET_READ_TIMEOUT = 5_000
    }
}
