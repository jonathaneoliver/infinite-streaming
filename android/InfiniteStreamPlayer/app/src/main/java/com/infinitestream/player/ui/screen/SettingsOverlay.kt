@file:kotlin.OptIn(androidx.tv.material3.ExperimentalTvMaterial3Api::class)

package com.infinitestream.player.ui.screen

import androidx.activity.compose.BackHandler
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.ChevronRight
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay
import androidx.tv.material3.Icon
import androidx.tv.material3.Text
import com.infinitestream.player.state.Codec
import com.infinitestream.player.state.DecodeBudget
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.state.Protocol
import com.infinitestream.player.state.Segment
import com.infinitestream.player.state.UiState
import com.infinitestream.player.ui.theme.AppType
import com.infinitestream.player.ui.theme.Motion
import com.infinitestream.player.ui.theme.Radius
import com.infinitestream.player.ui.theme.Space
import com.infinitestream.player.ui.theme.Tokens
import com.infinitestream.player.ui.theme.tvFocus

/**
 * Slide-from-right settings drawer. Spec: 46% width on TV, slides in over
 * playback (240ms), backdrop gradient fade, list rows with label-left /
 * value-right / chevron, single-column pickers pushed in over the list.
 *
 * Replaces every pill-button group from the old UI.
 */
@Composable
fun SettingsOverlay(
    state: UiState,
    vm: PlayerViewModel,
    onDismiss: () -> Unit,
    onOpenServerPicker: () -> Unit,
) {
    AnimatedVisibility(
        visible = state.settingsOpen,
        enter = fadeIn(animationSpec = tween(Motion.drawerMs)),
        exit = fadeOut(animationSpec = tween(Motion.drawerMs)),
    ) {
        // Backdrop — gradient fade from the right side, dims playback.
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(
                    Brush.horizontalGradient(
                        listOf(Color.Transparent, Color.Black.copy(alpha = 0.55f))
                    )
                )
                .clickable(onClick = onDismiss)
                .onKeyEvent { ev ->
                    if (ev.type == KeyEventType.KeyDown && ev.key == Key.Back) {
                        onDismiss(); true
                    } else false
                },
        )
    }

    AnimatedVisibility(
        visible = state.settingsOpen,
        enter = slideInHorizontally(
            initialOffsetX = { it },
            animationSpec = tween(Motion.drawerMs),
        ) + fadeIn(animationSpec = tween(Motion.drawerMs)),
        exit = slideOutHorizontally(
            targetOffsetX = { it },
            animationSpec = tween(Motion.drawerMs),
        ) + fadeOut(animationSpec = tween(Motion.drawerMs)),
    ) {
        SettingsPanel(
            state = state, vm = vm,
            onDismiss = onDismiss,
            onOpenServerPicker = onOpenServerPicker,
        )
    }
}

