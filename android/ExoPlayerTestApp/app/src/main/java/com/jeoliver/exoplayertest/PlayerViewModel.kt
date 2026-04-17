package com.jeoliver.exoplayertest

import android.content.Context
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.media3.common.*
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.hls.HlsMediaSource
import androidx.media3.exoplayer.source.MediaSource
import androidx.media3.datasource.DefaultHttpDataSource
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.util.UUID

@androidx.annotation.OptIn(androidx.media3.common.util.UnstableApi::class)
class PlayerViewModel : ViewModel() {

    private val _env = MutableStateFlow(ServerEnvironment.UBUNTU)
    val env = _env.asStateFlow()

    private val _contentList = MutableStateFlow<List<ContentItem>>(emptyList())
    val contentList = _contentList.asStateFlow()

    private val _currentContent = MutableStateFlow<String?>(null)
    val currentContent = _currentContent.asStateFlow()

    private val _playerState = MutableStateFlow("idle")
    val playerState = _playerState.asStateFlow()

    private val _statusMessage = MutableStateFlow("Select content to play")
    val statusMessage = _statusMessage.asStateFlow()

    val playerId: String = UUID.randomUUID().toString().take(8)

    var player: ExoPlayer? = null
        private set

    private var sessionId: String? = null
    private var metricsReporter: MetricsReporter? = null
    private var sessionResolver: SessionResolver? = null
    private var metricsJob: Job? = null
    private var stallCount = 0
    private var stallStartMs = 0L
    private var totalStallMs = 0L
    private var profileShiftCount = 0
    private var lastVideoBitrate = 0L
    private var restartCount = 0

    fun setEnvironment(env: ServerEnvironment) {
        _env.value = env
        stopPlayback()
        loadContent()
    }

    fun loadContent() {
        viewModelScope.launch {
            _statusMessage.value = "Loading content..."
            val items = fetchContent(_env.value.contentBaseUrl)
            _contentList.value = items
            _statusMessage.value = if (items.isEmpty()) "No content found" else "Select content to play"
        }
    }

    fun initPlayer(context: Context) {
        if (player != null) return
        player = ExoPlayer.Builder(context).build().apply {
            playWhenReady = true
            addListener(playerListener)
            trackSelectionParameters = trackSelectionParameters.buildUpon()
                .setMaxVideoSize(1280, 720)
                .build()
        }
    }

    fun playContent(
        contentName: String,
        protocol: ProtocolFilter = ProtocolFilter.HLS,
        segment: SegmentFilter = SegmentFilter.S2
    ) {
        val p = player ?: return
        stopMetrics()
        _currentContent.value = contentName
        stallCount = 0
        totalStallMs = 0
        stallStartMs = 0
        profileShiftCount = 0
        lastVideoBitrate = 0
        restartCount = 0

        val segmentSuffix = when (segment) {
            SegmentFilter.ALL -> "2s"
            SegmentFilter.S2 -> "2s"
            SegmentFilter.S6 -> "6s"
        }
        val masterFile = if (protocol == ProtocolFilter.DASH) "manifest_${segmentSuffix}.mpd" else "master_${segmentSuffix}.m3u8"
        val url = "${_env.value.playbackBaseUrl}/go-live/$contentName/$masterFile?player_id=$playerId"
        _statusMessage.value = "Playing $contentName ($masterFile)"

        val dataSourceFactory = DefaultHttpDataSource.Factory()
        if (protocol == ProtocolFilter.DASH) {
            val dashSource = androidx.media3.exoplayer.dash.DashMediaSource.Factory(dataSourceFactory)
                .createMediaSource(MediaItem.fromUri(url))
            p.setMediaSource(dashSource)
        } else {
            val hlsSource = HlsMediaSource.Factory(dataSourceFactory)
                .createMediaSource(MediaItem.fromUri(url))
            p.setMediaSource(hlsSource)
        }
        p.prepare()

        metricsReporter = MetricsReporter(_env.value.contentBaseUrl)
        sessionResolver?.stop()
        sessionResolver = SessionResolver(_env.value.contentBaseUrl, playerId) { sid ->
            sessionId = sid
        }
        sessionResolver?.start(viewModelScope)
        startMetrics()
    }

    fun stopPlayback() {
        stopMetrics()
        sessionResolver?.stop()
        sessionResolver = null
        sessionId = null
        player?.stop()
        player?.clearMediaItems()
        _currentContent.value = null
        _playerState.value = "idle"
        _statusMessage.value = "Select content to play"
    }

    private fun startMetrics() {
        metricsJob?.cancel()
        metricsJob = viewModelScope.launch {
            while (isActive) {
                delay(5000)
                sendMetrics("heartbeat")
            }
        }
    }

    private fun stopMetrics() {
        metricsJob?.cancel()
        metricsJob = null
    }

