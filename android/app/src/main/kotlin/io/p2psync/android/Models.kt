package io.p2psync.android

import org.json.JSONArray
import org.json.JSONObject
import java.util.Base64

// Plain data and parsing, with no Android dependency so it can be unit-tested on
// the JVM. The JSON comes from the Go engine (see ../mobile in the repository).

data class PeerInfo(val name: String, val relayed: Boolean)

data class FolderStatus(
    val id: String,
    val label: String,
    val state: String,
    val pendingChanges: Int,
    val storedBytes: Long,
    val quotaBytes: Long,
    val waitingForDevices: Int,
    val connectedPeers: List<String>,
)

data class EngineStatus(
    val running: Boolean,
    val deviceId: String = "",
    val deviceName: String = "",
    val addresses: List<String> = emptyList(),
    val folders: List<FolderStatus> = emptyList(),
    val peers: List<PeerInfo> = emptyList(),
) {
    val pendingTotal get() = folders.sumOf { it.pendingChanges }
    val storedTotal get() = folders.sumOf { it.storedBytes }
}

data class DeviceCfg(val id: String, val name: String, val managed: Boolean = false)

data class FolderCfg(
    val id: String,
    val label: String,
    val role: String,
    val quotaMb: Long,
    val retentionHours: Int,
    val devices: List<DeviceCfg>,
    val managed: Boolean = false, // introduced by the device this phone was paired with
) {
    val title get() = label.ifBlank { id }
}

data class AppConfig(
    val deviceName: String,
    val listenPort: Int,
    val lanDiscovery: Boolean,
    val devices: List<DeviceCfg>,
    val folders: List<FolderCfg>,
)

private fun JSONArray?.strings(): List<String> =
    if (this == null) emptyList() else List(length()) { getString(it) }

private inline fun <T> JSONArray?.objects(f: (JSONObject) -> T): List<T> =
    if (this == null) emptyList() else List(length()) { f(getJSONObject(it)) }

fun parseStatus(json: String): EngineStatus {
    val root = JSONObject(json)
    if (!root.optBoolean("running")) return EngineStatus(running = false)
    val st = root.getJSONObject("status")
    return EngineStatus(
        running = true,
        deviceId = st.optString("device_id"),
        deviceName = st.optString("device_name"),
        addresses = st.optJSONArray("addresses").strings(),
        folders = st.optJSONArray("folders").objects { f ->
            FolderStatus(
                id = f.optString("id"),
                label = f.optString("label"),
                state = f.optString("state"),
                pendingChanges = f.optInt("pending_changes"),
                storedBytes = f.optLong("stored_bytes"),
                quotaBytes = f.optLong("quota_bytes"),
                waitingForDevices = f.optJSONArray("waiting_for_devices")?.length() ?: 0,
                connectedPeers = f.optJSONArray("connected_peers").strings(),
            )
        },
        peers = st.optJSONArray("peers").objects { p ->
            PeerInfo(name = p.optString("name"), relayed = p.optBoolean("relayed"))
        },
    )
}

fun parseConfig(json: String): AppConfig {
    val root = JSONObject(json)
    fun devices(a: JSONArray?) = a.objects {
        DeviceCfg(it.optString("id"), it.optString("name"), it.optBoolean("managed"))
    }
    return AppConfig(
        deviceName = root.optString("device_name"),
        listenPort = root.optInt("listen_port"),
        lanDiscovery = root.optBoolean("lan_discovery"),
        devices = devices(root.optJSONArray("devices")),
        folders = root.optJSONArray("folders").objects { f ->
            FolderCfg(
                id = f.optString("id"),
                label = f.optString("label"),
                role = f.optString("role"),
                quotaMb = f.optLong("quota_mb"),
                retentionHours = f.optInt("retention_hours"),
                devices = devices(f.optJSONArray("devices")),
                managed = f.optBoolean("managed"),
            )
        },
    )
}

/** What a scanned invitation says, shown to the user *before* anything is done. */
data class InviteInfo(
    val code: String,
    val inviterId: String,
    val inviterName: String,
    val folder: String,
    val label: String,
    val forRelay: Boolean,
    val expiresAtSec: Long,
    /** Joining the inviter's whole network: it introduces this phone to its other devices, for [folders]. */
    val network: Boolean = false,
    val folders: List<String> = emptyList(),
    /** The inviter's owner has to accept the request: the phone waits for an answer. */
    val approve: Boolean = false,
) {
    fun expired(nowMs: Long = System.currentTimeMillis()) = nowMs / 1000 > expiresAtSec
    val shownName get() = inviterName.ifBlank { shortId }
    val shownFolder get() = if (network) folders.joinToString(", ") else label.ifBlank { folder }
    /** The start and end of the inviter's ID (all IDs begin alike): names are free text, the ID is what actually identifies a device. */
    val shortId get() = if (inviterId.length <= 14) inviterId else inviterId.take(6) + "…" + inviterId.takeLast(6)
}

// Text in a QR code is chosen by whoever made it. Before it is shown in a
// confirmation dialog, drop control and bidirectional-override characters (which
// can reorder or hide text) and cap the length.
private val INVISIBLE = Regex("[\\p{Cc}\\p{Cf}\\p{Zl}\\p{Zp}]")

internal fun cleanText(s: String) = s.replace(INVISIBLE, "").trim().take(48)

const val INVITE_PREFIX = "p2psync://pair/"

/**
 * Reads the text of a pairing QR code for display. The Go engine does the real
 * validation when pairing; this only decides what to show in the confirmation.
 * Returns null if the text is not an invitation.
 */
fun parseInvite(text: String): InviteInfo? {
    val payload = text.trim().removePrefix(INVITE_PREFIX)
    // The engine only accepts unpadded base64url: refuse here what it would refuse later.
    if (payload.contains('=')) return null
    return try {
        val obj = JSONObject(String(Base64.getUrlDecoder().decode(payload), Charsets.UTF_8))
        val v = obj.optInt("v")
        val network = v == 2 && obj.optBoolean("nw")
        val folders = obj.optJSONArray("fs").strings().take(64).map { cleanText(it) }.filter { it.isNotEmpty() }
        if ((v != 1 && v != 2) || obj.optString("id").isBlank() || obj.optString("t").isBlank() ||
            (v == 2 && !network) || (network && folders.isEmpty()) || (v == 1 && obj.has("nw"))
        ) null
        else InviteInfo(
            code = INVITE_PREFIX + payload,
            inviterId = obj.getString("id"),
            inviterName = cleanText(obj.optString("n")),
            folder = cleanText(obj.optString("f")),
            label = cleanText(obj.optString("l")),
            forRelay = obj.optBoolean("er"),
            expiresAtSec = obj.optLong("x"),
            network = network,
            folders = if (network) folders else emptyList(),
            approve = network && obj.optBoolean("ap"),
        )
    } catch (e: Exception) {
        null
    }
}

fun formatBytes(b: Long): String = when {
    b >= 1_000_000_000 -> "%.1f GB".format(b / 1e9)
    b >= 1_000_000 -> "%.1f MB".format(b / 1e6)
    b >= 1_000 -> "%.0f kB".format(b / 1e3)
    else -> "$b B"
}
