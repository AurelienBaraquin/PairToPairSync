package io.p2psync.android.ui

import android.Manifest
import android.content.ClipData
import android.content.ClipDescription
import android.content.ClipboardManager
import android.content.Context
import android.content.ContextWrapper
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.PersistableBundle
import android.provider.Settings as AndroidSettings
import android.util.Size
import android.os.PowerManager
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.Preview
import androidx.camera.core.resolutionselector.ResolutionSelector
import androidx.camera.core.resolutionselector.ResolutionStrategy
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Slider
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.ContextCompat
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.repeatOnLifecycle
import io.p2psync.android.FolderCfg
import io.p2psync.android.FolderStatus
import io.p2psync.android.Phase
import io.p2psync.android.QrAnalyzer
import io.p2psync.android.R
import io.p2psync.android.MainViewModel
import io.p2psync.android.RelayState
import io.p2psync.android.Screen
import io.p2psync.android.UiState
import io.p2psync.android.formatBytes
import io.p2psync.android.qrBitmap
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import java.text.DateFormat
import java.util.Date
import java.util.concurrent.Executors

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AppRoot(vm: MainViewModel, onToggleRelay: (Boolean) -> Unit) {
    val ui by vm.ui.collectAsStateWithLifecycle()
    val phase by RelayState.phase.collectAsStateWithLifecycle()
    val lifecycle = LocalLifecycleOwner.current.lifecycle
    val snackbar = remember { SnackbarHostState() }

    // Poll the engine while the app is visible.
    LaunchedEffect(Unit) {
        lifecycle.repeatOnLifecycle(Lifecycle.State.STARTED) {
            while (true) {
                withContext(Dispatchers.IO) { vm.refresh() }
                delay(2000)
            }
        }
    }
    LaunchedEffect(ui.notice) {
        ui.notice?.let {
            snackbar.showSnackbar(it.text)
            vm.notice(null)
        }
    }
    BackHandler(enabled = ui.screen != Screen.Home) { vm.go(Screen.Home) }

    val title = when (ui.screen) {
        Screen.Home -> R.string.home_title
        Screen.Scan -> R.string.scan_title
        Screen.Invite -> R.string.invite_title
        Screen.Settings -> R.string.settings_title
        Screen.Logs -> R.string.logs_title
    }
    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = { Text(stringResource(title)) },
                navigationIcon = {
                    if (ui.screen != Screen.Home) {
                        IconButton(onClick = { vm.go(if (ui.screen == Screen.Logs) Screen.Settings else Screen.Home) }) {
                            Icon(Icons.AutoMirrored.Filled.ArrowBack, stringResource(R.string.back))
                        }
                    }
                },
                actions = {
                    if (ui.screen == Screen.Home) {
                        IconButton(onClick = { vm.go(Screen.Settings) }) {
                            Icon(Icons.Default.Settings, stringResource(R.string.settings_title))
                        }
                    }
                },
            )
        },
    ) { pad ->
        Box(Modifier.padding(pad).fillMaxSize()) {
            when (ui.screen) {
                Screen.Home -> HomeScreen(ui, phase, vm, onToggleRelay)
                Screen.Scan -> ScanScreen(onCode = vm::submitCode, onError = vm::cameraError)
                Screen.Invite -> InviteScreen(ui, vm)
                Screen.Settings -> SettingsScreen(ui, vm)
                Screen.Logs -> LogsScreen(ui, vm)
            }
        }
    }

    ui.toConfirm?.let { info ->
        AlertDialog(
            onDismissRequest = vm::cancelPair,
            title = { Text(stringResource(R.string.confirm_title, info.shownName)) },
            text = {
                Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    if (info.network) {
                        Text(stringResource(R.string.confirm_network_body, info.shownName, info.shownFolder))
                        if (info.approve) Text(stringResource(R.string.confirm_approve_note, info.shownName))
                    } else {
                        Text(stringResource(R.string.confirm_body, info.shownFolder))
                    }
                    // A name is free text chosen by whoever made the code; the ID is what identifies a device.
                    Text(stringResource(R.string.confirm_id, info.inviterId), fontFamily = FontFamily.Monospace, fontSize = 12.sp)
                    if (ui.config?.folders?.any { it.id == info.folder } == true) { // looked up now, not when the code arrived
                        Text(
                            stringResource(R.string.confirm_exists, info.shownFolder),
                            color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
            },
            confirmButton = { Button(onClick = vm::confirmPair) { Text(stringResource(R.string.confirm_yes)) } },
            dismissButton = { TextButton(onClick = vm::cancelPair) { Text(stringResource(R.string.cancel)) } },
        )
    }
    ui.busy?.let { msg ->
        AlertDialog(
            onDismissRequest = {},
            confirmButton = {},
            dismissButton = if (ui.busyCancelable) {
                { TextButton(onClick = vm::cancelWait) { Text(stringResource(R.string.cancel)) } }
            } else null,
            title = { Text(msg) },
            text = { Box(Modifier.fillMaxWidth(), Alignment.Center) { CircularProgressIndicator() } },
        )
    }
    if (ui.showPaste) PasteDialog(onDismiss = { vm.showPaste(false) }, onSubmit = vm::submitCode)
}

// ---- home --------------------------------------------------------------------------------

@Composable
private fun HomeScreen(ui: UiState, phase: Phase, vm: MainViewModel, onToggle: (Boolean) -> Unit) {
    val ctx = LocalContext.current
    val on = phase is Phase.Running || phase is Phase.Starting || phase is Phase.PausedNoWifi
    val folders = ui.config?.folders.orEmpty()
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Card(Modifier.fillMaxWidth()) {
            Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) {
                        Text(phaseText(phase), style = MaterialTheme.typography.titleMedium)
                        if (ui.status.running) {
                            val peers = ui.status.peers
                            val viaOther = stringResource(R.string.stat_relayed)
                            Text(
                                if (peers.isEmpty()) stringResource(R.string.stat_none_connected)
                                else stringResource(
                                    R.string.stat_connected,
                                    peers.joinToString { it.name + if (it.relayed) viaOther else "" },
                                ),
                                style = MaterialTheme.typography.bodyMedium,
                            )
                        }
                    }
                    Switch(checked = on, onCheckedChange = onToggle, enabled = folders.isNotEmpty() || on)
                }
                Text(stringResource(R.string.relay_hint), style = MaterialTheme.typography.bodySmall)
            }
        }

        if (folders.isEmpty()) {
            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text(stringResource(R.string.empty_title), style = MaterialTheme.typography.titleMedium)
                    Text(stringResource(R.string.empty_body), style = MaterialTheme.typography.bodyMedium)
                }
            }
        } else {
            Text(stringResource(R.string.folders_title), style = MaterialTheme.typography.titleSmall)
            folders.forEach { cfg ->
                FolderCard(cfg, ui.status.folders.firstOrNull { it.id == cfg.id }, ui.status.running)
            }
        }

        Button(onClick = { vm.go(Screen.Scan) }, Modifier.fillMaxWidth()) { Text(stringResource(R.string.btn_scan)) }
        OutlinedButton(onClick = { vm.showPaste(true) }, Modifier.fillMaxWidth()) { Text(stringResource(R.string.btn_paste)) }
        if (folders.isNotEmpty()) {
            OutlinedButton(onClick = { vm.go(Screen.Invite) }, Modifier.fillMaxWidth()) { Text(stringResource(R.string.btn_invite)) }
        }

        if (on && !ignoringBatteryOptimisations(ctx)) {
            Card(Modifier.fillMaxWidth(), colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer)) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(stringResource(R.string.battery_title), style = MaterialTheme.typography.titleSmall)
                    Text(stringResource(R.string.battery_body), style = MaterialTheme.typography.bodySmall)
                    // The settings screen needs no special permission, unlike requesting the exemption directly.
                    TextButton(onClick = {
                        ctx.startActivity(Intent(AndroidSettings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                    }) { Text(stringResource(R.string.battery_action)) }
                }
            }
        }
    }
}

