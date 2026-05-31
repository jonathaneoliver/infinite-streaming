package com.infinitestream.player;

import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import androidx.annotation.OptIn;
import androidx.media3.common.C;
import androidx.media3.common.Format;
import androidx.media3.common.PlaybackException;
import androidx.media3.common.Player;
import androidx.media3.common.Timeline;
import androidx.media3.common.VideoSize;
import androidx.media3.common.util.UnstableApi;
import androidx.media3.exoplayer.ExoPlayer;
import androidx.media3.exoplayer.upstream.BandwidthMeter;
import androidx.media3.ui.PlayerView;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.text.SimpleDateFormat;
import java.util.Collections;
import java.util.Date;
import java.util.HashMap;
import java.util.Iterator;
import java.util.Locale;
import java.util.Map;
import java.util.TimeZone;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Stages A+B: player metrics heartbeat + lifecycle events. Mirrors the iOS
 * PlaybackViewModel contract — PATCHes to
 * {baseURL}/api/session/{session_id}/metrics at 1 Hz plus dispatches
 * discrete events (video_first_frame, video_bitrate_change, rate_shift_*,
 * error, restart).
 *
 * <p>Session resolution: GET /api/sessions, find entry whose player_id
 * matches ours, cache session_id for 30s.
 */
@OptIn(markerClass = UnstableApi.class)
public final class PlaybackMetrics {

    public interface BaseUrlProvider {
        String get();
    }

    public interface UrlProvider {
        String currentStreamUrl();
    }

    private static final String TAG = "InfiniteStream";
    private static final long HEARTBEAT_INTERVAL_MS = 1000;
    private static final long SESSION_LOOKUP_INTERVAL_MS = 30_000;
    private static final int CONNECT_TIMEOUT_MS = 3000;
    private static final int READ_TIMEOUT_MS = 3000;
    private static final double RATE_SHIFT_THRESHOLD_MBPS = 0.1;
    private static final int FROZEN_THRESHOLD_TICKS = 3;
    private static final long SEGMENT_STALL_THRESHOLD_MS = 10_000;

    private final ExoPlayer player;
    private final PlayerView playerView;
    private final BandwidthMeter bandwidthMeter;
    private final String playerId;
    private final BaseUrlProvider baseUrlProvider;
    private final UrlProvider urlProvider;
    private final Handler mainHandler = new Handler(Looper.getMainLooper());
    private final ExecutorService networkExecutor = Executors.newSingleThreadExecutor();
    private final SimpleDateFormat iso8601;

    private String sessionId;
    private long lastSessionLookupMs;
    private boolean running;

    // Counters (accumulated across app lifetime, matching iOS).
    private final AtomicLong framesRenderedTotal = new AtomicLong();
    private long droppedFramesTotal;
    private int profileShiftCount;
    private int playerRestarts;
    private Double lastReportedBitrateMbps;

    // Per-playback timing.
    private long playbackStartAtMs;
    private Double videoFirstFrameSeconds;
    private Double videoStartTimeSeconds;
    private boolean firstFrameReported;
    private boolean playingReported;

    // Stall tracking.
    private int stallCount;
    private double totalStallTimeS;
    private double lastStallDurationS;
    private long stallStartAtMs = -1;

    // Buffering tracking (broader than stall — every BUFFERING transition).
    private boolean buffering;

    // #550 Phase 1 — per-state residency accumulators. Driven by
    // ExoPlayer state transitions + heartbeat ticks via
    // recomputeResidencyState(). Wire contract is accumulated-only —
    // the forwarder derives the per-row *_ms_delta columns.
    //
    // "trickplaying" stays 0 on Android (ExoPlayer setPlaybackParameters
    // can change speed but this app doesn't expose trick-play UI; the
    // field is emitted for cross-platform schema parity).
    private String currentResidencyState;     // gerund: playing/pausing/buffering/stalling/idling
    private long residencyAnchorMs;           // wall-time the current bucket started accumulating
    private long playingTimeMs;
    private long pausingTimeMs;
    private long bufferingTimeMs;
    private long stallingTimeMs;
    private long idlingTimeMs;
    private long seekingTimeMs;
    private int playingCount;
    private int pausingCount;
    private int bufferingCount;
    private int stallingCount;
    private int idlingCount;
    private int seekingCount;
    // Seek captured on DISCONTINUITY_REASON_SEEK; consumed on the
    // next transition to "playing" so seek-induced rebuffer time
    // gets attributed to BOTH buffering_time_ms and seeking_time_ms
    // (Conviva CIRR/CIRT — intentional overlap).
    private long seekingStartAtMs = -1;

    // #550 Phase 2 — structured error fields. Sticky after first
    // observation so dashboards see the most recent error on every
    // heartbeat until the play ends or recovers.
    private int errorCount;
    private int lastErrorCode;
    private String lastErrorDomain = "";
    private String lastErrorDetails = "";

    // #550 Phase 2 — terminal status/reason. Null while in_progress;
    // set exactly once at session_end via markTerminal(). Preserved
    // across retry(); cleared on play-boundary reset (loadStream).
    // Read by buildPayload() to stamp every payload AFTER the terminal
    // event with the terminal values (so a late-arriving heartbeat
    // after teardown still reads correctly).
    private String terminalStatus;
    private String terminalReason;

    // EBVS — wall-clock instant the current play started. Used to
    // detect Exit-Before-Video-Start: user back BEFORE first frame
    // AND elapsed > LONG_STATE_THRESHOLD_MS → abandoned_start /
    // slow_startup. Set in resetResidency() at play boundaries.
    private long playStartAtMsForEBVS = -1;

    // stall_stuck — sticky true when ExoPlayer is in STATE_BUFFERING
    // without recovering for the heartbeat that follows a long stall.
    // Cleared the moment ExoPlayer reaches STATE_READY again.
    private boolean stallStuck;

    // Per-variant cumulative dwell. Key = "{height}p@{kbps}kbps" to
    // match the iOS payload key format. variantDwellMs is the LIVE
    // total; priorVariantDwellMs is the snapshot from the previous
    // play attempt so retry() preserves the dashboard's Time-per-
    // Variant tile across recovery.
    private final Map<String, Long> variantDwellMs = new HashMap<>();
    private final Map<String, Long> priorVariantDwellMs = new HashMap<>();

    // Residency snapshots preserved across retry() — same pattern as
    // iOS PlaybackDiagnostics' prior* fields. snapshotForRestart()
    // captures current values into these; resetResidency() restores
    // FROM these so the new attempt continues accumulating rather
    // than zeroing. resetForFreshPlay() (Reload button) zeroes them
    // explicitly before calling reset() — user-driven fresh play
    // starts from scratch.
    private long priorPlayingTimeMs;
    private long priorPausingTimeMs;
    private long priorBufferingTimeMs;
    private long priorStallingTimeMs;
    private long priorIdlingTimeMs;
    private long priorSeekingTimeMs;
    private int priorPlayingCount;
    private int priorPausingCount;
    private int priorBufferingCount;
    private int priorStallingCount;
    private int priorIdlingCount;
    private int priorSeekingCount;
    private String currentVariantKey;
    private long currentVariantAnchorMs;
    private int currentVariantKbps;

    // Manifest variant ladder snapshot — populated whenever
    // onVideoFormatChanged sees a richer asset (or via onTracksChanged
    // hook from PlayerViewModel.kt). variantLadderKbps -> resolution
    // label so fetchingResolution() / quality% can look up.
    // Tied to Format.bitrate and Format width/height.
    private final Map<Integer, String> variantLadder = new HashMap<>();
    private int observedMaxVariantKbps;  // self-healing for cap-violating selections

    // Quality-history samples (most-recent first) — accumulate as
    // ABR shifts so videoQualityAvgPct + _60sPct can compute on
    // demand without re-walking the residency machine.
    private static final long QUALITY_60S_WINDOW_MS = 60_000;
    private static final double QUALITY_BASELINE_FLOOR = 0.20;
    private static final long LONG_STATE_THRESHOLD_MS = 10_000;
    private static final long EBVS_THRESHOLD_MS = 10_000;

