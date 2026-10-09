package dev.sammesh.connect

import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import android.content.Intent
import android.util.Log
import android.content.Context

class MainActivity : FlutterActivity() {
    private val CHANNEL = "dev.sammesh.connect/mesh_expose"
    private val ENROLL_LINK_CHANNEL = "dev.sammesh.connect/enroll_link"
    private var enrollLinkChannel: MethodChannel? = null

    // A mesh://enroll link that arrived before Dart asked for it (cold start).
    private var pendingEnrollLink: String? = null

    override fun configureFlutterEngine(flutterEngine: FlutterEngine) {
        super.configureFlutterEngine(flutterEngine)

        pendingEnrollLink = enrollLinkOf(intent)
        enrollLinkChannel = MethodChannel(flutterEngine.dartExecutor.binaryMessenger, ENROLL_LINK_CHANNEL).also { channel ->
            channel.setMethodCallHandler { call, result ->
                when (call.method) {
                    "getInitialLink" -> {
                        result.success(pendingEnrollLink)
                        pendingEnrollLink = null
                    }
                    else -> result.notImplemented()
                }
            }
        }
        
        MethodChannel(flutterEngine.dartExecutor.binaryMessenger, CHANNEL).setMethodCallHandler { call, result ->
            when (call.method) {
                "startBackgroundService" -> {
                    try {
                        val intent = android.content.Intent(this, SamNodeForegroundService::class.java)
                        if (android.os.Build.VERSION.SDK_INT >= android.os.Build.VERSION_CODES.O) {
                            startForegroundService(intent)
                        } else {
                            startService(intent)
                        }
                        result.success(true)
                    } catch (e: Exception) {
                        result.error("SERVICE_START_ERROR", "Failed to start background service: ${e.message}", null)
                    }
                }
                "stopBackgroundService" -> {
                    try {
                        val intent = android.content.Intent(this, SamNodeForegroundService::class.java)
                        stopService(intent)
                        result.success(true)
                    } catch (e: Exception) {
                        result.error("SERVICE_STOP_ERROR", "Failed to stop background service: ${e.message}", null)
                    }
                }
                "setExposeBattery" -> {
                    val enabled = call.argument<Boolean>("enabled") ?: false
                    Log.d("SAM_NODE", "setExposeBattery: $enabled")
                    // TODO: Start/Stop Battery service and register with SAM
                    result.success(true)
                }
                "setExposeLocation" -> {
                    val enabled = call.argument<Boolean>("enabled") ?: false
                    Log.d("SAM_NODE", "setExposeLocation: $enabled")
                    if (enabled) {
                        // Coarse only: the tool promises an approximate position, so
                        // the app must not hold a permission that could give more.
                        if (androidx.core.content.ContextCompat.checkSelfPermission(this, android.Manifest.permission.ACCESS_COARSE_LOCATION) != android.content.pm.PackageManager.PERMISSION_GRANTED) {
                            androidx.core.app.ActivityCompat.requestPermissions(this, arrayOf(android.Manifest.permission.ACCESS_COARSE_LOCATION), 1001)
                        }
                    }
                    result.success(true)
                }
                "getBatteryData" -> {
                    try {
                        val batteryManager = getSystemService(Context.BATTERY_SERVICE) as android.os.BatteryManager
                        val batteryLevel = batteryManager.getIntProperty(android.os.BatteryManager.BATTERY_PROPERTY_CAPACITY)
                        
                        val intent = registerReceiver(null, android.content.IntentFilter(android.content.Intent.ACTION_BATTERY_CHANGED))
                        val status = intent?.getIntExtra(android.os.BatteryManager.EXTRA_STATUS, -1) ?: -1
                        val isCharging = status == android.os.BatteryManager.BATTERY_STATUS_CHARGING || status == android.os.BatteryManager.BATTERY_STATUS_FULL
                        
                        result.success("{\"battery_level\": $batteryLevel, \"charging\": $isCharging}")
                    } catch (e: Exception) {
                        result.error("BATTERY_ERROR", "Failed to get battery data: ${e.message}", null)
                    }
                }
                "getLocationData" -> {
                    try {
                        if (androidx.core.content.ContextCompat.checkSelfPermission(this, android.Manifest.permission.ACCESS_COARSE_LOCATION) != android.content.pm.PackageManager.PERMISSION_GRANTED) {
                            result.success("{\"error\": \"Location permission not granted\"}")
                            return@setMethodCallHandler
                        }

                        // Network provider only, and rounded to two decimals (about
                        // a kilometre): a mesh peer gets the neighbourhood, not the
                        // building, whatever the platform would hand this app.
                        val locationManager = getSystemService(Context.LOCATION_SERVICE) as android.location.LocationManager
                        val location: android.location.Location? =
                            locationManager.getLastKnownLocation(android.location.LocationManager.NETWORK_PROVIDER)

                        if (location != null) {
                            val lat = coarsen(location.latitude)
                            val lon = coarsen(location.longitude)
                            result.success("{\"latitude\": $lat, \"longitude\": $lon, \"precision_km\": 1}")
                        } else {
                            result.success("{\"error\": \"No location available\"}")
                        }
                    } catch (e: Exception) {
                        result.error("LOCATION_ERROR", "Failed to get location data: ${e.message}", null)
                    }
                }
                else -> {
                    result.notImplemented()
                }
            }
        }
    }

    // Two decimal places of a degree is roughly 1.1 km at the equator.
    private fun coarsen(degrees: Double): Double = Math.round(degrees * 100.0) / 100.0

    // singleTop: a link opened while the app is running lands here instead of
    // in a new activity, so it is pushed to Dart rather than queued.
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        val link = enrollLinkOf(intent) ?: return
        val channel = enrollLinkChannel
        if (channel != null) {
            channel.invokeMethod("onLink", link)
        } else {
            pendingEnrollLink = link
        }
    }

    private fun enrollLinkOf(intent: Intent?): String? {
        val data = intent?.data ?: return null
        if (intent.action != Intent.ACTION_VIEW || data.scheme != "mesh") return null
        return data.toString()
    }
}
