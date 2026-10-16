package io.p2psync.android

import android.content.Context
import android.os.Build
import mobile.Mobile
import java.io.File

/**
 * Thin wrapper around the Go engine (package `mobile`, built with gomobile).
 * All state lives in the app's private storage: other apps cannot read it, and
 * it is excluded from backups (the device key must never leave the phone).
 */
object Engine {
    fun home(context: Context): String =
        File(context.filesDir, "p2psync").apply { mkdirs() }.absolutePath

    /** Where undelivered changes are kept temporarily (encrypted by the engine). */
    fun storage(context: Context): String =
        File(context.filesDir, "relay-store").apply { mkdirs() }.absolutePath

    fun defaultName(): String {
        val m = Build.MODEL?.takeIf { it.isNotBlank() } ?: "Android"
        return m.filter { it.isLetterOrDigit() || it == ' ' || it == '-' }.take(40).ifBlank { "Android" }
    }

    /** Creates the device identity on first use; returns the device ID. */
    fun ensureInitialised(context: Context): String = Mobile.init(home(context), defaultName())

    fun status(): EngineStatus = runCatching { parseStatus(Mobile.statusJSON()) }.getOrDefault(EngineStatus(false))

    fun config(context: Context): AppConfig? =
        runCatching { parseConfig(Mobile.configJSON(home(context))) }.getOrNull()

    fun hasFolders(context: Context): Boolean = config(context)?.folders?.isNotEmpty() == true
}
