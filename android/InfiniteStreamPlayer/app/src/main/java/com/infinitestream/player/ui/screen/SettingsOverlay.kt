@file:kotlin.OptIn(androidx.tv.material3.ExperimentalTvMaterial3Api::class)

package com.infinitestream.player.ui.screen

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
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Icon
import androidx.tv.material3.Text
import com.infinitestream.player.state.Codec
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

            if (picker == null) {
                MainList(state, vm,
                    onPick = { picker = it },
                    onOpenServerPicker = onOpenServerPicker)
            } else {
                PickerList(picker!!, state, vm, onBack = { picker = null })
            }

            Spacer(Modifier.weight(1f))
            Text("◀ Press Back to return", style = AppType.mono.copy(color = Tokens.fgDim))
        }
    }
}

private enum class PickerKind { Stream, Protocol, SegmentLength, Codec, Advanced }

@Composable
private fun MainList(
    state: UiState,
    vm: PlayerViewModel,
    onPick: (PickerKind) -> Unit,
    onOpenServerPicker: () -> Unit,
) {
    val rows = listOf(
        SettingRow("Server", state.activeServer?.name ?: "—") { onOpenServerPicker() },
        SettingRow("Stream", state.selectedContent.ifEmpty { "—" }) { onPick(PickerKind.Stream) },
        SettingRow("Protocol", state.protocol.label) { onPick(PickerKind.Protocol) },
        SettingRow("Segment length", state.segment.label) { onPick(PickerKind.SegmentLength) },
        SettingRow("Codec", state.codec.label) { onPick(PickerKind.Codec) },
        SettingRow("Advanced", if (state.developerMode) "Developer mode on" else "Default") {
            onPick(PickerKind.Advanced)
        },
    )
    LazyColumn(verticalArrangement = Arrangement.spacedBy(Space.s1)) {
        items(rows, key = { it.label }) { row -> RowView(row) }
    }
}

private data class SettingRow(val label: String, val value: String, val onClick: () -> Unit)

@Composable
private fun RowView(row: SettingRow) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(56.dp)
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
    onBack: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth().fillMaxHeight()) {
        Text(
            kind.headerLabel(),
            style = AppType.titleSm.copy(color = Tokens.fg),
        )
        Spacer(Modifier.height(Space.s4))
        when (kind) {
            PickerKind.Stream -> {
                LazyColumn(verticalArrangement = Arrangement.spacedBy(Space.s1)) {
                    items(state.filteredContent, key = { it.name }) { item ->
                        PickerItem(
                            label = item.name,
                            selected = item.name == state.selectedContent,
                            onClick = { vm.setSelectedContent(item.name); onBack() },
                        )
                    }
                }
            }
            PickerKind.Protocol -> {
                LazyColumn(verticalArrangement = Arrangement.spacedBy(Space.s1)) {
                    items(Protocol.values().toList()) { p ->
                        PickerItem(p.label, p == state.protocol) {
                            vm.setProtocol(p); onBack()
                        }
                    }
                }
            }
            PickerKind.SegmentLength -> {
                LazyColumn(verticalArrangement = Arrangement.spacedBy(Space.s1)) {
                    items(Segment.values().toList()) { s ->
                        PickerItem(s.label, s == state.segment) {
                            vm.setSegment(s); onBack()
                        }
                    }
                }
            }
            PickerKind.Codec -> {
                LazyColumn(verticalArrangement = Arrangement.spacedBy(Space.s1)) {
                    items(Codec.values().toList()) { c ->
                        PickerItem(c.label, c == state.codec) {
                            vm.setCodec(c); onBack()
                        }
                    }
                }
            }
            PickerKind.Advanced -> {
                LazyColumn(verticalArrangement = Arrangement.spacedBy(Space.s1)) {
                    item {
                        PickerItem(
                            label = "Developer mode (AVG/PEAK overlay)",
                            selected = state.developerMode,
                            onClick = { vm.setDeveloperMode(!state.developerMode) },
                        )
                    }
                }
            }
        }
        Spacer(Modifier.weight(1f))
        Text("◀ Back", style = AppType.mono.copy(color = Tokens.fgDim),
            modifier = Modifier
                .clip(RoundedCornerShape(Radius.row))
                .clickable(onClick = onBack)
                .padding(Space.s2)
        )
    }
}

@Composable
private fun PickerItem(label: String, selected: Boolean, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(48.dp)
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
