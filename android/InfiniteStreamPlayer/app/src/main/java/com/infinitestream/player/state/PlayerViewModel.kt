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
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull
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

    /**
     * Count of "decoder leases" currently held by tile / hero ExoPlayer
     * instances on Home (or any other surface that decodes video). Each
     * `ActivePlayerSurface` calls [acquireDecoderLease] when it builds its
     * ExoPlayer and [releaseDecoderLease] from `DisposableEffect.onDispose`.
     *
     * The main playback flow uses this signal so it doesn't allocate its
     * own decoder while the chip's hardware pool is still draining tile
     * leases — see [setSelectedContentDeferred].
     *
     * Exposed as a public StateFlow so the dev-mode HUD can render the
     * live count for debugging codec-budget issues.
     */
    private val _decoderLeases = MutableStateFlow(0)
    val decoderLeases: StateFlow<Int> = _decoderLeases.asStateFlow()

    fun acquireDecoderLease() { _decoderLeases.update { it + 1 } }
    fun releaseDecoderLease() {
        _decoderLeases.update { (it - 1).coerceAtLeast(0) }
    }

    /**
     * NO_MEMORY retry counter — incremented in [onPlayerError] when the
     * cause chain hits a `MediaCodec$CodecException` with NO_MEMORY,
     * reset to 0 on first-frame-rendered. Each retry waits with linear
     * backoff before reloading the same URL.
     */
    private var noMemoryRetries = 0
    private val maxNoMemoryRetries = 3

    init {
        loadServers()
        loadAdvancedFlags()
        attachPlayerListeners()
        applyTrackSelectionParameters()
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

    // -- Advanced flags (persisted alongside developer mode) -----------------

    private fun loadAdvancedFlags() {
        val p = prefs()
        _state.update {
            it.copy(
                developerMode = p.getBoolean(DEV_MODE_KEY, false),
                allow4K       = p.getBoolean(FLAG_4K, true),
                localProxy    = p.getBoolean(FLAG_LOCAL_PROXY, true),
                autoRecovery  = p.getBoolean(FLAG_AUTO_RECOVERY, false),
                goLive        = p.getBoolean(FLAG_GO_LIVE, false),
                lastPlayed    = p.getString(LAST_PLAYED_KEY, "") ?: "",
            )
        }
    }

    fun setDeveloperMode(on: Boolean) {
        _state.update { it.copy(developerMode = on) }
        prefs().edit().putBoolean(DEV_MODE_KEY, on).apply()
    }

    fun setAllow4K(on: Boolean) {
        _state.update { it.copy(allow4K = on) }
        prefs().edit().putBoolean(FLAG_4K, on).apply()
        applyTrackSelectionParameters()
    }

    fun setLocalProxy(on: Boolean) {
        _state.update { it.copy(localProxy = on) }
        prefs().edit().putBoolean(FLAG_LOCAL_PROXY, on).apply()
        // URL is built from current flags — rebuild + reload.
        buildUrlAndLoad()
    }

    fun setAutoRecovery(on: Boolean) {
        _state.update { it.copy(autoRecovery = on) }
        prefs().edit().putBoolean(FLAG_AUTO_RECOVERY, on).apply()
    }

    fun setGoLive(on: Boolean) {
        _state.update { it.copy(goLive = on) }
        prefs().edit().putBoolean(FLAG_GO_LIVE, on).apply()
    }

    /**
     * Push the current flag set into ExoPlayer's track selector. Today only
     * `allow4K` matters here — when off we cap to 1080 p so the chip's
     * decoder isn't asked to do 4K H.264 if the network would otherwise
     * pull the top rung of the ladder.
     */
    private fun applyTrackSelectionParameters() {
        val cap = if (_state.value.allow4K) Int.MAX_VALUE else 1080
        player.trackSelectionParameters = player.trackSelectionParameters.buildUpon()
            .setMaxVideoSize(if (_state.value.allow4K) Int.MAX_VALUE else 1920, cap)
            .build()
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

    /**
     * Set the selected content once it's safe to allocate a hardware
     * decoder for the main player. "Safe" is signalled by all currently-
     * held tile [decoderLeases] dropping to zero — meaning every Home
     * preview tile has released its ExoPlayer — plus a small fixed grace
     * (50 ms) for the OS-level Codec2 teardown that lags the synchronous
     * `player.release()` call.
     *
     * If for any reason leases never drop within `maxWaitMs` (a stuck
     * tile, an unmount race), proceed anyway — the NO_MEMORY-retry path
     * in [onPlayerError] will catch us if the decoder allocation actually
     * fails. Best-effort gating, not a hard barrier.
     *
     * Lives on viewModelScope so it survives the Home → Playback route
     * change — a previous version used the calling Composable's coroutine
     * scope and the await was cancelled when HomeScreen unmounted,
     * leaving the main player with no media item to play.
     */
    fun setSelectedContentDeferred(name: String, maxWaitMs: Long = 1000) {
        viewModelScope.launch {
            withTimeoutOrNull(maxWaitMs) {
                _decoderLeases.first { it == 0 }
            }
            kotlinx.coroutines.delay(50)
            setSelectedContent(name)
        }
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
                    val name = o.getString("name")
                    ContentItem(
                        name = name,
                        hasHls = o.optBoolean("has_hls", false),
                        hasDash = o.optBoolean("has_dash", false),
                        // clip_id / codec are emitted by go-upload's
                        // ContentInfo (server-computed). Old servers
                        // without those fields fall back to deriving
                        // a key from the name so the client still
                        // dedupes sensibly.
                        clipId = o.optString("clip_id", "").ifEmpty {
                            name.lowercase().replace(
                                Regex("_p200_(h264|hevc|h265|av1)(_|$)"), "$2"
                            ).trimEnd('_')
                        },
                        codec = o.optString("codec", "").lowercase(),
                        segmentDuration = if (o.isNull("segment_duration")) null
                                          else o.optInt("segment_duration", -1).takeIf { it >= 0 },
                        thumbnailPath = o.optString("thumbnail_url", "").ifEmpty { null },
                        thumbnailPathSmall = o.optString("thumbnail_url_small", "").ifEmpty { null },
                        thumbnailPathLarge = o.optString("thumbnail_url_large", "").ifEmpty { null },
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
        // Whether to actually re-load the player at the end. Only true when
        // we already had a stream loaded (user is on Playback or just came
        // from it). On a cold app launch / Home view, the content list
        // arrives, this filter runs, and we set a default selection — but
        // we DON'T silently start the main player and pull the master
        // playlist (which has audio). That bled audio into Home in the
        // background.
        val wasPlaying = s.currentUrl.isNotEmpty()
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
        if (wasPlaying) buildUrlAndLoad()
    }

    private fun buildUrlAndLoad() {
        val s = _state.value
        val server = s.activeServer ?: return
        if (s.selectedContent.isEmpty()) return
        val manifest = if (s.protocol == Protocol.HLS) "master${s.segment.suffix}.m3u8"
                       else "manifest${s.segment.suffix}.mpd"
        // Local Proxy ON → playback port (per-session go-proxy with failure
        // injection). OFF → API/main nginx port (no proxy in front of the
        // stream). Same /go-live route in both cases.
        val port = if (s.localProxy) server.port else server.apiPort
        val url = "http://${server.host}:$port/go-live/${s.selectedContent}/$manifest?player_id=$playerId"
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
        if (_state.value.goLive) {
            // Snap to live edge after the manifest finishes parsing — ExoPlayer
            // honours seekToDefaultPosition() once the period is known.
            player.seekToDefaultPosition()
        }
        metrics?.onPlaybackStarted()
    }

    /** Lightest reset: re-load the same stream URL without rebuilding. */
    /** Clear the "currently playing" URL marker. Called by MainActivity on
     *  every leave-Playback so applyContentFilter knows we're not actively
     *  playing and shouldn't reload. */
    fun clearCurrentUrl() { _state.update { it.copy(currentUrl = "") } }

    fun retry() { if (_state.value.currentUrl.isNotEmpty()) loadStream(_state.value.currentUrl) }

    /** Medium reset: stop the player, rebuild the URL from the current
     *  selection (picks up flag changes), and reload. */
    fun restart() {
        metrics?.onRestart("manual")
        player.stop(); player.clearMediaItems()
        buildUrlAndLoad()
    }

    /** Heaviest reset: re-fetch the content list from /api/content, then
     *  reload. Used to recover after the server has restarted or content
     *  has been added/removed mid-session. */
    fun reload() {
        metrics?.onRestart("reload")
        fetchContentList()
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
                metrics?.onPlayerError(error.message)
                // First: NO_MEMORY decoder-budget retry. The lease-counting
                // gate in setSelectedContentDeferred is the happy path, but
                // there are edge cases — a tile player that hasn't quite
                // finished disposing, the OS taking longer than 50 ms to
                // free the codec slot, etc. If prepare() trips the chip's
                // pool we retry up to maxNoMemoryRetries with linear
                // backoff. Reset noMemoryRetries on first-frame-rendered.
                if (isCodecNoMemory(error)
                    && noMemoryRetries < maxNoMemoryRetries
                    && _state.value.currentUrl.isNotEmpty()) {
                    noMemoryRetries++
                    val backoff = 150L * noMemoryRetries
                    _state.update {
                        it.copy(statusText = "Decoder busy — retry $noMemoryRetries/$maxNoMemoryRetries")
                    }
                    viewModelScope.launch {
                        kotlinx.coroutines.delay(backoff)
                        retry()
                    }
                    return
                }
                _state.update { it.copy(statusText = "Error: ${error.message}") }
                // Auto-recovery for non-decoder errors: if enabled, queue
                // a single retry. We don't loop — if retry also errors
                // the user sees the next error and decides what to do.
                if (_state.value.autoRecovery && _state.value.currentUrl.isNotEmpty()) {
                    viewModelScope.launch {
                        kotlinx.coroutines.delay(500)
                        retry()
                    }
                }
            }
            override fun onRenderedFirstFrame() {
                metrics?.onFirstFrameRendered()
                // Successful frame = the chip allocated a decoder for us,
                // so wipe the NO_MEMORY retry counter. Next time we hit
                // it (e.g. user navigates back to Home and forward again)
                // we get a fresh budget.
                noMemoryRetries = 0
                // First frame on screen = the stream actually started, so
                // mark this content as successfully played. Persisted via
                // SharedPreferences so the Continue Watching hero on Home
                // can resume it after navigation / app restart.
                val current = _state.value.selectedContent
                if (current.isNotEmpty()) {
                    prefs().edit().putString(LAST_PLAYED_KEY, current).apply()
                    _state.update { it.copy(lastPlayed = current) }
                }
            }
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

    /**
     * Walk a [PlaybackException] cause chain looking for evidence that the
     * underlying problem was the chip refusing a hardware decoder
     * allocation. Catches both the structured signal (MediaCodec's
     * CodecException with errno -12) and the diagnostic-string fallback
     * (some vendor codecs only set the message). Conservatively false-y
     * if the chain is empty or unrelated.
     */
    private fun isCodecNoMemory(error: Throwable): Boolean {
        var cause: Throwable? = error
        // -12 = ENOMEM in the Linux errno table; that's the value the
        // mtk codec returns when its decoder pool is exhausted.
        val noMemErrno = -12
        while (cause != null) {
            val current = cause
            if (current is android.media.MediaCodec.CodecException) {
                val info = (current.diagnosticInfo ?: "") + " " + (current.message ?: "")
                if ("NO_MEMORY" in info.uppercase()) return true
                // Some firmwares stash the errno in the message but not
                // the diagnostic string — also check errorCode if it's
                // ever a negative errno-style value.
                if (runCatching { current.errorCode }.getOrNull() == noMemErrno) return true
            }
            val msg = current.message ?: ""
            if ("NO_MEMORY" in msg.uppercase()) return true
            cause = current.cause
        }
        return false
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
        private const val FLAG_4K = "advanced_4k"
        private const val FLAG_LOCAL_PROXY = "advanced_local_proxy"
        private const val FLAG_AUTO_RECOVERY = "advanced_auto_recovery"
        private const val FLAG_GO_LIVE = "advanced_go_live"
        private const val LAST_PLAYED_KEY = "last_played_content"
    }
}