@Composable
private fun phaseText(phase: Phase): String = when (phase) {
    Phase.Stopped -> stringResource(R.string.relay_stopped)
    Phase.Starting -> stringResource(R.string.relay_starting)
    Phase.Running -> stringResource(R.string.relay_running)
    Phase.PausedNoWifi -> stringResource(R.string.relay_paused)
    is Phase.Error -> stringResource(R.string.relay_error, phase.message)
}

@Composable
private fun FolderCard(cfg: FolderCfg, st: FolderStatus?, running: Boolean) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text(cfg.title, style = MaterialTheme.typography.titleMedium)
            Text(cfg.devices.joinToString { it.name.ifBlank { it.id.take(8) } }, style = MaterialTheme.typography.bodySmall)
            if (running && st != null) {
                Text(stringResource(R.string.stat_pending, st.pendingChanges))
                if (st.quotaBytes > 0) {
                    LinearProgressIndicator(
                        progress = { (st.storedBytes.toFloat() / st.quotaBytes).coerceIn(0f, 1f) },
                        modifier = Modifier.fillMaxWidth(),
                    )
                    Text(stringResource(R.string.stat_storage, formatBytes(st.storedBytes), formatBytes(st.quotaBytes)), style = MaterialTheme.typography.bodySmall)
                }
                if (st.waitingForDevices > 0) {
                    Text(stringResource(R.string.stat_waiting, st.waitingForDevices), style = MaterialTheme.typography.bodySmall)
                }
            }
        }
    }
}

