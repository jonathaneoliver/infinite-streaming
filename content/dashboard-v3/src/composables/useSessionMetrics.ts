/**
 * useSessionMetrics(opts) — port of the legacy testing-session.html
 * client-side metric pipeline. Reports player-side state (buffer,
 * position, video resolution, dropped frames, stalls, profile shifts,
 * errors) back to the proxy via the v1 ingestion endpoint
 *
 *     POST /api/session/{session_id}/metrics
 *
 * That endpoint is deliberately v1: it's the canonical telemetry path
 * for ALL player platforms (iOS, Apple TV, Android, Roku, web) and the
 * v2 PATCH endpoint is intentionally control-plane-only
 * (`PlayerPatch.additionalProperties: false`). The proxy translates v1
 * metric writes into v2 SSE `player.updated` events so the dashboard
 * sees the updates without anybody round-tripping through v2.
 *
 * Wiring lives here (not in VideoPlayerFrame.vue) because the same
 * pipeline applies whether the engine is hls.js, native HLS, or Shaka.
 *
 * Lifecycle: caller hands us reactive refs; we attach listeners when
 * videoEl/hlsInst become non-null and detach on unmount or change.
 */
import { onBeforeUnmount, ref, watch, type Ref } from 'vue';
import * as repo from '@/repo/v2-repo';

type Engine = 'hlsjs' | 'native' | 'shaka' | 'videojs' | 'auto';

export interface UseSessionMetricsOptions {
  playerId: Ref<string>;
  videoEl: Ref<HTMLVideoElement | null>;
  /** Live hls.js instance, or null when another engine is active. */
  hlsInst: Ref<any | null>;
  /** Current playback engine for telemetry attribution. */
  engine: Ref<Engine>;
  /** Heartbeat cadence in milliseconds. Matches legacy default. */
  heartbeatMs?: number;
}

/* ─── Counters local to one playback session ────────────────────────
 *  These would be component-instance scoped if declared inside the
 *  exported function. We *want* them per-instance: each VideoPlayerFrame
 *  has its own counters. The composable returns nothing — its only side
 *  effect is the POST stream. */

function detectBrowserFamily(): string {
  const ua = (navigator.userAgent || '').toLowerCase();
  if (ua.includes('firefox')) return 'firefox';
  if (ua.includes('edg/')) return 'edge';
  if (ua.includes('chrome')) return 'chrome';
  if (ua.includes('safari')) return 'safari';
  return 'unknown';
}

function fmtRes(w: number, h: number): string | null {
  if (!w || !h) return null;
  return `${w}x${h}`;
}

/** Round to 3 decimals. Matches legacy `toMetricSeconds`. */
function s3(v: number | null | undefined): number | null {
  if (v == null || !Number.isFinite(v)) return null;
  return Math.round(v * 1000) / 1000;
}

/** Round to 2 decimals. */
function n2(v: number | null | undefined): number | null {
  if (v == null || !Number.isFinite(v)) return null;
  return Math.round(v * 100) / 100;
}

