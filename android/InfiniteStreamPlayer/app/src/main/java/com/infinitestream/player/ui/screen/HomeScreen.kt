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
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.focus.onFocusChanged
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
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

    // Deferred-pick: switch routes immediately so Home unmounts and its 4
    // tile decoders release, then 300 ms later set the selected content
    // (which triggers the main player's prepare() and a fresh decoder
    // allocation). Without the delay the main player races the tile
    // releases and trips Codec2Client::createComponent NO_MEMORY.
    val scope = rememberCoroutineScope()
    val playPicked: (String) -> Unit = { name ->
        onPlay()
        scope.launch {
            delay(300)
            vm.setSelectedContent(name)
        }
    }

    val items = state.filteredContent
    val featured = items.firstOrNull()
    // First N *distinct* clips get a live preview tile. The content list
    // typically carries each clip three times (one per codec — h264/hevc/
    // av1) and we want N different *streams* on the row, not the same
    // windsurfer in three encodings. Dedupe by stripping the codec/
    // timestamp suffix; for each distinct clip prefer the H.264 entry
    // (universally hardware-decodable, smallest decoder cost).
    //
    // The MTK c2.mtk.avc.decoder on the Streamer maxes out at 3
    // simultaneous H.264 hardware decoders. Rather than picking 3 clips
    // and ignoring the rest, we render the *whole* eligible H.264 pool
    // as cards and only the 3 around the focused index actually decode
    // (`active = true`). Focus naturally walks the row, LazyRow scrolls
    // the cards, the active window slides → exactly one decoder dispose
    // + one alloc per step.
    //
    // The pool is also visually deduped: greedy accept-if-different-
    // enough so we don't show four near-duplicate names from the same
    // clip family (INSANE_FPV_NEW vs INSANE_FPV_SHOTS_..., redbull vs
    // red_bull_storm_chase, etc).
    val activeWindowSize = 3
    val previewPool = mutableListOf<ContentItem>().apply {
        for (c in items) {
            if ("h264" !in c.name.lowercase()) continue
            if (none { similarPreviewKey(it.name, c.name) }) add(c)
        }
    }
    val rest = items - previewPool.toSet()

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Tokens.bg)
    ) {
        Column(modifier = Modifier.fillMaxSize().padding(Space.s7)) {
            // Top nav (Home/Streams/Library/Search/server-pill/Settings) was
            // removed — settings is reachable via Playback HUD → gear and
            // the server picker via Settings → Server. Brand mark stays as
            // a quiet anchor.
            Text("InfiniteStream", style = AppType.bodySm.copy(color = Tokens.fgDim))
            Spacer(Modifier.height(Space.s4))

            Hero(featured, state, onResume = {
                if (featured != null) playPicked(featured.name)
            })

            Spacer(Modifier.height(Space.s7))

            val activeServer = state.activeServer
            if (previewPool.isNotEmpty() && activeServer != null) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    StatusDot(color = Tokens.live)
                    Spacer(Modifier.width(Space.s1))
                    Text("LIVE STREAMS", style = AppType.label.copy(color = Tokens.fg))
                }
                Spacer(Modifier.height(Space.s3))
                // Track which tile in the pool is focused. The 3 around it
                // get active=true and decode; the rest render as static
                // QUEUED placeholders. Stepping focus right releases one
                // decoder on the left and acquires one on the right —
                // exactly the carousel behaviour the user asked for.
                var focusedIndex by remember { mutableStateOf(0) }
                val activeRange = activeRangeAround(focusedIndex, previewPool.size, activeWindowSize)
                LazyRow(horizontalArrangement = Arrangement.spacedBy(Space.s3)) {
                    itemsIndexed(previewPool, key = { _, c -> c.name }) { i, c ->
                        LivePreviewTile(
                            content = c,
                            server = activeServer,
                            active = i in activeRange,
                            onClick = { picked -> playPicked(picked.name) },
                            modifier = Modifier
                                .onFocusChanged { if (it.isFocused) focusedIndex = i },
                        )
                    }
                }
                Spacer(Modifier.height(Space.s7))
            }

            if (rest.isNotEmpty()) {
                Text("MORE STREAMS", style = AppType.label.copy(color = Tokens.fg))
                Spacer(Modifier.height(Space.s3))
                ContentRow(items = rest, isLive = false, onClick = { c ->
                    playPicked(c.name)
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
private fun Hero(featured: ContentItem?, state: UiState, onResume: () -> Unit) {
    val activeServer = state.activeServer
    // Hero used to autoplay the featured clip as a 360p video background.
    // Removed because the combination of (hero + 6 preview tiles + main
    // playback) blew past the chip's hardware-decode budget on the home →
    // playback transition. Static gradient + text only now.
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
                    activeServer?.let { "From ${it.name}" } ?: "Pick a server to start",
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
 * Compute the inclusive index range of preview pool entries that should be
 * actively decoding around the focused index. Returns a window of size
 * `windowSize` centred on `focused`, clamped to the bounds of `total` so
 * the count never exceeds `windowSize`. Examples for windowSize=3:
 *
 *   focused=0, total=10 → 0..2
 *   focused=4, total=10 → 3..5
 *   focused=9, total=10 → 7..9
 */
private fun activeRangeAround(focused: Int, total: Int, windowSize: Int): IntRange {
    if (total <= windowSize) return 0 until total
    val half = windowSize / 2
    val start = (focused - half).coerceAtLeast(0).coerceAtMost(total - windowSize)
    return start until (start + windowSize)
}

/**
 * Reduce a content name to its visual identity — strip the trailing
 * `_p200_codec[_timestamp]` so we can compare two streams by what they
 * *show*, not how they're encoded.
 *
 *   "redbull_p200_h264"                                 → "redbull"
 *   "INSANE_FPV..._p200_h264_20260423_212139"           → "insane_fpv..."
 */
private fun previewKey(name: String): String =
    name.lowercase().substringBefore("_p200")

/**
 * Are two content names visually similar enough that we shouldn't show
 * both in the same preview row? Two heuristics, OR'd:
 *
 * 1. Underscore-token overlap ≥ 2 (tokens of length ≥ 3, so noise like
 *    "of"/"to" doesn't trip it). Catches the structured cases —
 *    "INSANE_FPV_NEW" vs "INSANE_FPV_SHOTS_Hydrofoil_Windsurfing" share
 *    {insane, fpv}.
 * 2. With underscores stripped, one is a prefix of the other (length ≥ 5
 *    on the shorter side). Catches "redbull" vs "red_bull_storm_chase"
 *    where token-overlap is empty but the visual identity is the same.
 */
private fun similarPreviewKey(a: String, b: String): Boolean {
    val ka = previewKey(a)
    val kb = previewKey(b)
    val tokensA = ka.split("_", "-").filter { it.length >= 3 }.toSet()
    val tokensB = kb.split("_", "-").filter { it.length >= 3 }.toSet()
    if ((tokensA intersect tokensB).size >= 2) return true
    val flatA = ka.replace("_", "").replace("-", "")
    val flatB = kb.replace("_", "").replace("-", "")
    if (flatA.length >= 5 && flatB.startsWith(flatA)) return true
    if (flatB.length >= 5 && flatA.startsWith(flatB)) return true
    return false
}