private fun ignoringBatteryOptimisations(ctx: Context): Boolean =
    ctx.getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(ctx.packageName)

@Composable
private fun PasteDialog(onDismiss: () -> Unit, onSubmit: (String) -> Unit) {
    var text by remember { mutableStateOf("") }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(stringResource(R.string.paste_title)) },
        text = {
            OutlinedTextField(
                value = text, onValueChange = { text = it }, singleLine = false, minLines = 3,
                placeholder = { Text(stringResource(R.string.paste_hint)) }, modifier = Modifier.fillMaxWidth(),
            )
        },
        confirmButton = { Button(onClick = { onSubmit(text) }, enabled = text.isNotBlank()) { Text(stringResource(R.string.continue_)) } },
        dismissButton = { TextButton(onClick = onDismiss) { Text(stringResource(R.string.cancel)) } },
    )
}

// ---- scanner -------------------------------------------------------------------------------

@Composable
private fun ScanScreen(onCode: (String) -> Unit, onError: () -> Unit) {
    val ctx = LocalContext.current
    var granted by remember { mutableStateOf(ContextCompat.checkSelfPermission(ctx, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) }
    val ask = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted = it }

    if (!granted) {
        Column(Modifier.fillMaxSize().padding(24.dp), verticalArrangement = Arrangement.Center, horizontalAlignment = Alignment.CenterHorizontally) {
            Text(stringResource(R.string.scan_permission), style = MaterialTheme.typography.bodyLarge)
            Spacer(Modifier.height(16.dp))
            Button(onClick = { ask.launch(Manifest.permission.CAMERA) }) { Text(stringResource(R.string.scan_grant)) }
        }
        return
    }
    val owner = LocalLifecycleOwner.current
    val executor = remember { Executors.newSingleThreadExecutor() }
    val provider = remember { arrayOfNulls<ProcessCameraProvider>(1) }
    // Stop the camera *before* the analysis thread goes away, or a late frame would be
    // posted to a dead executor.
    DisposableEffect(Unit) {
        onDispose {
            runCatching { provider[0]?.unbindAll() }
            executor.shutdown()
        }
    }

    Box(Modifier.fillMaxSize()) {
        AndroidView(
            modifier = Modifier.fillMaxSize(),
            factory = { c ->
                val view = PreviewView(c)
                val future = ProcessCameraProvider.getInstance(c)
                future.addListener({
                    try {
                        val p = future.get()
                        provider[0] = p
                        val preview = Preview.Builder().build().also { it.setSurfaceProvider(view.surfaceProvider) }
                        // A dense QR code needs more than the 640x480 default.
                        val resolution = ResolutionSelector.Builder()
                            .setResolutionStrategy(ResolutionStrategy(Size(1280, 720), ResolutionStrategy.FALLBACK_RULE_CLOSEST_HIGHER_THEN_LOWER))
                            .build()
                        val analysis = ImageAnalysis.Builder()
                            .setResolutionSelector(resolution)
                            .setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST)
                            .build()
                            .also { a -> a.setAnalyzer(executor, QrAnalyzer { text -> view.post { onCode(text) } }) }
                        p.unbindAll()
                        p.bindToLifecycle(owner, CameraSelector.DEFAULT_BACK_CAMERA, preview, analysis)
                    } catch (e: Exception) {
                        // No back camera, camera in use, policy-disabled...: say so, never crash.
                        onError()
                    }
                }, ContextCompat.getMainExecutor(c))
                view
            },
        )
        Text(
            stringResource(R.string.scan_hint),
            Modifier.align(Alignment.BottomCenter).padding(24.dp),
            style = MaterialTheme.typography.bodyLarge,
            color = androidx.compose.ui.graphics.Color.White,
        )
    }
}

