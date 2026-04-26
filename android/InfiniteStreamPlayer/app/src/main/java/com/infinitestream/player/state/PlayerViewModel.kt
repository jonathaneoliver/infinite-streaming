package com.infinitestream.player.state

import android.app.Application
import android.content.Context
import android.content.SharedPreferences
import androidx.annotation.OptIn
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import androidx.media3.common.C
import androidx.media3.common.Format
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.DecoderReuseEvaluation
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.exoplayer.analytics.AnalyticsListener
import androidx.media3.exoplayer.source.LoadEventInfo
import androidx.media3.exoplayer.source.MediaLoadData
import androidx.media3.exoplayer.upstream.DefaultBandwidthMeter
import androidx.media3.exoplayer.video.VideoFrameMetadataListener
import androidx.media3.ui.PlayerView
import com.infinitestream.player.PlaybackMetrics
import com.infinitestream.player.RendezvousService
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.net.HttpURLConnection
import java.net.URL
import java.util.UUID

/**
 * Owns every persistent piece of UI state plus the ExoPlayer/metrics pipeline.
 * Screens are pure rendering surfaces — they read [state] and call methods on
 * this VM.
 *
 * Storage formats stay byte-compatible with the previous Java MainActivity
 * (SERVERS_PREFS + SERVERS_KEY/SERVERS_ACTIVE_KEY) so an upgrade-in-place
 * doesn't lose the user's server list.
 */
@OptIn(UnstableApi::class)
class PlayerViewModel(app: Application) : AndroidViewModel(app) {

    private val _state = MutableStateFlow(UiState())
    val state: StateFlow<UiState> = _state.asStateFlow()

    val playerId: String = UUID.randomUUID().toString()

    val bandwidthMeter: DefaultBandwidthMeter = DefaultBandwidthMeter.Builder(app).build()
    val player: ExoPlayer = ExoPlayer.Builder(app)
        .setBandwidthMeter(bandwidthMeter)
        .build()

    private var metrics: PlaybackMetrics? = null

    init {
        loadServers()
        loadDeveloperMode()
        attachPlayerListeners()
    }

    // -- Server list (SharedPreferences-backed) ------------------------------

    private fun prefs(): SharedPreferences =
        getApplication<Application>().getSharedPreferences(SERVERS_PREFS, Context.MODE_PRIVATE)

    private fun loadServers() {
        val list = mutableListOf<ServerEnvironment>()
        val json = prefs().getString(SERVERS_KEY, "") ?: ""
        if (json.isNotEmpty()) {
            try {
                val arr = JSONArray(json)
                for (i in 0 until arr.length()) {
                    val o = arr.getJSONObject(i)
                    list += ServerEnvironment(
                        name = o.optString("name", "Server $i"),
                        host = o.optString("host", ""),
                        port = o.optString("port", ""),
                        apiPort = o.optString("apiPort", ""),
                    )
                }
            } catch (_: Exception) { /* corrupt prefs — start fresh */ }
        }
        val active = prefs().getInt(SERVERS_ACTIVE_KEY, 0).coerceIn(0, (list.size - 1).coerceAtLeast(0))
        _state.update { it.copy(servers = list, activeServerIndex = active) }
    }

    private fun persistServers() {
        val arr = JSONArray()
        _state.value.servers.forEach { s ->
            arr.put(JSONObject().apply {
                put("name", s.name); put("host", s.host)
                put("port", s.port); put("apiPort", s.apiPort)
            })
        }
        prefs().edit().putString(SERVERS_KEY, arr.toString()).apply()
    }

    fun selectServer(index: Int) {
        if (index < 0 || index >= _state.value.servers.size) return
        _state.update { it.copy(activeServerIndex = index) }
        prefs().edit().putInt(SERVERS_ACTIVE_KEY, index).apply()
        fetchContentList()
    }

    fun forgetServer(index: Int) {
        val s = _state.value.servers
        if (index !in s.indices) return
        val updated = s.toMutableList().also { it.removeAt(index) }
        val newActive = _state.value.activeServerIndex
            .let { if (it >= updated.size) (updated.size - 1).coerceAtLeast(0) else it }
        _state.update { it.copy(servers = updated, activeServerIndex = newActive) }
        persistServers()
        prefs().edit().putInt(SERVERS_ACTIVE_KEY, newActive).apply()
        if (updated.isEmpty()) {
            _state.update { it.copy(content = emptyList(), selectedContent = "", currentUrl = "") }
            player.stop(); player.clearMediaItems()
        } else {
            fetchContentList()
        }
    }

