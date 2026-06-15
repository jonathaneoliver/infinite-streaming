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

    // #714 config-on-connect: honor a harness-provided player_id (captured from
    // the launch intent extra by MainActivity) so the app inherits the
    // pre-configured proxy session; otherwise mint a fresh per-launch id.
    val playerId: String =
        com.infinitestream.player.LaunchConfig.playerId ?: UUID.randomUUID().toString()

    /**
     * `play_id` (issue #280) — a UUID regenerated at every fresh
     * playback episode (loadStream / reload / retry / variant change).
     * Threaded through every URL the player issues as `?play_id=...`
     * so go-proxy can scope its NetworkLogEntry ring buffer per play.
     * HAR snapshots filter to the most-recent play_id by default.
     */
    private var currentPlayId: String = UUID.randomUUID().toString()

    /**
     * `start_time` (#587) — client-supplied, play-scoped play start
     * (ISO-8601 UTC). Minted with `currentPlayId` and rotated at the SAME
     * boundaries; threaded through every URL as `?start_time=...` so the
     * play's start is play-scoped end-to-end (the server-derived
     * `started_at` is session-scoped and goes stale on a play rotation).
     */
    private var currentStartTime: String = java.time.Instant.now().toString()

    private fun regeneratePlayId() {
        currentPlayId = UUID.randomUUID().toString()
        // Rotate the play-scoped start with the play_id (#587).
        currentStartTime = java.time.Instant.now().toString()
        // Fresh play boundary — reset the per-play counters so the new play's
        // metrics start from zero. Every play_id rotation (content switch,
        // segment/filter swap, soak rotation, reload) funnels through here.
        // reload() additionally recreates the metrics instance; this makes the
        // reuse-the-instance boundaries consistent. retry() keeps play_id
        // stable and never calls this, so recovery attempts still preserve
        // counters via snapshotForRestart().
        metrics?.resetForFreshPlay()
    }

    /** Rotation Job armed after every successful loadStream and
     *  rescheduled when the user picks a new period (the new period is
     *  applied to the in-progress play via remaining-time arithmetic).
     *  Cancelled on `onCleared`. Issue #403. */
    private var playIdRotationJob: kotlinx.coroutines.Job? = null
    /** Wall-clock millis of when the current play_id minted. Drives the
     *  age-based rotation deadline. */
    private var playIdMintedAt: Long = 0L
    /** Wall-clock millis of the last "interesting" event (stall/error).
     *  Rate shifts are NOT counted — they happen routinely on healthy
     *  ABR streams and would block soak rotation indefinitely. */
    private var playIdLastActivityAt: Long = 0L
    private fun markPlayIdActivity() {
        playIdLastActivityAt = System.currentTimeMillis()
    }

    /** Replace any existing `play_id` + `start_time` query params with the
     *  current play's values (#587 — start_time travels with play_id). */
    private fun withPlayId(url: String): String {
        if (url.isEmpty()) return url
        val (base, query) = url.split("?", limit = 2).let {
            if (it.size == 2) it[0] to it[1] else it[0] to ""
        }
        val params = if (query.isEmpty()) mutableListOf() else query.split("&").toMutableList()
        params.removeAll { it.startsWith("play_id=") }
        params.removeAll { it.startsWith("start_time=") }
        params.add("play_id=$currentPlayId")
        params.add("start_time=$currentStartTime")
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

    /** Read-only view of the metrics pipeline for the on-device DiagnosticHud. */
    val metricsRef: PlaybackMetrics? get() = metrics

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

    /** Wired from MainActivity's BackHandler when leaving the Playback
     *  route. Picks user_stopped vs abandoned_start based on first-frame
     *  + elapsed-since-play-start (EBVS). Forwards to PlaybackMetrics
     *  which classifies ended_buffering / ended_stalling refinement
     *  client-side before emitting play_end. */
    fun endSessionForUserBack() {
        metrics?.endSessionForUserBack()
    }

    /** App-terminated path — fired from MainActivity's onDestroy /
     *  the process-lifecycle owner. Best-effort: if the OS reaps us
     *  before the POST completes, the row stays in_progress. */
    fun endSessionAsAppTerminated() {
        metrics?.endSession("user_stopped", "app_terminated")
    }

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
                        scheme = o.optString("scheme", "https"),
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
                put("scheme", s.scheme)
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
        // Re-push the (now-default) flags into the track selector so a cleared
        // peak-bitrate cap / 4K setting actually takes effect (#797).
        applyTrackSelectionParameters()
    }

    /** Returns the index of the (possibly newly-added) server, or -1. */
    fun addServerFromUrl(url: String): Int {
        val u = android.net.Uri.parse(url)
        val host = u.host ?: return -1
        val scheme = u.scheme?.lowercase() ?: "https"
        val port = if (u.port >= 0) u.port else if (scheme == "https") 443 else 80
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
        val updated = list + ServerEnvironment(name, host, playPort, apiPort, scheme)
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
                // #797 P2: `is.flag.4k` / `is.flag.go_live` launch overrides
                // outrank the persisted value for this launch (not written back).
                allow4K           = com.infinitestream.player.LaunchConfig.allow4K
                    ?: p.getBoolean(FLAG_4K, true),
                localProxy        = p.getBoolean(FLAG_LOCAL_PROXY, true),
                autoRecovery      = p.getBoolean(FLAG_AUTO_RECOVERY, false),
                goLive            = com.infinitestream.player.LaunchConfig.goLive
                    ?: p.getBoolean(FLAG_GO_LIVE, false),
                skipHomeOnLaunch  = p.getBoolean(FLAG_SKIP_HOME, false),
                // A launch-provided override (the `is.flag.live_offset_s` intent
                // extra captured by MainActivity) wins over the persisted value,
                // matching iOS where NSArgumentDomain outranks the saved default.
                // It is NOT persisted — like an NSArgumentDomain arg it lives
                // only for this launch, so a manual change in Settings later
                // still writes through normally.
                liveOffsetSeconds = (com.infinitestream.player.LaunchConfig.liveOffsetSeconds
                    ?: p.getInt(FLAG_LIVE_OFFSET, 0)).coerceAtLeast(0),
                // #797 characterization launch levers. segment/protocol are not
                // persisted on Android (UI-only state), so a launch override
                // just seeds the initial value for this launch; absent one the
                // current default stands. peak_bitrate_mbps IS persisted (iOS
                // parity) — a launch override outranks the stored value and is
                // not written back. 0 Mbps = no ABR ceiling.
                segment  = com.infinitestream.player.LaunchConfig.segment ?: it.segment,
                protocol = com.infinitestream.player.LaunchConfig.streamProtocol ?: it.protocol,
                peakBitrateMbps = (com.infinitestream.player.LaunchConfig.peakBitrateMbps
                    ?: p.getInt(FLAG_PEAK_BITRATE, 0)).coerceAtLeast(0),
                // #797 P2: codec is UI-only state (not persisted), so a launch
                // override just seeds it for this launch. starts_first_variant
                // IS persisted (iOS parity); a launch override outranks it.
                codec = com.infinitestream.player.LaunchConfig.codec ?: it.codec,
                startsFirstVariant = com.infinitestream.player.LaunchConfig.startsFirstVariant
                    ?: p.getBoolean(FLAG_STARTS_FIRST_VARIANT, false),
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
                // #797 P2: `is.lastPlayed` (the harness's content-selection arg;
                // `is.content` is an alias) pins which clip the hero / auto-resume
                // targets. Not persisted from launch — first-frame still writes
                // the real lastPlayed through normally.
                lastPlayed    = com.infinitestream.player.LaunchConfig.lastPlayed
                    ?: (p.getString(LAST_PLAYED_KEY, "") ?: ""),
                viewCounts    = readViewCounts(p),
                playIdRotationSeconds = (com.infinitestream.player.LaunchConfig.playIdRotationSeconds
                    ?: p.getInt(FLAG_PLAY_ID_ROTATION, 0)).coerceAtLeast(0),
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

    /** #797 starts_first_variant — pin the startup rung to the lowest variant,
     *  releasing to free ABR once the first frame renders. The analog of iOS
     *  `AVPlayerItem.startsOnFirstEligibleVariant`. Takes effect on the next
     *  (re)load; re-applies the selector now so a toggle while paused-idle is
     *  consistent. Persisted alongside the other Advanced flags. */
    fun setStartsFirstVariant(on: Boolean) {
        _state.update { it.copy(startsFirstVariant = on) }
        prefs().edit().putBoolean(FLAG_STARTS_FIRST_VARIANT, on).apply()
        applyTrackSelectionParameters()
    }

    fun setSkipHomeOnLaunch(on: Boolean) {
        _state.update { it.copy(skipHomeOnLaunch = on) }
        prefs().edit().putBoolean(FLAG_SKIP_HOME, on).apply()
    }

    /** User-driven setter for the live-edge offset (seconds). 0 disables
     *  (manifest HOLD-BACK / Go Live decides). The offset is baked into the
     *  MediaItem's LiveConfiguration in [loadStream], so apply a change to an
     *  in-progress play by rebuilding + reloading the stream — mirrors how
     *  [setLocalProxy] reloads, and gives the same immediate effect as iOS's
     *  live re-seek. Issue #266 / #793. */
    fun setLiveOffsetSeconds(seconds: Int) {
        val clamped = seconds.coerceAtLeast(0)
        _state.update { it.copy(liveOffsetSeconds = clamped) }
        prefs().edit().putInt(FLAG_LIVE_OFFSET, clamped).apply()
        if (_state.value.currentUrl.isNotEmpty()) buildUrlAndLoad()
    }

    fun setPreviewVideoSlots(value: Int) {
        val clamped = value.coerceIn(0, DecodeBudget.maxConcurrent)
        _state.update { it.copy(previewVideoSlots = clamped) }
        prefs().edit().putInt(FLAG_PREVIEW_VIDEO_SLOTS, clamped).apply()
    }

    /** #797 ABR peak-bitrate ceiling (Mbps; 0 = no cap). Maps to ExoPlayer's
     *  track selector `setMaxVideoBitrate` — the analog of iOS
     *  `AVPlayerItem.preferredPeakBitRate`. The selector parameter is mutable
     *  mid-play, so re-applying it takes effect on the next ABR decision
     *  without a reload. Persisted like the other Advanced flags. */
    fun setPeakBitrateMbps(mbps: Int) {
        val clamped = mbps.coerceAtLeast(0)
        _state.update { it.copy(peakBitrateMbps = clamped) }
        prefs().edit().putInt(FLAG_PEAK_BITRATE, clamped).apply()
        applyTrackSelectionParameters()
    }

    /** #797 starts_first_variant: latched false at the start of every play,
     *  set true once the first frame renders. While the flag is on and this
     *  is still false, the track selector is pinned to the lowest rung. */
    private var startupVariantLockReleased = false

    /**
     * Push the current flag set into ExoPlayer's track selector. `allow4K`
     * caps the resolution (1080 p when off, so the chip isn't asked to decode
     * 4K). `peakBitrateMbps` caps the bitrate of the rung ABR may select
     * (#797) — the analog of iOS `preferredPeakBitRate`; 0 leaves it
     * uncapped. `startsFirstVariant` forces the lowest rung until the first
     * frame is up (the #797 analog of iOS `startsOnFirstEligibleVariant`),
     * after which [onRenderedFirstFrame] releases the lock and ABR adapts.
     */
    private fun applyTrackSelectionParameters() {
        val cap = if (_state.value.allow4K) Int.MAX_VALUE else 1080
        // Mbps → bps. 0 (or anything that would overflow Int) = no cap.
        val peakMbps = _state.value.peakBitrateMbps
        val peakBps = if (peakMbps in 1..2000) peakMbps * 1_000_000 else Int.MAX_VALUE
        val forceLowest = _state.value.startsFirstVariant && !startupVariantLockReleased
        player.trackSelectionParameters = player.trackSelectionParameters.buildUpon()
            .setMaxVideoSize(if (_state.value.allow4K) Int.MAX_VALUE else 1920, cap)
            .setMaxVideoBitrate(peakBps)
            .setForceLowestBitrate(forceLowest)
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
                    segmentDurations = o.optJSONArray("segmentDurations")?.let { ja ->
                        (0 until ja.length()).map { ja.getInt(it) }
                    },
                    hasLL = if (o.has("hasLL")) o.optBoolean("hasLL") else null,
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
                c.segmentDurations?.let { put("segmentDurations", JSONArray(it)) }
                c.hasLL?.let { put("hasLL", it) }
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
                        segmentDurations = o.optJSONArray("segment_durations")?.let { ja ->
                            (0 until ja.length()).map { ja.getInt(it) }
                        },
                        hasLL = if (o.has("has_ll")) o.optBoolean("has_ll") else null,
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

    private fun buildUrlAndLoad(rotatePlayId: Boolean = true) {
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
        // go-proxy can scope its network log per play. reload() rotates the
        // id itself (so play_start carries the new id) and passes false here
        // to avoid minting a second id the play_start row wouldn't match.
        if (rotatePlayId) regeneratePlayId()
        // Anchor the age clock for the soak-rotation timer. Every
        // fresh loadStream resets the boundary; the Job is rescheduled
        // below once the player has been handed off.
        playIdMintedAt = System.currentTimeMillis()
        playIdLastActivityAt = 0L
        var url = "${server.scheme}://${server.host}:$port/go-live/${s.selectedContent}/$manifest?player_id=$playerId&play_id=$currentPlayId&start_time=$currentStartTime"
        // #714 Approach B (config-on-connect driven by the player): append a
        // launch-provided raw proxy.* query fragment so the proxy materializes
        // the session config on THIS bootstrap request — no pre-flight curl.
        // Mirrors iOS Models.playbackURL; the 302 strips proxy.* for children.
        com.infinitestream.player.LaunchConfig.proxyQuery?.let { pq ->
            if (pq.isNotEmpty()) url += "&$pq"
        }
        _state.update { it.copy(currentUrl = url, statusText = url) }
        loadStream(url)
        schedulePlayIdRotation()
    }

    /** Cancel any pending rotation Job and (if the setting is non-zero)
     *  arm a fresh one for the *remaining* time relative to
     *  `playIdMintedAt`. Issue #403. */
    private fun schedulePlayIdRotation() {
        playIdRotationJob?.cancel()
        playIdRotationJob = null
        val target = _state.value.playIdRotationSeconds
        if (target <= 0) return
        val elapsedMs = System.currentTimeMillis() - playIdMintedAt
        val remainingMs = (target * 1000L - elapsedMs).coerceAtLeast(0L)
        val quiescenceMs = 60_000L
        playIdRotationJob = viewModelScope.launch {
            if (remainingMs > 0) kotlinx.coroutines.delay(remainingMs)
            while (true) {
                if (playIdLastActivityAt == 0L) break
                val sinceMs = System.currentTimeMillis() - playIdLastActivityAt
                if (sinceMs >= quiescenceMs) break
                kotlinx.coroutines.delay(quiescenceMs - sinceMs)
            }
            val ageS = (System.currentTimeMillis() - playIdMintedAt) / 1000
            android.util.Log.i("InfiniteStream",
                "[PLAY_ID] rotating after ${ageS}s (target ${target}s)")
            buildUrlAndLoad()
        }
    }

    /** User-driven setter for the soak-rotation period. 0 disables.
     *  Reschedules using the *remaining* time since the current play_id
     *  minted, so a setting change applies to the in-progress play. If
     *  the new period is shorter than the elapsed age, the rescheduled
     *  Job fires immediately. Issue #403. */
    fun setPlayIdRotationSeconds(seconds: Int) {
        val clamped = seconds.coerceAtLeast(0)
        _state.update { it.copy(playIdRotationSeconds = clamped) }
        prefs().edit().putInt(FLAG_PLAY_ID_ROTATION, clamped).apply()
        if (_state.value.currentUrl.isNotEmpty()) schedulePlayIdRotation()
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
        // #797 starts_first_variant: re-arm the startup low-rung lock for this
        // play (released again at first frame). No-op when the flag is off.
        startupVariantLockReleased = false
        applyTrackSelectionParameters()
        // Live-edge offset policy (issues #266 / #793):
        //  - Go Live ON      → snap to the edge (seekToDefaultPosition below);
        //                      leave the offset UNSET so the manifest decides
        //                      the window and Go Live takes precedence — same
        //                      ordering as iOS (goLive beats liveOffsetSeconds).
        //  - offset > 0       → pin target/min/max to that offset so ABR
        //                      rate-adjustment holds it rather than drifting,
        //                      overriding the manifest's HOLD-BACK.
        //  - otherwise        → UNSET: let manifest's EXT-X-SERVER-CONTROL pick
        //                      the start point (default behaviour).
        // The narrow 0.97–1.03 speed window lets ExoPlayer recover toward the
        // target via rate adjustment (not seeks) after a stall in every case.
        val offsetMs = if (!_state.value.goLive && _state.value.liveOffsetSeconds > 0)
            _state.value.liveOffsetSeconds * 1000L
        else
            C.TIME_UNSET
        val live = MediaItem.LiveConfiguration.Builder()
            .setTargetOffsetMs(offsetMs)
            .setMinOffsetMs(offsetMs)
            .setMaxOffsetMs(offsetMs)
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
        } else if (_state.value.liveOffsetSeconds > 0) {
            // Pinning the LiveConfiguration target alone is NOT enough: ExoPlayer
            // joins at the manifest's EXT-X-START / HOLD-BACK position (~21s for
            // 6s segments) and then converges to the target ONLY through the
            // narrow 0.97–1.03 speed window — ~0.03 s/s, so a 40s target takes
            // ~10 min to reach. Seek straight to liveEdge − offset (iOS parity
            // with scheduleLiveOffsetSeek); the pinned target/min/max then holds
            // it there. Issue #266 / #793.
            scheduleLiveOffsetSeek("playback started")
        }
        metrics?.onPlaybackStarted()
    }

    /** One-shot job that jumps the playhead to `liveEdge − liveOffsetSeconds`
     *  once ExoPlayer reports a valid live offset, then lets the pinned
     *  LiveConfiguration (target=min=max) hold it. Polls every 250 ms for up
     *  to 20 s while the live window forms. No-op when the offset is 0 or Go
     *  Live is on. Mirrors iOS `scheduleLiveOffsetSeek`. */
    private var liveOffsetSeekJob: kotlinx.coroutines.Job? = null
    private fun scheduleLiveOffsetSeek(reason: String) {
        liveOffsetSeekJob?.cancel()
        liveOffsetSeekJob = null
        val targetSeconds = _state.value.liveOffsetSeconds
        if (targetSeconds <= 0 || _state.value.goLive) return
        val targetMs = targetSeconds * 1000L
        liveOffsetSeekJob = viewModelScope.launch {
            val deadline = System.currentTimeMillis() + 20_000L
            while (System.currentTimeMillis() < deadline) {
                val current = player.currentLiveOffset
                if (player.isCurrentMediaItemLive && current != C.TIME_UNSET && current > 0) {
                    // delta > 0 → sit further back (seek earlier); < 0 → closer
                    // to live (seek later). Only act when we're meaningfully off
                    // target so we don't fight the speed controller near the mark.
                    val delta = targetMs - current
                    if (kotlin.math.abs(delta) > 1000L) {
                        val seekTo = (player.currentPosition - delta).coerceAtLeast(0L)
                        player.seekTo(seekTo)
                        android.util.Log.i("InfiniteStream",
                            "LIVE OFFSET: seek to ${targetSeconds}s behind live " +
                                "(was ${current / 1000.0}s, $reason)")
                    }
                    return@launch
                }
                kotlinx.coroutines.delay(250)
            }
            android.util.Log.i("InfiniteStream",
                "LIVE OFFSET: gave up after 20s (live offset not ready)")
        }
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
    fun retry(reason: String = "user_retry") {
        val url = _state.value.currentUrl
        if (url.isEmpty()) return
        // #550 retry contract — recovery attempt WITHIN the same play.
        // Mirrors iOS PlayerViewModel.retry():
        //   - play_id stays stable (do NOT rotate)
        //   - residency + variant-dwell counters preserved across the
        //     player_item replacement via metrics.snapshotForRestart()
        //   - the next resetResidency() (inside loadStream's
        //     onPlaybackStarted) restores from those priors rather
        //     than zeroing
        // Reload (separate UI action) rotates play_id + clears priors;
        // the proxy's network log scopes the new round of requests
        // accordingly.
        metrics?.snapshotForRestart()
        // #603 — emit a `restart` event (reason=user_retry) so the mid-play
        // recovery is observable; onRestart also fires the user-driven HAR.
        // markRestartPending() makes onPlaybackStarted preserve the play's
        // video_start_time + fold the re-prepare wait into residency.
        metrics?.onRestart(reason)
        metrics?.markRestartPending()
        player.stop()
        player.clearMediaItems()
        loadStream(url)
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
        // #566 — terminate the OUTGOING play first so it gets an outcome
        // row + QoE labels instead of dangling `in_progress`. sendEvent
        // snapshots the payload synchronously, so the play_end row carries
        // the OLD play_id + final state even though the POST is async; the
        // play_id only rotates on the regeneratePlayId() below. status=
        // user_stopped, reason=reloaded (distinct from a back-tap's
        // user_quit).
        metrics?.endSession("user_stopped", "reloaded")
        // #603 — rotate to the NEW play_id BEFORE emitting play_start so the
        // play-open boundary carries the new play's id (iOS parity). buildUrlAndLoad
        // below is told NOT to rotate again so the stream URL uses this same id.
        // regeneratePlayId() also zeroes the per-play accumulators (variant dwell
        // etc.) on the still-bound metrics instance — the new PlaybackMetrics built
        // in bindMetrics() starts empty anyway, but this guards a cached reference.
        regeneratePlayId()
        playIdMintedAt = System.currentTimeMillis()
        playIdLastActivityAt = 0L
        // #603 — a reload opens a NEW play, so emit play_start (the play-open
        // boundary, symmetric to play_end), NOT restart. `restart` is reserved
        // for mid-play recovery (retry / auto-recovery). Emitted on the still-live
        // metrics instance whose payload reads currentPlayId live → carries the
        // new id just minted above, with zeroed residency.
        metrics?.onPlayStart()
        metrics?.release()
        metrics = null
        boundPlayerView = null
        player.release()
        bandwidthMeter = DefaultBandwidthMeter.Builder(getApplication()).build()
        player = ExoPlayer.Builder(getApplication<Application>())
            .setBandwidthMeter(bandwidthMeter)
            .build()
        attachPlayerListeners()
        applyTrackSelectionParameters()
        _state.update { it.copy(currentUrl = "", playerEpoch = it.playerEpoch + 1) }
        buildUrlAndLoad(rotatePlayId = false)
    }

    // -- Metrics binding -----------------------------------------------------

    /**
     * Bound from the playback screen once the [PlayerView] is composed.
     *
     * IMPORTANT: AndroidView.update fires on every Compose recomposition,
     * so this gets called many times per second during normal interaction.
     * It MUST be idempotent — recreating PlaybackMetrics on every call
     * zeroes the residency / variant-dwell / counters mid-play, which the
     * dashboard sees as Playing Time / Pausing Time inexplicably resetting.
     *
     * We compare PlayerView identity to detect a genuinely-new view (e.g.
     * after vm.reload() rebuilds the player) and only recreate then.
     */
    fun bindMetrics(view: PlayerView) {
        if (metrics != null && boundPlayerView === view) return
        metrics?.release()
        boundPlayerView = view
        metrics = PlaybackMetrics(
            player, view, bandwidthMeter, playerId,
            { _state.value.activeServer?.apiUrl ?: "" },
            { _state.value.currentUrl },
            // Read selectedContent LIVE per emit (urlProvider pattern)
            // so a late-arriving content pick lands on the next heartbeat
            // instead of staying empty if bindMetrics fired before
            // selectedContent was set.
            { _state.value.selectedContent },
            // #603 — pin play-scoped ids onto metrics POST URLs (iOS parity).
            // Read live per emit; PlaybackMetrics captures them synchronously in
            // buildPayload at fire time, so a play_end at a reload boundary keeps
            // the OLD play_id even though the POST is async + play_id later rotates.
            object : PlaybackMetrics.PlayContextProvider {
                override fun currentPlayId() = currentPlayId
                override fun currentStartTime() = currentStartTime
            },
        ).also { it.start() }
    }

    /** Cached PlayerView reference used by bindMetrics() to short-circuit
     *  no-op rebinds during Compose recompositions. Reset to null on
     *  unbindMetrics + reload so the next bind genuinely creates fresh. */
    private var boundPlayerView: PlayerView? = null

    fun unbindMetrics() {
        metrics?.release()
        metrics = null
        boundPlayerView = null
    }

    private fun attachPlayerListeners() {
        player.addListener(object : Player.Listener {
            override fun onPlayerError(error: PlaybackException) {
                metrics?.onPlayerError(error)
                markPlayIdActivity()
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
                        retry("auto_recovery")
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
                        retry("auto_recovery")
                    }
                    return
                }
                // No auto-recovery → this error is terminal. Phase 2:
                // metrics emits a play_end with start_failure (no
                // first frame yet) or mid_stream_failure (post-first-
                // frame), stamping playback_status into CH.
                metrics?.markFatalTerminal(error.message ?: "")
            }
            override fun onRenderedFirstFrame() {
                tag("main player: first frame for '${_state.value.selectedContent}'")
                metrics?.onFirstFrameRendered()
                // #797 starts_first_variant: first frame is up on the startup
                // (lowest) rung; release the lock so ABR can adapt upward for
                // the rest of the play. Mirrors iOS startsOnFirstEligibleVariant.
                if (_state.value.startsFirstVariant && !startupVariantLockReleased) {
                    startupVariantLockReleased = true
                    applyTrackSelectionParameters()
                    tag("starts_first_variant: released startup low-rung lock")
                }
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
                    markPlayIdActivity()
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
            override fun onMediaItemTransition(item: MediaItem?, reason: Int) {
                // Source looped — fire loop counter increment for the
                // player_metrics_loop_count_player payload field.
                if (reason == Player.MEDIA_ITEM_TRANSITION_REASON_REPEAT
                    || reason == Player.MEDIA_ITEM_TRANSITION_REASON_AUTO) {
                    metrics?.onLoop()
                }
            }
        })
        player.addAnalyticsListener(object : AnalyticsListener {
            override fun onDroppedVideoFrames(
                eventTime: AnalyticsListener.EventTime, droppedFrames: Int, elapsedMs: Long
            ) { metrics?.onFramesDropped(droppedFrames) }

            override fun onVideoInputFormatChanged(
                eventTime: AnalyticsListener.EventTime, format: Format,
                decoderReuseEvaluation: DecoderReuseEvaluation?
            ) {
                // Note: rate shifts are intentionally NOT marked as
                // play_id activity — issue #403.
                metrics?.onVideoFormatChanged(format)
            }

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
        playIdRotationJob?.cancel()
        liveOffsetSeekJob?.cancel()
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
        private const val FLAG_LIVE_OFFSET = "advanced_live_offset_s"
        private const val FLAG_PEAK_BITRATE = "advanced_peak_bitrate_mbps"
        private const val FLAG_STARTS_FIRST_VARIANT = "advanced_starts_first_variant"
        private const val FLAG_PREVIEW_VIDEO_SLOTS = "advanced_preview_video_slots"
        private const val FLAG_PLAY_ID_ROTATION = "advanced_play_id_rotation_s"
        private const val LAST_PLAYED_KEY = "last_played_content"
        private const val VIEW_COUNTS_KEY = "view_counts"
        private const val CONTENT_CACHE_PREFIX = "content_cache_"
    }
}