    // Frozen detection.
    private long lastHeartbeatPositionMs = -1;
    private int frozenTicks;
    private boolean frozenReported;

    // Segment-stall detection.
    private long lastVideoLoadCompletedAtMs;
    private boolean segmentStallReported;

    private final Runnable heartbeatRunnable = new Runnable() {
        @Override
        public void run() {
            // Catches state transitions that ExoPlayer doesn't surface
            // via onPlaybackStateChanged / onIsPlayingChanged — e.g. the
            // playWhenReady→stalling reclassification once first frame
            // arrives mid-buffer.
            recomputeResidencyState();
            refreshVariantLadder();
            updateStallStuck();
            logOffsetHeartbeat();
            sendEvent("heartbeat", Collections.<String, Object>emptyMap());
            maybeReportVideoStart();
            maybeDetectFrozen();
            maybeDetectSegmentStall();
            if (running) {
                mainHandler.postDelayed(this, HEARTBEAT_INTERVAL_MS);
            }
        }
    };

    /** Walks Player.getCurrentTracks() and stamps every video-track
     *  rendition's bitrate → resolution into variantLadder. Run from
     *  heartbeat tick so the ladder converges as ExoPlayer discovers
     *  the manifest. Idempotent — only adds new entries. */
    private void refreshVariantLadder() {
        try {
            androidx.media3.common.Tracks tracks = player.getCurrentTracks();
            if (tracks == null) return;
            for (androidx.media3.common.Tracks.Group group : tracks.getGroups()) {
                if (group.getType() != C.TRACK_TYPE_VIDEO) continue;
                int len = group.length;
                for (int i = 0; i < len; i++) {
                    Format f = group.getTrackFormat(i);
                    if (f == null || f.bitrate <= 0) continue;
                    int kbps = (int) Math.round(f.bitrate / 1000.0);
                    String label = (f.width > 0 && f.height > 0)
                        ? f.width + "x" + f.height
                        : "";
                    String prev = variantLadder.get(kbps);
                    if (prev == null || prev.isEmpty()) {
                        variantLadder.put(kbps, label);
                    }
                }
            }
        } catch (Throwable t) {
            // ExoPlayer's track-group accessors can throw mid-rebuild.
            // Stop short rather than crash the heartbeat.
        }
    }

    /** stall_stuck mirrors iOS's diagnostics.stallStuck — sticky true
     *  when buffering for ≥ LONG_STATE_THRESHOLD_MS, cleared the
     *  moment ExoPlayer reaches STATE_READY again. Acts as the
     *  orthogonal "needs intervention" signal alongside the state
     *  lane staying on stalled/buffering for residency continuity. */
    private void updateStallStuck() {
        if (player.getPlaybackState() == Player.STATE_READY && player.isPlaying()) {
            stallStuck = false;
            return;
        }
        if ("stalled".equals(mapState()) || "buffering".equals(mapState())) {
            if (bufferingTimeMs >= LONG_STATE_THRESHOLD_MS
                    || stallingTimeMs >= LONG_STATE_THRESHOLD_MS) {
                stallStuck = true;
            }
        }
    }

    /**
     * Greppable 1 Hz heartbeat for cross-platform live-offset observation.
     * Field names match iOS PlaybackDiagnostics.playbackSnapshot() so a single
     * grep across logs covers all clients.
     *
     * <p>{@code pdt} is the encoded wall-clock at the playhead, derived from
     * the timeline window's {@code windowStartTimeMs} (HLS PROGRAM-DATE-TIME)
     * plus {@code currentPosition}. {@code trueOff} is host-now − pdt — a
     * ground-truth live offset that survives stalls and HOLD-BACK changes
     * that fool ExoPlayer's own {@link Player#getCurrentLiveOffset()}.
     */
    private void logOffsetHeartbeat() {
        long nowMs = System.currentTimeMillis();
        long posMs = player.getCurrentPosition();
        long bufferedMs = player.getBufferedPosition();
        double bufferDepthS = Math.max(0, bufferedMs - posMs) / 1000.0;
        long liveOffsetMs = player.getCurrentLiveOffset();
        String liveOff = liveOffsetMs == C.TIME_UNSET
            ? "nil"
            : String.format(Locale.US, "%.1fs", liveOffsetMs / 1000.0);

        Long pdtMs = null;
        Timeline timeline = player.getCurrentTimeline();
        if (!timeline.isEmpty()) {
            int windowIndex = player.getCurrentMediaItemIndex();
            if (windowIndex >= 0 && windowIndex < timeline.getWindowCount()) {
                Timeline.Window window = new Timeline.Window();
                timeline.getWindow(windowIndex, window);
                if (window.windowStartTimeMs != C.TIME_UNSET) {
                    pdtMs = window.windowStartTimeMs + posMs;
                }
            }
        }
        String pdt = pdtMs == null ? "nil" : iso8601.format(new Date(pdtMs));
        String trueOff = pdtMs == null
            ? "nil"
            : String.format(Locale.US, "%.2fs", (nowMs - pdtMs) / 1000.0);
        String wall = iso8601.format(new Date(nowMs));
        String pos = String.format(Locale.US, "%.2fs", posMs / 1000.0);
        String buf = String.format(Locale.US, "%.1fs", bufferDepthS);

        Log.i(TAG, String.format(Locale.US,
            "[OFFSET] state=%s wall=%s pdt=%s trueOff=%s pos=%s liveOff=%s bufDepth=%s",
            mapState(), wall, pdt, trueOff, pos, liveOff, buf));
    }

    public PlaybackMetrics(ExoPlayer player,
                    PlayerView playerView,
                    BandwidthMeter bandwidthMeter,
                    String playerId,
                    BaseUrlProvider baseUrlProvider,
                    UrlProvider urlProvider) {
        this.player = player;
        this.playerView = playerView;
        this.bandwidthMeter = bandwidthMeter;
        this.playerId = playerId;
        this.baseUrlProvider = baseUrlProvider;
        this.urlProvider = urlProvider;
        this.iso8601 = new SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss.SSS'Z'", Locale.US);
        this.iso8601.setTimeZone(TimeZone.getTimeZone("UTC"));
        // Initialise the start clock at construction so first-frame elapsed
        // math is sane even when bindMetrics happens AFTER loadStream's
        // onPlaybackStarted() (e.g. the PlayerView composes asynchronously).
        // Without this, elapsedMs = currentTimeMillis() - 0 ≈ a Unix epoch
        // ms value, which UInt32-wraps in CH (the ~24-day garbage we saw).
        // onPlaybackStarted resets to its own value when it does fire.
        this.playbackStartAtMs = System.currentTimeMillis();
    }

    public void start() {
        if (running) return;
        running = true;
        mainHandler.postDelayed(heartbeatRunnable, HEARTBEAT_INTERVAL_MS);
    }

    public void stop() {
        running = false;
        mainHandler.removeCallbacks(heartbeatRunnable);
    }

    public void release() {
        stop();
        networkExecutor.shutdown();
    }

    // --- Event hooks (call from main thread unless noted) ---

    /** Called when a new stream starts loading. Resets per-playback state. */
    public void onPlaybackStarted() {
        playbackStartAtMs = System.currentTimeMillis();
        videoFirstFrameSeconds = null;
        videoStartTimeSeconds = null;
        firstFrameReported = false;
        playingReported = false;
        lastReportedBitrateMbps = null;
        stallStartAtMs = -1;
        lastHeartbeatPositionMs = -1;
        frozenTicks = 0;
        frozenReported = false;
        segmentStallReported = false;
        lastVideoLoadCompletedAtMs = System.currentTimeMillis();
        resetResidency();
        // Phase 2: clear sticky error state on play boundary so a
        // recovered new attempt doesn't carry the previous attempt's
        // error_code / error_domain forward on its heartbeats.
        errorCount = 0;
        lastErrorCode = 0;
        lastErrorDomain = "";
        lastErrorDetails = "";
        recomputeResidencyState();
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_content_url", urlProvider.currentStreamUrl());
        sendEvent("playing", extra);
    }

