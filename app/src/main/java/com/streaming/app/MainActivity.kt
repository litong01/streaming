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
        binding.configWebView.postDelayed(
            { loadConfigurationPage() },
            SERVER_START_DELAY_MS,
        )
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

    private fun loadConfigurationPage() {
        val port = (application as StreamingApplication).configStore.load().httpPort
        binding.configWebView.loadUrl("http://127.0.0.1:$port/config")
    }

    companion object {
        private const val SERVER_START_DELAY_MS = 400L
    }
}
