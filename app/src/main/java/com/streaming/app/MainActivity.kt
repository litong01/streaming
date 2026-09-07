package com.streaming.app

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.webkit.WebViewClient
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.streaming.app.databinding.ActivityMainBinding
import com.streaming.app.service.StreamingForegroundService
import java.net.HttpURLConnection
import java.net.URL

class MainActivity : AppCompatActivity() {

    private lateinit var binding: ActivityMainBinding

    private val notificationPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityMainBinding.inflate(layoutInflater)
        setContentView(binding.root)

        binding.configWebView.apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            webViewClient = WebViewClient()
        }

        StreamingForegroundService.start(this)
        requestNotificationPermissionIfNeeded()
        waitForServerAndLoadConfiguration()
    }

    override fun onBackPressed() {
        if (binding.configWebView.canGoBack()) {
            binding.configWebView.goBack()
        } else {
            super.onBackPressed()
        }
    }

    private fun requestNotificationPermissionIfNeeded() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val granted = ContextCompat.checkSelfPermission(
                this,
                Manifest.permission.POST_NOTIFICATIONS,
            ) == PackageManager.PERMISSION_GRANTED
            if (!granted) {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }
    }

    private fun waitForServerAndLoadConfiguration() {
        Thread({
            val bootstrap = (application as StreamingApplication).serverBootstrap
            var port = bootstrap.currentHttpPort()
            repeat(SERVER_READY_ATTEMPTS) {
                port = bootstrap.currentHttpPort(port)
                if (serverIsReady(port)) {
                    runOnUiThread { loadConfigurationPage(port) }
                    return@Thread
                }
                try {
                    Thread.sleep(SERVER_READY_RETRY_MS)
                } catch (_: InterruptedException) {
                    return@Thread
                }
            }
            runOnUiThread { loadConfigurationPage(bootstrap.currentHttpPort(port)) }
        }, "server-ready").start()
    }

    private fun serverIsReady(port: Int): Boolean {
        return try {
            val connection = URL("http://127.0.0.1:$port/api/status")
                .openConnection() as HttpURLConnection
            connection.connectTimeout = SERVER_READY_TIMEOUT_MS
            connection.readTimeout = SERVER_READY_TIMEOUT_MS
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

    private fun loadConfigurationPage(port: Int) {
        binding.configWebView.loadUrl("http://127.0.0.1:$port/config")
    }

    companion object {
        private const val SERVER_READY_ATTEMPTS = 30
        private const val SERVER_READY_RETRY_MS = 500L
        private const val SERVER_READY_TIMEOUT_MS = 400
    }
}
