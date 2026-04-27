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
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.Renderer
import androidx.media3.exoplayer.audio.AudioRendererEventListener
import androidx.media3.exoplayer.audio.AudioSink
import androidx.media3.exoplayer.mediacodec.MediaCodecSelector
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import androidx.tv.material3.Text
import coil.compose.AsyncImage
import com.infinitestream.player.state.ContentItem
import com.infinitestream.player.state.ServerEnvironment
import com.infinitestream.player.ui.theme.AppType
import com.infinitestream.player.ui.theme.Radius
import com.infinitestream.player.ui.theme.Tokens
import com.infinitestream.player.ui.theme.tvFocus

/**
 * A small autoplay video card used on the Home screen.
 *
 * `active=true`  — owns an ExoPlayer hardwired to the 360p HLS rendition,
 *                  muted, looping. Hits the API port directly so the
 *                  per-session go-proxy isn't injecting failures.
 * `active=false` — static placeholder card, no codec, no network.
 *                  Used for off-window items in a sliding-decoder
 *                  carousel: only the 3 tiles around focus actually
 *                  decode, the rest render as cards.
 *
 * URL: `http://{host}:{apiPort}/go-live/{name}/playlist_6s_360p.m3u8`
 *
 * The 6 s segment variant keeps decoder workload low; the MTK c2.mtk.avc
 * decoder on the Google TV Streamer caps at 3 simultaneous instances,
 * which is why we gate the active-decoder window.
 */
@Composable
fun LivePreviewTile(
    content: ContentItem,
    server: ServerEnvironment,
    active: Boolean,
    onClick: (ContentItem) -> Unit,
    modifier: Modifier = Modifier,
    /** Called when the inner ExoPlayer is built — the tile is now
     *  holding a hardware decoder slot. The PlayerViewModel uses this
     *  signal to gate the main player's prepare() call so it doesn't
     *  race the chip's codec budget on Home → Playback navigation. */
    onAcquireDecoderLease: () -> Unit = {},
    /** Called from DisposableEffect.onDispose, after `player.release()`. */
    onReleaseDecoderLease: () -> Unit = {},
) {
    Box(
        modifier = modifier
            .size(width = 220.dp, height = 124.dp)
            .tvFocus(cornerRadius = Radius.card)
            .clip(RoundedCornerShape(Radius.card))
            .background(Tokens.bgSoft)
            .clickable { onClick(content) },
    ) {
        // Poster thumbnail underneath everything else. Renders for both
        // active and inactive tiles so the video has something to fade
        // in over while the first segment buffers (instead of a black
        // tile). Falls back to a flat fill when the server hasn't yet
        // generated a thumbnail.jpg for this clip.
        if (content.thumbnailPath != null) {
            AsyncImage(
                model = "${server.apiUrl}${content.thumbnailPath}",
                contentDescription = null,
                contentScale = androidx.compose.ui.layout.ContentScale.Crop,
                modifier = Modifier.fillMaxSize(),
            )
        } else {
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .background(Tokens.bgCard)
            )
        }
        if (active) {
            ActivePlayerSurface(
                content = content,
                server = server,
                onAcquireDecoderLease = onAcquireDecoderLease,
                onReleaseDecoderLease = onReleaseDecoderLease,
            )
        }

        // Bottom gradient + title overlay so the name stays legible against
        // any frame (or the placeholder fill).
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
            StatusDot(color = if (active) Tokens.live else Tokens.fgFaint)
            Spacer(Modifier.width(4.dp))
            Text(
                if (active) "LIVE" else "QUEUED",
                style = AppType.monoSm.copy(
                    color = if (active) Tokens.live else Tokens.fgDim
                ),
            )
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

/**
 * Inner Composable that actually instantiates an ExoPlayer + PlayerView.
 * Pulled out so the whole player lifecycle is scoped to the active branch
 * — entering/leaving active state runs proper enterComposition / dispose
 * which release the hardware decoder slot back to the chip's pool.
 */
@Composable
private fun ActivePlayerSurface(
    content: ContentItem,
    server: ServerEnvironment,
    onAcquireDecoderLease: () -> Unit,
    onReleaseDecoderLease: () -> Unit,
) {
    val context = LocalContext.current
    val player = remember(content.name, server.host, server.apiPort) {
        // Tile players use a RenderersFactory that builds *no* audio
        // renderers. Just disabling the audio track via TrackSelection
        // wasn't enough — go-live emits a sibling Opus audio playlist
        // (`playlist_6s_audio.m3u8`) and ExoPlayer's HLS source still
        // span up an audio decoder for it, which the user heard playing
        // on Home. With no audio renderer the audio track has nowhere
        // to go and is never decoded.
        val videoOnlyRenderers = object : DefaultRenderersFactory(context) {
            override fun buildAudioRenderers(
                context: android.content.Context,
                extensionRendererMode: Int,
                mediaCodecSelector: MediaCodecSelector,
                enableDecoderFallback: Boolean,
                audioSink: AudioSink,
                eventHandler: android.os.Handler,
                eventListener: AudioRendererEventListener,
                out: ArrayList<Renderer>,
            ) { /* nothing — silent previews */ }
        }
        ExoPlayer.Builder(context, videoOnlyRenderers).build().apply {
            volume = 0f
            repeatMode = Player.REPEAT_MODE_ONE
            // Cap to 360p in case go-live decides to serve a master playlist
            // somewhere — otherwise we accidentally pull 1080 p+ for a tile.
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
    // Publish the lease around the player's actual hardware-decoder
    // ownership window: acquired here when the Composable enters the
    // tree (the player has already been built by `remember`), released
    // *after* `player.release()` returns. The VM gates main playback
    // start on this count dropping to zero so we don't race the chip's
    // pool.
    DisposableEffect(player) {
        onAcquireDecoderLease()
        onDispose {
            player.release()
            onReleaseDecoderLease()
        }
    }
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
}