    /** Returns the index of the (possibly newly-added) server, or -1. */
    fun addServerFromUrl(url: String): Int {
        val u = android.net.Uri.parse(url)
        val host = u.host ?: return -1
        val port = if (u.port >= 0) u.port else if (u.scheme.equals("https", true)) 443 else 80
        val apiPort = port.toString()
        // Convention: playback port = api port + 81 (matches iOS ServerProfile.fromDashboardURL).
        val playPort = (port + 81).toString()
        val name = "$host:$port"
        val list = _state.value.servers
        list.forEachIndexed { i, ex ->
            if (ex.host.equals(host, true) && ex.apiPort == apiPort) {
                selectServer(i); return i
            }
        }
        val updated = list + ServerEnvironment(name, host, playPort, apiPort)
        _state.update { it.copy(servers = updated, activeServerIndex = updated.size - 1) }
        persistServers()
        prefs().edit().putInt(SERVERS_ACTIVE_KEY, updated.size - 1).apply()
        fetchContentList()
        return updated.size - 1
    }

    // -- Discovery -----------------------------------------------------------

    fun discoverServers(onComplete: (List<RendezvousService.DiscoveredServer>) -> Unit) {
        _state.update { it.copy(discovering = true, discoveryError = null) }
        RendezvousService.discoverServers(getApplication()) { found, error ->
            _state.update { it.copy(discovering = false, discoveryError = error) }
            onComplete(found ?: emptyList())
        }
    }

    // -- Developer mode (flag in same prefs file) ----------------------------

    private fun loadDeveloperMode() {
        _state.update { it.copy(developerMode = prefs().getBoolean(DEV_MODE_KEY, false)) }
    }

    fun setDeveloperMode(on: Boolean) {
        _state.update { it.copy(developerMode = on) }
        prefs().edit().putBoolean(DEV_MODE_KEY, on).apply()
    }

    // -- Selection setters ---------------------------------------------------

    fun setProtocol(p: Protocol) {
        _state.update { it.copy(protocol = p) }
        applyContentFilter()
    }

    fun setSegment(s: Segment) {
        _state.update { it.copy(segment = s) }
        buildUrlAndLoad()
    }

    fun setCodec(c: Codec) {
        _state.update { it.copy(codec = c) }
        applyContentFilter()
    }

    fun setSelectedContent(name: String) {
        _state.update { it.copy(selectedContent = name) }
        buildUrlAndLoad()
    }

    fun setHudVisible(visible: Boolean) {
        _state.update { it.copy(hudVisible = visible) }
    }

    fun setSettingsOpen(open: Boolean) {
        _state.update { it.copy(settingsOpen = open) }
    }

    // -- Content fetch -------------------------------------------------------

    fun fetchContentList() {
        val server = _state.value.activeServer ?: run {
            _state.update { it.copy(content = emptyList(), statusText = "No server selected") }
            return
        }
        _state.update { it.copy(statusText = "Loading content list…") }
        viewModelScope.launch {
            val (list, err) = withContext(Dispatchers.IO) { fetchContent(server.apiUrl) }
            if (err != null) {
                _state.update { it.copy(statusText = "Fetch failed: $err", content = emptyList()) }
            } else {
                _state.update { it.copy(content = list, statusText = "Loaded ${list.size} items") }
            }
            applyContentFilter()
        }
    }

    private fun fetchContent(apiUrl: String): Pair<List<ContentItem>, String?> {
        var conn: HttpURLConnection? = null
        return try {
            conn = (URL("$apiUrl/api/content").openConnection() as HttpURLConnection).apply {
                connectTimeout = 5000; readTimeout = 5000
            }
            if (conn.responseCode != 200) {
                emptyList<ContentItem>() to "HTTP ${conn.responseCode}"
            } else {
                val body = conn.inputStream.bufferedReader().use { it.readText() }
                val arr = JSONArray(body)
                val items = (0 until arr.length()).map { i ->
                    val o = arr.getJSONObject(i)
                    ContentItem(
                        name = o.getString("name"),
                        hasHls = o.optBoolean("has_hls", false),
                        hasDash = o.optBoolean("has_dash", false),
                    )
                }
                items to null
            }
        } catch (e: Exception) {
            emptyList<ContentItem>() to "${e.javaClass.simpleName}: ${e.message}"
        } finally {
            conn?.disconnect()
        }
    }

    private fun applyContentFilter() {
        val s = _state.value
        val filtered = s.filteredContent
        if (filtered.isEmpty()) {
            _state.update { it.copy(selectedContent = "", currentUrl = "") }
            player.stop(); player.clearMediaItems()
            return
        }
        val pick = if (filtered.any { it.name == s.selectedContent }) s.selectedContent
                   else filtered.first().name
        if (pick != s.selectedContent) {
            _state.update { it.copy(selectedContent = pick) }
        }
        buildUrlAndLoad()
    }

    private fun buildUrlAndLoad() {
        val s = _state.value
        val server = s.activeServer ?: return
        if (s.selectedContent.isEmpty()) return
        val manifest = if (s.protocol == Protocol.HLS) "master${s.segment.suffix}.m3u8"
                       else "manifest${s.segment.suffix}.mpd"
        val url = "${server.baseUrl}/go-live/${s.selectedContent}/$manifest?player_id=$playerId"
        _state.update { it.copy(currentUrl = url, statusText = url) }
        loadStream(url)
    }

