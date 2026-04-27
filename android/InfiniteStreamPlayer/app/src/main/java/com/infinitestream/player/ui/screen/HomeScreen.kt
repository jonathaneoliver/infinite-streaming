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
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
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

    // Local Composable scope for things that fire while HomeScreen is
    // still on screen (e.g. carousel-rotation focus restoration).
    val scope = rememberCoroutineScope()

    // Deferred-pick: switch routes immediately so Home unmounts and its
    // tile decoders release, then 300 ms later set the selected content
    // (which triggers the main player's prepare() + decoder alloc).
    // The delay lives on the ViewModel's scope (`viewModelScope`)
    // because the local rememberCoroutineScope is tied to HomeScreen's
    // lifetime — the route change unmounts HomeScreen and cancels the
    // scope before the delay fires, so the main player would never
    // receive setSelectedContent and the playback screen would just
    // sit there with nothing to play.
    val playPicked: (String) -> Unit = { name ->
        onPlay()
        vm.setSelectedContentDeferred(name)
    }

    val items = state.filteredContent
    // Continue Watching hero: prefer the most recently *successfully played*
    // clip (first-frame-rendered, persisted across app restarts). Fall
    // back to the first item in the catalogue if the persisted entry no
    // longer exists or hasn't been set yet.
    val featured = items.firstOrNull { it.name == state.lastPlayed }
        ?: items.firstOrNull()
    // 3 visible preview slots; the pool of eligible clips can be much
    // larger and the user scrolls through it via D-pad-Left/Right at the
    // row edges (carousel rotation). Each carousel step is exactly 1
    // decoder dispose + 1 alloc thanks to the content-keyed LazyRow.
    //
    // Dedupe by `clip_id` (server-computed), preferring the H.264 entry
    // since that's universally hardware-decodable on every TV chip. This
    // replaces the earlier substring/token heuristic that was over-
    // collapsing — "redbull" matched "red_bull_storm_chase", every
    // samsung_* clip merged into one, and the 45-item content list ended
    // up as 6 visible cards.
    val visibleSlots = 3
    // Pool of unique H.264 clips for the carousel. The first slot is
    // pinned to the same logical clip as the Continue Watching hero so
    // the user's most-recent stream is always the leftmost preview —
    // and so resuming from the hero or clicking the leading tile do the
    // same thing.
    val rawPool = items
        .filter { it.codec.isEmpty() || it.codec == "h264" }
        .distinctBy { it.clipId }
    val featuredClipId = featured?.clipId
    val previewPool = if (featuredClipId != null) {
        val pinned = rawPool.firstOrNull { it.clipId == featuredClipId }
        if (pinned != null) listOf(pinned) + rawPool.filter { it !== pinned } else rawPool
    } else rawPool
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
                // Carousel offset: the visible window starts here in the
                // pool. Increment on D-pad-Right past the rightmost slot,
                // decrement on D-pad-Left past the leftmost. Modulo wraps
                // at the pool ends so the row keeps cycling.
                var offset by remember { mutableStateOf(0) }
                val poolSize = previewPool.size
                val visible = remember(offset, poolSize) {
                    if (poolSize <= visibleSlots) previewPool
                    else (0 until visibleSlots).map { i ->
                        previewPool[((offset + i) % poolSize + poolSize) % poolSize]
                    }
                }
                // FocusRequester per pool item — used to pin focus to the
                // item that just slid in after a rotation.
                val focusReqs = remember(poolSize) {
                    previewPool.associateWith { FocusRequester() }
                }
                LazyRow(horizontalArrangement = Arrangement.spacedBy(Space.s3)) {
                    itemsIndexed(visible, key = { _, c -> c.name }) { i, c ->
                        val isFirst = i == 0
                        val isLast = i == visible.lastIndex
                        val canRotate = poolSize > visibleSlots
                        LivePreviewTile(
                            content = c,
                            server = activeServer,
                            active = true,
                            onClick = { picked -> playPicked(picked.name) },
                            onAcquireDecoderLease = vm::acquireDecoderLease,
                            onReleaseDecoderLease = vm::releaseDecoderLease,
                            modifier = Modifier
                                .focusRequester(focusReqs[c] ?: FocusRequester())
                                .onPreviewKeyEvent { ev ->
                                    if (ev.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                                    when {
                                        canRotate && ev.key == Key.DirectionRight && isLast -> {
                                            offset = (offset + 1) % poolSize
                                            scope.launch {
                                                delay(60)
                                                val newLast = previewPool[
                                                    ((offset + visibleSlots - 1) % poolSize + poolSize) % poolSize
                                                ]
                                                runCatching { focusReqs[newLast]?.requestFocus() }
                                            }
                                            true
                                        }
                                        canRotate && ev.key == Key.DirectionLeft && isFirst -> {
                                            offset = ((offset - 1) % poolSize + poolSize) % poolSize
                                            scope.launch {
                                                delay(60)
                                                val newFirst = previewPool[
                                                    ((offset) % poolSize + poolSize) % poolSize
                                                ]
                                                runCatching { focusReqs[newFirst]?.requestFocus() }
                                            }
                                            true
                                        }
                                        else -> false
                                    }
                                },
                        )
                    }
                }
                Spacer(Modifier.height(Space.s7))
            }

            if (rest.isNotEmpty()) {
                Text("MORE STREAMS", style = AppType.label.copy(color = Tokens.fg))
                Spacer(Modifier.height(Space.s3))
                ContentRow(
                    items = rest,
                    apiUrlBase = activeServer?.apiUrl,
                    isLive = false,
                    onClick = { c -> playPicked(c.name) },
                )
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
private fun ContentRow(
    items: List<ContentItem>,
    apiUrlBase: String?,
    isLive: Boolean,
    onClick: (ContentItem) -> Unit,
) {
    LazyRow(horizontalArrangement = Arrangement.spacedBy(Space.s3)) {
        items(items, key = { it.name }) { c -> ContentCard(c, apiUrlBase, isLive, onClick) }
    }
}

@Composable
private fun ContentCard(
    c: ContentItem,
    apiUrlBase: String?,
    isLive: Boolean,
    onClick: (ContentItem) -> Unit,
) {
    Box(
        modifier = Modifier
            .size(width = 174.dp, height = 100.dp)
            .tvFocus(cornerRadius = Radius.card)
            .clip(RoundedCornerShape(Radius.card))
            .background(Tokens.bgSoft)
            .border(1.dp, Tokens.line, RoundedCornerShape(Radius.card))
            .clickable { onClick(c) },
    ) {
        // Poster thumbnail behind the label. Prefer the 320-wide small
        // variant since these cards are 174 px wide on screen — saves
        // bandwidth and gives Coil less to decode at scroll.
        val thumb = c.thumbnailPathSmall ?: c.thumbnailPath
        if (thumb != null && apiUrlBase != null) {
            coil.compose.AsyncImage(
                model = "$apiUrlBase$thumb",
                contentDescription = null,
                contentScale = androidx.compose.ui.layout.ContentScale.Crop,
                modifier = Modifier.fillMaxSize(),
            )
            // Bottom gradient so the title stays legible against any frame.
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .background(
                        Brush.verticalGradient(
                            0f to Color.Transparent,
                            0.55f to Color.Transparent,
                            1f to Color.Black.copy(alpha = 0.85f),
                        )
                    )
            )
        }
        Column(modifier = Modifier.fillMaxSize().padding(Space.s2)) {
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

