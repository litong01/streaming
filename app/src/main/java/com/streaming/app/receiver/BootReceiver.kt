package com.streaming.app.receiver

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import com.streaming.app.service.StreamingForegroundService

class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent?) {
        if (intent?.action != Intent.ACTION_BOOT_COMPLETED) {
            return
        }
        StreamingForegroundService.start(context)
    }
}
