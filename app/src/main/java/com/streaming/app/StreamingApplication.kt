package com.streaming.app

import android.app.Application
import com.streaming.app.config.ServerBootstrap

class StreamingApplication : Application() {

    lateinit var serverBootstrap: ServerBootstrap
        private set

    override fun onCreate() {
        super.onCreate()
        serverBootstrap = ServerBootstrap(this)
    }
}