// ---- invite a computer ---------------------------------------------------------------------

@Composable
private fun InviteScreen(ui: UiState, vm: MainViewModel) {
    val ctx = LocalContext.current
    // The QR code is a credential: keep it out of screenshots and the recents screen.
    DisposableEffect(Unit) {
        val w = ctx.findActivity()?.window
        w?.addFlags(WindowManager.LayoutParams.FLAG_SECURE)
        onDispose { w?.clearFlags(WindowManager.LayoutParams.FLAG_SECURE) }
    }
    val folders = ui.config?.folders.orEmpty()
    var chosen by remember { mutableStateOf(folders.firstOrNull()?.id) }

    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        Text(stringResource(R.string.invite_body), style = MaterialTheme.typography.bodyMedium)
        if (!ui.status.running) {
            Text(stringResource(R.string.invite_need_running), color = MaterialTheme.colorScheme.error)
        }
        if (folders.size > 1) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                folders.forEach { f ->
                    FilterChip(selected = chosen == f.id, onClick = { chosen = f.id }, label = { Text(f.title) })
                }
            }
        }
        Button(onClick = { chosen?.let(vm::generateInvite) }, enabled = ui.status.running && chosen != null) {
            Text(stringResource(R.string.invite_generate))
        }
        ui.invite?.let { inv ->
            val bmp = remember(inv.code) { qrBitmap(inv.code) }
            Image(bmp.asImageBitmap(), contentDescription = null, modifier = Modifier.size(300.dp))
            Text(stringResource(R.string.invite_expires, DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(inv.expiresAtMs))))
            // Not selectable: the system copy toolbar would not mark it sensitive.
            Text(inv.code, fontFamily = FontFamily.Monospace, fontSize = 11.sp)
            OutlinedButton(onClick = { copySensitive(ctx, inv.code) }) { Text(stringResource(R.string.copy)) }
        }
    }
}

/** Copies text flagged as sensitive so the clipboard preview hides it (Android 13+). */
private fun copySensitive(ctx: Context, text: String) {
    val clip = ClipData.newPlainText("invitation", text)
    if (Build.VERSION.SDK_INT >= 33) {
        clip.description.extras = PersistableBundle().apply { putBoolean(ClipDescription.EXTRA_IS_SENSITIVE, true) }
    }
    ctx.getSystemService(ClipboardManager::class.java).setPrimaryClip(clip)
}

private tailrec fun Context.findActivity(): ComponentActivity? = when (this) {
    is ComponentActivity -> this
    is ContextWrapper -> baseContext.findActivity()
    else -> null
}

// ---- settings ------------------------------------------------------------------------------

private val QUOTAS = listOf(128L, 256L, 512L, 1024L, 2048L, 4096L, 8192L)
private val RETENTIONS = listOf(6, 24, 72, 168, 720)