@Composable
private fun SettingsPanel(
    state: UiState,
    vm: PlayerViewModel,
    onDismiss: () -> Unit,
    onOpenServerPicker: () -> Unit,
) {
    // Picker stack — null = main list, non-null = single-column picker pushed
    // in over the list (per spec, "not a popover, same width").
    var picker by remember { mutableStateOf<PickerKind?>(null) }
    // Sticky memory of the most-recently-opened picker. When we pop back to
    // the main list we re-focus the row that pushed it, instead of jumping
    // to "Server" every time — Apple TV / Android Settings both behave this
    // way and otherwise navigation feels lossy.
    var lastPicker by remember { mutableStateOf<PickerKind?>(null) }

    // Per-row FocusRequesters in the main list, plus one shared by every
    // picker's first item. The LaunchedEffect below picks which to focus
    // based on whether we're entering the drawer fresh, pushing a picker,
    // or popping back from one.
    val serverFocus = remember { FocusRequester() }
    val streamFocus = remember { FocusRequester() }
    val protocolFocus = remember { FocusRequester() }
    val segmentFocus = remember { FocusRequester() }
    val codecFocus = remember { FocusRequester() }
    val advancedFocus = remember { FocusRequester() }
    val pickerFirstFocus = remember { FocusRequester() }

    // Back inside a picker pops back to the main list. Sits inside the
    // MainActivity drawer-close BackHandler so this one consumes Back
    // first whenever a picker is open; the outer handler closes the
    // drawer when we're already on the main list.
    BackHandler(enabled = state.settingsOpen && picker != null) { picker = null }

    LaunchedEffect(picker, state.settingsOpen) {
        if (!state.settingsOpen) return@LaunchedEffect
        // Drawer slides in over 240ms — wait for the row to be laid out
        // before requesting focus, otherwise the FocusRequester throws.
        delay(280)
        val target = when {
            picker != null -> pickerFirstFocus
            lastPicker == PickerKind.Stream -> streamFocus
            lastPicker == PickerKind.Protocol -> protocolFocus
            lastPicker == PickerKind.SegmentLength -> segmentFocus
            lastPicker == PickerKind.Codec -> codecFocus
            lastPicker == PickerKind.Advanced -> advancedFocus
            else -> serverFocus
        }
        try { target.requestFocus() } catch (_: Throwable) {}
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .padding(start = 0.dp), // overlay anchored to right edge
    ) {
        Column(
            modifier = Modifier
                .align(Alignment.CenterEnd)
                .fillMaxWidth(0.46f)
                .fillMaxHeight()
                .background(Tokens.bg)
                .border(width = 1.dp, color = Tokens.line, shape = RoundedCornerShape(0.dp))
                .padding(horizontal = Space.s7, vertical = Space.s7),
        ) {
            Text("NOW PLAYING", style = AppType.label.copy(color = Tokens.fgDim))
            Spacer(Modifier.height(Space.s1))
            Text(
                state.selectedContent.ifEmpty { "—" },
                style = AppType.title.copy(color = Tokens.fg),
            )
            Spacer(Modifier.height(Space.s7))

            // Body region — fills the panel between the header and the
            // "Press Back" hint. Without this Box, MainList's weight(1f)
            // was sharing the leftover height with a sibling Spacer(weight 1f),
            // so the list could only ever use half the panel height.
            Box(modifier = Modifier.weight(1f).fillMaxWidth()) {
                if (picker == null) {
                    MainList(state, vm,
                        serverFocus = serverFocus,
                        streamFocus = streamFocus,
                        protocolFocus = protocolFocus,
                        segmentFocus = segmentFocus,
                        codecFocus = codecFocus,
                        advancedFocus = advancedFocus,
                        onPick = { kind -> lastPicker = kind; picker = kind },
                        onOpenServerPicker = onOpenServerPicker)
                } else {
                    PickerList(picker!!, state, vm,
                        firstRowFocus = pickerFirstFocus,
                        onBack = { picker = null },
                        // Reset All Settings (in the Advanced picker)
                        // wipes the server list, so AppRoot needs to
                        // route back to ServerPicker. Reuse the same
                        // callback the Server row uses.
                        onResetComplete = onOpenServerPicker)
                }
            }

            Spacer(Modifier.height(Space.s2))
            Text("◀ Press Back to return", style = AppType.mono.copy(color = Tokens.fgDim))
        }
    }
}

private enum class PickerKind { Stream, Protocol, SegmentLength, Codec, Advanced }

