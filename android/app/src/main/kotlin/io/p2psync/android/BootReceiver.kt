package io.p2psync.android

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Restarts the relay after a reboot if the user had it on and something is paired. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        // Anyone can send this action to an exported receiver: act only on the real
        // system broadcast's action and only if the user asked for the relay to run.
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        if (!Settings(context).relayEnabled) return
        runCatching { RelayService.start(context) }
    }
}
