@file:kotlin.OptIn(androidx.tv.material3.ExperimentalTvMaterial3Api::class)

package com.infinitestream.player.ui.screen

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
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
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Text
import com.infinitestream.player.state.ContentItem
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.state.UiState
import com.infinitestream.player.ui.component.LivePreviewTile
import com.infinitestream.player.ui.component.StatusDot
import com.infinitestream.player.ui.theme.AppType
import com.infinitestream.player.ui.theme.Radius
import com.infinitestream.player.ui.theme.Space
import com.infinitestream.player.ui.theme.Tokens
import com.infinitestream.player.ui.theme.tvFocus

/**
 * Home/Browse — top nav, hero "Continue Watching" band, then horizontal rows
 * (Live Streams, Recent Sessions). Selecting any card jumps to playback.
 */
@Composable
fun HomeScreen(
    state: UiState,
    vm: PlayerViewModel,
    onPlay: () -> Unit,
    onOpenServerPicker: () -> Unit,
    onOpenSettings: () -> Unit,
) {
    LaunchedEffect(state.activeServer) {
        if (state.content.isEmpty() && state.activeServer != null) {
            vm.fetchContentList()
        }
    }

    val items = state.filteredContent
    val featured = items.firstOrNull()
    // First N *distinct* clips get a live preview tile. The content list
    // typically carries each clip three times (one per codec — h264/hevc/
    // av1) and we want six different *streams* on the row, not three of
    // the same windsurfer in different encodings. Dedupe by stripping the
    // codec/timestamp suffix; for each distinct clip prefer the H.264
    // entry (universally hardware-decodable, smallest decoder cost).
    val previewCount = 6
    val previews = items
        .groupBy { dedupeKey(it.name) }
        .map { (_, group) -> group.minByOrNull { codecPreference(it.name) }!! }
        .take(previewCount)
    val rest = items - previews.toSet()

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Tokens.bg)
    ) {
        Column(modifier = Modifier.fillMaxSize().padding(Space.s7)) {
            TopNav(state, onOpenServerPicker = onOpenServerPicker, onOpenSettings = onOpenSettings)
            Spacer(Modifier.height(Space.s7))

            Hero(featured, state, onResume = {
                if (featured != null) {
                    vm.setSelectedContent(featured.name)
                    onPlay()
                }
            })

            Spacer(Modifier.height(Space.s7))

            val activeServer = state.activeServer
            if (previews.isNotEmpty() && activeServer != null) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    StatusDot(color = Tokens.live)
                    Spacer(Modifier.width(Space.s1))
                    Text("LIVE STREAMS", style = AppType.label.copy(color = Tokens.fg))
                }
                Spacer(Modifier.height(Space.s3))
                LazyRow(horizontalArrangement = Arrangement.spacedBy(Space.s3)) {
                    items(previews, key = { it.name }) { c ->
                        LivePreviewTile(
                            content = c,
                            server = activeServer,
                            onClick = { picked ->
                                vm.setSelectedContent(picked.name); onPlay()
                            },
                        )
                    }
                }
                Spacer(Modifier.height(Space.s7))
            }

            if (rest.isNotEmpty()) {
                Text("MORE STREAMS", style = AppType.label.copy(color = Tokens.fg))
                Spacer(Modifier.height(Space.s3))
                ContentRow(items = rest, isLive = false, onClick = { c ->
                    vm.setSelectedContent(c.name); onPlay()
                })
            }

            if (items.isEmpty()) {
                Box(
                    modifier = Modifier.fillMaxWidth().padding(top = Space.s7),
                    contentAlignment = Alignment.Center,
                ) {
                    Text(
                        if (state.activeServer == null) "No server selected"
                        else "No content available — check server",
                        style = AppType.body.copy(color = Tokens.fgDim),
                    )
                }
            }
        }
    }
}

@Composable
private fun TopNav(state: UiState, onOpenServerPicker: () -> Unit, onOpenSettings: () -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
        Text("InfiniteStream", style = AppType.titleSm.copy(color = Tokens.fg))
        Spacer(Modifier.width(Space.s7))
        NavItem("Home", selected = true) {}
        Spacer(Modifier.width(Space.s4))
        NavItem("Streams", selected = false) {}
        Spacer(Modifier.width(Space.s4))
        NavItem("Library", selected = false) {}
        Spacer(Modifier.width(Space.s4))
        NavItem("Search", selected = false) {}
        Spacer(Modifier.weight(1f))
        Text(
            state.activeServer?.let { "${it.host}:${it.apiPort}" } ?: "no server",
            style = AppType.mono.copy(color = Tokens.fgDim),
            modifier = Modifier
                .clip(RoundedCornerShape(Radius.pill))
                .tvFocus(cornerRadius = Radius.pill)
                .clickable(onClick = onOpenServerPicker)
                .padding(horizontal = Space.s3, vertical = Space.s1),
        )
        Spacer(Modifier.width(Space.s2))
        NavItem("Settings", selected = false, onClick = onOpenSettings)
    }
}

