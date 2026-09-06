package com.streaming.app

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.streaming.app.databinding.ActivityMainBinding
import com.streaming.app.service.StreamingForegroundService

class MainActivity : AppCompatActivity() {

    private lateinit var binding: ActivityMainBinding
    private var serviceRunning = false

    private val notificationPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { granted ->
        if (granted) {
            ensureServiceRunning()
        } else {
            updateUi()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityMainBinding.inflate(layoutInflater)
        setContentView(binding.root)

        binding.toggleServiceButton.setOnClickListener {
            if (serviceRunning) {
                StreamingForegroundService.stop(this)
                serviceRunning = false
            } else {
                requestNotificationsIfNeeded()
            }
            updateUi()
        }

        binding.openControlButton.setOnClickListener {
            openUrl(controlUrl())
        }

        binding.openConfigButton.setOnClickListener {
            openUrl(configUrl())
        }

        if (savedInstanceState?.getBoolean(KEY_SERVICE_RUNNING) == true) {
            serviceRunning = true
        } else {
            requestNotificationsIfNeeded()
        }

        updateUi()
    }

    override fun onResume() {
        super.onResume()
        updateUi()
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putBoolean(KEY_SERVICE_RUNNING, serviceRunning)
    }

    private fun requestNotificationsIfNeeded() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val granted = ContextCompat.checkSelfPermission(
                this,
                Manifest.permission.POST_NOTIFICATIONS,
            ) == PackageManager.PERMISSION_GRANTED
            if (!granted) {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
                return
            }
        }
        ensureServiceRunning()
    }

    private fun ensureServiceRunning() {
        StreamingForegroundService.start(this)
        serviceRunning = true
        updateUi()
    }

    private fun updateUi() {
        val config = (application as StreamingApplication).configStore.load()
        binding.serviceStatusText.text = if (serviceRunning) {
            getString(R.string.service_status_running)
        } else {
            getString(R.string.service_status_stopped)
        }
        binding.localUrlText.text = controlUrl(config.httpPort)
        binding.toggleServiceButton.text = if (serviceRunning) {
            getString(R.string.stop_service)
        } else {
            getString(R.string.start_service)
        }
        binding.openControlButton.isEnabled = serviceRunning
        binding.openConfigButton.isEnabled = serviceRunning
    }

    private fun controlUrl(port: Int = currentPort()): String {
        return "http://127.0.0.1:$port/"
    }

    private fun configUrl(port: Int = currentPort()): String {
        return "http://127.0.0.1:$port/config"
    }

    private fun currentPort(): Int {
        return (application as StreamingApplication).configStore.load().httpPort
    }

    private fun openUrl(url: String) {
        val intent = Intent(Intent.ACTION_VIEW, Uri.parse(url))
        startActivity(intent)
    }

    companion object {
        private const val KEY_SERVICE_RUNNING = "service_running"
    }
}
