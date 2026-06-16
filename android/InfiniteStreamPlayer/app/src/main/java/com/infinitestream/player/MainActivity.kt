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

    // #266 / #793 live-offset lever: a test-provided override (seconds behind
    // live) captured from the launch intent so the harness can pin the offset
    // at startup without driving the Settings UI. Mirrors iOS reading
    // `-is.flag.live_offset_s` from NSArgumentDomain. null = no override.
    @Volatile
    var liveOffsetSeconds: Int? = null

    // #797 characterization launch levers, mirroring iOS `-is.segment` /
    // `-is.protocol` / `-is.flag.peak_bitrate_mbps`. Each captured from the
    // launch intent so the harness can drive the test matrix without touching
    // the Settings UI. null = no override; the VM's loadAdvancedFlags lets a
    // non-null value outrank the default/persisted value for this launch,
    // matching iOS NSArgumentDomain.
    @Volatile
    var segment: com.infinitestream.player.state.Segment? = null
    @Volatile
    var streamProtocol: com.infinitestream.player.state.Protocol? = null
    @Volatile
    var peakBitrateMbps: Int? = null

    // #797 priority-2 levers, same launch-override semantics as above. codec is
    // UI-only state (not persisted); 4k / go_live / play_id_rotation_s /
    // starts_first_variant override their persisted Advanced flags; lastPlayed
    // pins which clip the Continue-Watching hero / auto-resume targets (the
    // harness drives this via `is.lastPlayed`, with `is.content` as an alias).
    @Volatile
    var codec: com.infinitestream.player.state.Codec? = null
    @Volatile
    var allow4K: Boolean? = null
    @Volatile
    var goLive: Boolean? = null
    @Volatile
    var playIdRotationSeconds: Int? = null
    @Volatile
    var startsFirstVariant: Boolean? = null
    @Volatile
    var lastPlayed: String? = null
}

/** Parse a launch-arg boolean intent extra. The harness sends booleans as
 *  `true`/`false` strings (NSArgumentDomain on iOS); tolerate `1`/`0`/`yes`/`no`
 *  too. null = unparseable (leave the override unset). #797. */
private fun boolArg(raw: String): Boolean? = when (raw.trim().lowercase()) {
    "true", "1", "yes" -> true
    "false", "0", "no" -> false
    else -> null
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
        // #266 / #793 live-offset lever — `--es is.flag.live_offset_s 6`. Parsed
        // as a number (tolerating "6.0") and rounded to whole seconds; the VM's
        // loadAdvancedFlags lets this outrank the persisted value for this
        // launch. Mirrors iOS's `-is.flag.live_offset_s` launch arg.
        intent?.getStringExtra("is.flag.live_offset_s")?.toDoubleOrNull()?.let { raw ->
            LaunchConfig.liveOffsetSeconds = raw.toInt().coerceAtLeast(0)
            tag("launch-arg live_offset_s=${LaunchConfig.liveOffsetSeconds}")
        }
        // #797 segment lever — `--es is.segment s2` (iOS rawValues ll / s2 / s6).
        // Selects the master_2s / master_6s / LL ladder; unblocks the 2s/LL
        // matrix on Android. Unrecognised values are ignored (no override).
        intent?.getStringExtra("is.segment")?.let { raw ->
            com.infinitestream.player.state.Segment.fromArg(raw)?.let { seg ->
                LaunchConfig.segment = seg
                tag("launch-arg segment=${seg.label}")
            }
        }
        // #797 protocol lever — `--es is.protocol dash` (hls / dash). Drives
        // Android to DASH, which the Settings-only Protocol picker couldn't.
        intent?.getStringExtra("is.protocol")?.let { raw ->
            com.infinitestream.player.state.Protocol.fromArg(raw)?.let { proto ->
                LaunchConfig.streamProtocol = proto
                tag("launch-arg protocol=${proto.label}")
            }
        }
        // #797 ABR peak-bitrate cap — `--es is.flag.peak_bitrate_mbps 4` (Mbps;
        // 0 = no cap). Parsed as a number (tolerating "4.0"). Mirrors iOS's
        // `-is.flag.peak_bitrate_mbps` (AVPlayerItem.preferredPeakBitRate).
        intent?.getStringExtra("is.flag.peak_bitrate_mbps")?.toDoubleOrNull()?.let { raw ->
            LaunchConfig.peakBitrateMbps = raw.toInt().coerceAtLeast(0)
            tag("launch-arg peak_bitrate_mbps=${LaunchConfig.peakBitrateMbps}")
        }
        // #797 P2 codec lever — `--es is.codec hevc` (auto / h264 / hevc / av1).
        intent?.getStringExtra("is.codec")?.let { raw ->
            com.infinitestream.player.state.Codec.fromArg(raw)?.let { c ->
                LaunchConfig.codec = c
                tag("launch-arg codec=${c.label}")
            }
        }
        // #797 P2 4K-ladder lever — `--es is.flag.4k true`. A baseline flag the
        // harness sets on every launch, so honouring it stops a stale persisted
        // 4K toggle from leaking into a run (mirrors iOS NSArgumentDomain).
        intent?.getStringExtra("is.flag.4k")?.let { raw ->
            boolArg(raw)?.let { on ->
                LaunchConfig.allow4K = on
                tag("launch-arg 4k=$on")
            }
        }
        // #797 P2 Go-Live lever — `--es is.flag.go_live true` (snap to live edge).
        intent?.getStringExtra("is.flag.go_live")?.let { raw ->
            boolArg(raw)?.let { on ->
                LaunchConfig.goLive = on
                tag("launch-arg go_live=$on")
            }
        }
        // #797 P2 play_id rotation lever — `--es is.flag.play_id_rotation_s 0`
        // (seconds; 0 = one play_id per session). Soak-run knob.
        intent?.getStringExtra("is.flag.play_id_rotation_s")?.toDoubleOrNull()?.let { raw ->
            LaunchConfig.playIdRotationSeconds = raw.toInt().coerceAtLeast(0)
            tag("launch-arg play_id_rotation_s=${LaunchConfig.playIdRotationSeconds}")
        }
        // #797 P2 startup-rung lever — `--es is.flag.starts_first_variant true`
        // (pin the start to the lowest rung, then ABR adapts up at first frame).
        intent?.getStringExtra("is.flag.starts_first_variant")?.let { raw ->
            boolArg(raw)?.let { on ->
                LaunchConfig.startsFirstVariant = on
                tag("launch-arg starts_first_variant=$on")
            }
        }
        // #797 P2 content selection — the harness pins the played clip with
        // `--es is.lastPlayed <name>`; accept `is.content` as an alias for the
        // name the issue uses. Seeds lastPlayed so the hero / auto-resume target
        // that clip (iOS reads `is.lastPlayed` the same way).
        (intent?.getStringExtra("is.lastPlayed") ?: intent?.getStringExtra("is.content"))?.let { raw ->
            if (raw.isNotEmpty()) {
                LaunchConfig.lastPlayed = raw
                tag("launch-arg lastPlayed=$raw")
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
