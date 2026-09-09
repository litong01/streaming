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
 * server configuration lives in credential-encrypted storage, which is only
 * readable once the user has unlocked the device the first time.
 */
class BootReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent?) {
        val action = intent?.action ?: return
        if (action !in HANDLED_ACTIONS) {
            return
        }

        val app = context.applicationContext as? StreamingApplication
        if (app?.isUserUnlocked() == false) {
            // Storage is still encrypted. BOOT_COMPLETED arrives at unlock.
            Log.i(TAG, "Ignoring $action until the device is unlocked")
            return
        }

        Log.i(TAG, "Starting the control server after $action")
        StreamingForegroundService.start(context)
        ServerWatchdogWorker.schedule(context)
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