@Composable
private fun NavItem(label: String, selected: Boolean, onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .clip(RoundedCornerShape(Radius.pill))
            .tvFocus(cornerRadius = Radius.pill)
            .clickable(onClick = onClick)
            .padding(horizontal = Space.s3, vertical = Space.s1),
    ) {
        Text(
            label,
            style = AppType.body.copy(
                color = if (selected) Tokens.fg else Tokens.fgDim,
            ),
        )
    }
}

@Composable
private fun Hero(featured: ContentItem?, state: UiState, onResume: () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(220.dp)
            .clip(RoundedCornerShape(Radius.cardLg))
            .background(
                Brush.horizontalGradient(
                    listOf(Tokens.bgCard, Tokens.bgSoft)
                )
            )
            .padding(Space.s7),
    ) {
        Column(
            modifier = Modifier.fillMaxSize(),
            verticalArrangement = Arrangement.SpaceBetween,
        ) {
            Column {
                Text("CONTINUE WATCHING", style = AppType.label.copy(color = Tokens.accent))
                Spacer(Modifier.height(Space.s1))
                Text(
                    featured?.name ?: "—",
                    style = AppType.display.copy(color = Tokens.fg),
                )
                Spacer(Modifier.height(Space.s1))
                Text(
                    state.activeServer?.let { "From ${it.name}" } ?: "Pick a server to start",
                    style = AppType.body.copy(color = Tokens.fgDim),
                )
            }
            Row {
                PrimaryButton("Resume", onClick = onResume, accent = true)
            }
        }
    }
}

@Composable
private fun ContentRow(items: List<ContentItem>, isLive: Boolean, onClick: (ContentItem) -> Unit) {
    LazyRow(horizontalArrangement = Arrangement.spacedBy(Space.s3)) {
        items(items, key = { it.name }) { c -> ContentCard(c, isLive, onClick) }
    }
}

@Composable
private fun ContentCard(c: ContentItem, isLive: Boolean, onClick: (ContentItem) -> Unit) {
    Box(
        modifier = Modifier
            .size(width = 174.dp, height = 100.dp)
            .tvFocus(cornerRadius = Radius.card)
            .clip(RoundedCornerShape(Radius.card))
            .background(Tokens.bgSoft)
            .border(1.dp, Tokens.line, RoundedCornerShape(Radius.card))
            .clickable { onClick(c) }
            .padding(Space.s2),
    ) {
        Column(modifier = Modifier.fillMaxSize()) {
            if (isLive) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    StatusDot(color = Tokens.live)
                    Spacer(Modifier.width(4.dp))
                    Text("LIVE", style = AppType.monoSm.copy(color = Tokens.live))
                }
            }
            Spacer(Modifier.weight(1f))
            Text(
                c.name,
                style = AppType.body.copy(color = Tokens.fg),
                maxLines = 2,
            )
        }
    }
}

/**
 * Strip per-codec / per-timestamp suffixes so the three encodings of the
 * same clip collapse to a single key. Examples:
 *   "redbull_p200_h264"                                 → "redbull_p200"
 *   "redbull_p200_hevc"                                 → "redbull_p200"
 *   "INSANE_FPV..._p200_h264_20260423_212139"           → "insane_fpv..._p200"
 *   "INSANE_FPV..._p200_hevc_20260423_212139"           → "insane_fpv..._p200"
 */
private fun dedupeKey(name: String): String =
    name.lowercase().replace(Regex("_(h264|hevc|h265|av1)(_\\d{8}_\\d{6})?$"), "")

/** Lower number = preferred. H.264 first because every TV hardware-decodes
 *  it; HEVC second; AV1 last (still software-decoded on many chips). */
private fun codecPreference(name: String): Int {
    val lower = name.lowercase()
    return when {
        "h264" in lower -> 0
        "hevc" in lower || "h265" in lower -> 1
        "av1" in lower -> 2
        else -> 3
    }
}

