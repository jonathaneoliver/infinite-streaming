package com.infinitestream.player

import android.os.Bundle
import android.view.WindowManager
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.viewModels
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.ui.screen.HomeScreen
import com.infinitestream.player.ui.screen.PlaybackScreen
import com.infinitestream.player.ui.screen.ServerPickerScreen
import com.infinitestream.player.ui.screen.SettingsOverlay
import com.infinitestream.player.ui.theme.InfiniteStreamTheme
import com.infinitestream.player.ui.theme.Tokens

/**
 * Process-global launch configuration captured from the launch Intent before
 * the PlayerViewModel is constructed. #714 config-on-connect: the test harness
 * mints a player_id, pre-configures the proxy session for it via a bootstrap
 * curl, and passes it as an intent extra (`--es is.player_id <uuid>`) so the
 * app inherits that already-configured session instead of minting its own
 * per-launch id. Mirrors how iOS reads `-is.player_id` from NSArgumentDomain.
 */
object LaunchConfig {
    @Volatile
    var playerId: String? = null

    // #714 Approach B: raw proxy.* query fragment appended to the bootstrap
    // URL (e.g. "proxy.shape.rate_mbps=2.5"), captured from the launch intent.
    @Volatile
    var proxyQuery: String? = null
}

class MainActivity : ComponentActivity() {
    private val tStart = android.os.SystemClock.uptimeMillis()
    private fun tag(s: String) = android.util.Log.i(
        "InfiniteStream", "T+${android.os.SystemClock.uptimeMillis() - tStart}ms $s")

    private val vm: PlayerViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        tag("MainActivity onCreate begin")
        super.onCreate(savedInstanceState)
        // #714 config-on-connect: capture a harness-provided player_id from the
        // launch intent BEFORE the PlayerViewModel is created (via viewModels()
        // below), so the VM's playerId picks it up instead of minting its own.
        intent?.getStringExtra("is.player_id")?.let { raw ->
            if (runCatching { java.util.UUID.fromString(raw) }.isSuccess) {
                LaunchConfig.playerId = raw
                tag("launch-arg player_id=$raw")
            }
        }
        // #714 Approach B: capture a raw proxy.* query fragment to append to
        // the bootstrap URL (config-on-connect driven by the player).
        intent?.getStringExtra("is.proxy_query")?.let { raw ->
            if (raw.isNotEmpty()) {
                LaunchConfig.proxyQuery = raw
                tag("launch-arg proxy_query=$raw")
            }
        }
        // Keep the screen on while playback is active — release in onStop.
        window.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        tag("MainActivity onCreate setContent")
        setContent {
            androidx.compose.runtime.SideEffect { tag("first composition") }
            InfiniteStreamTheme {
                Box(modifier = Modifier.fillMaxSize().background(Tokens.bg)) {
                    AppRoot()
                }
            }
        }
        tag("MainActivity onCreate end")
    }

    override fun onStart() {
        super.onStart()
        vm.onActivityStarted()
    }

    override fun onStop() {
        super.onStop()
        // User pressed Home / switched apps. Fully release video-decoding
        // resources so we don't keep MediaCodec instances allocated in
        // the background while another app (e.g. YouTube) is in the
        // foreground. The VM stops + clearMediaItems on the main player
        // and broadcasts appStopped=true so every LivePreviewTile on
        // Home releases its decoder too. Re-prepares on onStart.
        //
        // We do NOT mark terminal here — onStop fires for a 1-second
        // app-switch as well as for a real session end, and the
        // false-positive rate of user_stopped/app_backgrounded rows
        // would be too high. onDestroy (real-quit) handles the
        // unambiguous case via vm.endSessionAsAppTerminated below.
        vm.onActivityStopped()
    }

    override fun onDestroy() {
        super.onDestroy()
        // #550 Phase 2 — best-effort terminal stamp on real activity
        // teardown. isFinishing rules out config-change destruction
        // (rotation, theme switch); only fires on the operator-driven
        // back-to-quit / OS-reaped finish. The metrics POST is
        // fire-and-forget; if the OS reaps us mid-flight the row
        // stays in_progress (treated as "user closed without notifying"
        // by downstream).
        if (isFinishing) {
            vm.endSessionAsAppTerminated()
        }
    }
}

