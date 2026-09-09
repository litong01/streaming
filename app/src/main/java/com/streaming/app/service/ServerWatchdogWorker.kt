package com.streaming.app.service

import android.content.Context
import android.util.Log
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.Worker
import androidx.work.WorkerParameters
import com.streaming.app.StreamingApplication
import java.util.concurrent.TimeUnit

/**
 * Safety net for tablets that never deliver a boot broadcast, or that kill the
 * service without restarting it. Starting an already running service is a
 * no-op, so this only has an effect when the server is actually gone.
 */
class ServerWatchdogWorker(
    context: Context,
    parameters: WorkerParameters,
) : Worker(context, parameters) {

    override fun doWork(): Result {
        val app = applicationContext as StreamingApplication
        val port = app.serverBootstrap.currentHttpPort()
        if (StreamingForegroundService.isServerListening(port)) {
            return Result.success()
        }
        Log.w(TAG, "Control server is not answering on port $port, starting it")
        StreamingForegroundService.start(applicationContext)
        return Result.success()
    }

    companion object {
        private const val TAG = "StreamingWatchdog"
        private const val WORK_NAME = "streaming-server-watchdog"

        fun schedule(context: Context) {
            val request = PeriodicWorkRequestBuilder<ServerWatchdogWorker>(
                PERIOD_MINUTES,
                TimeUnit.MINUTES,
            ).build()

            WorkManager.getInstance(context).enqueueUniquePeriodicWork(
                WORK_NAME,
                ExistingPeriodicWorkPolicy.UPDATE,
                request,
            )
        }

        private const val PERIOD_MINUTES = 15L
    }
}
