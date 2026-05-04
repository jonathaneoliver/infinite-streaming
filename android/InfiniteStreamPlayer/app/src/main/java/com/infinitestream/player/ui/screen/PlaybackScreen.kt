@file:kotlin.OptIn(
    androidx.tv.material3.ExperimentalTvMaterial3Api::class,
    androidx.media3.common.util.UnstableApi::class,
)

package com.infinitestream.player.ui.screen

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInVertically
import androidx.compose.animation.slideOutVertically
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.focusGroup
import androidx.compose.foundation.focusable
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
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Forward10
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Replay10
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.SkipNext
import androidx.compose.material.icons.filled.SkipPrevious
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.PlayerView
import androidx.tv.material3.Icon
import androidx.tv.material3.Text
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.state.UiState
import com.infinitestream.player.ui.theme.AppType
import com.infinitestream.player.ui.theme.Radius
import com.infinitestream.player.ui.theme.Space
import com.infinitestream.player.ui.theme.Tokens
import com.infinitestream.player.ui.theme.tvFocus
import kotlinx.coroutines.delay

private const val HUD_AUTO_HIDE_MS = 3000L

/**
 * Fullscreen playback. Spec: zero persistent chrome. The HUD is a translucent
 * gradient bar at the bottom that appears on D-pad-Up/OK and auto-hides after
 * 3s of no input. The settings drawer is rendered by [SettingsOverlayHost]
 * one level up so it can layer over playback without re-mounting the player.
 */
