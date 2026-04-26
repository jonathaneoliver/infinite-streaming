@file:kotlin.OptIn(
    androidx.tv.material3.ExperimentalTvMaterial3Api::class,
    androidx.media3.common.util.UnstableApi::class,
)

package com.infinitestream.player.ui.component

import android.view.ViewGroup
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.MimeTypes
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import androidx.tv.material3.Text
import com.infinitestream.player.state.ContentItem
import com.infinitestream.player.state.ServerEnvironment
import com.infinitestream.player.ui.theme.AppType
import com.infinitestream.player.ui.theme.Radius
import com.infinitestream.player.ui.theme.Tokens
import com.infinitestream.player.ui.theme.tvFocus

/**
 * A small autoplay video card used on the Home screen. Owns its own
 * ExoPlayer hardwired to the 360p HLS rendition, hits the API port directly
 * (not the per-session go-proxy port — these previews shouldn't be subject
 * to failure injection), muted, looping. Several render in parallel.
 *
 * URL: `http://{host}:{apiPort}/go-live/{name}/playlist_6s_360p.m3u8`
 *
 * The 6 s segment variant keeps decoder workload low — at 360 p H.264 that's
 * roughly 1 Mbps and a few % of one ARM core per tile. The Google TV
 * Streamer's hardware decoder advertises enough sessions that 4–6 tiles
 * play simultaneously without falling back to software.
 */
@Composable
fun LivePreviewTile(
    content: ContentItem,
    server: ServerEnvironment,
    onClick: (ContentItem) -> Unit,
) {
    val context = LocalContext.current
    val player = remember(content.name, server.host, server.apiPort) {
        ExoPlayer.Builder(context).build().apply {
            volume = 0f
            repeatMode = Player.REPEAT_MODE_ONE
            // Cap to 360p in case go-live decides to serve a master playlist
            // somewhere — otherwise we accidentally pull 1080 p+ for a tile.
            // Also disable audio entirely: even with volume = 0 the audio
            // track still gets decoded and the player grabs audio focus,
            // which the user heard playing on Home from one of the tiles.
            trackSelectionParameters = trackSelectionParameters.buildUpon()
                .setMaxVideoSize(640, 360)
                .setPreferredVideoMimeType(MimeTypes.VIDEO_H264)
                .setTrackTypeDisabled(C.TRACK_TYPE_AUDIO, true)
                .build()
            setMediaItem(
                MediaItem.fromUri(
                    "${server.apiUrl}/go-live/${content.name}/playlist_6s_360p.m3u8"
                )
            )
            prepare()
            playWhenReady = true
        }
    }
    DisposableEffect(player) { onDispose { player.release() } }

    Box(
        modifier = Modifier
            .size(width = 220.dp, height = 124.dp)
            .tvFocus(cornerRadius = Radius.card)
            .clip(RoundedCornerShape(Radius.card))
            .background(Tokens.bgSoft)
            .clickable { onClick(content) },
    ) {
        AndroidView(
            modifier = Modifier.fillMaxSize(),
            factory = { ctx ->
                PlayerView(ctx).apply {
                    this.player = player
                    useController = false
                    setBackgroundColor(android.graphics.Color.BLACK)
                    isFocusable = false
                    isFocusableInTouchMode = false
                    descendantFocusability = ViewGroup.FOCUS_BLOCK_DESCENDANTS
                    resizeMode = AspectRatioFrameLayout.RESIZE_MODE_ZOOM
                }
            },
        )

        // Bottom gradient + title overlay so the name stays legible against
        // any frame.
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(
                    Brush.verticalGradient(
                        0f to Color.Transparent,
                        0.6f to Color.Transparent,
                        1f to Color.Black.copy(alpha = 0.85f),
                    )
                )
        )

        Row(
            modifier = Modifier.align(Alignment.TopStart).padding(8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            StatusDot(color = Tokens.live)
            Spacer(Modifier.width(4.dp))
            Text("LIVE", style = AppType.monoSm.copy(color = Tokens.live))
        }

        Text(
            content.name,
            modifier = Modifier
                .align(Alignment.BottomStart)
                .padding(horizontal = 10.dp, vertical = 8.dp),
            style = AppType.monoSm.copy(color = Tokens.fg),
            maxLines = 1,
        )
    }
}
