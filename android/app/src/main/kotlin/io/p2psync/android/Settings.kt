package io.p2psync.android

import android.content.Context
import android.content.SharedPreferences

/** The few preferences that belong to the app rather than to the engine. */
class Settings(context: Context) {
    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences("settings", Context.MODE_PRIVATE)

    /** The user wants the relay running (restored after a reboot). */
    var relayEnabled: Boolean
        get() = prefs.getBoolean("relay_enabled", false)
        set(v) = prefs.edit().putBoolean("relay_enabled", v).apply()

    /** Pause on mobile data. On by default: a relay can move a lot of data. */
    var wifiOnly: Boolean
        get() = prefs.getBoolean("wifi_only", true)
        set(v) = prefs.edit().putBoolean("wifi_only", v).apply()
}