@Composable
fun PlaybackScreen(
    state: UiState,
    vm: PlayerViewModel,
    onOpenSettings: () -> Unit,
) {
    // Auto-hide HUD after the spec'd timeout. The nonce bumps on every key
    // event while the HUD is visible (see onPreviewKeyEvent below) so users
    // navigating the transport bar don't get the HUD yanked out from under
    // them — only true 3 s of inactivity dismisses it.
    var hudActivityNonce by remember { mutableIntStateOf(0) }
    LaunchedEffect(state.hudVisible, hudActivityNonce) {
        if (state.hudVisible) {
            delay(HUD_AUTO_HIDE_MS)
            vm.setHudVisible(false)
        }
    }

    // Pull initial focus onto the root so D-pad / MENU actually reach the
    // Compose layer instead of getting absorbed by the embedded PlayerView.
    // When the HUD opens, focus jumps to the centre play/pause so the user
    // can move L/R through the transport with the D-pad.
    val rootFocus = remember { FocusRequester() }
    val transportFocus = remember { FocusRequester() }
    LaunchedEffect(state.hudVisible, state.settingsOpen) {
        if (state.settingsOpen) return@LaunchedEffect
        if (state.hudVisible) {
            try { transportFocus.requestFocus() } catch (_: Throwable) {}
        } else {
            try { rootFocus.requestFocus() } catch (_: Throwable) {}
        }
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Color.Black)
            .focusRequester(rootFocus)
            .focusable()
            // Bump the activity nonce on every key event while the HUD is
            // visible so the auto-hide timer resets — the user navigating
            // through transport buttons should keep it open.
            .onPreviewKeyEvent { ev ->
                if (state.hudVisible && ev.type == KeyEventType.KeyDown) {
                    hudActivityNonce++
                }
                false
            }
            .onKeyEvent { ev ->
                if (ev.type != KeyEventType.KeyDown) return@onKeyEvent false
                // Don't swallow D-pad while the settings drawer is open —
                // keys must route into the drawer's own focusable rows.
                if (state.settingsOpen) return@onKeyEvent false
                when (ev.key) {
                    // HUD lives at the bottom of the screen, so D-pad-Down
                    // is the spatially intuitive reveal — reaching toward
                    // where the chrome will appear.
                    Key.DirectionDown, Key.Menu -> { vm.setHudVisible(true); true }
                    Key.DirectionCenter, Key.Enter -> {
                        vm.setHudVisible(true)
                        if (state.hudVisible) togglePlayPause(vm.player)
                        true
                    }
                    Key.MediaPlayPause, Key.MediaPlay, Key.MediaPause -> {
                        togglePlayPause(vm.player); true
                    }
                    else -> false
                }
            },
    ) {
        // The video itself — wrap PlayerView so existing PlaybackMetrics keeps
        // working. We disable Media3's built-in controller because the HUD
        // below is the spec'd surface.
        //
        // Keyed on state.playerEpoch so a Reload (which releases and
        // rebuilds the underlying ExoPlayer) remounts this PlayerView
        // and binds to the new player — otherwise the view would keep
        // a reference to the released instance.
        androidx.compose.runtime.key(state.playerEpoch) {
            AndroidView(
                modifier = Modifier.fillMaxSize(),
                factory = { ctx ->
                    PlayerView(ctx).apply {
                        player = vm.player
                        useController = false
                        setBackgroundColor(android.graphics.Color.BLACK)
                        // Don't let the PlayerView steal D-pad focus from the
                        // Compose root — without this, MENU/UP get absorbed
                        // before our onKeyEvent runs.
                        isFocusable = false
                        isFocusableInTouchMode = false
                        descendantFocusability = android.view.ViewGroup.FOCUS_BLOCK_DESCENDANTS
                    }
                },
                update = { view ->
                    vm.bindMetrics(view)
                },
            )
        }

        // Top-right shortcut hint — the only signpost telling the user
        // how to reveal the HUD. (Top-left is reserved for source-video
        // burnt-in overlays we don't control.)
        AnimatedVisibility(
            visible = !state.hudVisible && !state.settingsOpen,
            enter = fadeIn(), exit = fadeOut(),
            modifier = Modifier.align(Alignment.TopEnd).padding(Space.s5),
        ) {
            Text(
                "▼ HUD",
                style = AppType.monoSm.copy(color = Tokens.fg.copy(alpha = 0.55f)),
            )
        }

        // Mid-right diagnostic readout — gated on Developer mode. Same
        // field list and units as the Apple DiagnosticHUD so an operator
        // can read either readout during a cross-platform soak run.
        AnimatedVisibility(
            visible = state.developerMode && !state.settingsOpen,
            enter = fadeIn(), exit = fadeOut(),
            modifier = Modifier.align(Alignment.CenterEnd).padding(end = Space.s4),
        ) {
            DiagnosticHud(vm)
        }

        // Bottom HUD overlay — gradient + transport.
        AnimatedVisibility(
            visible = state.hudVisible,
            enter = fadeIn() + slideInVertically(initialOffsetY = { it / 4 }),
            exit = fadeOut() + slideOutVertically(targetOffsetY = { it / 4 }),
            modifier = Modifier.align(Alignment.BottomCenter),
        ) {
            HudBar(state, vm,
                transportFocus = transportFocus,
                onActivity = { hudActivityNonce++ },
                onOpenSettings = {
                    vm.setHudVisible(false)
                    onOpenSettings()
                })
        }
    }
}

@Composable
private fun HudBar(
    state: UiState,
    vm: PlayerViewModel,
    transportFocus: FocusRequester,
    onActivity: () -> Unit,
    onOpenSettings: () -> Unit,
) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .background(
                Brush.verticalGradient(
                    listOf(Color.Transparent, Color.Black.copy(alpha = 0.85f))
                )
            )
            .padding(horizontal = Space.s8, vertical = Space.s7),
    ) {
        Column(modifier = Modifier.fillMaxWidth()) {
            Row(verticalAlignment = Alignment.Bottom, modifier = Modifier.fillMaxWidth()) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        state.selectedContent.ifEmpty { "—" },
                        style = AppType.title.copy(color = Tokens.fg),
                    )
                    Spacer(Modifier.height(Space.s1))
                    Text(
                        state.activeServer?.let { "${it.host}:${it.port}" } ?: "no server",
                        style = AppType.bodySm.copy(color = Tokens.fgDim),
                    )
                }
                MetadataPills(state, vm)
            }
            Spacer(Modifier.height(Space.s4))
            Scrubber(vm.player)
            Spacer(Modifier.height(Space.s4))
            // Single row: transport (-10 / play / +10 / settings) +
            // recovery (Retry / Restart / Reload). focusGroup() keeps
            // D-pad-Right/Left contained so the search can't bail out
            // into the embedded PlayerView before reaching the last
            // pill. Recovery pills sit to the right of the gear since
            // they're escalating-severity "fix it" controls, secondary
            // to the play/scrub primary group on the left.
            Row(
                modifier = Modifier.fillMaxWidth().focusGroup(),
                horizontalArrangement = Arrangement.Center,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                TransportButton(Icons.Default.Replay10, onActivity) {
                    vm.player.seekBack()
                }
                Spacer(Modifier.width(Space.s3))
                BigPlayPause(vm, transportFocus, onActivity)
                Spacer(Modifier.width(Space.s3))
                TransportButton(Icons.Default.Forward10, onActivity) {
                    vm.player.seekForward()
                }
                Spacer(Modifier.width(Space.s5))
                TransportButton(Icons.Default.Settings, onActivity, onClick = onOpenSettings)
                Spacer(Modifier.width(Space.s5))
                RecoveryButton("Retry", onActivity) { vm.retry() }
                Spacer(Modifier.width(Space.s2))
                RecoveryButton("Reload", onActivity) { vm.reload() }
                Spacer(Modifier.width(Space.s2))
                RecoveryButton("911", onActivity) { vm.mark911() }
            }
        }
    }
}

