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
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Settings
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
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
import androidx.tv.material3.Icon
import androidx.tv.material3.Text
import com.infinitestream.player.state.ContentItem
import com.infinitestream.player.state.DecodeBudget
import com.infinitestream.player.state.PlayerViewModel
import com.infinitestream.player.state.UiState
import coil.compose.AsyncImage
import com.infinitestream.player.ui.component.LivePreviewTile
import com.infinitestream.player.ui.component.LiveVideoSurface
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
    // Visible preview slots — capped at min(user pref, hardware AVC
    // decoder limit). User pref comes from Settings → Advanced and
    // defaults to the hardware cap on first launch. The pool of
    // eligible clips can be much larger; user scrolls through it via
    // D-pad-Left/Right at the row edges (carousel rotation).
    //
    // Dedupe by `clip_id` (server-computed), preferring the H.264 entry
    // since that's universally hardware-decodable on every TV chip.
    val hardwareCap = remember { DecodeBudget.maxConcurrent }
    val totalVideoSlots = state.previewVideoSlots.coerceIn(0, hardwareCap)
    // Hero claims 1 of the decoder slots when the user has at least one
    // preview slot enabled — mirrors iOS where the Continue Watching
    // hero is the visual focal point and gets first priority. Tiles get
    // whatever's left over; everything past that renders its thumbnail
    // (the LivePreviewTile already handles `active=false` via the
    // poster underneath).
    val heroVideoActive = state.activeServer != null && totalVideoSlots > 0
    val visibleSlots = (totalVideoSlots - (if (heroVideoActive) 1 else 0)).coerceAtLeast(0)
    // Pool of unique H.264 clips for the carousel.
    //
    // Ordering policy (most-prominent first):
    //   1. The Continue Watching clip — slot 0, even if it's not the
    //      most-watched; resume should always be the leftmost.
    //   2. By view count DESC — "frequently viewed" surfaced near the
    //      front so the user's most-played content is never more than
    //      a press or two away.
    //   3. Catalogue order as the final tiebreaker.
    val rawPool = items
        .filter { it.codec.isEmpty() || it.codec == "h264" }
        .distinctBy { it.clipId }
    val frequentlyViewed = rawPool.sortedWith(
        compareByDescending<ContentItem> { state.viewCounts[it.clipId] ?: 0 }
            .thenBy { rawPool.indexOf(it) }
    )
    val featuredClipId = featured?.clipId
    val previewPool = if (featuredClipId != null) {
        val pinned = frequentlyViewed.firstOrNull { it.clipId == featuredClipId }
        if (pinned != null) listOf(pinned) + frequentlyViewed.filter { it !== pinned }
        else frequentlyViewed
    } else frequentlyViewed
    val rest = items - previewPool.toSet()

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Tokens.bg)
    ) {
        Column(modifier = Modifier.fillMaxSize().padding(Space.s7)) {
            // Header — big serif brand + monospace active-server label
            // on the left, a focusable gear on the right that opens
            // SettingsOverlay (mirrors the iOS/tvOS HomeScreen header).
            // Typography matches iOS: displayLg (Fraunces 44sp) for the
            // title, monoSm for the server label, so the two platforms
            // read as siblings.
            Row(
                modifier = Modifier.fillMaxWidth(),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        "InfiniteStream",
                        style = AppType.displayLg.copy(color = Tokens.fg),
                    )
                    val activeServerLabel = state.activeServer?.name
                    if (!activeServerLabel.isNullOrBlank()) {
                        Text(
                            activeServerLabel,
                            style = AppType.monoSm.copy(color = Tokens.fgFaint),
                        )
                    }
                }
                HomeSettingsButton(onClick = onOpenSettings)
            }
            Spacer(Modifier.height(Space.s5))

            // "NOW PLAYING" pulled OUTSIDE the Hero so the hero band
            // reads as a clean video poster, matching the iPad layout.
            Text("NOW PLAYING", style = AppType.label.copy(color = Tokens.fgDim))
            Spacer(Modifier.height(Space.s2))
            val heroAppStopped by vm.appStopped.collectAsStateWithLifecycle()
            Hero(
                featured = featured,
                state = state,
                videoActive = heroVideoActive && !heroAppStopped,
                onAcquireDecoderLease = vm::acquireDecoderLease,
                onReleaseDecoderLease = vm::releaseDecoderLease,
                onResume = { if (featured != null) playPicked(featured.name) },
            )

            Spacer(Modifier.height(Space.s7))

            val activeServer = state.activeServer
            // LIVE row — flat thumbnail row over the whole pool (minus
            // the featured clip, which is already showing as the hero
            // band above). Tiles render with `active=false` so they
            // skip the ExoPlayer build entirely and stay as static
            // posters. The hero is the only video decoder in use on
            // Home; the MTK shared-decoder flicker we saw with two
            // concurrent decoders on the same URL goes away.
            val tilePool = if (heroVideoActive) {
                previewPool.filter { it.clipId != featured?.clipId }
            } else previewPool
            if (tilePool.isNotEmpty() && activeServer != null) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    StatusDot(color = Tokens.live)
                    Spacer(Modifier.width(Space.s1))
                    Text("LIVE", style = AppType.label.copy(color = Tokens.fgDim))
                }
                Spacer(Modifier.height(Space.s3))
                LazyRow(horizontalArrangement = Arrangement.spacedBy(Space.s3)) {
                    items(tilePool, key = { c -> c.name }) { c ->
                        LivePreviewTile(
                            content = c,
                            server = activeServer,
                            // Always inactive — thumbnail-only. The
                            // hardware decoder budget belongs to the
                            // hero band on this screen.
                            active = false,
                            appStopped = false,
                            onClick = { picked -> playPicked(picked.name) },
                            onAcquireDecoderLease = vm::acquireDecoderLease,
                            onReleaseDecoderLease = vm::releaseDecoderLease,
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
private fun Hero(
    featured: ContentItem?,
    state: UiState,
    videoActive: Boolean,
    onAcquireDecoderLease: () -> Unit,
    onReleaseDecoderLease: () -> Unit,
    onResume: () -> Unit,
) {
    val activeServer = state.activeServer
    // Hero now plays the featured clip as a silent 360p background when
    // (a) the user has at least one preview slot enabled, (b) the app
    // is in foreground, and (c) the catalogue has a featured clip. The
    // hero takes one DecodeBudget slot — HomeScreen reserves it ahead
    // of the preview tile carousel so total in-flight decoders stays
    // within `DecodeBudget.maxConcurrent` (the chip's hard cap, e.g. 3
    // on the Google TV Streamer MTK chip). Past three, tiles render
    // their static thumbnail poster instead of building an ExoPlayer.
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(220.dp)
            .clip(RoundedCornerShape(Radius.cardLg))
            .background(
                Brush.horizontalGradient(
                    listOf(Tokens.bgCard, Tokens.bgSoft)
                )
            ),
    ) {
        // Thumbnail poster underneath — same boot-frame trick the LIVE
        // preview tiles use so the hero doesn't flash black while the
        // first segment buffers. Falls through to the gradient when no
        // thumbnail has been generated yet.
        if (featured != null && activeServer != null && featured.thumbnailPathLarge != null) {
            AsyncImage(
                model = "${activeServer.apiUrl}${featured.thumbnailPathLarge}",
                contentDescription = null,
                contentScale = androidx.compose.ui.layout.ContentScale.Crop,
                modifier = Modifier.fillMaxSize(),
            )
        }
        if (videoActive && featured != null && activeServer != null) {
            LiveVideoSurface(
                content = featured,
                server = activeServer,
                onAcquireDecoderLease = onAcquireDecoderLease,
                onReleaseDecoderLease = onReleaseDecoderLease,
                // Hero is the visual focal point on a TV. Crank it to
                // 720p (matches iOS HeroLiveVideo). Tiles stay at 360p
                // since they're tiny.
                resolutionLabel = "720p",
                maxVideoWidth = 1280,
                maxVideoHeight = 720,
            )
        }
        // Bottom gradient + copy stay legible against any frame the
        // video might render. Resume button anchors the bottom-right.
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(
                    Brush.verticalGradient(
                        0f to Color.Transparent,
                        0.55f to Color.Transparent,
                        1f to Color.Black.copy(alpha = 0.75f),
                    )
                )
        )
        Column(
            modifier = Modifier.fillMaxSize().padding(Space.s7),
            verticalArrangement = Arrangement.SpaceBetween,
        ) {
            Column {
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

/** Circular gear icon in the HomeScreen header. D-pad up from the
 *  content tiles reaches it; Enter opens [SettingsOverlay] (the existing
 *  drawer that PlaybackScreen's transport gear also opens). Same shape
 *  language as [TransportButton] in PlaybackScreen so the two surfaces
 *  read as siblings. */
@Composable
private fun HomeSettingsButton(onClick: () -> Unit) {
    Box(
        modifier = Modifier
            .size(44.dp)
            .tvFocus(cornerRadius = Radius.pill)
            .clip(RoundedCornerShape(Radius.pill))
            .background(Tokens.bgCard.copy(alpha = 0.6f))
            .clickable(onClick = onClick),
        contentAlignment = Alignment.Center,
    ) {
        Icon(Icons.Default.Settings, contentDescription = "Settings", tint = Tokens.fg)
    }
}

