package com.streaming.app

import android.app.Application
import com.streaming.app.config.ConfigStore
import com.streaming.app.smp.SmpClient

class StreamingApplication : Application() {

    lateinit var configStore: ConfigStore
        private set

    lateinit var smpClient: SmpClient
        private set

    override fun onCreate() {
        super.onCreate()
        configStore = ConfigStore(this)
        smpClient = SmpClient()
    }
}