    private fun loadStream(url: String) {
        if (url.isEmpty()) return
        // Match iOS AVPlayer behaviour: let manifest's EXT-X-SERVER-CONTROL pick
        // the start point, narrow the playback-speed window so ExoPlayer recovers
        // via rate adjustment (not seeks) after a stall.
        val live = MediaItem.LiveConfiguration.Builder()
            .setTargetOffsetMs(C.TIME_UNSET)
            .setMinOffsetMs(C.TIME_UNSET)
            .setMaxOffsetMs(C.TIME_UNSET)
            .setMinPlaybackSpeed(0.97f)
            .setMaxPlaybackSpeed(1.03f)
            .build()
        val item = MediaItem.Builder().setUri(url).setLiveConfiguration(live).build()
        player.setMediaItem(item)
        player.prepare()
        player.playWhenReady = true
        metrics?.onPlaybackStarted()
    }

    fun retry() { if (_state.value.currentUrl.isNotEmpty()) loadStream(_state.value.currentUrl) }

    fun restart() {
        metrics?.onRestart("manual")
        player.stop(); player.clearMediaItems()
        buildUrlAndLoad()
    }

    // -- Metrics binding -----------------------------------------------------

    /**
     * Bound from the playback screen once the [PlayerView] is composed. We
     * re-create [PlaybackMetrics] each time because it captures the PlayerView
     * for display-resolution reads.
     */
    fun bindMetrics(view: PlayerView) {
        metrics?.release()
        metrics = PlaybackMetrics(
            player, view, bandwidthMeter, playerId,
            { _state.value.activeServer?.apiUrl ?: "" },
            { _state.value.currentUrl },
        ).also { it.start() }
    }

    fun unbindMetrics() {
        metrics?.release()
        metrics = null
    }

    private fun attachPlayerListeners() {
        player.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                _state.update { it.copy(statusText = "Error: ${error.message}") }
                metrics?.onPlayerError(error.message)
            }
            override fun onRenderedFirstFrame() { metrics?.onFirstFrameRendered() }
            override fun onIsPlayingChanged(isPlaying: Boolean) {
                if (isPlaying) metrics?.onStallEnd()
            }
            override fun onPlaybackStateChanged(state: Int) {
                if (state == Player.STATE_BUFFERING) {
                    metrics?.onBufferingStart()
                    if (player.playWhenReady && !player.isPlaying) metrics?.onStallStart()
                } else {
                    metrics?.onBufferingEnd()
                }
            }
            override fun onPositionDiscontinuity(
                old: Player.PositionInfo, new: Player.PositionInfo, reason: Int
            ) {
                val name = when (reason) {
                    Player.DISCONTINUITY_REASON_AUTO_TRANSITION -> "auto_transition"
                    Player.DISCONTINUITY_REASON_SEEK -> "seek"
                    Player.DISCONTINUITY_REASON_SEEK_ADJUSTMENT -> "seek_adjustment"
                    Player.DISCONTINUITY_REASON_SKIP -> "skip"
                    Player.DISCONTINUITY_REASON_REMOVE -> "remove"
                    Player.DISCONTINUITY_REASON_INTERNAL -> "internal"
                    else -> "unknown"
                }
                metrics?.onTimeJump(old.positionMs, new.positionMs, name)
            }
        })
        player.addAnalyticsListener(object : AnalyticsListener {
            override fun onDroppedVideoFrames(
                eventTime: AnalyticsListener.EventTime, droppedFrames: Int, elapsedMs: Long
            ) { metrics?.onDroppedFrames(droppedFrames) }

            override fun onVideoInputFormatChanged(
                eventTime: AnalyticsListener.EventTime, format: Format,
                decoderReuseEvaluation: DecoderReuseEvaluation?
            ) { metrics?.onVideoFormatChanged(format) }

            override fun onLoadCompleted(
                eventTime: AnalyticsListener.EventTime, loadEventInfo: LoadEventInfo,
                mediaLoadData: MediaLoadData
            ) {
                if (mediaLoadData.trackType == C.TRACK_TYPE_VIDEO) metrics?.onVideoLoadCompleted()
            }
        })
        player.setVideoFrameMetadataListener(VideoFrameMetadataListener { _, _, _, _ ->
            metrics?.onFrameRendered()
        })
    }

    override fun onCleared() {
        super.onCleared()
        metrics?.release()
        player.release()
    }

    companion object {
        private const val SERVERS_PREFS = "servers"
        private const val SERVERS_KEY = "list"
        private const val SERVERS_ACTIVE_KEY = "active_index"
        private const val DEV_MODE_KEY = "developer_mode"
    }
}