@Composable
private fun MetadataPills(state: UiState, vm: PlayerViewModel) {
    val player = vm.player
    val videoFormat = remember { mutableStateOf(player.videoFormat) }
    DisposableEffect(player) {
        val listener = object : Player.Listener {
            override fun onTracksChanged(tracks: androidx.media3.common.Tracks) {
                videoFormat.value = player.videoFormat
            }
        }
        player.addListener(listener)
        onDispose { player.removeListener(listener) }
    }

    val resolution = videoFormat.value?.let { f -> if (f.width > 0) "${f.width}x${f.height}" else null }
    val fps = videoFormat.value?.frameRate?.takeIf { it > 0f }?.let { String.format("%.0ffps", it) }
    val codec = videoFormat.value?.codecs?.takeIf { it.isNotEmpty() }
        ?: state.codec.label

    Row(verticalAlignment = Alignment.CenterVertically) {
        MetaPill(state.protocol.label)
        if (resolution != null) { Spacer(Modifier.width(Space.s1)); MetaPill(resolution) }
        Spacer(Modifier.width(Space.s1)); MetaPill(state.segment.label)
        Spacer(Modifier.width(Space.s1)); MetaPill(codec)
        if (fps != null) { Spacer(Modifier.width(Space.s1)); MetaPill(fps) }
    }
}

@Composable
private fun MetaPill(text: String) {
    Box(
        modifier = Modifier
            .clip(RoundedCornerShape(4.dp))
            .border(1.dp, Tokens.line, RoundedCornerShape(4.dp))
            .padding(horizontal = Space.s2, vertical = 4.dp)
    ) {
        Text(text.uppercase(), style = AppType.monoSm.copy(color = Tokens.fg))
    }
}

@Composable
private fun BigPlayPause(vm: PlayerViewModel, focus: FocusRequester, onActivity: () -> Unit) {
    val isPlaying = remember { mutableStateOf(vm.player.isPlaying) }
    DisposableEffect(vm.player) {
        val l = object : Player.Listener {
            override fun onIsPlayingChanged(playing: Boolean) { isPlaying.value = playing }
        }
        vm.player.addListener(l)
        onDispose { vm.player.removeListener(l) }
    }
    Box(
        modifier = Modifier
            .size(56.dp)
            .focusRequester(focus)
            .onFocusChanged { if (it.isFocused) onActivity() }
            .tvFocus(cornerRadius = Radius.pill)
            .clip(RoundedCornerShape(Radius.pill))
            .background(Tokens.fg)
            .clickable { togglePlayPause(vm.player) },
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            imageVector = if (isPlaying.value) Icons.Default.Pause else Icons.Default.PlayArrow,
            contentDescription = null,
            tint = Tokens.bg,
        )
    }
}

@Composable
private fun TransportButton(icon: ImageVector, onActivity: () -> Unit, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .size(44.dp)
            .onFocusChanged { if (it.isFocused) onActivity() }
            .tvFocus(cornerRadius = Radius.pill)
            .clip(RoundedCornerShape(Radius.pill))
            .background(Tokens.bgCard.copy(alpha = 0.6f))
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Icon(icon, contentDescription = null, tint = Tokens.fg)
    }
}

