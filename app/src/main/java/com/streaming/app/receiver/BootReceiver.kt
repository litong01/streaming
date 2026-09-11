package com.streaming.app.receiver

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import com.streaming.app.StreamingApplication
import com.streaming.app.service.ServerWatchdogWorker
import com.streaming.app.service.StreamingForegroundService

/**
 * Starts the control server again after a reboot or an app update. Generic
 * tablets do not all send the same broadcast, so several are accepted. The
 * server keeps its state in device-protected storage, so LOCKED_BOOT_COMPLETED
 * is enough to bring it up on a tablet that is still at its screen lock.
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent?) {
        val action = intent?.action ?: return
        if (action !in HANDLED_ACTIONS) {
            return
        }

        Log.i(TAG, "Starting the control server after $action")
        StreamingForegroundService.start(context)

        // WorkManager keeps its database in credential-protected storage, so
        // the watchdog can only be scheduled once someone has signed in.
        val app = context.applicationContext as? StreamingApplication
        if (app == null || app.isUserUnlocked()) {
            ServerWatchdogWorker.schedule(context)
        }
    }

    private companion object {
        private const val TAG = "StreamingBootReceiver"

        private val HANDLED_ACTIONS = setOf(
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_LOCKED_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED,
            "android.intent.action.QUICKBOOT_POWERON",
            "com.htc.intent.action.QUICKBOOT_POWERON",
        )
    }
}