// Lives inside the panel's body Box, so the LazyColumn fills the full
// available height and scrolls when its rows overflow. LazyColumn
// composes the first row immediately, so the row FocusRequesters
// resolve cleanly through the drawer's post-mount focus delay.
@Composable
private fun MainList(
    state: UiState,
    vm: PlayerViewModel,
    serverFocus: FocusRequester,
    streamFocus: FocusRequester,
    protocolFocus: FocusRequester,
    segmentFocus: FocusRequester,
    codecFocus: FocusRequester,
    advancedFocus: FocusRequester,
    onPick: (PickerKind) -> Unit,
    onOpenServerPicker: () -> Unit,
) {
    LazyColumn(
        modifier = Modifier.fillMaxSize(),
        verticalArrangement = Arrangement.spacedBy(Space.s1),
    ) {
        item {
            RowView(SettingRow("Server", state.activeServer?.name ?: "—") { onOpenServerPicker() },
                focusRequester = serverFocus)
        }
        item {
            RowView(SettingRow("Stream", state.selectedContent.ifEmpty { "—" }) { onPick(PickerKind.Stream) },
                focusRequester = streamFocus)
        }
        item {
            RowView(SettingRow("Protocol", state.protocol.label) { onPick(PickerKind.Protocol) },
                focusRequester = protocolFocus)
        }
        item {
            RowView(SettingRow("Segment length", state.segment.label) { onPick(PickerKind.SegmentLength) },
                focusRequester = segmentFocus)
        }
        item {
            RowView(SettingRow("Codec", state.codec.label) { onPick(PickerKind.Codec) },
                focusRequester = codecFocus)
        }
        item {
            RowView(SettingRow("Advanced", if (state.developerMode) "HUD on" else "Default") {
                onPick(PickerKind.Advanced)
            }, focusRequester = advancedFocus)
        }
    }
}

private data class SettingRow(val label: String, val value: String, val onClick: () -> Unit)

@Composable
private fun RowView(row: SettingRow, focusRequester: FocusRequester? = null) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(56.dp)
            .let { if (focusRequester != null) it.focusRequester(focusRequester) else it }
            .tvFocus(cornerRadius = Radius.row)
            .clip(RoundedCornerShape(Radius.row))
            .background(Tokens.bgSoft)
            .clickable(onClick = row.onClick)
            .padding(horizontal = Space.s4),
    ) {
        Row(
            modifier = Modifier.fillMaxSize(),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(row.label, style = AppType.body.copy(color = Tokens.fg))
            Spacer(Modifier.weight(1f))
            Text(row.value, style = AppType.mono.copy(color = Tokens.fgDim))
            Spacer(Modifier.width(Space.s2))
            Icon(Icons.Default.ChevronRight, contentDescription = null, tint = Tokens.fgDim)
        }
    }
}