/** Text pill for the recovery row — Retry / Restart / Reload. Visually
 *  lighter than the transport buttons so they read as a secondary control
 *  group, but still focusable + clickable with the standard tvFocus ring. */
@Composable
private fun RecoveryButton(label: String, onActivity: () -> Unit, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .height(36.dp)
            .onFocusChanged { if (it.isFocused) onActivity() }
            .tvFocus(cornerRadius = Radius.pill)
            .clip(RoundedCornerShape(Radius.pill))
            .background(Tokens.bgCard.copy(alpha = 0.6f))
            .clickable(onClick = onClick)
            .padding(horizontal = Space.s4),
        contentAlignment = Alignment.Center,
    ) {
        Text(label, style = AppType.label.copy(color = Tokens.fg))
    }
}

@Composable
private fun Scrubber(player: ExoPlayer) {
    // Live scrubber driven off ExoPlayer.currentPosition / duration. The
    // playback engine already keeps these up to date — we just sample at 4Hz
    // while the HUD is visible, which matches Apple TV's transport bar feel.
    val durationMs = remember { mutableStateOf(0L) }
    val positionMs = remember { mutableStateOf(0L) }
    LaunchedEffect(player) {
        while (true) {
            durationMs.value = player.duration.coerceAtLeast(0L)
            positionMs.value = player.currentPosition.coerceAtLeast(0L)
            delay(250)
        }
    }
    val pct = if (durationMs.value > 0)
        positionMs.value.toFloat() / durationMs.value.toFloat()
    else 1f // live edge

    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
        Text(formatTime(positionMs.value), style = AppType.mono.copy(color = Tokens.fgDim))
        Spacer(Modifier.width(Space.s2))
        Box(
            modifier = Modifier
                .weight(1f)
                .height(3.dp)
                .clip(RoundedCornerShape(2.dp))
                .background(Tokens.line),
        ) {
            Box(
                modifier = Modifier
                    .fillMaxWidth(pct.coerceIn(0f, 1f))
                    .height(3.dp)
                    .background(Tokens.accent)
            )
        }
        Spacer(Modifier.width(Space.s2))
        Text(
            if (durationMs.value > 0) formatTime(durationMs.value) else "LIVE",
            style = AppType.mono.copy(
                color = if (durationMs.value > 0) Tokens.fgDim else Tokens.live
            ),
        )
    }
}

/**
 * Mid-right semi-transparent diagnostic readout. Gated on Developer
 * mode in [PlaybackScreen]. Pure formatting over `vm.player`,
 * `vm.bandwidthMeter`, and the read-only accessors on [PlaybackMetrics].
 *
 * Companion to the Apple `DiagnosticHUD` SwiftUI view; field list,
 * labels, and units must stay in lockstep so an operator can read
 * either readout during a cross-platform soak run.
 */
