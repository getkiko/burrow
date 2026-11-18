package com.getkiko.burrow

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import androidx.activity.ComponentActivity
import androidx.activity.SystemBarStyle
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import com.getkiko.burrow.ui.App
import com.getkiko.burrow.ui.BurrowTheme

class MainActivity : ComponentActivity() {

    private var service by mutableStateOf<TunnelService?>(null)

    private val connection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName?, binder: IBinder?) {
            service = (binder as? TunnelService.LocalBinder)?.service
        }

        override fun onServiceDisconnected(name: ComponentName?) {
            service = null
        }
    }

    private val requestNotificationPermission =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { /* result ignored */ }

    override fun onCreate(savedInstanceState: Bundle?) {
        // enableEdgeToEdge must be called before super.onCreate.
        //
        // SystemBarStyle.auto(TRANSPARENT, TRANSPARENT) lets the system pick dark
        // or light status bar icons for the current theme: dark icons on a light
        // background, light icons on a dark one. Without it the system defaulted to
        // white icons on the app's light background, making battery, signal and
        // clock invisible.
        //
        // detectDarkMode follows the Configuration UI mode directly instead of the
        // Activity theme, so it agrees with isSystemInDarkTheme() on the Compose
        // side and the content and status bar icons never disagree.
        enableEdgeToEdge(
            statusBarStyle = SystemBarStyle.auto(
                android.graphics.Color.TRANSPARENT,
                android.graphics.Color.TRANSPARENT,
            ) { resources ->
                (resources.configuration.uiMode and
                    android.content.res.Configuration.UI_MODE_NIGHT_MASK) ==
                    android.content.res.Configuration.UI_MODE_NIGHT_YES
            },
        )
        super.onCreate(savedInstanceState)

        // Android 13+ needs a runtime grant, or the foreground service notification is not shown.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            requestNotificationPermission.launch(android.Manifest.permission.POST_NOTIFICATIONS)
        }

        requestIgnoreBatteryOptimizations()

        val configStore = ConfigStore(this)
        setContent {
            BurrowTheme {
                App(service = service, configStore = configStore)
            }
        }
    }

    /**
     * Asks the user to exempt the app from battery optimizations.
     *
     * A foreground service only survives stock Doze, not vendor ROM power savers
     * (Xiaomi, Huawei, OPPO, ...), which freeze the network of background apps:
     * the tunnel works in the foreground and drops once the app is backgrounded.
     * The allowlist is the only remedy available from the app side.
     *
     * Only asks while the app is not yet exempt. If the user declines, the tunnel
     * still runs; it may just be frozen in the background.
     */
    private fun requestIgnoreBatteryOptimizations() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return
        val power = getSystemService(POWER_SERVICE) as? android.os.PowerManager ?: return
        if (power.isIgnoringBatteryOptimizations(packageName)) return
        try {
            startActivity(
                Intent(
                    android.provider.Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                    android.net.Uri.parse("package:$packageName"),
                ),
            )
        } catch (e: Throwable) {
            // Some ROMs lack this Activity or reject the Intent. Ignore it; the user
            // can still add the allowlist entry in system settings.
        }
    }

    override fun onStart() {
        super.onStart()
        bindService(
            Intent(this, TunnelService::class.java),
            connection,
            Context.BIND_AUTO_CREATE,
        )
    }

    override fun onStop() {
        super.onStop()
        // Only unbind, do not stop the service: the tunnel keeps running while the UI is in the background.
        try {
            unbindService(connection)
        } catch (e: Throwable) {
            // unbindService throws when not bound; ignore
        }
    }
}