@Composable
private fun PickerList(
    kind: PickerKind,
    state: UiState,
    vm: PlayerViewModel,
    firstRowFocus: FocusRequester,
    onBack: () -> Unit,
    onResetComplete: () -> Unit,
) {
    // Confirmation dialog state for the destructive Reset All Settings
    // row inside the Advanced picker. Local to this composable.
    var showResetConfirm by remember { mutableStateOf(false) }
    Column(modifier = Modifier.fillMaxWidth().fillMaxHeight()) {
        Text(
            kind.headerLabel(),
            style = AppType.titleSm.copy(color = Tokens.fg),
        )
        Spacer(Modifier.height(Space.s4))
        // Fills the available height between the header and the Back
        // hint, scrolling when items overflow. fill=true so the list
        // body uses the full panel — fill=false would let the trailing
        // Back hint eat half the space.
        LazyColumn(
            modifier = Modifier.weight(1f),
            verticalArrangement = Arrangement.spacedBy(Space.s1),
        ) {
            when (kind) {
                PickerKind.Stream -> itemsIndexed(state.filteredContent, key = { _, c -> c.name }) { i, item ->
                    PickerItem(
                        label = item.name,
                        selected = item.name == state.selectedContent,
                        focusRequester = if (i == 0) firstRowFocus else null,
                        onClick = { vm.setSelectedContent(item.name); onBack() },
                    )
                }
                PickerKind.Protocol -> {
                    val list = Protocol.values().toList()
                    itemsIndexed(list) { i, p ->
                        PickerItem(p.label, p == state.protocol,
                            focusRequester = if (i == 0) firstRowFocus else null) {
                            vm.setProtocol(p); onBack()
                        }
                    }
                }
                PickerKind.SegmentLength -> {
                    val list = Segment.values().toList()
                    itemsIndexed(list) { i, s ->
                        PickerItem(s.label, s == state.segment,
                            focusRequester = if (i == 0) firstRowFocus else null) {
                            vm.setSegment(s); onBack()
                        }
                    }
                }
                PickerKind.Codec -> {
                    val list = Codec.values().toList()
                    itemsIndexed(list) { i, c ->
                        PickerItem(c.label, c == state.codec,
                            focusRequester = if (i == 0) firstRowFocus else null) {
                            vm.setCodec(c); onBack()
                        }
                    }
                }
                PickerKind.Advanced -> {
                    item {
                        PickerItem(
                            label = "4K (allow renditions above 1080p)",
                            selected = state.allow4K,
                            focusRequester = firstRowFocus,
                            onClick = { vm.setAllow4K(!state.allow4K) },
                        )
                    }
                    item {
                        PickerItem(
                            label = "Local Proxy (route through go-proxy port)",
                            selected = state.localProxy,
                            onClick = { vm.setLocalProxy(!state.localProxy) },
                        )
                    }
                    item {
                        PickerItem(
                            label = "Auto-Recovery (retry on player error)",
                            selected = state.autoRecovery,
                            onClick = { vm.setAutoRecovery(!state.autoRecovery) },
                        )
                    }
                    item {
                        PickerItem(
                            label = "Go Live (snap to live edge on every load)",
                            selected = state.goLive,
                            onClick = { vm.setGoLive(!state.goLive) },
                        )
                    }
                    item {
                        PickerItem(
                            label = "Skip Home on launch (auto-resume last stream)",
                            selected = state.skipHomeOnLaunch,
                            onClick = { vm.setSkipHomeOnLaunch(!state.skipHomeOnLaunch) },
                        )
                    }
                    item {
                        PreviewVideoSlotsRow(
                            slots = state.previewVideoSlots,
                            hardwareCap = DecodeBudget.maxConcurrent,
                            onChange = { vm.setPreviewVideoSlots(it) },
                        )
                    }
                    item {
                        PlayIdRotationRow(
                            seconds = state.playIdRotationSeconds,
                            onChange = { vm.setPlayIdRotationSeconds(it) },
                        )
                    }
                    item {
                        PickerItem(
                            label = "HUD",
                            selected = state.developerMode,
                            onClick = { vm.setDeveloperMode(!state.developerMode) },
                        )
                    }
                    item {
                        DestructiveRow(
                            label = "Reset All Settings",
                            onClick = { showResetConfirm = true },
                        )
                    }
                }
            }
        }
    }

    if (showResetConfirm) {
        ResetConfirmDialog(
            onConfirm = {
                showResetConfirm = false
                vm.resetAllSettings()
                onResetComplete()
            },
            onDismiss = { showResetConfirm = false },
        )
    }
}

@Composable
private fun PickerItem(
    label: String,
    selected: Boolean,
    focusRequester: FocusRequester? = null,
    onClick: () -> Unit,
) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(48.dp)
            .let { if (focusRequester != null) it.focusRequester(focusRequester) else it }
            .tvFocus(cornerRadius = Radius.row)
            .clip(RoundedCornerShape(Radius.row))
            .background(if (selected) Tokens.bgCard else Tokens.bgSoft)
            .clickable(onClick = onClick)
            .padding(horizontal = Space.s4),
    ) {
        Row(
            modifier = Modifier.fillMaxSize(),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(label, style = AppType.body.copy(color = Tokens.fg))
            Spacer(Modifier.weight(1f))
            if (selected) {
                Icon(Icons.Default.Check, contentDescription = "Selected", tint = Tokens.accent)
            }
        }
    }
}

