package com.streaming.app.server

import android.content.res.AssetManager

/**
 * The pages served to Fully Kiosk Browser. They are the files in the
 * repository's `web` directory, packaged as assets by the Gradle build, so the
 * Android app and the standalone Go server serve exactly the same HTML.
 */
class WebPages(private val assets: AssetManager) {

    private val control by lazy { read("control.html") }
    private val config by lazy { read("config.html") }

    fun controlPage(): String = control

    fun configPage(): String = config

    private fun read(name: String): String =
        assets.open(name).bufferedReader().use { reader -> reader.readText() }
}