@Composable
private fun DiagnosticHud(vm: PlayerViewModel) {
    var stateText by remember { mutableStateOf("—") }
    // NET is the iOS-only LocalHTTPProxy per-chunk wire rate; we PATCH
    // null for it on Android, so the row stays at "—".
    val netText = "—"
    var avgNetText by remember { mutableStateOf("—") }
    var videoText by remember { mutableStateOf("—") }
    var resText by remember { mutableStateOf("—") }
    var bufferText by remember { mutableStateOf("—") }
    var offsetText by remember { mutableStateOf("—") }
    var shiftsText by remember { mutableStateOf("0") }
    var stallsText by remember { mutableStateOf("0") }
    var droppedText by remember { mutableStateOf("—") }

    LaunchedEffect(vm.player) {
        while (true) {
            val player = vm.player
            val metrics = vm.metricsRef

            // STATE (+ waiting reason in parens when non-empty).
            val s = (metrics?.currentMappedState() ?: "idle").uppercase()
            val reason = metrics?.currentMappedWaitingReason().orEmpty()
            stateText = if (reason.isEmpty()) s else "$s ($reason)"

            // AVG NET = ExoPlayer's session-wide bandwidth estimate, the
            // analogue of AVPlayer's observedBitrate that the iOS HUD
            // reads. This is the same value we PATCH as
            // `player_metrics_avg_network_bitrate_mbps`.
            val avg = vm.bandwidthMeter.bitrateEstimate.takeIf { it > 0 }
            avgNetText = avg?.let { String.format("%.2f Mbps", it / 1_000_000.0) } ?: "—"
            val format = player.videoFormat
            val videoBps = format?.bitrate?.takeIf { it > 0 }
            videoText = videoBps?.let { String.format("%.2f Mbps", it / 1_000_000.0) } ?: "—"

            // RES: video stream native resolution.
            val vs = player.videoSize
            resText = if (vs.width > 0 && vs.height > 0) "${vs.width}×${vs.height}" else "—"

            // BUFFER: seconds of video ahead of playhead.
            val bufMs = (player.bufferedPosition - player.currentPosition).coerceAtLeast(0)
            bufferText = String.format("%.1fs", bufMs / 1000.0)

            // OFFSET: ground-truth seconds behind live edge, derived from
            // PROGRAM-DATE-TIME at the playhead. Falls back to ExoPlayer's
            // getCurrentLiveOffset when the timeline doesn't carry a wall-
            // clock origin.
            offsetText = run {
                val tl = player.currentTimeline
                if (!tl.isEmpty) {
                    val idx = player.currentMediaItemIndex
                    if (idx in 0 until tl.windowCount) {
                        val w = androidx.media3.common.Timeline.Window()
                        tl.getWindow(idx, w)
                        if (w.windowStartTimeMs != androidx.media3.common.C.TIME_UNSET) {
                            val pdtMs = w.windowStartTimeMs + player.currentPosition
                            return@run String.format("%.1fs", (System.currentTimeMillis() - pdtMs) / 1000.0)
                        }
                    }
                }
                val live = player.currentLiveOffset
                if (live != androidx.media3.common.C.TIME_UNSET) String.format("%.1fs", live / 1000.0) else "—"
            }

            // SHIFTS / STALLS / DROPPED — pulled from PlaybackMetrics counters.
            shiftsText = (metrics?.profileShiftCount ?: 0).toString()
            val stalls = metrics?.stallCount ?: 0
            stallsText = if (stalls == 0) "0"
                else String.format("%d (last %.1fs)", stalls, metrics?.lastStallSeconds ?: 0.0)
            droppedText = metrics?.droppedFrames?.toString() ?: "—"

            delay(1000)
        }
    }

    Column(
        modifier = Modifier
            .width(240.dp)
            .clip(RoundedCornerShape(Radius.row))
            .background(Color.Black.copy(alpha = 0.45f))
            .padding(horizontal = Space.s3, vertical = Space.s2),
    ) {
        DiagnosticRow("STATE", stateText)
        DiagnosticRow("NET", netText)
        DiagnosticRow("AVG NET", avgNetText)
        DiagnosticRow("VIDEO", videoText)
        DiagnosticRow("RES", resText)
        DiagnosticRow("BUFFER", bufferText)
        DiagnosticRow("OFFSET", offsetText)
        DiagnosticRow("SHIFTS", shiftsText)
        DiagnosticRow("STALLS", stallsText)
        DiagnosticRow("DROPPED", droppedText)
    }
}

@Composable
private fun DiagnosticRow(label: String, value: String) {
    Row(
        modifier = Modifier.fillMaxWidth().padding(vertical = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Text(
            label,
            style = AppType.monoSm.copy(color = Tokens.diag),
            modifier = Modifier.width(72.dp),
        )
        Spacer(Modifier.width(Space.s1))
        Text(
            value,
            style = AppType.monoSm.copy(color = Tokens.fg),
            maxLines = 1,
        )
    }
}

private fun togglePlayPause(player: ExoPlayer) {
    if (player.isPlaying) player.pause() else player.play()
}

private fun formatTime(ms: Long): String {
    if (ms <= 0) return "0:00"
    val totalSec = ms / 1000
    val s = totalSec % 60
    val m = (totalSec / 60) % 60
    val h = totalSec / 3600
    return if (h > 0) String.format("%d:%02d:%02d", h, m, s)
    else String.format("%d:%02d", m, s)
}