private fun PickerKind.headerLabel(): String = when (this) {
    PickerKind.Stream -> "Stream"
    PickerKind.Protocol -> "Protocol"
    PickerKind.SegmentLength -> "Segment length"
    PickerKind.Codec -> "Codec"
    PickerKind.Advanced -> "Advanced"
}

/**
 * Destructive action row — used at the bottom of the Advanced picker
 * for "Reset All Settings". Same shape as [PickerItem] but the label
 * renders in [Tokens.destructive] red so users see the danger signal
 * before they tap. Tap fires [onClick]; the caller is expected to
 * surface a confirmation dialog before doing anything irreversible.
 */
@Composable
private fun DestructiveRow(
    label: String,
    onClick: () -> Unit,
) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(48.dp)
            .tvFocus(cornerRadius = Radius.row)
            .clip(RoundedCornerShape(Radius.row))
            .background(Tokens.bgSoft)
            .clickable(onClick = onClick)
            .padding(horizontal = Space.s4),
    ) {
        Row(
            modifier = Modifier.fillMaxSize(),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(label, style = AppType.body.copy(color = Tokens.destructive))
        }
    }
}

/**
 * Confirmation dialog for [PlayerViewModel.resetAllSettings].
 *
 * Built with [androidx.compose.ui.window.Dialog] from core compose-ui
 * — the project uses `androidx.tv:tv-material` and intentionally has
 * no `androidx.compose.material3` dependency, so AlertDialog isn't
 * available. The custom surface keeps us in the cinematic dark theme.
 *
 * Cancel is auto-focused on entry so D-pad Center on the remote
 * dismisses by default, matching tvOS / iOS where the system alert's
 * Cancel button is the safe action.
 */
@Composable
private fun ResetConfirmDialog(
    onConfirm: () -> Unit,
    onDismiss: () -> Unit,
) {
    androidx.compose.ui.window.Dialog(onDismissRequest = onDismiss) {
        val cancelFocus = remember { FocusRequester() }
        LaunchedEffect(Unit) {
            // Wait for the dialog's compose tree to mount, then put
            // focus on Cancel — same delay-then-requestFocus pattern
            // the surrounding drawer uses for picker rows.
            delay(120)
            try { cancelFocus.requestFocus() } catch (_: Throwable) {}
        }
        Column(
            modifier = Modifier
                .fillMaxWidth(0.6f)
                .clip(RoundedCornerShape(Radius.card))
                .background(Tokens.bgCard)
                .border(width = 1.dp, color = Tokens.line, shape = RoundedCornerShape(Radius.card))
                .padding(Space.s5),
        ) {
            Text("Reset All Settings?", style = AppType.titleSm.copy(color = Tokens.fg))
            Spacer(Modifier.height(Space.s3))
            Text(
                "This will forget all saved servers and return the app to its first-launch state. Downloaded content and account data are unaffected.",
                style = AppType.body.copy(color = Tokens.fgDim),
            )
            Spacer(Modifier.height(Space.s5))
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.End,
            ) {
                DialogButton(
                    label = "Cancel",
                    color = Tokens.fg,
                    focusRequester = cancelFocus,
                    onClick = onDismiss,
                )
                Spacer(Modifier.width(Space.s2))
                DialogButton(
                    label = "Reset",
                    color = Tokens.destructive,
                    onClick = onConfirm,
                )
            }
        }
    }
}

/** Plain text button used inside [ResetConfirmDialog]. Picks up the
 * same gold focus ring as the rest of the app via [tvFocus]. */
@Composable
private fun DialogButton(
    label: String,
    color: Color,
    focusRequester: FocusRequester? = null,
    onClick: () -> Unit,
) {
    Box(
        modifier = Modifier
            .height(40.dp)
            .let { if (focusRequester != null) it.focusRequester(focusRequester) else it }
            .tvFocus(cornerRadius = Radius.row)
            .clip(RoundedCornerShape(Radius.row))
            .background(Tokens.bgSoft)
            .clickable(onClick = onClick)
            .padding(horizontal = Space.s4),
    ) {
        Row(
            modifier = Modifier.fillMaxHeight(),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(label, style = AppType.body.copy(color = color))
        }
    }
}

