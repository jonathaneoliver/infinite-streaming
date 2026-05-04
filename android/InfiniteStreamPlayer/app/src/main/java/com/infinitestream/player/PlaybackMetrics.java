package com.infinitestream.player;

import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import androidx.annotation.OptIn;
import androidx.media3.common.C;
import androidx.media3.common.Format;
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
        if (!firstFrameReported) return;
        if (stallStartAtMs > 0) return;
        stallStartAtMs = System.currentTimeMillis();
        sendEvent("stall_start", Collections.<String, Object>emptyMap());
    }

    /** Called when a stall ends (back to playing). */
    public void onStallEnd() {
        if (stallStartAtMs <= 0) return;
        double duration = (System.currentTimeMillis() - stallStartAtMs) / 1000.0;
        stallStartAtMs = -1;
        if (duration <= 0) return;
        stallCount++;
        totalStallTimeS += duration;
        lastStallDurationS = roundSeconds(duration);
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_last_stall_time_s", lastStallDurationS);
        sendEvent("stall_end", extra);
    }

    /**
     * Called on every transition into ExoPlayer STATE_BUFFERING. Distinct
     * from onStallStart, which is gated on first-frame + playWhenReady so
     * initial loads and short pre-roll buffering don't register as stalls.
     */
    public void onBufferingStart() {
        if (buffering) return;
        buffering = true;
        sendEvent("buffering_start", Collections.<String, Object>emptyMap());
    }

    /** Called when ExoPlayer leaves STATE_BUFFERING. */
    public void onBufferingEnd() {
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
        double elapsed = roundSeconds((System.currentTimeMillis() - playbackStartAtMs) / 1000.0);
        videoFirstFrameSeconds = elapsed;
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_video_first_frame_time_s", elapsed);
        sendEvent("video_first_frame", extra);
    }

    /** Called from AnalyticsListener.onVideoInputFormatChanged. */
    public void onVideoFormatChanged(Format format) {
        if (format == null) return;
        double mbps = format.bitrate > 0 ? round2(format.bitrate / 1_000_000.0) : 0;
        if (mbps <= 0) return;
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
    public void onPlayerError(String message) {
        Map<String, Object> extra = new HashMap<>();
        extra.put("player_metrics_error", message == null ? "" : message);
        sendEvent("error", extra);
        requestHarSnapshot("player_error", 0, /* force= */ false);
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
            double elapsed = roundSeconds((System.currentTimeMillis() - playbackStartAtMs) / 1000.0);
            videoStartTimeSeconds = elapsed;
            Map<String, Object> extra = new HashMap<>();
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
            }
            if (videoStartTimeSeconds != null) {
                p.put("player_metrics_video_start_time_s", videoStartTimeSeconds);
            }

            for (Map.Entry<String, Object> e : extra.entrySet()) {
                p.put(e.getKey(), e.getValue());
            }
        } catch (JSONException e) {
            return null;
        }
        return p;
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