    private suspend fun sendMetrics(eventType: String) {
        val sid = sessionId ?: return
        val p = player ?: return
        val reporter = metricsReporter ?: return

        val positionMs: Long
        val bufferedMs: Long
        val videoFormat: Format?
        val counters: androidx.media3.exoplayer.DecoderCounters?
        val playbackState: Int
        val playWhenReady: Boolean
        val speed: Float

        withContext(Dispatchers.Main) {
            positionMs = p.currentPosition
            bufferedMs = p.bufferedPosition
            videoFormat = p.videoFormat
            counters = p.videoDecoderCounters
            playbackState = p.playbackState
            playWhenReady = p.playWhenReady
            speed = p.playbackParameters.speed
        }

        val state = when (playbackState) {
            Player.STATE_IDLE -> "idle"
            Player.STATE_BUFFERING -> "buffering"
            Player.STATE_READY -> if (playWhenReady) "playing" else "paused"
            Player.STATE_ENDED -> "ended"
            else -> "unknown"
        }

        val metrics = mapOf<String, Any?>(
            "player_metrics_source" to "android",
            "player_metrics_playback_engine" to "exoplayer",
            "player_metrics_last_event" to eventType,
            "player_metrics_trigger_type" to eventType,
            "player_metrics_last_event_at" to reporter.nowISO(),
            "player_metrics_event_time" to reporter.nowISO(),
            "player_metrics_state" to state,
            "player_metrics_position_s" to round3(positionMs / 1000.0),
            "player_metrics_playback_rate" to speed.toDouble(),
            "player_metrics_buffer_depth_s" to round3((bufferedMs - positionMs) / 1000.0),
            "player_metrics_buffer_end_s" to round3(bufferedMs / 1000.0),
            "player_metrics_video_bitrate_mbps" to videoFormat?.bitrate?.let { round3(it / 1_000_000.0) },
            "player_metrics_video_resolution" to videoFormat?.let { "${it.width}x${it.height}" },
            "player_metrics_network_bitrate_mbps" to null,
            "player_metrics_frames_displayed" to (counters?.renderedOutputBufferCount ?: 0),
            "player_metrics_dropped_frames" to (counters?.droppedBufferCount ?: 0),
            "player_metrics_total_video_frames" to ((counters?.renderedOutputBufferCount ?: 0) + (counters?.droppedBufferCount ?: 0)),
            "player_metrics_stall_count" to stallCount,
            "player_metrics_stall_time_s" to round3(totalStallMs / 1000.0),
            "player_metrics_profile_shift_count" to profileShiftCount,
            "player_metrics_loop_count_player" to 0,
            "player_restarts" to restartCount,
            "player_auto_recovery_enabled" to true
        )

        reporter.postMetrics(sid, metrics)
    }

    private val playerListener = object : Player.Listener {
        override fun onPlaybackStateChanged(state: Int) {
            val name = when (state) {
                Player.STATE_IDLE -> "idle"
                Player.STATE_BUFFERING -> {
                    if (stallStartMs == 0L) {
                        stallStartMs = System.currentTimeMillis()
                        stallCount++
                    }
                    "buffering"
                }
                Player.STATE_READY -> {
                    if (stallStartMs > 0) {
                        totalStallMs += System.currentTimeMillis() - stallStartMs
                        stallStartMs = 0
                    }
                    "playing"
                }
                Player.STATE_ENDED -> "ended"
                else -> "unknown"
            }
            _playerState.value = name
            viewModelScope.launch { sendMetrics("state_change") }
        }

        override fun onPlayerError(error: PlaybackException) {
            _statusMessage.value = "Error: ${error.message}"
            _playerState.value = "error"
            viewModelScope.launch {
                sendMetrics("error")
                delay(3000)
                val content = _currentContent.value
                if (content != null) {
                    restartCount++
                    playContent(content)
                }
            }
        }

        override fun onVideoSizeChanged(videoSize: VideoSize) {
            viewModelScope.launch { sendMetrics("video_size_change") }
        }

        override fun onTracksChanged(tracks: Tracks) {
            val videoTrack = tracks.groups.firstOrNull { it.type == C.TRACK_TYPE_VIDEO }
            val format = videoTrack?.getTrackFormat(0)
            val newBitrate = format?.bitrate?.toLong() ?: 0
            if (lastVideoBitrate > 0 && newBitrate > 0 && newBitrate != lastVideoBitrate) {
                profileShiftCount++
                viewModelScope.launch { sendMetrics("bitrate_change") }
            }
            lastVideoBitrate = newBitrate
        }
    }

    override fun onCleared() {
        stopMetrics()
        sessionResolver?.stop()
        player?.removeListener(playerListener)
        player?.release()
        player = null
    }

    private fun round3(v: Double) = Math.round(v * 1000.0) / 1000.0
}