/**
 * Numeric stepper for the LIVE preview-row decode budget. 0 = preview
 * video off (every tile shows its thumbnail). Otherwise the value is
 * the number of simultaneous decodes allowed; tiles past this number
 * fall back to the static thumbnail. Capped at the device's hardware
 * max ([DecodeBudget.maxConcurrent]).
 */
@Composable
private fun PreviewVideoSlotsRow(
    slots: Int,
    hardwareCap: Int,
    onChange: (Int) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .height(60.dp)
            .clip(RoundedCornerShape(Radius.row))
            .background(Tokens.bgSoft)
            .padding(horizontal = Space.s4),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(
                "Preview video (live tiles on Home)",
                style = AppType.body.copy(color = Tokens.fg),
            )
            Spacer(Modifier.height(2.dp))
            Text(
                if (slots <= 0) "Off — thumbnails only"
                else "$slots of $hardwareCap (device max) decoding",
                style = AppType.monoSm.copy(color = Tokens.fgFaint),
            )
        }
        StepperButton(
            symbol = "−",
            enabled = slots > 0,
            onClick = { onChange(slots - 1) },
        )
        Spacer(Modifier.width(Space.s2))
        StepperButton(
            symbol = "+",
            enabled = slots < hardwareCap,
            onClick = { onChange(slots + 1) },
        )
    }
}

@Composable
private fun PlayIdRotationRow(seconds: Int, onChange: (Int) -> Unit) {
    val presets = listOf(
        "Off" to 0,
        "5m" to 300,
        "30m" to 1800,
        "1h" to 3600,
        "6h" to 21600,
    )
    val subtitle = when {
        seconds == 0 -> "Off — one play_id per session"
        seconds < 60 -> "${seconds}s (clamped to 60s at fire time)"
        seconds < 3600 -> "Rotate every ${seconds / 60}m"
        else -> "Rotate every ${seconds / 3600}h"
    }
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(Radius.row))
            .background(Tokens.bgSoft)
            .padding(horizontal = Space.s4, vertical = Space.s3),
    ) {
        Text(
            "Rotate play_id (soak runs)",
            style = AppType.body.copy(color = Tokens.fg),
        )
        Spacer(Modifier.height(2.dp))
        Text(subtitle, style = AppType.monoSm.copy(color = Tokens.fgFaint))
        Spacer(Modifier.height(Space.s2))
        Row(verticalAlignment = Alignment.CenterVertically) {
            presets.forEachIndexed { index, (label, value) ->
                if (index > 0) Spacer(Modifier.width(Space.s2))
                val active = seconds == value
                Box(
                    modifier = Modifier
                        .clip(RoundedCornerShape(8.dp))
                        .background(if (active) Tokens.accent else Tokens.bgCard)
                        .tvFocus(cornerRadius = 8.dp)
                        .clickable(onClick = { onChange(value) })
                        .padding(horizontal = 12.dp, vertical = 6.dp),
                    contentAlignment = Alignment.Center,
                ) {
                    Text(
                        label,
                        style = AppType.monoSm.copy(
                            color = if (active) Tokens.bg else Tokens.fg,
                        ),
                    )
                }
            }
        }
    }
}

@Composable
private fun StepperButton(symbol: String, enabled: Boolean, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .size(40.dp)
            .clip(RoundedCornerShape(20.dp))
            .background(Tokens.bgCard)
            .tvFocus(cornerRadius = 20.dp)
            .clickable(enabled = enabled, onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Text(
            symbol,
            style = AppType.body.copy(color = if (enabled) Tokens.fg else Tokens.fgFaint),
        )
    }
}
