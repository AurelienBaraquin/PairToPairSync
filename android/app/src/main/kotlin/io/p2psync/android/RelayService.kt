package io.p2psync.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.wifi.WifiManager
import android.os.Build
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat
import mobile.Mobile
import java.util.concurrent.Executors

/**
 * Keeps the Go engine running as a foreground service, so that the user's
 * computers can reach this phone at any time. It also:
 *  - pauses the engine on mobile data when "Wi‑Fi only" is on, and resumes it on Wi‑Fi;
 *  - tells the engine when the network changed so it re-binds and re-dials;
 *  - holds the Wi‑Fi multicast lock, which local-network discovery (mDNS) needs.
 */
class RelayService : Service() {
    private val worker = Executors.newSingleThreadExecutor() // serialises engine start/stop
    private val main = Handler(Looper.getMainLooper())
    private lateinit var settings: Settings
    private lateinit var connectivity: ConnectivityManager
    private var multicastLock: WifiManager.MulticastLock? = null
    private var callback: ConnectivityManager.NetworkCallback? = null
    private var lastNetworkKey: Pair<Network?, Boolean>? = null
    private var begun = false
    @Volatile private var stopping = false // a stop is in progress; cleared by any new start

    // The engine's status call can wait for its start-up lock: never ask for it on
    // the main thread. The worker thread builds the notification, the main thread
    // only schedules the next refresh.
    private val ticker = object : Runnable {
        override fun run() {
            refreshNotification()
            main.postDelayed(this, 10_000)
        }
    }

    private fun refreshNotification() {
        runCatching { worker.execute { notify(buildNotification()) } }
    }

