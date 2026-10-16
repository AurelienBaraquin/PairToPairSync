package io.p2psync.android

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/** What the relay service is doing, observable from the UI (same process). */
sealed interface Phase {
    data object Stopped : Phase
    data object Starting : Phase
    data object Running : Phase
    /** Wi‑Fi only is on and the phone is on mobile data. */
    data object PausedNoWifi : Phase
    data class Error(val message: String) : Phase
}

object RelayState {
    private val _phase = MutableStateFlow<Phase>(Phase.Stopped)
    val phase: StateFlow<Phase> = _phase.asStateFlow()
    fun set(p: Phase) { _phase.value = p }
}