    /**
     * Called when the player transitions into a stall — BUFFERING while
     * playWhenReady=true. Gated on firstFrameReported so initial load doesn't
     * register as a stall.
     */
    public void onStallStart() {
        recomputeResidencyState();
        if (!firstFrameReported) return;
        if (stallStartAtMs > 0) return;
        stallStartAtMs = System.currentTimeMillis();
        sendEvent("stall_start", Collections.<String, Object>emptyMap());
    }

    /** Called when a stall ends (back to playing). */
    public void onStallEnd() {
        recomputeResidencyState();
        if (stallStartAtMs <= 0) return;
        double duration = (System.currentTimeMillis() - stallStartAtMs) / 1000.0;
        stallStartAtMs = -1;
        if (duration <= 0) return;
        stallCount++;
        totalStallTimeS += duration;
        lastStallDurationS = roundSeconds(duration);
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_last_stall_time_s", lastStallDurationS);
        // #550 Phase 1: canonical *_ms emission alongside the legacy
        // *_s mirror so the forwarder picks up either form during the
        // soft-cutover window.
        extra.put("player_metrics_stall_duration_ms", (long) Math.round(duration * 1000.0));
        sendEvent("stall_end", extra);
    }

    /**
     * Called on every transition into ExoPlayer STATE_BUFFERING. Distinct
     * from onStallStart, which is gated on first-frame + playWhenReady so
     * initial loads and short pre-roll buffering don't register as stalls.
     */
    public void onBufferingStart() {
        recomputeResidencyState();
        if (buffering) return;
        buffering = true;
        sendEvent("buffering_start", Collections.<String, Object>emptyMap());
    }

    /** Called when ExoPlayer leaves STATE_BUFFERING. */
    public void onBufferingEnd() {
        recomputeResidencyState();
        if (!buffering) return;
        buffering = false;
        sendEvent("buffering_end", Collections.<String, Object>emptyMap());
    }

    /**
     * Called from Player.Listener.onPositionDiscontinuity — fires on
     * HLS discontinuity boundaries, explicit seeks, auto-transitions
     * between media items, etc. Equivalent of AVPlayerItemTimeJumped
     * on iOS; emits a `timejump` metrics event with from/to/delta in
     * seconds plus the discontinuity reason name.
     */
    public void onTimeJump(long fromMs, long toMs, String reason) {
        // #550 Phase 1 — only DISCONTINUITY_REASON_SEEK is a user-driven
        // seek; other reasons (auto_transition, internal, etc.) are
        // playlist plumbing and shouldn't inflate seeking_count.
        if ("seek".equals(reason)) {
            seekingStartAtMs = System.currentTimeMillis();
            seekingCount++;
        }
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_timejump_from_s", roundSeconds(fromMs / 1000.0));
        extra.put("player_metrics_timejump_to_s", roundSeconds(toMs / 1000.0));
        extra.put("player_metrics_timejump_delta_s", roundSeconds((toMs - fromMs) / 1000.0));
        extra.put("player_metrics_timejump_origin", reason == null ? "unknown" : reason);
        sendEvent("timejump", extra);
    }

    /** Called from AnalyticsListener.onLoadCompleted for video track. */
    public void onVideoLoadCompleted() {
        lastVideoLoadCompletedAtMs = System.currentTimeMillis();
        if (segmentStallReported) {
            segmentStallReported = false;
        }
    }

    /** Called from Player.Listener.onRenderedFirstFrame. */
    public void onFirstFrameRendered() {
        if (firstFrameReported) return;
        firstFrameReported = true;
        long elapsedMs = Math.max(0, System.currentTimeMillis() - playbackStartAtMs);
        double elapsed = roundSeconds(elapsedMs / 1000.0);
        videoFirstFrameSeconds = elapsed;
        recomputeResidencyState();
        Map<String, Object> extra = new HashMap<>();
        // #550 Phase 1 — emit ms canonical alongside _s mirror.
        extra.put("player_metrics_video_first_frame_time_ms", elapsedMs);
        extra.put("player_metrics_video_first_frame_time_s", elapsed);
        sendEvent("video_first_frame", extra);
    }

    /** Called from AnalyticsListener.onVideoInputFormatChanged. */
    public void onVideoFormatChanged(Format format) {
        if (format == null) return;
        double mbps = format.bitrate > 0 ? round2(format.bitrate / 1_000_000.0) : 0;
        if (mbps <= 0) return;
        // Per-variant dwell tracking — flush previous bucket, start new.
        int kbps = (int) Math.round(format.bitrate / 1000.0);
        onVariantSelected(kbps, format.width, format.height);
        if (lastReportedBitrateMbps != null) {
            double previous = lastReportedBitrateMbps;
            if (mbps != previous) {
                profileShiftCount++;
                Map<String, Object> bitrateChange = new HashMap<>();
                bitrateChange.put("player_metrics_video_bitrate_from_mbps", previous);
                bitrateChange.put("player_metrics_video_bitrate_to_mbps", mbps);
                bitrateChange.put("player_metrics_profile_shift_count", profileShiftCount);
                sendEvent("video_bitrate_change", bitrateChange);
                double delta = mbps - previous;
                if (Math.abs(delta) >= RATE_SHIFT_THRESHOLD_MBPS) {
                    String event = delta > 0 ? "rate_shift_up" : "rate_shift_down";
                    Map<String, Object> shift = new HashMap<>();
                    shift.put("player_metrics_rate_from_mbps", previous);
                    shift.put("player_metrics_rate_to_mbps", mbps);
                    sendEvent(event, shift);
                }
            }
        }
        lastReportedBitrateMbps = mbps;
    }

    /** Called from AnalyticsListener.onDroppedVideoFrames. */
    public void onDroppedFrames(int count) {
        if (count <= 0) return;
        droppedFramesTotal += count;
    }

    /**
     * Called from VideoFrameMetadataListener.onVideoFrameAboutToBeRendered,
     * which runs on the playback thread — atomic counter is safe.
     */
    public void onFrameRendered() {
        framesRenderedTotal.incrementAndGet();
    }

    /** Called from Player.Listener.onPlayerError. */
    public void onPlayerError(PlaybackException error) {
        String message = error == null ? "" : (error.getMessage() == null ? "" : error.getMessage());
        // #550 Phase 2 — structured error fields. ExoPlayer doesn't
        // expose a domain string like NSError does on iOS, so we stamp
        // a constant "ExoPlayer" domain. The integer errorCode and
        // human-readable errorCodeName (e.g. ERROR_CODE_IO_NETWORK_
        // CONNECTION_FAILED) become error_code + error_details
        // respectively. Forwarder's error_classifier.go matches on
        // (domain, code) tuples; seed mappings for the Exo domain
        // alongside the Apple ones in a follow-up if needed.
        int code = error == null ? 0 : error.errorCode;
        String codeName = error == null ? "" : safeErrorCodeName(error);
        errorCount++;
        lastErrorCode = code;
        lastErrorDomain = "ExoPlayer";
        lastErrorDetails = codeName.isEmpty()
            ? message
            : (message.isEmpty() ? codeName : codeName + ": " + message);
        Map<String, Object> extra = new HashMap<>();
        // Legacy concatenated form — kept for the deprecation window.
        extra.put("player_metrics_error", message);
        // Structured form — what the forwarder Phase 2 classifier reads.
        extra.put("player_metrics_error_code", code);
        extra.put("player_metrics_error_domain", lastErrorDomain);
        extra.put("player_metrics_error_details", lastErrorDetails);
        sendEvent("error", extra);
        requestHarSnapshot("player_error", 0, /* force= */ false);
    }

    /** errorCodeName throws if the code is out of range; defend
     *  against that so a forward-rev runtime can't crash an older
     *  build's metrics path. */
    private static String safeErrorCodeName(PlaybackException error) {
        try {
            String n = error.getErrorCodeName();
            return n == null ? "" : n;
        } catch (Throwable t) {
            return "";
        }
    }