private enum class Route { ServerPicker, Home, Playback }

@Composable
private fun AppRoot() {
    val vm: PlayerViewModel = viewModel()
    val state by vm.state.collectAsStateWithLifecycle()

    // Initial route policy:
    //   - No saved servers → ServerPicker (guided setup).
    //   - skipHomeOnLaunch ON + we have a lastPlayed → Playback, so the
    //     user is back inside their stream without waiting for Home's
    //     /api/content fetch. Home will mount only if Back is pressed
    //     from Playback, at which point its visuals initialize for the
    //     first time this session.
    //   - Otherwise → Home (the previous default).
    var route by remember {
        mutableStateOf(when {
            state.servers.isEmpty() -> Route.ServerPicker
            state.skipHomeOnLaunch && state.lastPlayed.isNotEmpty() -> Route.Playback
            else -> Route.Home
        })
    }
    // One-shot auto-resume when the cold-start route is Playback. Uses
    // setSelectedContent (not the deferred variant) since there are no
    // tile decoders to wait on — Home didn't mount.
    androidx.compose.runtime.LaunchedEffect(Unit) {
        if (route == Route.Playback
            && state.selectedContent.isEmpty()
            && state.lastPlayed.isNotEmpty()) {
            vm.setSelectedContent(state.lastPlayed)
        }
    }

    BackHandler(enabled = state.settingsOpen) {
        vm.setSettingsOpen(false)
    }
    BackHandler(enabled = !state.settingsOpen && state.hudVisible && route == Route.Playback) {
        vm.setHudVisible(false)
    }
    BackHandler(enabled = !state.settingsOpen && !state.hudVisible && route == Route.Playback) {
        // #550 Phase 2 — emit play_end with user_stopped (or
        // abandoned_start if EBVS conditions met) before navigating
        // away. Stamping happens here at the back-press because the
        // route flip + downstream LaunchedEffect tear down the
        // player + metrics; we'd lose the chance to mark terminal
        // after that.
        vm.endSessionForUserBack()
        route = Route.Home
    }
    BackHandler(enabled = !state.settingsOpen && route == Route.Home) {
        route = Route.ServerPicker
    }

    // The main vm.player is shared across Playback / Home and keeps its
    // own lifecycle. When leaving Playback we fully stop it: pause()
    // alone would leave the audio bleeding into Home AND keep the
    // hardware decoder allocated, eating one of the chip's four
    // H.264 slots that we need for tile previews. stop() drops the
    // codec; entering Playback again calls buildUrlAndLoad() which
    // re-prepares from scratch.
    androidx.compose.runtime.LaunchedEffect(route) {
        if (route != Route.Playback) {
            vm.player.stop()
            vm.player.clearMediaItems()
            // Also clear the URL state so applyContentFilter (triggered by
            // any later setProtocol / setCodec / re-fetch) treats us as
            // "not currently playing" and doesn't silently re-spin the
            // main player on Home.
            vm.clearCurrentUrl()
        }
    }

    when (route) {
        Route.ServerPicker -> ServerPickerScreen(
            state = state, vm = vm,
            onServerChosen = { route = Route.Home },
        )
        Route.Home -> HomeScreen(
            state = state, vm = vm,
            onPlay = { route = Route.Playback },
            onOpenServerPicker = { route = Route.ServerPicker },
            onOpenSettings = { vm.setSettingsOpen(true) },
        )
        Route.Playback -> PlaybackScreen(
            state = state, vm = vm,
            onOpenSettings = { vm.setSettingsOpen(true) },
        )
    }
    // Settings drawer renders above every route, so the nav-bar's
    // "Settings" item on Home opens it just like the gear in playback.
    SettingsOverlay(
        state = state, vm = vm,
        onDismiss = { vm.setSettingsOpen(false) },
        onOpenServerPicker = {
            vm.setSettingsOpen(false)
            route = Route.ServerPicker
        },
    )
}
