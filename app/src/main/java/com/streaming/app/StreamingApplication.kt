package com.streaming.app

import android.app.Application
import android.os.UserManager
import com.streaming.app.config.ServerBootstrap
import com.streaming.app.service.ServerWatchdogWorker

class StreamingApplication : Application() {

    /**
     * Created lazily because the process can start before the user has
     * unlocked the device, and the encrypted preferences behind it are not
     * readable until then.
     */
    val serverBootstrap: ServerBootstrap by lazy { ServerBootstrap(this) }

    override fun onCreate() {
        super.onCreate()
        if (isUserUnlocked()) {
            ServerWatchdogWorker.schedule(this)
        }
    }

    fun isUserUnlocked(): Boolean {
        val userManager = getSystemService(UserManager::class.java) ?: return true
        return userManager.isUserUnlocked
    }
}