    override fun onCreate() {
        super.onCreate()
        settings = Settings(this)
        connectivity = getSystemService(ConnectivityManager::class.java)
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel(CHANNEL, getString(R.string.notif_channel), NotificationManager.IMPORTANCE_LOW)
                .apply { setShowBadge(false) }
        )
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            settings.relayEnabled = false
            shutDown()
            return START_NOT_STICKY
        }
        // The system restarts a killed sticky service with a null intent: honour a Stop the
        // user pressed in the meantime instead of silently turning the relay back on.
        if (intent == null && !settings.relayEnabled) {
            stopSelf()
            return START_NOT_STICKY
        }
        stopping = false // a start during a stop wins; the pending stop then does nothing
        if (!goForeground()) return START_NOT_STICKY
        settings.relayEnabled = true
        if (!begun) {
            begun = true
            RelayState.set(Phase.Starting)
            acquireMulticast()
            watchNetwork()
            main.postDelayed(ticker, 10_000)
        }
        worker.execute { reconcile(networkChanged = false) }
        return START_STICKY // restarted by the system if it is killed
    }

    private fun goForeground(): Boolean = try {
        val n = buildNotification(quick = true) // must be shown within seconds: no engine calls
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, n)
        }
        true
    } catch (e: Exception) {
        // e.g. started from the background where Android forbids it: do not crash.
        RelayState.set(Phase.Error(e.message ?: "cannot run in the foreground"))
        stopSelf()
        false
    }

    /** Brings the engine in line with what is wanted and what the network allows. */
    private fun reconcile(networkChanged: Boolean) {
        if (stopping) return
        val allowed = !settings.wifiOnly || onUnmeteredNetwork()
        try {
            if (allowed) {
                if (!Mobile.isRunning()) {
                    RelayState.set(Phase.Starting)
                    Engine.ensureInitialised(this)
                    Mobile.start(Engine.home(this))
                } else if (networkChanged) {
                    Mobile.restart()
                }
                RelayState.set(Phase.Running)
                main.removeCallbacks(retry)
            } else {
                Mobile.stop() // unconditional: also ends an engine that is retrying a failed restart
                RelayState.set(Phase.PausedNoWifi)
                main.removeCallbacks(retry)
            }
        } catch (e: Exception) {
            RelayState.set(Phase.Error(e.message ?: e.javaClass.simpleName))
            main.removeCallbacks(retry)
            main.postDelayed(retry, 30_000) // do not wait for the next network change
        }
        notify(buildNotification()) // already on the worker thread
    }

    private val retry = Runnable {
        if (!stopping) runCatching { worker.execute { reconcile(networkChanged = false) } }
    }

    private fun onUnmeteredNetwork(): Boolean {
        val caps = connectivity.getNetworkCapabilities(connectivity.activeNetwork) ?: return false
        return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED)
    }

    private fun watchNetwork() {
        lastNetworkKey = null
        val cb = object : ConnectivityManager.NetworkCallback() {
            private val debounce = Runnable { runCatching { worker.execute { reconcile(networkChanged = true) } } }

            /** Acts only when the network, or whether it is metered, really changed. */
            private fun evaluate(network: Network?) {
                val metered = network?.let {
                    connectivity.getNetworkCapabilities(it)?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_METERED) != true
                } ?: false
                val key = network to metered
                val first = lastNetworkKey == null
                if (key == lastNetworkKey) return
                lastNetworkKey = key
                // The first event only describes the network the engine was just started on.
                if (first) return
                // Networks flap while switching; act once things settle.
                main.removeCallbacks(debounce)
                main.postDelayed(debounce, 1500)
            }

            override fun onAvailable(network: Network) = evaluate(network)
            override fun onLost(network: Network) = evaluate(null)
            // Wi‑Fi <-> mobile data can flip the "metered" flag without a new Network.
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) = evaluate(network)
        }
        callback = cb
        connectivity.registerDefaultNetworkCallback(cb)
    }

    private fun acquireMulticast() {
        val wifi = applicationContext.getSystemService(WifiManager::class.java)
        multicastLock = wifi?.createMulticastLock("p2psync")?.apply {
            setReferenceCounted(false)
            runCatching { acquire() }
        }
    }

    private fun shutDown() {
        stopping = true
        begun = false
        main.removeCallbacksAndMessages(null)
        callback?.let { runCatching { connectivity.unregisterNetworkCallback(it) } }
        callback = null
        runCatching { multicastLock?.release() }
        RelayState.set(Phase.Stopped)
        worker.execute {
            runCatching { Mobile.stop() }
            main.post {
                // If the user switched the relay back on while it was stopping, keep running.
                if (stopping) {
                    stopForeground(STOP_FOREGROUND_REMOVE)
                    stopSelf()
                }
            }
        }
    }

    override fun onDestroy() {
        main.removeCallbacksAndMessages(null)
        callback?.let { runCatching { connectivity.unregisterNetworkCallback(it) } }
        runCatching { multicastLock?.release() }
        worker.execute { runCatching { Mobile.stop() } }
        worker.shutdown()
        if (RelayState.phase.value !is Phase.Error) RelayState.set(Phase.Stopped)
        super.onDestroy()
    }

    // ---- notification --------------------------------------------------------------------

    private fun notify(n: Notification) {
        runCatching { getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, n) }
    }

    private fun buildNotification(quick: Boolean = false): Notification {
        val phase = RelayState.phase.value
        val paused = phase == Phase.PausedNoWifi
        val st = if (paused || quick || phase is Phase.Error) EngineStatus(false) else Engine.status()
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val stop = PendingIntent.getService(
            this, 1, Intent(this, RelayService::class.java).setAction(ACTION_STOP),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        return NotificationCompat.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(getString(if (paused) R.string.notif_paused else R.string.notif_running))
            .setContentText(
                when {
                    phase is Phase.Error -> getString(R.string.relay_error, phase.message)
                    paused -> getString(R.string.relay_paused)
                    quick -> getString(R.string.relay_starting)
                    else -> getString(R.string.notif_text, st.pendingTotal, st.peers.size)
                }
            )
            .setContentIntent(open)
            .addAction(0, getString(R.string.notif_stop), stop)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setVisibility(NotificationCompat.VISIBILITY_SECRET) // file counts are private
            .build()
    }

    companion object {
        private const val CHANNEL = "relay"
        private const val NOTIFICATION_ID = 1
        const val ACTION_STOP = "io.p2psync.android.STOP"

        fun start(context: Context) {
            ContextCompat.startForegroundService(context, Intent(context, RelayService::class.java))
        }

        fun stop(context: Context) {
            context.startService(Intent(context, RelayService::class.java).setAction(ACTION_STOP))
        }
    }
}