    /**
     * 911 button. Fires a "user_marked" metrics event; the server
     * recognises that and writes a HAR snapshot to /api/incidents
     * with reason="user_marked", so the user can review what was on
     * the wire when they tapped. Posting via the metrics path (not
     * /har/snapshot directly) keeps the marker visible on the
     * dashboard's events swim lane too. buildPayload already stamps
     * an event timestamp — no extras needed.
     */
    public void onUserMarked() {
        // Console marker — easy to grep adb logcat / OS logs for "911"
        // alongside the network log entry the server writes.
        Log.i("InfiniteStream", "911 user-marked at " + iso8601.format(new Date()));
        sendEvent("user_marked", Collections.<String, Object>emptyMap());
    }

    /** Called when user triggers a restart (Restart Playback button, etc). */
    public void onRestart(String reason) {
        playerRestarts = Math.max(0, playerRestarts) + 1;
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_restart_reason", reason);
        extra.put("player_restarts", playerRestarts);
        sendEvent("restart", extra);
        // User-driven restarts should always produce a HAR — bypass the
        // server-side per-player debounce window.
        requestHarSnapshot(reason, 0, /* force= */ true);
    }

    /**
     * Ask go-proxy to dump the current session timeline to disk as a HAR
     * file. Issue #273. {@code force=true} bypasses the per-player
     * 30s debounce — use it for user-driven Reload/Retry and per-attempt
     * auto-recovery snapshots.
     */
    public void requestHarSnapshot(String reason, int attempt, boolean force) {
        if (urlProvider.currentStreamUrl().isEmpty()) return;
        final JSONObject metadata = new JSONObject();
        try {
            metadata.put("player_state", mapState());
            metadata.put("auto_recovery_attempt", attempt);
            metadata.put("player_restarts", playerRestarts);
            long bufferedPositionMs = player.getBufferedPosition();
            long currentPositionMs = player.getCurrentPosition();
            metadata.put("buffer_depth_s",
                roundSeconds(Math.max(0, bufferedPositionMs - currentPositionMs) / 1000.0));
        } catch (Exception ignored) {
        }
        final JSONObject body = new JSONObject();
        try {
            body.put("reason", reason == null ? "manual" : reason);
            body.put("source", "player");
            body.put("force", force);
            body.put("metadata", metadata);
        } catch (Exception ignored) {
            return;
        }
        networkExecutor.execute(() -> {
            String sid = resolveSessionId();
            if (sid == null || sid.isEmpty()) return;
            postSnapshot(sid, body);
        });
    }

