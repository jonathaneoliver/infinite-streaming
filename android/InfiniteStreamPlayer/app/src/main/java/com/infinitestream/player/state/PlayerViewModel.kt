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

    /**
     * `play_id` (issue #280) — a UUID regenerated at every fresh
     * playback episode (loadStream / reload / retry / variant change).
     * Threaded through every URL the player issues as `?play_id=...`
     * so go-proxy can scope its NetworkLogEntry ring buffer per play.
     * HAR snapshots filter to the most-recent play_id by default.
     */
    private var currentPlayId: String = UUID.randomUUID().toString()

    private fun regeneratePlayId() {
        currentPlayId = UUID.randomUUID().toString()
    }

    /** Replace any existing `play_id` query param with `currentPlayId`. */
    private fun withPlayId(url: String): String {
        if (url.isEmpty()) return url
        val (base, query) = url.split("?", limit = 2).let {
            if (it.size == 2) it[0] to it[1] else it[0] to ""
        }
        val params = if (query.isEmpty()) mutableListOf() else query.split("&").toMutableList()
        params.removeAll { it.startsWith("play_id=") }
        params.add("play_id=$currentPlayId")
        return "$base?${params.joinToString("&")}"
    }

    // Mutable so [recreatePlayer] can drop and rebuild them when the
    // current ExoPlayer instance gets wedged in a state its own retry
    // logic can't escape. Always non-null after init.
    var bandwidthMeter: DefaultBandwidthMeter = DefaultBandwidthMeter.Builder(app).build()
        private set
    var player: ExoPlayer = ExoPlayer.Builder(app)
        .setBandwidthMeter(bandwidthMeter)
        .build()
        private set

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

    /**
     * True while the host Activity is in the STOPPED state (user pressed
     * Home, switched apps, etc.). Tile players observe this and tear
     * down their ExoPlayer + decoder when it flips true so we don't
     * keep video decoders allocated in the background. Flipping back
     * to false on Activity-resumed cues a re-prepare.
     */
    private val _appStopped = MutableStateFlow(false)
    val appStopped: StateFlow<Boolean> = _appStopped.asStateFlow()
    fun onActivityStopped() {
        _appStopped.value = true
        // Drop the main player's hardware decoder too — pause() alone
        // keeps the codec instance alive, which the user heard as
        // InfiniteStream audio bleeding into YouTube after homing out.
        player.stop()
        player.clearMediaItems()
    }
    fun onActivityStarted() { _appStopped.value = false }

    fun acquireDecoderLease() { _decoderLeases.update { it + 1 } }
    fun releaseDecoderLease() {
        _decoderLeases.update { (it - 1).coerceAtLeast(0) }
    }

    /**
     * Codec-recovery retry counter — incremented in [onPlayerError] when
     * the cause chain hits any `MediaCodec$CodecException` or one of
     * Media3's decoder-exception subclasses (init failure, runtime
     * decode failure). Reset to 0 on first-frame-rendered. Each retry
     * waits with linear backoff before reloading the same URL.
     *
     * Catches both:
     *   - NO_MEMORY decoder allocation failures (chip's hw pool
     *     exhausted while tiles drain).
     *   - Runtime decoder errors mid-playback (e.g. the AAC decoder
     *     reporting -14 on the first audio frame, which is usually
     *     transient and recovers on the next allocation).
     */
    private var codecRetries = 0
    private val maxCodecRetries = 3

    /** Process-elapsed-millis at VM construction so individual init steps,
     *  fetches and first-frame events can log offsets relative to "the
     *  moment the VM started waking up". Read in [tag] below. */
    private val tStart = android.os.SystemClock.uptimeMillis()
    fun tag(label: String) {
        android.util.Log.i("InfiniteStream",
            "T+${android.os.SystemClock.uptimeMillis() - tStart}ms $label")
    }

    init {
        tag("vm:init begin")
        val tA = android.os.SystemClock.uptimeMillis()
        loadServers()
        val tB = android.os.SystemClock.uptimeMillis()
        loadAdvancedFlags()
        val tC = android.os.SystemClock.uptimeMillis()
        attachPlayerListeners()
        val tD = android.os.SystemClock.uptimeMillis()
        applyTrackSelectionParameters()
        val tE = android.os.SystemClock.uptimeMillis()
        android.util.Log.i("InfiniteStream",
            "vm:init steps loadServers=${tB - tA}ms loadAdvancedFlags=${tC - tB}ms attachPlayerListeners=${tD - tC}ms applyTrackSelection=${tE - tD}ms total=${tE - tA}ms")
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

    /**
     * Wipe every persisted preference (server list, Advanced flags,
     * playback history, view counts) and reset in-memory state to
     * first-launch defaults. AppRoot reads `state.servers.isEmpty()` to
     * route back to ServerPicker after reset.
     *
     * Stops the player first so nothing keeps a reference to a
     * now-stale session.
     */
    fun resetAllSettings() {
        player.stop(); player.clearMediaItems()
        prefs().edit().clear().apply()
        // Reset in-memory state to declared defaults; loadAdvancedFlags
        // then re-reads the now-empty prefs to pick up each flag's
        // default-on-miss value.
        _state.update {
            it.copy(
                servers = emptyList(),
                activeServerIndex = 0,
                content = emptyList(),
                selectedContent = "",
                currentUrl = "",
                lastPlayed = "",
                viewCounts = emptyMap(),
                statusText = "",
            )
        }
        loadAdvancedFlags()
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
                allow4K           = p.getBoolean(FLAG_4K, true),
                localProxy        = p.getBoolean(FLAG_LOCAL_PROXY, true),
                autoRecovery      = p.getBoolean(FLAG_AUTO_RECOVERY, false),
                goLive            = p.getBoolean(FLAG_GO_LIVE, false),
                skipHomeOnLaunch  = p.getBoolean(FLAG_SKIP_HOME, false),
                previewVideoSlots = run {
                    // First launch (no key) → hardware default. Otherwise
                    // clamp the stored value to the device's current cap.
                    val hwCap = DecodeBudget.maxConcurrent
                    val stored = if (p.contains(FLAG_PREVIEW_VIDEO_SLOTS)) {
                        p.getInt(FLAG_PREVIEW_VIDEO_SLOTS, hwCap)
                    } else {
                        hwCap
                    }
                    stored.coerceIn(0, hwCap)
                },
                lastPlayed    = p.getString(LAST_PLAYED_KEY, "") ?: "",
                viewCounts    = readViewCounts(p),
            )
        }
    }

    private fun readViewCounts(p: SharedPreferences): Map<String, Int> {
        val raw = p.getString(VIEW_COUNTS_KEY, null) ?: return emptyMap()
        return try {
            val o = JSONObject(raw)
            buildMap {
                o.keys().forEach { k -> put(k, o.optInt(k, 0)) }
            }
        } catch (_: Exception) { emptyMap() }
    }

    private fun writeViewCounts(counts: Map<String, Int>) {
        val o = JSONObject()
        counts.forEach { (k, v) -> o.put(k, v) }
        prefs().edit().putString(VIEW_COUNTS_KEY, o.toString()).apply()
    }

    /**
     * Resolve a content name to its clip_id by looking it up in the
     * current catalogue. Falls back to a string-strip of `_p200_codec`
     * (case-insensitive) when the lookup misses — covers cold-start
     * before /api/content has returned.
     */
    private fun clipIdForName(name: String): String {
        val match = _state.value.content.firstOrNull { it.name == name }
        if (match != null) return match.clipId
        return name.lowercase().replace(
            Regex("_p200_(h264|hevc|h265|av1)(_\\d{8}_\\d{6})?$"), ""
        )
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

    fun setSkipHomeOnLaunch(on: Boolean) {
        _state.update { it.copy(skipHomeOnLaunch = on) }
        prefs().edit().putBoolean(FLAG_SKIP_HOME, on).apply()
    }

    fun setPreviewVideoSlots(value: Int) {
        val clamped = value.coerceIn(0, DecodeBudget.maxConcurrent)
        _state.update { it.copy(previewVideoSlots = clamped) }
        prefs().edit().putInt(FLAG_PREVIEW_VIDEO_SLOTS, clamped).apply()
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
        // Optimistic cache: paint whatever we saved on the last successful
        // fetch *immediately*, so the home page populates with no
        // perceived delay even when the device's Wi-Fi link is slow (the
        // 19 KB /api/content response was reliably 2 s on the Streamer's
        // wireless vs 9 ms over Ethernet — it's the link, not the server).
        val cached = readContentCache(server)
        if (cached.isNotEmpty()) {
            _state.update { it.copy(content = cached, statusText = "Loaded ${cached.size} items (cached)") }
            applyContentFilter()
        }
        _state.update { it.copy(statusText = "Loading content list…") }
        tag("fetchContentList:dispatch")
        viewModelScope.launch {
            val (list, err) = withContext(Dispatchers.IO) { fetchContent(server.apiUrl) }
            tag("fetchContentList:done items=${list.size} err=${err ?: "ok"}")
            if (err != null) {
                // Keep the cached list visible if the network fetch failed —
                // surfacing "no content" because of a flaky link is worse
                // than a slightly stale catalogue.
                _state.update { it.copy(statusText = "Refresh failed: $err") }
            } else {
                _state.update { it.copy(content = list, statusText = "Loaded ${list.size} items") }
                writeContentCache(server, list)
            }
            applyContentFilter()
        }
    }

    private fun readContentCache(server: ServerEnvironment): List<ContentItem> {
        val raw = prefs().getString(contentCacheKey(server), null) ?: return emptyList()
        return try {
            val arr = JSONArray(raw)
            (0 until arr.length()).map { i ->
                val o = arr.getJSONObject(i)
                ContentItem(
                    name = o.getString("name"),
                    hasHls = o.optBoolean("hasHls", false),
                    hasDash = o.optBoolean("hasDash", false),
                    clipId = o.optString("clipId", ""),
                    codec = o.optString("codec", ""),
                    segmentDuration = if (o.isNull("segmentDuration")) null
                                      else o.optInt("segmentDuration", -1).takeIf { it >= 0 },
                    thumbnailPath = o.optString("thumbnailPath", "").ifEmpty { null },
                    thumbnailPathSmall = o.optString("thumbnailPathSmall", "").ifEmpty { null },
                    thumbnailPathLarge = o.optString("thumbnailPathLarge", "").ifEmpty { null },
                )
            }
        } catch (_: Exception) { emptyList() }
    }

    private fun writeContentCache(server: ServerEnvironment, list: List<ContentItem>) {
        val arr = JSONArray()
        list.forEach { c ->
            arr.put(JSONObject().apply {
                put("name", c.name)
                put("hasHls", c.hasHls)
                put("hasDash", c.hasDash)
                put("clipId", c.clipId)
                put("codec", c.codec)
                if (c.segmentDuration != null) put("segmentDuration", c.segmentDuration)
                c.thumbnailPath?.let { put("thumbnailPath", it) }
                c.thumbnailPathSmall?.let { put("thumbnailPathSmall", it) }
                c.thumbnailPathLarge?.let { put("thumbnailPathLarge", it) }
            })
        }
        prefs().edit().putString(contentCacheKey(server), arr.toString()).apply()
    }

    /** Cache is per-server so switching servers doesn't show stale content
     *  from a different host. */
    private fun contentCacheKey(server: ServerEnvironment) =
        "$CONTENT_CACHE_PREFIX${server.host}:${server.apiPort}"

    private fun fetchContent(apiUrl: String): Pair<List<ContentItem>, String?> {
        var conn: HttpURLConnection? = null
        val tFetchStart = System.currentTimeMillis()
        return try {
            conn = (URL("$apiUrl/api/content").openConnection() as HttpURLConnection).apply {
                connectTimeout = 5000
                readTimeout = 5000
            }
            val tConnected = System.currentTimeMillis()
            if (conn.responseCode != 200) {
                emptyList<ContentItem>() to "HTTP ${conn.responseCode}"
            } else {
                val body = conn.inputStream.bufferedReader().use { it.readText() }
                val tBody = System.currentTimeMillis()
                android.util.Log.i("InfiniteStream",
                    "fetchContent: connect=${tConnected - tFetchStart}ms read=${tBody - tConnected}ms total=${tBody - tFetchStart}ms (${body.length}B from $apiUrl)")
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
        // Fresh play_id at every loadStream boundary (issue #280) so
        // go-proxy can scope its network log per play.
        regeneratePlayId()
        val url = "http://${server.host}:$port/go-live/${s.selectedContent}/$manifest?player_id=$playerId&play_id=$currentPlayId"
        _state.update { it.copy(currentUrl = url, statusText = url) }
        loadStream(url)
    }

    private fun loadStream(url: String) {
        if (url.isEmpty()) return
        // Tear down the previous source first. Without this, switching
        // streams via setMediaItem() can leave the old audio decoder
        // alive long enough that you briefly hear two audio tracks with
        // a small lag while the new prepare() spins up. Stop+clear is
        // cheap and gives ExoPlayer a clean slate.
        if (player.mediaItemCount > 0 || player.playbackState != Player.STATE_IDLE) {
            player.stop()
            player.clearMediaItems()
        }
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

    /** Clear the "currently playing" URL marker. Called by MainActivity on
     *  every leave-Playback so applyContentFilter knows we're not actively
     *  playing and shouldn't reload. */
    fun clearCurrentUrl() { _state.update { it.copy(currentUrl = "") } }

    /**
     * Manual trigger for the auto-recovery path. Same call the codec-error
     * handler and the autoRecovery branch make in onPlayerError: stop +
     * clear + re-prepare the *same* URL on the *same* ExoPlayer. Useful
     * when the player has stalled or surfaced an error and you want the
     * built-in retry kicked off without waiting for it.
     */
    fun retry() {
        val url = _state.value.currentUrl
        if (url.isEmpty()) return
        // A retry is a new playback episode — fresh play_id so the
        // proxy's network log scopes the next round of requests
        // separately from the one that just failed. Issue #280.
        regeneratePlayId()
        val refreshed = withPlayId(url)
        _state.update { it.copy(currentUrl = refreshed, statusText = refreshed) }
        // User-driven Retry deserves its own HAR — bypass per-player debounce.
        metrics?.requestHarSnapshot("user_retry", 0, /* force= */ true)
        player.stop()
        player.clearMediaItems()
        loadStream(refreshed)
    }

    /**
     * Full tear-down + recreate. Releases the existing ExoPlayer, builds
     * a fresh one with new BandwidthMeter, re-attaches listeners and
     * track-selection params, then issues the original go-proxy URL —
     * the one *before* the per-session 302 redirect — so go-proxy hands
     * out a fresh redirect target. This is the right button after a
     * server restart: any cached per-session port the player followed
     * to earlier may be dead, and a brand-new ExoPlayer + brand-new
     * GET to the proxy gets us back on a live route.
     *
     * Bumps `playerEpoch` so the PlayerView in PlaybackScreen remounts
     * and binds to the new ExoPlayer instance — without that the UI
     * would still hold a reference to the released player.
     *
     * Same player_id (proxy session continuity), no catalogue refetch.
     */
    /**
     * 911 button — marks "something interesting just happened" so the
     * server fires a HAR snapshot of the current session timeline. The
     * metrics POST also surfaces a marker on the dashboard's events
     * swim lane. Doesn't touch playback — purely a forensic capture.
     */
    fun mark911() {
        metrics?.onUserMarked()
    }

    fun reload() {
        metrics?.onRestart("reload")
        metrics?.release()
        metrics = null
        player.release()
        bandwidthMeter = DefaultBandwidthMeter.Builder(getApplication()).build()
        player = ExoPlayer.Builder(getApplication<Application>())
            .setBandwidthMeter(bandwidthMeter)
            .build()
        attachPlayerListeners()
        applyTrackSelectionParameters()
        _state.update { it.copy(currentUrl = "", playerEpoch = it.playerEpoch + 1) }
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
                metrics?.onPlayerError(error.message)
                // Retry on any MediaCodec decoder failure — covers both
                // NO_MEMORY init failures (lease-counting in
                // setSelectedContentDeferred is the happy path; this is
                // the safety net) and runtime decoder errors mid-playback
                // (e.g. the audio renderer reporting CodecException -14
                // on the first AAC frame, which is usually transient and
                // recovers on the next allocation). Bounded retries so a
                // genuinely broken stream doesn't loop forever.
                val classification = classifyCodecError(error)
                if (classification != null
                    && codecRetries < maxCodecRetries
                    && _state.value.currentUrl.isNotEmpty()) {
                    codecRetries++
                    val backoff = 150L * codecRetries
                    _state.update {
                        it.copy(statusText = "$classification — retry $codecRetries/$maxCodecRetries")
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
                tag("main player: first frame for '${_state.value.selectedContent}'")
                metrics?.onFirstFrameRendered()
                // Successful frame = the chip allocated decoders for us
                // and they're functioning, so wipe the codec retry
                // counter. Next time we hit a decoder fault we get a
                // fresh budget of attempts.
                codecRetries = 0
                // First frame on screen = the stream actually started, so
                // (a) mark this content as the lastPlayed (powers the
                // Continue Watching hero) and (b) bump its clip_id's
                // view count (powers the "frequently viewed" ordering of
                // the preview row). View counts are codec-agnostic — the
                // h264, hevc, and av1 encodings of one clip share a
                // tally so toggling codecs doesn't fragment history.
                val current = _state.value.selectedContent
                if (current.isNotEmpty()) {
                    val clipId = clipIdForName(current)
                    val newCounts = _state.value.viewCounts.toMutableMap()
                    newCounts[clipId] = (newCounts[clipId] ?: 0) + 1
                    prefs().edit().putString(LAST_PLAYED_KEY, current).apply()
                    writeViewCounts(newCounts)
                    _state.update { it.copy(lastPlayed = current, viewCounts = newCounts) }
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
     * Walk a [PlaybackException] cause chain to decide whether the
     * underlying problem is a transient codec fault we should retry.
     *
     * Returns a short user-facing tag (used in statusText) when the error
     * looks codec-related, or null if it's something else (network,
     * source format, server, etc.) — those follow the autoRecovery
     * path instead, so a stuck server doesn't get hammered.
     */
    private fun classifyCodecError(error: Throwable): String? {
        var cause: Throwable? = error
        // -12 = ENOMEM in the Linux errno table — what the MTK chip
        // returns when its decoder pool is exhausted on init.
        val noMemErrno = -12
        while (cause != null) {
            val current = cause
            if (current is android.media.MediaCodec.CodecException) {
                val info = (current.diagnosticInfo ?: "") + " " + (current.message ?: "")
                if ("NO_MEMORY" in info.uppercase()
                    || runCatching { current.errorCode }.getOrNull() == noMemErrno) {
                    return "Decoder busy"
                }
                return "Codec fault"
            }
            // Media3-specific subclasses — covers init-time and runtime
            // decoder failures alike. The class names live under the
            // mediacodec package; check via simple-name so we don't pin
            // the import (these are @UnstableApi).
            val cls = current.javaClass.simpleName
            if (cls == "DecoderInitializationException"
                || cls == "MediaCodecDecoderException") {
                return "Codec fault"
            }
            val msg = current.message ?: ""
            if ("NO_MEMORY" in msg.uppercase()) return "Decoder busy"
            cause = current.cause
        }
        return null
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
        private const val FLAG_SKIP_HOME = "advanced_skip_home_on_launch"
        private const val FLAG_PREVIEW_VIDEO_SLOTS = "advanced_preview_video_slots"
        private const val LAST_PLAYED_KEY = "last_played_content"
        private const val VIEW_COUNTS_KEY = "view_counts"
        private const val CONTENT_CACHE_PREFIX = "content_cache_"
    }
}
