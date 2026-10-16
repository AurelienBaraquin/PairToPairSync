package io.p2psync.android

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.viewModels
import androidx.core.content.ContextCompat
import io.p2psync.android.ui.AppRoot
import io.p2psync.android.ui.P2PTheme

class MainActivity : ComponentActivity() {
    private val vm: MainViewModel by viewModels()

    private val notificationPermission =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { _ ->
            // The relay works either way; without the permission only its notification is hidden.
            vm.setRelay(true)
        }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        handleIntent(intent)
        setContent {
            P2PTheme {
                AppRoot(vm = vm, onToggleRelay = ::toggleRelay)
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        handleIntent(intent)
    }

    /** p2psync://pair/... from a QR reader or a link: ask the user, never pair silently. */
    private fun handleIntent(intent: Intent?) {
        if (intent?.action == Intent.ACTION_VIEW) {
            intent.dataString?.let { vm.submitCode(it) }
            intent.data = null // do not re-process after a rotation
        }
    }

    private fun toggleRelay(on: Boolean) {
        if (!on) {
            vm.setRelay(false)
            return
        }
        val needs = Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        if (needs) notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS) else vm.setRelay(true)
    }
}