    private void postSnapshot(String sid, JSONObject body) {
        String base = baseUrlProvider.get();
        if (base == null || base.isEmpty()) return;
        HttpURLConnection conn = null;
        try {
            URL url = new URL(base + "/api/session/" + sid + "/har/snapshot");
            conn = (HttpURLConnection) url.openConnection();
            conn.setConnectTimeout(CONNECT_TIMEOUT_MS);
            conn.setReadTimeout(READ_TIMEOUT_MS);
            conn.setRequestMethod("POST");
            conn.setRequestProperty("Content-Type", "application/json");
            conn.setDoOutput(true);
            try (OutputStream os = conn.getOutputStream()) {
                os.write(body.toString().getBytes(StandardCharsets.UTF_8));
            }
            conn.getResponseCode();
        } catch (Exception ignored) {
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    private void maybeDetectFrozen() {
        boolean actuallyPlaying = player.getPlaybackState() == Player.STATE_READY
            && player.isPlaying();
        long posMs = player.getCurrentPosition();
        if (!actuallyPlaying) {
            lastHeartbeatPositionMs = posMs;
            if (frozenReported) {
                frozenReported = false;
            }
            frozenTicks = 0;
            return;
        }
        if (lastHeartbeatPositionMs >= 0 && posMs == lastHeartbeatPositionMs) {
            frozenTicks++;
            if (frozenTicks >= FROZEN_THRESHOLD_TICKS && !frozenReported) {
                frozenReported = true;
                sendEvent("frozen", Collections.<String, Object>emptyMap());
                requestHarSnapshot("frozen", 0, /* force= */ false);
            }
        } else {
            frozenTicks = 0;
            frozenReported = false;
        }
        lastHeartbeatPositionMs = posMs;
    }

    private void maybeDetectSegmentStall() {
        boolean actuallyPlaying = player.getPlaybackState() == Player.STATE_READY
            && player.isPlaying();
        if (!actuallyPlaying) {
            return;
        }
        long since = System.currentTimeMillis() - lastVideoLoadCompletedAtMs;
        if (since >= SEGMENT_STALL_THRESHOLD_MS && !segmentStallReported) {
            segmentStallReported = true;
            sendEvent("segment_stall", Collections.<String, Object>emptyMap());
            requestHarSnapshot("segment_stall", 0, /* force= */ false);
        }
    }

    private void maybeReportVideoStart() {
        if (playingReported) return;
        if (playbackStartAtMs == 0) return;
        boolean actuallyPlaying = player.getPlaybackState() == Player.STATE_READY
            && player.isPlaying()
            && player.getPlaybackParameters().speed > 0;
        double positionS = player.getCurrentPosition() / 1000.0;
        if (actuallyPlaying && positionS >= 0.1) {
            playingReported = true;
            long elapsedMs = Math.max(0, System.currentTimeMillis() - playbackStartAtMs);
            double elapsed = roundSeconds(elapsedMs / 1000.0);
            videoStartTimeSeconds = elapsed;
            Map<String, Object> extra = new HashMap<>();
            // #550 Phase 1 — ms canonical alongside _s mirror.
            extra.put("player_metrics_video_start_time_ms", elapsedMs);
            extra.put("player_metrics_video_start_time_s", elapsed);
            sendEvent("video_start_time", extra);
        }
    }

    void sendEvent(String event, Map<String, Object> extra) {
        if (urlProvider.currentStreamUrl().isEmpty()) return;
        final JSONObject payload = buildPayload(event, extra);
        if (payload == null) return;
        networkExecutor.execute(() -> {
            String sid = resolveSessionId();
            if (sid == null || sid.isEmpty()) return;
            patchMetrics(sid, payload);
        });
    }

    private JSONObject buildPayload(String event, Map<String, Object> extra) {
        JSONObject p = new JSONObject();
        try {
            String timestamp = iso8601.format(new Date());
            p.put("player_metrics_source", "android");
            p.put("player_metrics_last_event", event);
            p.put("player_metrics_trigger_type", event);
            p.put("player_metrics_event_time", timestamp);
            p.put("player_metrics_state", mapState());
            p.put("player_metrics_waiting_reason", mapWaitingReason());
            p.put("player_metrics_position_s", roundSeconds(player.getCurrentPosition() / 1000.0));
            p.put("player_metrics_playback_rate", round2(player.getPlaybackParameters().speed));

            long bufferedPositionMs = player.getBufferedPosition();
            long currentPositionMs = player.getCurrentPosition();
            p.put("player_metrics_buffer_end_s", roundSeconds(bufferedPositionMs / 1000.0));
            p.put("player_metrics_buffer_depth_s",
                roundSeconds(Math.max(0, bufferedPositionMs - currentPositionMs) / 1000.0));

            long liveOffsetMs = player.getCurrentLiveOffset();
            if (liveOffsetMs != C.TIME_UNSET) {
                p.put("player_metrics_live_offset_s", roundSeconds(liveOffsetMs / 1000.0));
            }

            // Encoded wall-clock at the playhead (epoch ms) — derived from the
            // HLS timeline window's PROGRAM-DATE-TIME (windowStartTimeMs)
            // plus currentPosition. Pairs with the receiver's clock to compute
            // a ground-truth live offset.
            Timeline tl = player.getCurrentTimeline();
            if (!tl.isEmpty()) {
                int windowIndex = player.getCurrentMediaItemIndex();
                if (windowIndex >= 0 && windowIndex < tl.getWindowCount()) {
                    Timeline.Window window = new Timeline.Window();
                    tl.getWindow(windowIndex, window);
                    if (window.windowStartTimeMs != C.TIME_UNSET) {
                        long pdtMs = window.windowStartTimeMs + currentPositionMs;
                        p.put("player_metrics_playhead_wallclock_ms", pdtMs);
                        // Client-side fallback for receivers that can't pair
                        // with a server-stamped received_at. Biased by client
                        // clock skew vs. server clock.
                        p.put("player_metrics_true_offset_s",
                            roundSeconds((System.currentTimeMillis() - pdtMs) / 1000.0));
                    }
                }
            }

            VideoSize vs = player.getVideoSize();
            if (vs.width > 0 && vs.height > 0) {
                p.put("player_metrics_video_resolution", vs.width + "x" + vs.height);
            }

            int displayWidth = playerView.getWidth();
            int displayHeight = playerView.getHeight();
            if (displayWidth > 0 && displayHeight > 0) {
                p.put("player_metrics_display_resolution", displayWidth + "x" + displayHeight);
            }

            Format videoFormat = player.getVideoFormat();
            if (videoFormat != null && videoFormat.bitrate > 0) {
                p.put("player_metrics_video_bitrate_mbps",
                    round2(videoFormat.bitrate / 1_000_000.0));
            }

            if (bandwidthMeter != null) {
                long estimate = bandwidthMeter.getBitrateEstimate();
                if (estimate > 0) {
                    p.put("player_metrics_avg_network_bitrate_mbps",
                        round2(estimate / 1_000_000.0));
                } else {
                    p.put("player_metrics_avg_network_bitrate_mbps", JSONObject.NULL);
                }
            }
            // Wire-level (near-instantaneous) network bitrate has no Android
            // analogue. iOS populates this via LocalHTTPProxy; leave null here
            // rather than synthesize it from the averaged estimate.
            p.put("player_metrics_network_bitrate_mbps", JSONObject.NULL);

            p.put("player_metrics_frames_displayed", framesRenderedTotal.get());
            p.put("player_metrics_dropped_frames", droppedFramesTotal);
            p.put("player_metrics_profile_shift_count", profileShiftCount);
            p.put("player_metrics_stall_count", stallCount);
            p.put("player_metrics_stall_time_s", roundSeconds(totalStallTimeS));
            p.put("player_metrics_last_stall_time_s", lastStallDurationS);
            p.put("player_restarts", playerRestarts);

            if (videoFirstFrameSeconds != null) {
                p.put("player_metrics_video_first_frame_time_s", videoFirstFrameSeconds);
                p.put("player_metrics_video_first_frame_time_ms",
                    (long) Math.round(videoFirstFrameSeconds * 1000.0));
            }
            if (videoStartTimeSeconds != null) {
                p.put("player_metrics_video_start_time_s", videoStartTimeSeconds);
                p.put("player_metrics_video_start_time_ms",
                    (long) Math.round(videoStartTimeSeconds * 1000.0));
            }

            // #550 Phase 1 — residency accumulators (ms, cumulative
            // per-play; forwarder computes the *_delta columns).
            // Flush the current-state interval into its bucket so the
            // numbers we emit reflect the slice ending at this tick.
            flushResidency();
            p.put("player_metrics_playing_time_ms", playingTimeMs);
            p.put("player_metrics_playing_count", playingCount);
            p.put("player_metrics_pausing_time_ms", pausingTimeMs);
            p.put("player_metrics_pausing_count", pausingCount);
            p.put("player_metrics_buffering_time_ms", bufferingTimeMs);
            p.put("player_metrics_buffering_count", bufferingCount);
            p.put("player_metrics_stalling_time_ms", stallingTimeMs);
            p.put("player_metrics_stalling_count", stallingCount);
            p.put("player_metrics_idling_time_ms", idlingTimeMs);
            p.put("player_metrics_idling_count", idlingCount);
            p.put("player_metrics_seeking_time_ms", seekingTimeMs);
            p.put("player_metrics_seeking_count", seekingCount);
            // ExoPlayer setPlaybackParameters supports speed != 1.0 but
            // this app doesn't expose trick-play; stamp 0 for schema
            // parity with iOS / Roku payloads.
            p.put("player_metrics_trickplaying_time_ms", 0L);
            p.put("player_metrics_trickplaying_count", 0);

            // #550 Phase 2 — failure status + structured error.
            // playback_status: defaults to in_progress on heartbeats;
            // once endSession() runs the sticky terminal value lands
            // on this and every subsequent payload — covers the case
            // where a heartbeat races the activity teardown after the
            // session_end event fires.
            p.put("player_metrics_playback_status",
                terminalStatus != null ? terminalStatus : "in_progress");
            p.put("player_metrics_playback_reason",
                terminalReason != null ? terminalReason : mapState());
            p.put("player_metrics_stall_stuck", stallStuck);
            p.put("player_metrics_error_count", errorCount);
            if (lastErrorCode != 0 || !lastErrorDomain.isEmpty()) {
                p.put("player_metrics_error_code", lastErrorCode);
                p.put("player_metrics_error_domain", lastErrorDomain);
                p.put("player_metrics_error_details", lastErrorDetails);
            }

            // #550 Phase 4 — device / platform / version taxonomy.
            // Stamped on every row (Mux / Conviva pattern); the
            // LowCardinality columns in CH compress the repeated values
            // to near-zero so per-row stickiness is essentially free at
            // query time.
            android.content.Context ctx = playerView == null ? null : playerView.getContext();
            p.put("player_metrics_os_version_major", DeviceInfo.osVersionMajor());
            p.put("player_metrics_os_version_minor", DeviceInfo.osVersionMinor());
            p.put("player_metrics_app_version", DeviceInfo.appVersion());
            p.put("player_metrics_device_class", DeviceInfo.deviceClass(ctx));
            p.put("player_metrics_device_model", DeviceInfo.deviceModel());
            p.put("player_metrics_player_tech", DeviceInfo.playerTech());
            // device_resolution supersedes the trio of screen_width_px /
            // screen_height_px / screen_density. Single tile, orientation-
            // aware. Mirrors iOS DeviceInfo.deviceResolution() — same
            // schema column across platforms.
            p.put("player_metrics_device_resolution", DeviceInfo.deviceResolution(ctx));

            // Per-variant cumulative dwell — preserved across retry() via
            // priors so the dashboard Time-per-Variant tile doesn't reset
            // mid-play. JSON object string; chRowAdapter parses it.
            JSONObject tpv = buildTimePerVariantJson();
            if (tpv != null && tpv.length() > 0) {
                p.put("player_metrics_time_per_variant_s", tpv.toString());
            }

            // Quality% (log-bitrate) — instantaneous + lifetime + 60s
            // window. All computed from the same selectable ladder so
            // "playing the top variant" reads 100% even when ExoPlayer's
            // maxVideoSize cap excludes higher renditions.
            Double qNow = videoQualityPctSnapshot();
            if (qNow != null) p.put("player_metrics_video_quality_pct", round2(qNow));
            Double qAvg = videoQualityAvgPct();
            if (qAvg != null) p.put("player_metrics_video_quality_avg_pct", round2(qAvg));
            Double q60 = videoQuality60sPct();
            if (q60 != null) p.put("player_metrics_video_quality_60s_pct", round2(q60));

            // Fetching Res — current selected variant resolution.
            String fetching = fetchingResolution();
            if (fetching != null && !fetching.isEmpty()) {
                p.put("player_metrics_fetching_resolution", fetching);
            }

            for (Map.Entry<String, Object> e : extra.entrySet()) {
                p.put(e.getKey(), e.getValue());
            }
        } catch (JSONException e) {
            return null;
        }
        return p;
    }

    // ──────────────────────────────────────────────────────────────
    // #550 Phase 1 — residency tracking
    // ──────────────────────────────────────────────────────────────

    private void resetResidency() {
        currentResidencyState = null;
        residencyAnchorMs = System.currentTimeMillis();
        // Restore from priors — same pattern as iOS PlaybackDiagnostics.
        // resetForFreshPlay() zeroes priors first so a Reload starts at
        // zero; retry() preserves them so the new attempt continues
        // accumulating from the prior attempt's totals.
        playingTimeMs   = priorPlayingTimeMs;
        pausingTimeMs   = priorPausingTimeMs;
        bufferingTimeMs = priorBufferingTimeMs;
        stallingTimeMs  = priorStallingTimeMs;
        idlingTimeMs    = priorIdlingTimeMs;
        seekingTimeMs   = priorSeekingTimeMs;
        playingCount    = priorPlayingCount;
        pausingCount    = priorPausingCount;
        bufferingCount  = priorBufferingCount;
        stallingCount   = priorStallingCount;
        idlingCount     = priorIdlingCount;
        seekingCount    = priorSeekingCount;
        seekingStartAtMs = -1;
        // Snapshot variant dwell into priors so retry() preserves the
        // dashboard Time-per-Variant tile across the AVPlayerItem
        // replacement. MERGE (not overwrite) so multi-retry plays
        // keep stacking: Round 1 + Round 2 + Round 3 totals all
        // accumulate into priors rather than each retry replacing
        // the earlier values. Reload clears priors via
        // resetForFreshPlay() in PlayerViewModel.
        for (Map.Entry<String, Long> e : variantDwellMs.entrySet()) {
            Long prev = priorVariantDwellMs.get(e.getKey());
            priorVariantDwellMs.put(e.getKey(), (prev == null ? 0L : prev) + e.getValue());
        }
        variantDwellMs.clear();
        currentVariantKey = null;
        currentVariantAnchorMs = 0;
        currentVariantKbps = 0;
        // EBVS clock — set once at the start of THIS play so user-back
        // before first frame after threshold lands as abandoned_start.
        // resetForFreshPlay() will clear it before reset() runs.
        playStartAtMsForEBVS = System.currentTimeMillis();
        stallStuck = false;
        observedMaxVariantKbps = 0;
    }

    /** Called from PlayerViewModel.retry() — capture current residency
     *  + variant-dwell into priors so the subsequent resetResidency()
     *  (via loadStream → onPlaybackStarted) restores rather than
     *  zeroes. Mirrors iOS PlaybackDiagnostics.snapshotForRestart(). */
    public void snapshotForRestart() {
        flushResidency();  // close the open bucket first
        priorPlayingTimeMs   = playingTimeMs;
        priorPausingTimeMs   = pausingTimeMs;
        priorBufferingTimeMs = bufferingTimeMs;
        priorStallingTimeMs  = stallingTimeMs;
        priorIdlingTimeMs    = idlingTimeMs;
        priorSeekingTimeMs   = seekingTimeMs;
        priorPlayingCount    = playingCount;
        priorPausingCount    = pausingCount;
        priorBufferingCount  = bufferingCount;
        priorStallingCount   = stallingCount;
        priorIdlingCount     = idlingCount;
        priorSeekingCount    = seekingCount;
        // variant dwell snapshot happens inside resetResidency()
    }

    /** Called from PlayerViewModel.reload() — fresh play boundary,
     *  zero priors so the new play_id starts from scratch. */
    public void resetForFreshPlay() {
        priorPlayingTimeMs = 0;
        priorPausingTimeMs = 0;
        priorBufferingTimeMs = 0;
        priorStallingTimeMs = 0;
        priorIdlingTimeMs = 0;
        priorSeekingTimeMs = 0;
        priorPlayingCount = 0;
        priorPausingCount = 0;
        priorBufferingCount = 0;
        priorStallingCount = 0;
        priorIdlingCount = 0;
        priorSeekingCount = 0;
        priorVariantDwellMs.clear();
        terminalStatus = null;
        terminalReason = null;
        playStartAtMsForEBVS = -1;  // resetResidency() will set new
        observedMaxVariantKbps = 0;
        resetResidency();
    }

    // ──────────────────────────────────────────────────────────────
    // #550 Phase 2 — terminal status (markTerminal + ended_* refinement)
    // ──────────────────────────────────────────────────────────────

    /** First-call-wins. Refines reason via refineTerminalReason so a
     *  user_quit while buffering becomes ended_buffering[_long]. */
    private void markTerminal(String status, String reason) {
        if (terminalStatus != null) return;
        terminalStatus = status;
        terminalReason = refineTerminalReason(reason, status);
    }

    /** Mirrors iOS PlaybackDiagnostics.refineTerminalReason — generic
     *  user_quit becomes ended_buffering / ended_stalling (with _long
     *  suffix when sticky-event duration ≥ LONG_STATE_THRESHOLD_MS).
     *  Operator-explicit reasons (app_backgrounded etc.) pass through. */
    private String refineTerminalReason(String baseReason, String status) {
        if (!"user_stopped".equals(status)) return baseReason;
        if (baseReason != null && !baseReason.isEmpty() && !"user_quit".equals(baseReason)) {
            return baseReason;
        }
        String s = mapState();
        if ("stalled".equals(s)) {
            long durMs = (long) (lastStallDurationS * 1000);
            return durMs >= LONG_STATE_THRESHOLD_MS ? "ended_stalling_long" : "ended_stalling";
        }
        if ("buffering".equals(s)) {
            // No per-event "lastBufferingDurationS" yet on Android —
            // approximate via the running buffering bucket since the
            // most recent state transition. Acceptable: dashboards
            // only see this on session_end so the value is stable.
            long durMs = bufferingTimeMs;  // accumulated buffering this play
            return durMs >= LONG_STATE_THRESHOLD_MS ? "ended_buffering_long" : "ended_buffering";
        }
        return baseReason;
    }

    /** Mark terminal + emit a single session_end event. Subsequent
     *  payloads automatically pick up the terminal values from
     *  buildPayload's terminalStatus fallback. */
    public void endSession(String status, String reason) {
        markTerminal(status, reason);
        sendEvent("session_end", Collections.<String, Object>emptyMap());
    }

    /** Back press / system back. Picks EBVS-or-user_quit by whether
     *  the player ever crossed first frame and how long the play had
     *  been running. */
    public void endSessionForUserBack() {
        long now = System.currentTimeMillis();
        if (!firstFrameReported
                && playStartAtMsForEBVS > 0
                && (now - playStartAtMsForEBVS) >= EBVS_THRESHOLD_MS) {
            endSession("abandoned_start", "slow_startup");
        } else {
            endSession("user_stopped", "user_quit");
        }
    }

    /** Fatal AVPlayer-equivalent error path. iOS uses
     *  hasRenderedFirstFrame to pick start vs mid-stream failure;
     *  Android mirrors via firstFrameReported. */
    public void markFatalTerminal(String message) {
        String status = firstFrameReported ? "mid_stream_failure" : "start_failure";
        endSession(status, "unknown");
        Log.i(TAG, "[endSession] fatal status=" + status + " message=" + (message == null ? "" : message));
    }

    // ──────────────────────────────────────────────────────────────
    // #550 — Per-variant dwell tracking
    // ──────────────────────────────────────────────────────────────

    /** Called from onVideoFormatChanged. Flushes the previous variant's
     *  accumulator and starts the new one. Also stamps the variant
     *  ladder + selectable ceiling for video_quality_*. */
    private void onVariantSelected(int kbps, int width, int height) {
        flushVariantDwell();
        if (kbps <= 0) return;
        String label = (height > 0 ? height + "p" : "") + "@" + kbps + "kbps";
        if (height <= 0) label = kbps + "kbps";
        currentVariantKey = label;
        currentVariantKbps = kbps;
        currentVariantAnchorMs = System.currentTimeMillis();
        if (width > 0 && height > 0) {
            variantLadder.put(kbps, width + "x" + height);
        } else if (!variantLadder.containsKey(kbps)) {
            variantLadder.put(kbps, "");
        }
        if (kbps > observedMaxVariantKbps) observedMaxVariantKbps = kbps;
    }

    private void flushVariantDwell() {
        if (currentVariantKey == null) return;
        long now = System.currentTimeMillis();
        long delta = Math.max(0, now - currentVariantAnchorMs);
        currentVariantAnchorMs = now;
        Long prev = variantDwellMs.get(currentVariantKey);
        variantDwellMs.put(currentVariantKey, (prev == null ? 0L : prev) + delta);
    }

    /** Build the time_per_variant_s JSON map (seconds, rounded to 2dp).
     *  Includes priors so retry() preserves continuity. */
    private JSONObject buildTimePerVariantJson() {
        flushVariantDwell();
        if (variantDwellMs.isEmpty()
                && priorVariantDwellMs.isEmpty()
                && variantLadder.isEmpty()) {
            return null;
        }
        // Seed the merged map with EVERY known variant at 0 so the
        // dashboard's Time-per-Variant tile shows the full menu the
        // player can choose from, not just the ones it's tried.
        // Mirrors iOS perVariantTimeSeconds. variantLadder is populated
        // by refreshVariantLadder() on every heartbeat from the asset's
        // selectable tracks.
        Map<String, Long> merged = new HashMap<>();
        for (Map.Entry<Integer, String> e : variantLadder.entrySet()) {
            int kbps = e.getKey();
            String resLabel = e.getValue();
            String key = (resLabel != null && !resLabel.isEmpty())
                ? heightFromResolution(resLabel) + "p@" + kbps + "kbps"
                : kbps + "kbps";
            merged.put(key, 0L);
        }
        for (Map.Entry<String, Long> e : priorVariantDwellMs.entrySet()) {
            Long prev = merged.get(e.getKey());
            merged.put(e.getKey(), (prev == null ? 0L : prev) + e.getValue());
        }
        for (Map.Entry<String, Long> e : variantDwellMs.entrySet()) {
            Long prev = merged.get(e.getKey());
            merged.put(e.getKey(), (prev == null ? 0L : prev) + e.getValue());
        }
        JSONObject out = new JSONObject();
        try {
            for (Map.Entry<String, Long> e : merged.entrySet()) {
                double seconds = Math.round((e.getValue() / 1000.0) * 100.0) / 100.0;
                out.put(e.getKey(), seconds);
            }
        } catch (JSONException ex) {
            return null;
        }
        return out;
    }

    /** Pull the height (Y dimension) out of a "WxH" resolution string.
     *  Returns 0 on malformed input. */
    private static int heightFromResolution(String wxh) {
        if (wxh == null) return 0;
        int x = wxh.indexOf('x');
        if (x < 0 || x + 1 >= wxh.length()) return 0;
        try {
            return Integer.parseInt(wxh.substring(x + 1));
        } catch (NumberFormatException e) {
            return 0;
        }
    }

    // ──────────────────────────────────────────────────────────────
    // #550 — Quality % (log-bitrate, selectable ladder, self-healing)
    // ──────────────────────────────────────────────────────────────

    /** Top kbps the ExoPlayer track selector can currently pick.
     *  Includes observed runaway selections (self-heal: if ExoPlayer
     *  selected above the filter ceiling, expand to match reality
     *  rather than silently understate the denominator). */
    private int selectableTopKbps() {
        int top = 0;
        for (Integer k : variantLadder.keySet()) {
            if (k > top) top = k;
        }
        if (observedMaxVariantKbps > top) top = observedMaxVariantKbps;
        return top;
    }

    /** Minimum kbps from the same selectable set — denominator of
     *  the log-ratio. */
    private int selectableMinKbps() {
        int min = Integer.MAX_VALUE;
        for (Integer k : variantLadder.keySet()) {
            if (k < min) min = k;
        }
        return min == Integer.MAX_VALUE ? 0 : min;
    }

    /** Weber-Fechner log-bitrate weight with the 0.20 baseline floor.
     *  Mirrors iOS qualityWeightForBitrate. Caps at 1.0. */
    private double qualityWeight(int kbps, int minKbps, int maxKbps) {
        if (kbps <= 0 || minKbps <= 0 || maxKbps <= minKbps) return 0;
        double ratio = (double) kbps / (double) minKbps;
        double denom = Math.log((double) maxKbps / (double) minKbps);
        if (denom <= 0) return 0;
        double raw = Math.log(ratio) / denom;
        return Math.max(QUALITY_BASELINE_FLOOR, Math.min(1.0, raw));
    }

    private Double videoQualityPctSnapshot() {
        int minK = selectableMinKbps();
        int maxK = selectableTopKbps();
        if (currentVariantKbps <= 0 || minK <= 0 || maxK <= minK) return null;
        return qualityWeight(currentVariantKbps, minK, maxK) * 100.0;
    }

    private Double videoQualityAvgPct() {
        int minK = selectableMinKbps();
        int maxK = selectableTopKbps();
        if (minK <= 0 || maxK <= minK) return null;
        flushVariantDwell();
        Map<String, Long> merged = new HashMap<>(priorVariantDwellMs);
        for (Map.Entry<String, Long> e : variantDwellMs.entrySet()) {
            Long prev = merged.get(e.getKey());
            merged.put(e.getKey(), (prev == null ? 0L : prev) + e.getValue());
        }
        if (merged.isEmpty()) return null;
        double weighted = 0;
        long total = 0;
        for (Map.Entry<String, Long> e : merged.entrySet()) {
            int kbps = kbpsFromVariantKey(e.getKey());
            long ms = e.getValue();
            if (kbps <= 0 || ms <= 0) continue;
            weighted += qualityWeight(kbps, minK, maxK) * ms;
            total += ms;
        }
        if (total <= 0) return null;
        return (weighted / total) * 100.0;
    }

    /** Last 60s of watched time only. Without per-event timestamps we
     *  approximate by treating the current variant's most-recent
     *  interval up to QUALITY_60S_WINDOW_MS as the window. Coarser
     *  than iOS's access-log walk, but matches "what's the recent
     *  quality" intent. */
    private Double videoQuality60sPct() {
        int minK = selectableMinKbps();
        int maxK = selectableTopKbps();
        if (minK <= 0 || maxK <= minK || currentVariantKbps <= 0) return null;
        return qualityWeight(currentVariantKbps, minK, maxK) * 100.0;
    }

    /** Resolution AVPlayer-equivalent (ExoPlayer) is about to fetch.
     *  Mirrors iOS fetchingResolution() — matches current selected
     *  bitrate against the variant ladder. */
    private String fetchingResolution() {
        if (currentVariantKbps <= 0) return null;
        // Direct hit first
        String label = variantLadder.get(currentVariantKbps);
        if (label != null && !label.isEmpty()) return label;
        // Tolerant match (±10%) for EWMA-jittered bitrate selections
        int bestDelta = Integer.MAX_VALUE;
        String best = null;
        for (Map.Entry<Integer, String> e : variantLadder.entrySet()) {
            int delta = Math.abs(e.getKey() - currentVariantKbps);
            int tol = Math.max((int) (e.getKey() * 0.10), 50);
            if (delta > tol) continue;
            if (delta < bestDelta && e.getValue() != null && !e.getValue().isEmpty()) {
                bestDelta = delta;
                best = e.getValue();
            }
        }
        return best;
    }

    private static int kbpsFromVariantKey(String key) {
        if (key == null) return 0;
        int at = key.lastIndexOf('@');
        String tail = at >= 0 ? key.substring(at + 1) : key;
        // tail looks like "29857kbps"
        int kbpsEnd = tail.indexOf('k');
        if (kbpsEnd < 0) return 0;
        try {
            return Integer.parseInt(tail.substring(0, kbpsEnd));
        } catch (NumberFormatException e) {
            return 0;
        }
    }

    /** Add (now - anchor) to the current state's bucket and slide
     *  the anchor to now. Called from every emit + every transition. */
    private void flushResidency() {
        if (currentResidencyState == null) return;
        long now = System.currentTimeMillis();
        long delta = Math.max(0, now - residencyAnchorMs);
        residencyAnchorMs = now;
        switch (currentResidencyState) {
            case "playing":   playingTimeMs   += delta; break;
            case "pausing":   pausingTimeMs   += delta; break;
            case "buffering": bufferingTimeMs += delta; break;
            case "stalling":  stallingTimeMs  += delta; break;
            case "idling":    idlingTimeMs    += delta; break;
            default: break;
        }
    }

    /** Read ExoPlayer's current state, map to gerund, and transition
     *  the residency tracker accordingly. Safe to call repeatedly —
     *  same-state calls are no-ops apart from the flush. */
    private void recomputeResidencyState() {
        String s;
        switch (player.getPlaybackState()) {
            case Player.STATE_IDLE:
            case Player.STATE_ENDED:
                s = "idling";
                break;
            case Player.STATE_BUFFERING:
                // Same disambiguation as mapState(): pre-first-frame
                // and pause-induced buffering count as plain
                // "buffering"; once first frame's rendered and the
                // user wants playback, mid-stream STATE_BUFFERING is a
                // rebuffer == "stalling".
                s = (firstFrameReported && player.getPlayWhenReady())
                    ? "stalling" : "buffering";
                break;
            case Player.STATE_READY:
                s = player.isPlaying() ? "playing" : "pausing";
                break;
            default:
                s = "idling";
        }
        transitionResidencyTo(s);
    }

    private void transitionResidencyTo(String newState) {
        if (currentResidencyState == null) {
            currentResidencyState = newState;
            residencyAnchorMs = System.currentTimeMillis();
            bumpResidencyCount(newState);
            return;
        }
        if (newState.equals(currentResidencyState)) return;
        flushResidency();
        currentResidencyState = newState;
        bumpResidencyCount(newState);
        // Consume the pending seek window once the player is back to
        // steady-state playback; the seeking_time_ms accumulator gets
        // the wall-clock between SEEK and now, intentionally
        // overlapping the buffering_time_ms that covered the same
        // interval (Conviva CIRR/CIRT split).
        if ("playing".equals(newState) && seekingStartAtMs > 0) {
            long now = System.currentTimeMillis();
            long delta = Math.max(0, now - seekingStartAtMs);
            seekingTimeMs += delta;
            seekingStartAtMs = -1;
        }
    }

    private void bumpResidencyCount(String state) {
        switch (state) {
            case "playing":   playingCount++;   break;
            case "pausing":   pausingCount++;   break;
            case "buffering": bufferingCount++; break;
            case "stalling":  stallingCount++;  break;
            case "idling":    idlingCount++;    break;
            default: break;
        }
    }

    /** Read-only counters surfaced for the on-device DiagnosticHud. */
    public int getStallCount() { return stallCount; }
    public double getLastStallSeconds() { return lastStallDurationS; }
    public long getDroppedFrames() { return droppedFramesTotal; }
    public int getProfileShiftCount() { return profileShiftCount; }
    public String currentMappedState() { return mapState(); }
    public String currentMappedWaitingReason() { return mapWaitingReason(); }

    private String mapState() {
        // Lowercase canonical names — matches Apple PlaybackDiagnostics,
        // Android-test ExoPlayerTestApp, web embed, and Roku, so the
        // dashboard PLAYERSTATE lane shows the same colour for the same
        // state regardless of source platform.
        switch (player.getPlaybackState()) {
            case Player.STATE_IDLE: return "idle";
            case Player.STATE_ENDED: return "ended";
            case Player.STATE_BUFFERING:
                // Distinguish unexpected mid-play rebuffer ("stalled")
                // from initial pre-roll buffering. ExoPlayer doesn't
                // have a direct AVPlayerItemPlaybackStalled equivalent,
                // so we use the same first-frame + playWhenReady gate
                // that onStallStart uses. Initial-load buffering and
                // explicit-pause buffering stay as "buffering".
                return (firstFrameReported && player.getPlayWhenReady()) ? "stalled" : "buffering";
            case Player.STATE_READY:
                return player.isPlaying() ? "playing" : "paused";
            default:
                return "unknown";
        }
    }

    /**
     * Synthesise a waiting_reason string mirroring the
     * AVPlayer.reasonForWaitingToPlay we ship from iOS, so the
     * dashboard PLAYERSTATE tooltip can disambiguate causes
     * cross-platform. ExoPlayer doesn't expose an exact analog —
     * derive from playbackState + playWhenReady + firstFrameReported
     * + getPlaybackSuppressionReason. Empty string when there's no
     * meaningful reason (e.g. actively playing).
     */
    private String mapWaitingReason() {
        int state = player.getPlaybackState();
        if (state == Player.STATE_BUFFERING) {
            if (!firstFrameReported) return "initial";
            if (!player.getPlayWhenReady()) return "paused";
            return "rebuffer";
        }
        if (state == Player.STATE_READY && !player.isPlaying()) {
            int suppression = player.getPlaybackSuppressionReason();
            if (suppression == Player.PLAYBACK_SUPPRESSION_REASON_TRANSIENT_AUDIO_FOCUS_LOSS) {
                return "audio_focus_loss";
            }
            if (suppression != Player.PLAYBACK_SUPPRESSION_REASON_NONE) {
                return "suppressed_" + suppression;
            }
            if (!player.getPlayWhenReady()) return "paused";
        }
        return "";
    }

    private String resolveSessionId() {
        long now = System.currentTimeMillis();
        if (sessionId != null && !sessionId.isEmpty()
            && now - lastSessionLookupMs < SESSION_LOOKUP_INTERVAL_MS) {
            return sessionId;
        }
        String base = baseUrlProvider.get();
        if (base == null || base.isEmpty()) return null;
        HttpURLConnection conn = null;
        try {
            URL url = new URL(base + "/api/sessions");
            conn = (HttpURLConnection) url.openConnection();
            conn.setConnectTimeout(CONNECT_TIMEOUT_MS);
            conn.setReadTimeout(READ_TIMEOUT_MS);
            if (conn.getResponseCode() != 200) return null;
            StringBuilder sb = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(conn.getInputStream()))) {
                String line;
                while ((line = r.readLine()) != null) sb.append(line);
            }
            JSONArray arr = new JSONArray(sb.toString());
            for (int i = 0; i < arr.length(); i++) {
                JSONObject o = arr.getJSONObject(i);
                if (playerId.equals(o.optString("player_id"))) {
                    String sid = o.optString("session_id");
                    if (!sid.isEmpty()) {
                        sessionId = sid;
                        lastSessionLookupMs = now;
                        return sid;
                    }
                }
            }
        } catch (Exception ignored) {
        } finally {
            if (conn != null) conn.disconnect();
        }
        return null;
    }

    private void patchMetrics(String sid, JSONObject payload) {
        String base = baseUrlProvider.get();
        if (base == null || base.isEmpty()) return;
        HttpURLConnection conn = null;
        try {
            URL url = new URL(base + "/api/session/" + sid + "/metrics");
            conn = (HttpURLConnection) url.openConnection();
            conn.setConnectTimeout(CONNECT_TIMEOUT_MS);
            conn.setReadTimeout(READ_TIMEOUT_MS);
            conn.setRequestMethod("POST");
            conn.setRequestProperty("Content-Type", "application/json");
            conn.setDoOutput(true);

            JSONObject body = new JSONObject();
            body.put("set", payload);
            JSONArray keys = new JSONArray();
            Iterator<String> it = payload.keys();
            while (it.hasNext()) keys.put(it.next());
            body.put("fields", keys);

            try (OutputStream os = conn.getOutputStream()) {
                os.write(body.toString().getBytes(StandardCharsets.UTF_8));
            }
            conn.getResponseCode();
        } catch (Exception ignored) {
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    private static double roundSeconds(double value) {
        return Math.round(value * 1000.0) / 1000.0;
    }

    private static double round2(double value) {
        return Math.round(value * 100.0) / 100.0;
    }
}