@Composable
private fun SettingsScreen(ui: UiState, vm: MainViewModel) {
    val ctx = LocalContext.current
    val cfg = ui.config
    var name by remember(cfg?.deviceName) { mutableStateOf(cfg?.deviceName.orEmpty()) }
    var removeDevice by remember { mutableStateOf<Pair<String, String>?>(null) }
    var removeFolder by remember { mutableStateOf<FolderCfg?>(null) }

    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        SwitchRow(stringResource(R.string.setting_wifi_only), stringResource(R.string.setting_wifi_only_desc), ui.wifiOnly, vm::setWifiOnly)
        SwitchRow(stringResource(R.string.setting_lan), null, cfg?.lanDiscovery ?: true, vm::setLan)

        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedTextField(
                value = name, onValueChange = { name = it.take(40) }, singleLine = true,
                label = { Text(stringResource(R.string.setting_name)) }, modifier = Modifier.weight(1f),
            )
            Button(onClick = { vm.setName(name) }, enabled = name.isNotBlank() && name != cfg?.deviceName) { Text("OK") }
        }

        cfg?.folders?.forEach { f ->
            Card(Modifier.fillMaxWidth()) {
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(f.title, style = MaterialTheme.typography.titleMedium)
                    val qi = QUOTAS.indexOfFirst { it >= f.quotaMb }.let { if (it < 0) QUOTAS.lastIndex else it }
                    var q by remember(f.quotaMb) { mutableStateOf(qi.toFloat()) }
                    Text(stringResource(R.string.setting_quota, QUOTAS[q.toInt()].toInt()))
                    Slider(
                        value = q, onValueChange = { q = it }, valueRange = 0f..(QUOTAS.size - 1).toFloat(), steps = QUOTAS.size - 2,
                        onValueChangeFinished = { vm.setQuota(f.id, QUOTAS[q.toInt()]) },
                    )
                    val ri = RETENTIONS.indexOfFirst { it >= f.retentionHours }.let { if (it < 0) RETENTIONS.lastIndex else it }
                    var r by remember(f.retentionHours) { mutableStateOf(ri.toFloat()) }
                    Text(stringResource(R.string.setting_retention, RETENTIONS[r.toInt()]))
                    Slider(
                        value = r, onValueChange = { r = it }, valueRange = 0f..(RETENTIONS.size - 1).toFloat(), steps = RETENTIONS.size - 2,
                        onValueChangeFinished = { vm.setRetention(f.id, RETENTIONS[r.toInt()]) },
                    )
                    Text(stringResource(R.string.devices_title), style = MaterialTheme.typography.titleSmall)
                    f.devices.forEach { d ->
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(d.name.ifBlank { d.id.take(12) + "…" }, Modifier.weight(1f))
                            TextButton(onClick = { removeDevice = d.id to d.name.ifBlank { d.id.take(12) } }) { Text(stringResource(R.string.remove)) }
                        }
                    }
                    TextButton(onClick = { removeFolder = f }) { Text(stringResource(R.string.remove_folder), color = MaterialTheme.colorScheme.error) }
                }
            }
        }

        Text(stringResource(R.string.device_id), style = MaterialTheme.typography.titleSmall)
        SelectionContainer { Text(ui.deviceId, fontFamily = FontFamily.Monospace, fontSize = 12.sp) }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedButton(onClick = { copySensitive(ctx, ui.deviceId) }) { Text(stringResource(R.string.copy)) }
            OutlinedButton(onClick = { vm.go(Screen.Logs) }) {
                Icon(Icons.Default.Info, null, Modifier.size(18.dp))
                Spacer(Modifier.size(6.dp))
                Text(stringResource(R.string.logs_title))
            }
        }
        Text("v" + mobile.Mobile.version(), style = MaterialTheme.typography.bodySmall)
    }

    removeDevice?.let { (id, label) ->
        AlertDialog(
            onDismissRequest = { removeDevice = null },
            title = { Text(stringResource(R.string.remove_device_q, label)) },
            text = { Text(stringResource(R.string.remove_device_body)) },
            confirmButton = { Button(onClick = { vm.removeDevice(id); removeDevice = null }) { Text(stringResource(R.string.remove)) } },
            dismissButton = { TextButton(onClick = { removeDevice = null }) { Text(stringResource(R.string.cancel)) } },
        )
    }
    removeFolder?.let { f ->
        AlertDialog(
            onDismissRequest = { removeFolder = null },
            title = { Text(stringResource(R.string.remove_folder_q, f.title)) },
            text = { Text(stringResource(R.string.remove_folder_body)) },
            confirmButton = { Button(onClick = { vm.removeFolder(f.id); removeFolder = null }) { Text(stringResource(R.string.remove)) } },
            dismissButton = { TextButton(onClick = { removeFolder = null }) { Text(stringResource(R.string.cancel)) } },
        )
    }
}

@Composable
private fun SwitchRow(title: String, subtitle: String?, checked: Boolean, onChange: (Boolean) -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            subtitle?.let { Text(it, style = MaterialTheme.typography.bodySmall) }
        }
        Switch(checked = checked, onCheckedChange = onChange)
    }
}

// ---- diagnostics ---------------------------------------------------------------------------

@Composable
private fun LogsScreen(ui: UiState, vm: MainViewModel) {
    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(stringResource(R.string.logs_note), style = MaterialTheme.typography.bodySmall)
        OutlinedButton(onClick = vm::loadLogs) { Text(stringResource(R.string.refresh)) }
        Column(Modifier.weight(1f).verticalScroll(rememberScrollState())) {
            SelectionContainer {
                Text(
                    ui.logs.ifBlank { stringResource(R.string.logs_empty) },
                    fontFamily = FontFamily.Monospace, fontSize = 10.sp,
                )
            }
        }
    }
}