export function useSessionMetrics(opts: UseSessionMetricsOptions) {
  const heartbeatMs = opts.heartbeatMs ?? 1000;

  /* ─── Per-playback counters (reset on engine swap) ───────────────── */
  let stallCount = 0;
  let stallTimeS = 0;
  let lastStallDurationS = 0;
  let stallStartedAt: number | null = null;
  let profileShiftCount = 0;
  let lastRenditionMbps: number | null = null;
  let currentRenditionMbps: number | null = null;
  let videoFirstFrameS: number | null = null;
  let videoPlayingTimeS: number | null = null;
  let playStartedAt: number | null = null;
  let playerEstimatedMbps: number | null = null;

  /* ─── v1 session_id resolution ───────────────────────────────────── */
  // Cached lookup: the v2 GET projects `raw_session.session_id`, the
  // legacy numeric id our POST URL needs. Refresh on player_id change.
  const sessionId = ref<string | null>(null);
  let lookupInFlight: Promise<void> | null = null;

  async function resolveSessionId(): Promise<string | null> {
    if (sessionId.value) return sessionId.value;
    if (!opts.playerId.value) return null;
    if (!lookupInFlight) {
      lookupInFlight = (async () => {
        try {
          const { player } = await repo.getPlayer(opts.playerId.value);
          const raw = (player as any).raw_session;
          const sid = raw?.session_id;
          if (sid != null) sessionId.value = String(sid);
        } catch (e: any) {
          console.warn('[USM] resolveSessionId: GET failed', e?.status, e?.message);
        } finally {
          lookupInFlight = null;
        }
      })();
    }
    await lookupInFlight;
    return sessionId.value;
  }

  /* ─── Payload builder ────────────────────────────────────────────── */
  function buildPayload(
    eventType: string,
    extra: Record<string, unknown> = {},
  ): Record<string, unknown> {
    const v = opts.videoEl.value;
    if (!v) return { ...extra };

    const current = Number(v.currentTime || 0);
    const buffered = v.buffered;
    const seekable = v.seekable;
    let bufferedEnd: number | null = null;
    let bufferDepth = 0;
    let seekableEnd: number | null = null;
    if (buffered && buffered.length > 0) {
      const end = buffered.end(buffered.length - 1);
      bufferedEnd = s3(end);
      bufferDepth = s3(Math.max(0, end - current)) ?? 0;
    }
    if (seekable && seekable.length > 0) {
      const end = seekable.end(seekable.length - 1);
      seekableEnd = s3(end);
    }
    const liveOffset = seekableEnd != null ? s3(Math.max(0, seekableEnd - current)) : null;

    // EXT-X-PROGRAM-DATE-TIME → wall-clock at the playhead. hls.js
    // surfaces this as `playingDate`; native HLS has no equivalent.
    let playheadWallclockMs: number | null = null;
    try {
      const h = opts.hlsInst.value;
      if (h && h.playingDate instanceof Date) {
        const d = h.playingDate;
        if (!Number.isNaN(d.getTime())) playheadWallclockMs = d.getTime();
      }
    } catch {
      /* ignore */
    }

    // Frame counters from VideoPlaybackQuality (Chrome, Safari, FF all
    // support this; spec-stable since 2017).
    let totalFrames: number | null = null;
    let droppedFrames: number | null = null;
    let displayedFrames: number | null = null;
    try {
      const q =
        typeof (v as any).getVideoPlaybackQuality === 'function'
          ? (v as any).getVideoPlaybackQuality()
          : null;
      if (q) {
        totalFrames = Number(q.totalVideoFrames) || 0;
        droppedFrames = Number(q.droppedVideoFrames) || 0;
        displayedFrames = Math.max(0, totalFrames - droppedFrames);
      }
    } catch {
      /* ignore */
    }

    // Active hls.js bandwidth estimate when available.
    try {
      const h = opts.hlsInst.value;
      if (h && typeof h.bandwidthEstimate === 'number' && h.bandwidthEstimate > 0) {
        playerEstimatedMbps = h.bandwidthEstimate / 1_000_000;
      }
    } catch {
      /* ignore */
    }

    const base: Record<string, unknown> = {
      player_metrics_source: 'web',
      player_metrics_browser_family: detectBrowserFamily(),
      player_metrics_playback_engine: opts.engine.value,
      player_metrics_last_event: eventType,
      player_metrics_trigger_type: eventType,
      player_metrics_event_time: new Date().toISOString(),
      player_metrics_state: v.paused ? 'paused' : v.ended ? 'ended' : 'playing',
      player_metrics_position_s: s3(current),
      player_metrics_playback_rate: n2(v.playbackRate || 0),
      player_metrics_buffer_depth_s: bufferDepth,
      player_metrics_buffer_end_s: bufferedEnd,
      player_metrics_seekable_end_s: seekableEnd,
      player_metrics_live_edge_s: seekableEnd,
      player_metrics_live_offset_s: liveOffset,
      player_metrics_playhead_wallclock_ms: playheadWallclockMs,
      player_metrics_true_offset_s:
        playheadWallclockMs != null ? s3((Date.now() - playheadWallclockMs) / 1000) : null,
      player_metrics_display_resolution: fmtRes(v.clientWidth, v.clientHeight),
      player_metrics_video_resolution: fmtRes(v.videoWidth, v.videoHeight),
      player_metrics_video_first_frame_time_s: s3(videoFirstFrameS),
      player_metrics_video_start_time_s: s3(videoPlayingTimeS),
      player_metrics_video_bitrate_mbps: n2(currentRenditionMbps),
      player_metrics_avg_network_bitrate_mbps: n2(playerEstimatedMbps),
      player_metrics_profile_shift_count: profileShiftCount,
      player_metrics_frames_displayed: displayedFrames,
      player_metrics_total_video_frames: totalFrames,
      player_metrics_dropped_frames: droppedFrames,
      player_metrics_stall_count: stallCount,
      player_metrics_stall_time_s: s3(stallTimeS),
      player_metrics_last_stall_time_s: s3(lastStallDurationS),
    };
    return { ...base, ...extra };
  }

  /* ─── Serialized POST queue (matches legacy `metricsTaskTail`) ────── */
  // Without this, concurrent fetch()es race; older POSTs can clobber
  // newer ones in the proxy's per-session state, producing zigzag
  // event_time on the SSE stream.
  let tail: Promise<unknown> = Promise.resolve();
  let postEnabled = true;

  function send(eventType: string, extra: Record<string, unknown> = {}): void {
    if (!postEnabled) return;
    const payload = buildPayload(eventType, extra);
    const fields = Object.keys(payload).filter((k) => payload[k] !== undefined);
    if (!fields.length) return;
    tail = tail
      .catch(() => {})
      .then(async () => {
        const sid = await resolveSessionId();
        if (!sid) return; // not yet registered; next heartbeat will retry
        try {
          await fetch(`/api/session/${encodeURIComponent(sid)}/metrics`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ set: payload, fields }),
          });
        } catch (err) {
          console.warn('[USM] POST failed', eventType, err);
        }
      });
  }

  /* ─── Listener attach/detach ─────────────────────────────────────── */
  type EventBinding = [HTMLVideoElement, string, EventListener];
  let videoBindings: EventBinding[] = [];
  let hlsBindings: Array<{ hls: any; event: string; fn: Function }> = [];
  let heartbeatTimer: ReturnType<typeof setInterval> | null = null;

  function detachVideo() {
    for (const [el, ev, fn] of videoBindings) el.removeEventListener(ev, fn);
    videoBindings = [];
  }

  function detachHls() {
    for (const { hls, event, fn } of hlsBindings) {
      try {
        hls.off(event, fn);
      } catch {
        /* ignore */
      }
    }
    hlsBindings = [];
  }

  function attachVideo(v: HTMLVideoElement) {
    // Reset per-playback counters whenever the <video> rebinds.
    stallCount = 0;
    stallTimeS = 0;
    lastStallDurationS = 0;
    stallStartedAt = null;
    profileShiftCount = 0;
    lastRenditionMbps = null;
    currentRenditionMbps = null;
    videoFirstFrameS = null;
    videoPlayingTimeS = null;
    playStartedAt = Date.now();

    const bind = (ev: string, fn: EventListener) => {
      v.addEventListener(ev, fn);
      videoBindings.push([v, ev, fn]);
    };

    bind('loadeddata', () => {
      if (videoFirstFrameS == null && playStartedAt != null) {
        videoFirstFrameS = (Date.now() - playStartedAt) / 1000;
      }
      send('loadeddata');
    });
    bind('playing', () => {
      if (videoPlayingTimeS == null && playStartedAt != null) {
        videoPlayingTimeS = (Date.now() - playStartedAt) / 1000;
      }
      // End-of-stall accounting on resume.
      if (stallStartedAt != null) {
        lastStallDurationS = (Date.now() - stallStartedAt) / 1000;
        stallTimeS += lastStallDurationS;
        stallStartedAt = null;
      }
      send('playing');
    });
    bind('pause', () => send('pause'));
    bind('ended', () => send('ended'));
    bind('seeked', () => send('seeked'));
    bind('ratechange', () => send('ratechange'));
    bind('waiting', () => {
      stallCount += 1;
      if (stallStartedAt == null) stallStartedAt = Date.now();
      send('waiting');
    });
    bind('stalled', () => {
      // Native stall event — also bump the counter, mirrors legacy.
      if (stallStartedAt == null) {
        stallStartedAt = Date.now();
        stallCount += 1;
      }
      send('stalled');
    });
    bind('error', () => {
      const code = v.error?.code;
      send('error', {
        player_metrics_error: code ? `video error ${code}` : 'video error',
        player_metrics_error_code: code ?? null,
      });
    });
  }

  function attachHls(h: any) {
    if (!h || !h.constructor) return;
    const Hls = (window as any).Hls;
    if (!Hls?.Events) return;

    const bind = (event: string, fn: Function) => {
      h.on(event, fn);
      hlsBindings.push({ hls: h, event, fn });
    };

    bind(Hls.Events.MANIFEST_PARSED, () => send('manifest_parsed'));

    bind(Hls.Events.LEVEL_SWITCHED, (_e: any, data: any) => {
      try {
        const lvl = h.levels?.[data?.level];
        if (lvl && lvl.bitrate) {
          const numeric = lvl.bitrate / 1_000_000;
          if (lastRenditionMbps != null && Math.abs(numeric - lastRenditionMbps) > 0.01) {
            profileShiftCount += 1;
            send('video_bitrate_change', {
              player_metrics_video_bitrate_from_mbps: n2(lastRenditionMbps),
              player_metrics_video_bitrate_to_mbps: n2(numeric),
              player_metrics_profile_shift_count: profileShiftCount,
            });
          }
          lastRenditionMbps = numeric;
          currentRenditionMbps = numeric;
        }
      } catch (err) {
        console.warn('[useSessionMetrics] LEVEL_SWITCHED handler threw', err);
      }
    });

    bind(Hls.Events.BUFFER_STALLED, () => {
      stallCount += 1;
      if (stallStartedAt == null) stallStartedAt = Date.now();
      send('buffer_stalled');
    });

    bind(Hls.Events.ERROR, (_e: any, data: any) => {
      if (!data?.fatal) return;
      send('error', {
        player_metrics_error: `hls ${data?.type ?? '?'} ${data?.details ?? '?'}`.trim(),
        player_metrics_error_code: data?.type ?? null,
      });
    });
  }

  /* ─── Wire reactive refs ─────────────────────────────────────────── */
  watch(
    opts.videoEl,
    (v, old) => {
      if (old) detachVideo();
      if (v) attachVideo(v);
    },
    { immediate: true },
  );

  watch(
    opts.hlsInst,
    (h, old) => {
      if (old) detachHls();
      if (h) attachHls(h);
    },
    { immediate: true },
  );

  // Re-resolve session id when the player_id changes.
  watch(opts.playerId, () => {
    sessionId.value = null;
    lookupInFlight = null;
  });

  /* ─── 1Hz heartbeat ───────────────────────────────────────────────── */
  heartbeatTimer = setInterval(() => {
    if (opts.videoEl.value) send('heartbeat');
  }, heartbeatMs);

  /* ─── Cleanup ─────────────────────────────────────────────────────── */
  onBeforeUnmount(() => {
    postEnabled = false;
    if (heartbeatTimer != null) clearInterval(heartbeatTimer);
    detachVideo();
    detachHls();
  });

  return {
    /** Force an out-of-band metrics POST (e.g. after a manual retry). */
    flush(eventType: string, extra?: Record<string, unknown>) {
      send(eventType, extra ?? {});
    },
    /** Resolved v1 session id (null until first GET resolves). */
    sessionId,
  };
}
