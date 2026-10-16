package io.p2psync.android

import android.app.Application
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import mobile.Mobile

enum class Screen { Home, Scan, Invite, Settings, Logs }

data class Notice(val text: String, val error: Boolean = false)

data class InviteView(val code: String, val folderId: String, val expiresAtMs: Long)

data class UiState(
    val screen: Screen = Screen.Home,
    val status: EngineStatus = EngineStatus(false),
    val config: AppConfig? = null,
    val deviceId: String = "",
    val wifiOnly: Boolean = true,
    val busy: String? = null,
    val busyCancelable: Boolean = false, // waiting for the other device's owner to accept
    val notice: Notice? = null,
    val toConfirm: InviteInfo? = null,
    val showPaste: Boolean = false,
    val invite: InviteView? = null,
    val logs: String = "",
)

class MainViewModel(app: Application) : AndroidViewModel(app) {
    private val settings = Settings(app)
    private val _ui = MutableStateFlow(UiState(wifiOnly = settings.wifiOnly))
    val ui: StateFlow<UiState> = _ui.asStateFlow()

    private fun str(id: Int, vararg a: Any) = getApplication<Application>().getString(id, *a)
    private fun io(block: suspend () -> Unit) = viewModelScope.launch(Dispatchers.IO) { block() }

    init {
        io {
            runCatching { Engine.ensureInitialised(getApplication()) }
            refresh()
        }
    }

    /** Polled by the UI while it is visible. */
    fun refresh() {
        val ctx = getApplication<Application>()
        val status = Engine.status()
        val cfg = Engine.config(ctx)
        val id = runCatching { Mobile.deviceID(Engine.home(ctx)) }.getOrDefault("")
        _ui.update { it.copy(status = status, config = cfg, deviceId = id, wifiOnly = settings.wifiOnly) }
    }

    fun go(screen: Screen) {
        _ui.update { it.copy(screen = screen, invite = if (screen == Screen.Invite) it.invite else null) }
        if (screen == Screen.Logs) loadLogs()
    }

    fun notice(n: Notice?) = _ui.update { it.copy(notice = n) }
    fun showPaste(show: Boolean) = _ui.update { it.copy(showPaste = show) }

    // ---- pairing ---------------------------------------------------------------------

    /** A code arrived (camera, pasted text or a p2psync:// link). Nothing is done until the user confirms. */
    fun submitCode(text: String) {
        // A link or QR that arrives while a dialog is open or a pairing runs must not
        // replace what the user is looking at (a web page could fire links repeatedly).
        val cur = _ui.value
        if (cur.toConfirm != null || cur.busy != null) return
        val info = parseInvite(text)
        val problem = when {
            info == null -> R.string.invalid_code
            info.expired() -> R.string.confirm_expired
            !info.forRelay -> R.string.confirm_not_relay
            else -> null
        }
        _ui.update {
            if (problem == null) {
                it.copy(toConfirm = info, screen = Screen.Home, showPaste = false)
            } else {
                it.copy(notice = Notice(str(problem), error = true), screen = Screen.Home, showPaste = false)
            }
        }
    }

    fun cameraError() {
        _ui.update { it.copy(screen = Screen.Home, notice = Notice(str(R.string.camera_error), error = true)) }
    }

    fun cancelPair() = _ui.update { it.copy(toConfirm = null) }

    /** Gives up waiting for the other device's owner to accept. */
    fun cancelWait() = io { runCatching { Mobile.cancelPair() } }

    fun confirmPair() {
        val info = _ui.value.toConfirm ?: return
        val waiting = info.network && info.approve
        _ui.update {
            it.copy(
                toConfirm = null, busyCancelable = waiting,
                busy = if (waiting) str(R.string.pairing_waiting, info.shownName) else str(R.string.pairing_busy),
            )
        }
        io {
            val ctx = getApplication<Application>()
            val result = runCatching {
                Engine.ensureInitialised(ctx)
                Mobile.pair(Engine.home(ctx), info.code, Engine.storage(ctx), 0)
            }
            result.onSuccess {
                settings.relayEnabled = true
                // The user may have left the app during the pairing: Android then refuses to
                // start a foreground service. The pairing is saved either way; the relay
                // starts the next time the app is opened (see MainActivity.onStart).
                runCatching { RelayService.start(ctx) }
                _ui.update { s -> s.copy(busy = null, busyCancelable = false, notice = Notice(str(R.string.paired_ok, info.shownName))) }
            }.onFailure { e ->
                _ui.update { s -> s.copy(busy = null, busyCancelable = false, notice = Notice(str(R.string.pair_failed, e.message ?: "?"), error = true)) }
            }
            refresh()
        }
    }

    fun generateInvite(folderId: String) = io {
        val ctx = getApplication<Application>()
        runCatching { Mobile.newInvite(Engine.home(ctx), folderId) }
            .onSuccess { code ->
                _ui.update { it.copy(invite = InviteView(code, folderId, System.currentTimeMillis() + 10 * 60_000)) }
            }
            .onFailure { e ->
                val msg = if (!Mobile.isRunning()) str(R.string.invite_need_running) else e.message ?: "?"
                _ui.update { it.copy(notice = Notice(msg, error = true)) }
            }
    }

    // ---- relay and settings --------------------------------------------------------------

    fun setRelay(on: Boolean) {
        val ctx = getApplication<Application>()
        if (on) RelayService.start(ctx) else RelayService.stop(ctx)
    }

    fun setWifiOnly(on: Boolean) {
        settings.wifiOnly = on
        _ui.update { it.copy(wifiOnly = on) }
        if (settings.relayEnabled) RelayService.start(getApplication()) // re-evaluates immediately
    }

    private fun edit(block: (String) -> Unit) = io {
        val ctx = getApplication<Application>()
        runCatching { block(Engine.home(ctx)) }.onFailure { e ->
            _ui.update { it.copy(notice = Notice(e.message ?: "?", error = true)) }
        }
        refresh()
    }

    fun setLan(on: Boolean) = edit { Mobile.setLANDiscovery(it, on) }
    fun setName(name: String) = edit { Mobile.setDeviceName(it, name) }
    fun setQuota(folder: String, mb: Long) = edit { Mobile.setQuota(it, folder, mb) }
    fun setRetention(folder: String, hours: Int) = edit { Mobile.setRetentionHours(it, folder, hours.toLong()) }
    fun removeDevice(id: String) = edit { Mobile.removeDevice(it, id) }
    fun removeFolder(id: String) = edit { Mobile.removeFolder(it, id) }

    fun loadLogs() = io { _ui.update { it.copy(logs = Mobile.logs()) } }
}
