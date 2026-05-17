<script setup lang="ts">
/**
 * EventsTimeline.vue — multi-section swim-lane view of the session's
 * lifecycle. Mirrors the legacy `renderEventsTimelineChart` layout:
 *
 *   PLAYER section
 *     - VARIANT     (per (resolution, bitrate) sub-lane)
 *     - DISPLAY RES
 *     - PLAYERSTATE
 *     - PLAYBACK    (FIRST_FRAME / RESTART / TIMEJUMP / SHIFT_UP /
 *                    SHIFT_DOWN / PLAYBACK_START)
 *     - IMPAIRMENT  (STALL / FROZEN / SEGMENT_STALL / ERROR)
 *   CONTROL section
 *     - CONTROL     (CONTROL_CHANGE, USER_MARKED — operator + control
 *                    revision changes)
 *   SERVER section
 *     - LOOP SERVER
 *
 * Events are derived client-side from successive PlayerRecord ticks
 * (no separate events API). Ranges are coalesced for "heartbeat"-style
 * data (PLAYERSTATE, VARIANT, DISPLAY_RES) so an entire span of the
 * same value appears as one continuous segment.
 *
 * The legacy backed this with `parseBandwidthChartEventType` on the
 * SSE stream — that classifier doesn't exist in v2 yet. We approximate
 * with the diff-based detectors below; once the v2 server publishes an
 * `events` lane on the SSE feed, this component drops the diffing in
 * favour of consuming that.
 */
import { computed, onBeforeUnmount, ref, toRef, watch } from 'vue';
import { ensureVisTimeline } from '@/composables/useChartJs';
import { useChartCoordination } from '@/composables/useChartCoordination';
import type { Stream } from '@/composables/useSessionTimeSeries';

interface LaneCfg { label: string; color: string }
const EVENT_LANES: Record<string, LaneCfg> = {
  CONTROL:     { label: 'CONTROL',     color: '#7c3aed' },
  // Key is historically DISPLAY_RES (matches legacy session-shell.js)
  // but the value the lane shows is `pm.video_resolution` — relabel
  // accordingly so the on-screen header isn't misleading.
  DISPLAY_RES: { label: 'VIDEO RES', color: '#0ea5e9' },
  PLAYERSTATE: { label: 'PLAYERSTATE', color: '#6b7280' },
  PLAYBACK:    { label: 'PLAYBACK',    color: '#16a34a' },
  IMPAIRMENT:  { label: 'IMPAIRMENT',  color: '#000000' },
  LOOP_SERVER: { label: 'LOOP SERVER', color: '#84cc16' },
};

const PLAYER_STATE_COLOR: Record<string, string> = {
  playing:   '#16a34a',
  buffering: '#f59e0b',
  stalled:   '#dc2626',
  paused:    '#9333ea',
  idle:      '#6b7280',
  ended:     '#1f2937',
  unknown:   '#d1d5db',
};
function playerStateColor(s: string | null | undefined): string {
  return PLAYER_STATE_COLOR[String(s ?? '').trim().toLowerCase()] || '#d1d5db';
}
function variantColor(mbps: number): string {
  if (!Number.isFinite(mbps) || mbps <= 0) return '#9ca3af';
  if (mbps < 1)  return '#dc2626';
  if (mbps < 2)  return '#ea580c';
  if (mbps < 4)  return '#f59e0b';
  if (mbps < 8)  return '#84cc16';
  if (mbps < 16) return '#16a34a';
  return '#10b981';
}
function displayResColor(res: string | null | undefined): string {
  const r = String(res ?? '').toLowerCase();
  if (r.includes('2160')) return '#7c3aed';
  if (r.includes('1080')) return '#0ea5e9';
  if (r.includes('720'))  return '#22c55e';
  if (r.includes('540'))  return '#eab308';
  if (r.includes('480'))  return '#f97316';
  if (r.includes('360'))  return '#ef4444';
  return '#6b7280';
}

const props = defineProps<{
  playerId: string;
  /** Samples stream from SessionDisplay's useSessionTimeSeries model.
   *  Each row is a CH session_snapshots projection (lanes_v1 bundle).
   *  EventsTimeline derives swim-lane segments from successive rows. */
  samplesStream: Stream<Record<string, unknown>>;
}>();
const coord = useChartCoordination(toRef(props, 'playerId'));

/** Adapter — map a CH session_snapshots row (wire shape from the v3
 *  /api/v3/timeseries endpoint) to the small subset of fields ingest()
 *  reads. Keeps ingest() agnostic to the row shape so the next storage
 *  layer (materialised lanes_v2) can change column names without
 *  touching the lane-derivation logic. */
interface IngestRow {
  ts: number;
  state: string;
  waitingReason: string;
  videoResolution: string;
  videoBitrateMbps: number | null;
  stalls: number | null;
  droppedFrames: number | null;
  error: string;
  firstFrameTimeS: number | null;
  videoStartTimeS: number | null;
  loopCountServer: number | null;
  controlRevision: string | null;
  playId: string | null;
  manifestVariants: { bandwidth?: number; resolution?: string }[] | null;
}

function tsOfRow(row: Record<string, unknown>): number {
  const v = row.ts;
  if (typeof v === 'number') return v;
  if (typeof v !== 'string' || !v) return NaN;
  if (v.length > 10 && v.charAt(10) === ' ') {
    return Date.parse(v.replace(' ', 'T') + 'Z');
  }
  return Date.parse(v);
}

function num(v: unknown): number | null {
  if (v == null) return null;
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v === 'string') { const n = Number(v); return Number.isFinite(n) ? n : null; }
  return null;
}

function chRowToIngest(row: Record<string, unknown>): IngestRow | null {
  const t = tsOfRow(row);
  if (!Number.isFinite(t)) return null;
  // manifest_variants is stored as a JSON string in CH. Parse once
  // here so the lane resolver below can iterate it like an array.
  let variants: IngestRow['manifestVariants'] = null;
  const mv = row.manifest_variants;
  if (typeof mv === 'string' && mv.length > 0 && mv !== 'null') {
    try { const parsed = JSON.parse(mv); if (Array.isArray(parsed)) variants = parsed; }
    catch { /* ignore malformed manifest JSON */ }
  } else if (Array.isArray(mv)) {
    variants = mv as IngestRow['manifestVariants'];
  }
  // control_revision is UInt64 in CH → JSON string from JSONEachRow.
  // Stringify defensively so the diff comparator stays stable across
  // numeric vs. string representations.
  const rev = row.control_revision;
  const controlRevision = rev == null ? null : String(rev);
  return {
    ts: t,
    state: String(row.player_state ?? '').trim(),
    waitingReason: String(row.waiting_reason ?? '').trim(),
    videoResolution: String(row.video_resolution ?? '').trim(),
    videoBitrateMbps: num(row.video_bitrate_mbps),
    stalls: num(row.stall_count),
    droppedFrames: num(row.dropped_frames),
    error: String(row.player_error ?? ''),
    firstFrameTimeS: num(row.video_first_frame_time_s),
    videoStartTimeS: num(row.video_start_time_s),
    loopCountServer: num(row.loop_count_server),
    controlRevision,
    playId: typeof row.play_id === 'string' ? row.play_id : null,
    manifestVariants: variants,
  };
}

const container = ref<HTMLDivElement | null>(null);

let vis: any = null;
let timeline: any = null;
let itemsDS: any = null;
let groupsDS: any = null;
let nextId = 1;
let suppressNextRangeChange = false;
/** Set during a user pan/zoom drag (between vis-timeline's
 *  `rangechange` and `rangechanged`). The coord-version watcher
 *  skips its setWindow while this is true so an incoming live
 *  sample doesn't yank the timeline back to the live edge in the
 *  middle of the operator's drag. */
let userInteracting = false;
/** Tolerance for "right edge is at the live sample" — absorbs the gap
 *  between when a wheel event fires and when the next sample arrives.
 *  Same 2s tolerance the brush-drop-at-live heuristic uses. */
const LIVE_EDGE_TOLERANCE_MS = 2000;
let userInteracted = false;

interface TimelineRangeItem {
  id: number;
  group: string;
  content: string;
  start: number;
  end: number;
  ts0: number;       // original start, so we can extend
  style: string;
  type: 'range';
  title: string;
}
interface TimelinePointItem {
  id: number;
  group: string;
  content: string;
  start: number;
  style: string;
  type: 'point';
  title: string;
}
type TimelineItem = TimelineRangeItem | TimelinePointItem;

// Internal state: track current "open" ranges per lane (key → item).
// For POINT lanes (PLAYBACK / IMPAIRMENT / CONTROL / LOOP_SERVER) we
// still use diff-based emit. For STATEFUL lanes (PLAYERSTATE /
// DISPLAY_RES / VARIANT) we store every heartbeat in `statefulEvents`
// and rebuild the items on every render — mirrors the legacy
// session-shell.js `pushRanges()` pattern. The incremental
// "open-range" approach we tried first was fragile: any spurious
// state flicker (case, whitespace, play_id resynth) opened a new
// segment instead of extending. The legacy approach is robust to
// out-of-order events and watcher re-fires because rendering is a
// pure function of the events array.
const openRanges: Record<string, TimelineRangeItem | null> = {};
const variantOrder: { key: string; mbps: number; resolution: string }[] = [];
const items: TimelineItem[] = [];

interface StatefulEvent {
  ts: number;
  type: 'PLAYERSTATE' | 'DISPLAY_RES' | 'VARIANT';
  // PLAYERSTATE
  state?: string;
  reason?: string;
  // DISPLAY_RES
  resolution?: string;
  // VARIANT
  mbps?: number;
  variantRes?: string;
  variantKey?: string;
}

const statefulEvents: StatefulEvent[] = [];
// IDs assigned to coalesced range items so we can replace cleanly on
// every render. Range items are tagged by the lane key + rendered
// `range:<key>:<n>` so we can detect & remove them before re-adding.
const STATEFUL_LANES = new Set(['PLAYERSTATE', 'DISPLAY_RES']);
// Variant lane keys are dynamic (VARIANT::<res>|<mbps>); tracked
// via `variantOrder` and re-rendered alongside.

// Sample-tracking memory for diff-based POINT-event detection.
let prevStalls: number | null = null;
let prevDropped: number | null = null;
let prevLoopServer: number | null = null;
let prevControlRev: string | null = null;
let prevError: string | null = null;
let prevPlayId: string | null = null;
let prevFirstFrame: number | null = null;
let prevVideoStart: number | null = null;
let prevVariantMbps: number | null = null;
// Watermark of the latest CH row already fed through `ingest()`. The
// samples-stream watcher uses this to consume only NEW rows on each
// version bump (the stream cache holds the full backfill + live tail).
let lastIngestedMs = -Infinity;

function fmtTime(ms: number): string {
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}`;
}

function variantLabel(mbps: number, resolution: string): string {
  const m = mbps.toFixed(2);
  return `${resolution} · ${m} Mbps`;
}

/** Find the canonical resolution for a given bitrate by consulting the
 *  manifest's variant ladder. Mirrors the legacy session-shell.js
 *  `manifestResolutionForBitrateFromVariants`: the bitrate match is
 *  tolerant (±max(0.5 Mbps, 5% of the variant's Mbps)) so EWMA drift
 *  doesn't lose the match. Returns '' if no variant is close enough,
 *  so the caller can fall back to the player-reported resolution. */
function manifestResolutionForBitrate(
  variants: IngestRow['manifestVariants'],
  targetMbps: number,
): string {
  if (!Number.isFinite(targetMbps)) return '';
  if (!variants || variants.length === 0) return '';
  let best: string | null = null;
  let bestDelta = Infinity;
  for (const v of variants) {
    const vBw = Number(v?.bandwidth ?? 0);
    if (!Number.isFinite(vBw) || vBw <= 0) continue;
    const vMbps = vBw / 1_000_000;
    const delta = Math.abs(vMbps - targetMbps);
    const tol = Math.max(0.5, vMbps * 0.05);
    if (delta < tol && delta < bestDelta) {
      bestDelta = delta;
      const r = String(v?.resolution ?? '').trim();
      if (r) best = r;
    }
  }
  return best ?? '';
}

function laneClose(key: string, t: number) {
  const cur = openRanges[key];
  if (!cur) return;
  cur.end = t;
  const dur = ((t - cur.ts0) / 1000).toFixed(1);
  cur.title = `${EVENT_LANES[key]?.label ?? key}: ${cur.content}\n${fmtTime(cur.ts0)} → ${fmtTime(t)} (${dur}s)`;
  itemsDS?.update(cur);
  openRanges[key] = null;
}

function laneOpen(key: string, t: number, label: string, color: string) {
  const id = nextId++;
  const item: TimelineRangeItem = {
    id,
    group: key,
    content: label,
    start: t,
    end: t + 1,
    ts0: t,
    type: 'range',
    title: `${EVENT_LANES[key]?.label ?? key}: ${label}\n${fmtTime(t)}`,
    style: `background-color: ${color}; border-color: ${color}; color: #fff;`,
  };
  openRanges[key] = item;
  items.push(item);
  itemsDS?.add(item);
}

function variantLaneId(mbps: number, resolution: string): string {
  return `VARIANT::${resolution}|${mbps}`;
}

function ensureVariantLane(mbps: number, resolution: string) {
  const key = variantLaneId(mbps, resolution);
  if (variantOrder.find((v) => v.key === key)) return key;
  variantOrder.push({ key, mbps, resolution });
  // Sort descending by mbps so the ladder runs highest at top.
  variantOrder.sort((a, b) => b.mbps - a.mbps);
  rebuildGroups();
  return key;
}

function pushPoint(group: string, t: number, label: string, color: string, detail?: string) {
  const id = nextId++;
  const item: TimelinePointItem = {
    id,
    group,
    content: '',
    start: t,
    type: 'point',
    title: detail ? `${label}${detail}\n${fmtTime(t)}` : `${label}\n${fmtTime(t)}`,
    style: `background-color: ${color}; border-color: ${color};`,
  };
  items.push(item);
  itemsDS?.add(item);
}

function rebuildGroups() {
  if (!groupsDS) return;
  // One sub-lane PER ABR rung (legacy parity). The (mbps, res) key
  // for each lane uses the manifest's canonical resolution lookup so
  // a single rung never spans multiple lanes even if the player's
  // reported `video_resolution` flickers during a switch.
  const variantGroups = variantOrder.map((v) => ({
    id: v.key,
    content: variantLabel(v.mbps, v.resolution),
  }));
  const groups = [
    { id: 'PLAYER_SECTION', content: 'PLAYER', nestedGroups: [
      ...variantGroups.map((g) => g.id),
      'DISPLAY_RES', 'PLAYERSTATE', 'PLAYBACK', 'IMPAIRMENT',
    ] },
    ...variantGroups,
    { id: 'DISPLAY_RES', content: EVENT_LANES.DISPLAY_RES.label },
    { id: 'PLAYERSTATE', content: EVENT_LANES.PLAYERSTATE.label },
    { id: 'PLAYBACK',    content: EVENT_LANES.PLAYBACK.label },
    { id: 'IMPAIRMENT',  content: EVENT_LANES.IMPAIRMENT.label },
    { id: 'CONTROL_SECTION', content: 'CONTROL', nestedGroups: ['CONTROL'] },
    { id: 'CONTROL', content: EVENT_LANES.CONTROL.label },
    { id: 'SERVER_SECTION',  content: 'SERVER',  nestedGroups: ['LOOP_SERVER'] },
    { id: 'LOOP_SERVER', content: EVENT_LANES.LOOP_SERVER.label },
  ];
  groupsDS.clear();
  groupsDS.add(groups);
}

// Serialise the init so concurrent watch callbacks don't each create
// their own Timeline against the same container. `watch(player, ...,
// { immediate: true })` fires synchronously on mount AND on every
// player tick — both calls await ensureVisTimeline() before the first
// has populated `timeline`, so without the shared promise BOTH callers
// proceed to `new vis.Timeline(...)`. The second instance silently
// overlays the first → duplicate-looking swim lanes.
let ensurePromise: Promise<void> | null = null;
async function ensureTimeline(): Promise<void> {
  if (timeline) return;
  if (!container.value) return;
  if (ensurePromise) return ensurePromise;
  ensurePromise = (async () => {
    try {
      vis = await ensureVisTimeline();
      if (!container.value || timeline) return;
      itemsDS = new vis.DataSet([]);
      groupsDS = new vis.DataSet([]);
      rebuildGroups();
      const vp = coord.effectiveRange.value;
      timeline = new vis.Timeline(container.value, itemsDS, groupsDS, {
        stack: false,
        showCurrentTime: true,
        zoomMin: 1_000,
        zoomMax: 24 * 3600 * 1000,
        zoomKey: 'altKey',
        moveable: true,
        zoomable: true,
        margin: { item: 4 },
        orientation: { axis: 'top' },
        start: new Date(vp.min),
        end: new Date(vp.max),
      });
      // Pause/live transitions come from the Live toggle button, the
      // brush rail, and the rangechanged handler below (pan / zoom
      // auto-pin). Clicks on the strip itself do nothing — earlier
      // versions toggled pause on click but it produced too many
      // accidental pauses.
      // Track active drag so the coord-version watcher can skip its
      // setWindow while the operator is mid-pan. `rangechange` (no
      // 'd') fires continuously during drag; `rangechanged` fires
      // once at release.
      timeline.on('rangechange', (rc: any) => {
        if (rc?.byUser) userInteracting = true;
      });
      timeline.on('rangechanged', (rc: any) => {
        userInteracting = false;
        // The suppress flag is only for our own programmatic setWindow
        // calls (which fire rangechanged with byUser=false). NEVER let
        // it consume a real user pan — earlier versions did, and when
        // a new sample arrived mid-drag the version watcher would set
        // the flag, then the user's pan-end rangechanged got dropped
        // and the chart snapped back to live.
        if (!rc?.byUser) {
          if (suppressNextRangeChange) suppressNextRangeChange = false;
          return;
        }
        userInteracted = true;
        const a = rc.start instanceof Date ? rc.start.getTime() : Date.parse(rc.start);
        const b = rc.end instanceof Date ? rc.end.getTime() : Date.parse(rc.end);
        if (!Number.isFinite(a) || !Number.isFinite(b)) return;
        // Snap-back-to-live: if a mouse-anchored zoom or pan ends with
        // the right edge at the live sample, return to live tracking
        // — drop the sticky viewport, preserve the zoom span via
        // liveSpanMs, and stay unpaused. Any further Alt+wheel will
        // then take the left-edge-only path.
        const lastTs = coord.state.lastSampleMs;
        if (lastTs && b >= lastTs - LIVE_EDGE_TOLERANCE_MS) {
          coord.setViewport(null);
          coord.setLiveSpanMs(b - a);
          return;
        }
        if (!coord.state.paused) coord.setPaused(true);
        coord.setViewport({ min: a, max: b });
      });
      installLiveWheelAnchor();
    } finally {
      // Hold the resolved promise around so subsequent calls short-circuit
      // via the `if (timeline) return` check at the top.
    }
  })();
  return ensurePromise;
}

/* ─── Per-tick event derivation ─────────────────────────────────── */
function ingest(r: IngestRow) {
  const t = r.ts;
  coord.noteSample(t);

  // Track play_id transitions for the POINT-event diff trackers (so
  // e.g. STALL counters reset per-play). We DO NOT wipe
  // `statefulEvents` here — the synthetic play_id can flicker when
  // the proxy projects different variant manifest URLs between
  // metric ticks, and that flicker would otherwise erase the whole
  // PLAYERSTATE / VARIANT / DISPLAY_RES history (visible as the
  // PLAYERSTATE bar suddenly vanishing). Real "new play" transitions
  // surface naturally via `player_state` going through
  // idle/loading/playing — that's what drives the lane segmentation.
  if (r.playId !== prevPlayId) {
    prevPlayId = r.playId;
    prevStalls = prevDropped = null;
    prevLoopServer = null;
    prevError = null;
    prevFirstFrame = prevVideoStart = null;
  }

  // STATEFUL LANES — push every heartbeat as an event into a flat
  // array. The renderer coalesces runs of same-label entries below.
  if (r.state) {
    statefulEvents.push({ ts: t, type: 'PLAYERSTATE', state: r.state, reason: r.waitingReason });
  }

  // DISPLAY_RES — the decoded video resolution being displayed.
  if (r.videoResolution) {
    statefulEvents.push({ ts: t, type: 'DISPLAY_RES', resolution: r.videoResolution });
  }

  // VARIANT — keyed on the MANIFEST's canonical resolution for the
  // rung so transient `video_resolution` flicker during a switch
  // doesn't create phantom lanes.
  const mbpsRaw = r.videoBitrateMbps;
  if (mbpsRaw != null && mbpsRaw > 0) {
    const mbpsRounded = Math.round(mbpsRaw * 10) / 10;
    const variantRes = manifestResolutionForBitrate(r.manifestVariants, mbpsRounded);
    if (variantRes) {
      if (prevVariantMbps != null) {
        if (mbpsRaw > prevVariantMbps + 0.01) {
          pushPoint('PLAYBACK', t, 'SHIFT UP', '#3b82f6', `\n${prevVariantMbps.toFixed(2)} → ${mbpsRaw.toFixed(2)} Mbps`);
        } else if (mbpsRaw < prevVariantMbps - 0.01) {
          pushPoint('PLAYBACK', t, 'SHIFT DOWN', '#ef4444', `\n${prevVariantMbps.toFixed(2)} → ${mbpsRaw.toFixed(2)} Mbps`);
        }
      }
      prevVariantMbps = mbpsRaw;
      const key = variantLaneId(mbpsRounded, variantRes);
      ensureVariantLane(mbpsRounded, variantRes);
      statefulEvents.push({
        ts: t,
        type: 'VARIANT',
        mbps: mbpsRounded,
        variantRes,
        variantKey: key,
      });
    }
  }

  // IMPAIRMENT — STALL on counter increments; ERROR on string change;
  // FROZEN on dropped-frame surge (heuristic).
  if (r.stalls != null && prevStalls != null && r.stalls > prevStalls) {
    pushPoint('IMPAIRMENT', t, 'STALL', '#000000', `\n+${r.stalls - prevStalls} (total ${r.stalls})`);
  }
  if (r.droppedFrames != null && prevDropped != null && r.droppedFrames > prevDropped + 10) {
    pushPoint('IMPAIRMENT', t, 'FROZEN', '#4c1d95', `\n+${r.droppedFrames - prevDropped} dropped`);
  }
  if (r.error && r.error !== prevError) {
    pushPoint('IMPAIRMENT', t, 'ERROR', '#e11d48', `\n${r.error}`);
    prevError = r.error;
  }
  if (r.stalls != null) prevStalls = r.stalls;
  if (r.droppedFrames != null) prevDropped = r.droppedFrames;

  // PLAYBACK — FIRST_FRAME + START TIME on first observation per play.
  // (Legacy RESTART event was driven by `player_restarts`, which isn't
  // persisted in CH; drop until the schema gains it.)
  if (r.firstFrameTimeS != null && r.firstFrameTimeS > 0 && prevFirstFrame !== r.firstFrameTimeS) {
    pushPoint('PLAYBACK', t, 'FIRST FRAME', '#14b8a6', `\n${r.firstFrameTimeS.toFixed(3)}s`);
    prevFirstFrame = r.firstFrameTimeS;
  }
  if (r.videoStartTimeS != null && r.videoStartTimeS > 0 && prevVideoStart !== r.videoStartTimeS) {
    pushPoint('PLAYBACK', t, 'START TIME', '#15803d', `\n${r.videoStartTimeS.toFixed(3)}s`);
    prevVideoStart = r.videoStartTimeS;
  }

  // SERVER — LOOP increments.
  if (r.loopCountServer != null && prevLoopServer != null && r.loopCountServer > prevLoopServer) {
    pushPoint('LOOP_SERVER', t, 'LOOP', '#84cc16', `\n+${r.loopCountServer - prevLoopServer} (total ${r.loopCountServer})`);
  }
  if (r.loopCountServer != null) prevLoopServer = r.loopCountServer;

  // CONTROL — record any control_revision change.
  if (r.controlRevision && r.controlRevision !== '0' && r.controlRevision !== prevControlRev) {
    if (prevControlRev != null && prevControlRev !== '0') {
      pushPoint('CONTROL', t, 'CONTROL CHANGE', '#7c3aed', `\n${prevControlRev} → ${r.controlRevision}`);
    }
    prevControlRev = r.controlRevision;
  }

  scheduleStatefulRender(t);
}

/** Adaptive render throttle — matches the pattern in MetricsLineChart.
 *  renderStatefulLanes walks `statefulEvents` to coalesce runs into
 *  vis-timeline items; that walk is O(events). For a 2h archive
 *  replay we accumulate 5–10k events. At 1 Hz redraw the page burns
 *  noticeable CPU on the event timeline alone, so back off as the
 *  event array grows. */
let pendingRenderTimer: number | null = null;
let lastRenderAt = 0;
let pendingRenderTs = 0;
function pickRenderThrottleMs(): number {
  const n = statefulEvents.length;
  if (n >= 10_000) return 10_000;
  if (n >= 2_500) return 5_000;
  if (n >= 500) return 2_000;
  return 1_000;
}
function scheduleStatefulRender(ts: number) {
  pendingRenderTs = ts;
  if (pendingRenderTimer != null) return;
  const now = Date.now();
  const throttleMs = pickRenderThrottleMs();
  const dueAt = lastRenderAt + throttleMs;
  const delay = Math.max(0, dueAt - now);
  pendingRenderTimer = window.setTimeout(() => {
    pendingRenderTimer = null;
    lastRenderAt = Date.now();
    renderStatefulLanes(pendingRenderTs);
  }, delay);
}

/* ─── Stateful-lane render (legacy pushRanges pattern) ─────────────
 *
 * The events array is the source of truth — every metric tick has
 * appended one entry per active lane. This function walks the array,
 * coalesces runs of same-label events into ranges, and atomically
 * replaces the items in the DataSet. Idempotent: calling it multiple
 * times with no new events produces the same output. Matches the
 * legacy session-shell.js `pushRanges()` shape.
 */
function statefulItemId(lane: string, runIndex: number): string {
  return `stateful:${lane}:${runIndex}`;
}

function renderStatefulLanes(nowMs: number) {
  if (!itemsDS) return;

  // Build the desired set of stateful items. We then diff against the
  // current items in the DataSet and apply ONLY the changes — no
  // remove-and-readd flicker. With diff-based updates the bar never
  // visually disappears, even between heartbeat ticks.
  const desired: TimelineRangeItem[] = [];

  function coalesce(
    type: StatefulEvent['type'],
    laneFor: (e: StatefulEvent) => string,
    labelFor: (e: StatefulEvent) => string,
    colorFor: (e: StatefulEvent) => string,
  ) {
    const seq = statefulEvents
      .filter((e) => e.type === type)
      .sort((a, b) => a.ts - b.ts);
    let i = 0;
    let runIndex = 0;
    while (i < seq.length) {
      const start = seq[i].ts;
      const lane = laneFor(seq[i]);
      const label = labelFor(seq[i]);
      const color = colorFor(seq[i]);
      let j = i + 1;
      while (j < seq.length && labelFor(seq[j]) === label && laneFor(seq[j]) === lane) j++;
      // End at the next event's ts or `nowMs` for the last run so the
      // bar stretches across the visible window.
      const end = j < seq.length ? seq[j].ts : nowMs;
      const durSec = ((end - start) / 1000).toFixed(1);
      desired.push({
        id: statefulItemId(type, runIndex++) as any,
        group: lane,
        content: label,
        start,
        end: Math.max(end, start + 1),
        ts0: start,
        type: 'range',
        title: `${type}: ${label}\n${fmtTime(start)} → ${fmtTime(end)} (${durSec}s)`,
        style: `background-color: ${color}; border-color: ${color}; color: #fff;`,
      });
      i = j;
    }
  }

  coalesce(
    'PLAYERSTATE',
    () => 'PLAYERSTATE',
    (e) => (e.reason ? `${e.state} (${e.reason})` : (e.state ?? '?')),
    (e) => playerStateColor(e.state ?? null),
  );
  coalesce(
    'DISPLAY_RES',
    () => 'DISPLAY_RES',
    (e) => e.resolution ?? '?',
    (e) => displayResColor(e.resolution ?? null),
  );
  coalesce(
    'VARIANT',
    (e) => e.variantKey ?? 'VARIANT',
    () => '', // ranges are blank bars; label sits on the group header
    (e) => variantColor(e.mbps ?? 0),
  );

  // Atomic apply: update writes-or-adds (vis DataSet.update upserts
  // by id), then we remove any stateful items whose ids are no
  // longer in `desired`.
  const desiredIds = new Set(desired.map((d) => d.id as unknown as string));
  if (desired.length) {
    itemsDS.update(desired as any);
  }
  const stale = itemsDS.get({ filter: (it: any) => typeof it.id === 'string' && it.id.startsWith('stateful:') });
  if (stale && stale.length) {
    const remove: any[] = [];
    for (const it of stale) {
      if (!desiredIds.has((it as any).id)) remove.push((it as any).id);
    }
    if (remove.length) itemsDS.remove(remove);
  }
}

/* ─── Samples-stream consumer ──────────────────────────────────────
 *
 * Single feed: drain new rows from the unified time-series cache on
 * every version bump. `lastIngestedMs` is the high-water mark so we
 * only feed CH rows we haven't seen yet — the cache holds the full
 * backfill burst plus every live delta, so a naive re-ingest of the
 * whole range would re-emit every coalesced range.
 *
 * Pause-safe buffer: if the operator paused, hold the new rows in
 * `pendingLive` and drain on resume. `scheduleStatefulRender`'s
 * adaptive throttle collapses thousands of ingests into one DataSet
 * update, so even the initial backfill of 5–10 k rows lands in a
 * single render pass.
 */
const pendingLive: IngestRow[] = [];
let drainToken = 0;
async function drainNewRows() {
  if (lastIngestedMs === Infinity) return; // never happens, defensive
  const raw = props.samplesStream.inRange(
    lastIngestedMs === -Infinity ? 0 : lastIngestedMs + 1,
    Number.MAX_SAFE_INTEGER,
  );
  if (!raw.length) return;
  await ensureTimeline();
  const myToken = ++drainToken;
  const CHUNK = 500;
  let highWater = lastIngestedMs;
  for (let start = 0; start < raw.length; start += CHUNK) {
    if (myToken !== drainToken) return;
    const end = Math.min(start + CHUNK, raw.length);
    for (let i = start; i < end; i++) {
      const row = chRowToIngest(raw[i]);
      if (!row) continue;
      if (row.ts <= lastIngestedMs) continue; // belt-and-suspenders
      if (coord.state.range !== null) {
        pendingLive.push(row);
      } else {
        ingest(row);
      }
      if (row.ts > highWater) highWater = row.ts;
    }
    if (end < raw.length) {
      // Yield to the main thread between chunks so brush/scroll stay
      // responsive while the backfill drains.
      await new Promise<void>((r) => setTimeout(r, 0));
    }
  }
  lastIngestedMs = highWater;
}

watch(
  () => props.samplesStream.version.value,
  () => { void drainNewRows(); },
  { immediate: true },
);

// Resume drain — feed any buffered rows through `ingest()` in arrival
// order so the coalescing logic produces the same lane segments it
// would have produced live.
watch(
  () => coord.state.range,
  (range) => {
    if (range !== null || !pendingLive.length) return;
    const drained = pendingLive.splice(0, pendingLive.length);
    for (const r of drained) ingest(r);
  },
);

// Player swap — reset all per-player state so a picker swap clears
// the old session's lanes instead of accumulating both. Watching
// playerId (a string prop) keeps this simple; SessionDisplay's
// useSessionTimeSeries already re-subscribes the SSE stream.
watch(
  () => props.playerId,
  () => {
    statefulEvents.length = 0;
    pendingLive.length = 0;
    lastIngestedMs = -Infinity;
    prevStalls = prevDropped = null;
    prevLoopServer = null;
    prevControlRev = null;
    prevError = null;
    prevPlayId = null;
    prevFirstFrame = prevVideoStart = null;
    prevVariantMbps = null;
    if (itemsDS) {
      try { itemsDS.clear(); } catch { /* ignore */ }
    }
  },
);

watch(
  () => coord.effectiveRange.value,
  () => {
    if (!timeline) return;
    // Skip while the operator is mid-pan — a live sample arriving
    // during drag would otherwise call setWindow with the live edge,
    // visibly fighting the drag (and breaking the final rangechanged
    // event's intent).
    if (userInteracting) return;
    const vp = coord.effectiveRange.value;
    suppressNextRangeChange = true;
    timeline.setWindow(vp.min, vp.max, { animation: false });
  },
);

/**
 * Alt+wheel — handled entirely here so the wheel direction + zoom
 * speed match every other surface (line charts, focus bar). We never
 * fall through to vis-timeline's native zoom because its delta /
 * factor curve differs from ours and the result felt inconsistent.
 *
 * Convention everywhere: wheel UP (`deltaY < 0`) → zoom IN (smaller
 * span, factor 0.9).
 *
 *   - AT LIVE (`viewport == null`): grow/shrink the span by moving
 *     the LEFT edge only — right stays glued to the live sample.
 *     Updates `coord.liveSpanMs`.
 *   - OFF LIVE (`viewport != null`): mouse-anchored zoom. The
 *     timestamp under the cursor stays fixed while both edges move.
 *     If the new right edge reaches live, snap back to live tracking.
 *
 * Capture-phase listener so we run before vis-timeline's own handler. */
function installLiveWheelAnchor() {
  const el = container.value;
  if (!el) return;
  el.addEventListener(
    'wheel',
    (e: WheelEvent) => {
      if (!e.altKey) return;
      e.preventDefault();
      e.stopPropagation();
      const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
      const MIN_SPAN_MS = 1_000;
      const MAX_SPAN_MS = 24 * 3600 * 1000;
      const lastTs = coord.state.lastSampleMs;
      const vp = coord.state.range;

      if (vp == null) {
        const windowMs = coord.state.windowMs;
        const currentSpan = coord.state.liveSpan;
        const nextSpan = Math.max(MIN_SPAN_MS, Math.min(windowMs, currentSpan * factor));
        coord.setLiveSpanMs(nextSpan >= windowMs ? null : nextSpan);
        return;
      }

      const currentSpan = vp.max - vp.min;
      const nextSpan = Math.max(MIN_SPAN_MS, Math.min(MAX_SPAN_MS, currentSpan * factor));
      const rect = el.getBoundingClientRect();
      const frac = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5;
      const anchorTime = vp.min + frac * currentSpan;
      let newStart = anchorTime - frac * nextSpan;
      let newEnd = newStart + nextSpan;
      if (lastTs && newEnd >= lastTs - LIVE_EDGE_TOLERANCE_MS) {
        coord.setViewport(null);
        coord.setLiveSpanMs(nextSpan);
        if (coord.state.paused) coord.setPaused(false);
        return;
      }
      if (!coord.state.paused) coord.setPaused(true);
      coord.setViewport({ min: newStart, max: newEnd });
    },
    { capture: true, passive: false },
  );
}

const expandedClass = computed(() => (coord.state.expanded ? 'expanded' : ''));
// Live toggle is "checked" when we're currently following live —
// i.e. no sticky range. Reacts to all transitions (click, brush
// drag, pan, zoom-snap-to-live) because every reader binds against
// the shared `coord.state.range`.
const liveChecked = computed(() => coord.state.range === null);

/** Always togglePause — both directions preserve liveSpanMs.
 *  See MetricsLineChart.onLiveToggleClick for rationale. */
function onLiveToggleClick() {
  userInteracted = false;
  coord.togglePause();
}

/**
 * "Selected event" cursor — synchronized vertical marker matching
 * the line charts. vis-timeline ships an addCustomTime/setCustomTime
 * API that draws a labelled vertical line; we add it once and
 * shuffle position via setCustomTime, removing only when the cursor
 * is cleared so the timeline doesn't accumulate stray markers across
 * scope changes.
 */
const NAV_CURSOR_ID = 'nav-cursor';
let navCursorAdded = false;
watch(
  () => coord.state.cursorMs,
  async (ms) => {
    await ensureTimeline();
    if (!timeline) return;
    if (ms == null || !Number.isFinite(ms)) {
      if (navCursorAdded) {
        try { timeline.removeCustomTime(NAV_CURSOR_ID); } catch { /* ignore */ }
        navCursorAdded = false;
      }
      return;
    }
    if (!navCursorAdded) {
      try {
        timeline.addCustomTime(new Date(ms), NAV_CURSOR_ID);
        navCursorAdded = true;
      } catch { /* ignore */ }
    } else {
      try { timeline.setCustomTime(new Date(ms), NAV_CURSOR_ID); } catch { /* ignore */ }
    }
  },
  { immediate: true },
);

onBeforeUnmount(() => {
  try { timeline?.destroy(); } catch { /* ignore */ }
  timeline = null;
  itemsDS = null;
  groupsDS = null;
});
</script>

<template>
  <div class="events-timeline">
    <div class="bar">
      <div class="title">Events</div>
      <div class="actions">
        <!-- No expand/collapse: swim-lane heights are content-defined
             so doubling the chart height just adds empty space below
             the lanes. The line charts (bandwidth/RTT/buffer/FPS)
             get the toggle because their y-axes have meaningful
             vertical scale. -->
        <button
          type="button"
          class="btn live-toggle"
          :class="{ checked: liveChecked }"
          @click="coord.togglePause(); userInteracted = false"
          :title="liveChecked ? 'Pause at current live edge' : 'Resume following live (drops zoom and pan)'"
        >
          {{ liveChecked ? '●' : '○' }} Live
        </button>
        <span class="hint">Alt+scroll · drag pan</span>
      </div>
    </div>

    <div class="strip-wrap" :class="expandedClass">
      <div ref="container" class="strip" />
    </div>

    <!-- Colour key — placed BELOW the chart so the eye reads the
         swim lanes first and consults the legend only as needed. -->
    <div class="legend">
      <span class="key"><span class="sw" style="background:#16a34a"/>Playing</span>
      <span class="key"><span class="sw" style="background:#f59e0b"/>Buffering</span>
      <span class="key"><span class="sw" style="background:#dc2626"/>Stalled</span>
      <span class="key"><span class="sw" style="background:#9333ea"/>Paused</span>
      <span class="key"><span class="sw" style="background:#3b82f6"/>Shift up</span>
      <span class="key"><span class="sw" style="background:#ef4444"/>Shift down</span>
      <span class="key"><span class="sw" style="background:#000000"/>Stall</span>
      <span class="key"><span class="sw" style="background:#e11d48"/>Error</span>
      <span class="key"><span class="sw" style="background:#7c3aed"/>Control</span>
      <span class="key"><span class="sw" style="background:#84cc16"/>Loop</span>
    </div>
  </div>
</template>

<style scoped>
.events-timeline { display: grid; gap: 6px; }
.bar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.title { font-size: 12px; font-weight: 600; color: #374151; }
.actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.btn {
  background: #f3f4f6;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  padding: 2px 8px;
  font-size: 11px;
  cursor: pointer;
  color: #374151;
}
.btn:hover { background: #e5e7eb; }
.btn.active { background: #e0e7ff; border-color: #818cf8; color: #312e81; }
/* Live toggle: filled green when checked (following live), muted/
 * outlined when unchecked (pinned). Mirrors MetricsLineChart. */
.btn.live-toggle.checked {
  background: #10b981;
  border-color: #059669;
  color: white;
  font-weight: 600;
}
.btn.live-toggle.checked:hover { background: #059669; }
.btn.live-toggle:not(.checked) {
  background: #f3f4f6;
  border-color: #d1d5db;
  color: #6b7280;
}
.btn.live-toggle:not(.checked):hover { background: #e5e7eb; color: #374151; }
.hint { font-size: 10px; color: #9ca3af; }

.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  font-size: 10px;
  color: #5f6368;
}
.key { display: inline-flex; align-items: center; gap: 4px; }
.sw {
  display: inline-block;
  width: 10px;
  height: 10px;
  border-radius: 2px;
  border: 1px solid rgba(0,0,0,0.18);
}

.strip-wrap {
  position: relative;
  transition: height 0.15s ease;
  min-height: 260px;
  /* Reserve the same 60px right gutter that the Chart.js charts use
   * for their (optional) right-hand y-axis, so the plot area's right
   * edge aligns vertically with every chart below. */
  padding-right: 60px;
}
.strip-wrap.expanded { min-height: 540px; }

.btn-expand { display: inline-flex; align-items: center; gap: 4px; }
.chart-expand-icon { font-size: 13px; line-height: 1; }
.btn-expand.active {
  background: #2563eb;
  border-color: #1d4ed8;
  color: #ffffff;
}
.btn-expand.active:hover { background: #1d4ed8; }
.strip {
  width: 100%;
  border: 1px solid #e5e7eb;
  border-radius: 4px;
  min-height: inherit;
}
/* "Selected event" custom-time line — full visual parity with the
 * line-chart cursor: 1.5 px dashed blue line + a small filled
 * triangle at the top. vis-timeline renders <div class="vis-custom-time">
 * as a 1 px wide div spanning the full chart height; we hide its
 * own background and use the left border to draw the dashed line.
 * The down-arrow is a ::before pseudo-element absolute-positioned
 * just above the chart area. */
.events-timeline :deep(.vis-custom-time) {
  background: transparent !important;
  border-left: 1.5px dashed #1d4ed8 !important;
  width: 0 !important;
  pointer-events: none;
  z-index: 5;
}
.events-timeline :deep(.vis-custom-time::before) {
  content: '';
  position: absolute;
  left: -5px;
  top: 0;
  width: 0;
  height: 0;
  border-left: 5px solid transparent;
  border-right: 5px solid transparent;
  border-top: 6px solid #1d4ed8;
}

/* vis-timeline label panel + labelset pinned to the SAME 60px width
 * as the Chart.js charts' left y-axis (see MetricsLineChart.Y_WIDTH)
 * so the plot area starts at the same x-coordinate. Labels overflow
 * into the chart area on top of the swim-lane items, with a white
 * halo so they stay readable. */
.events-timeline :deep(.vis-panel.vis-left),
.events-timeline :deep(.vis-labelset) {
  width: 60px !important;
  max-width: 60px !important;
  overflow: visible !important;
}
/* Trim vis-timeline's nested-group indent. Default per-level indent
 * is ~15px, which produced wide empty boxes to the left of the child
 * labels. Tighten to ~6px per level — visible tree hierarchy without
 * stealing the swim-lane width. */
.events-timeline :deep(.vis-label),
.events-timeline :deep(.vis-label.vis-nested-group),
.events-timeline :deep(.vis-label.vis-nesting-group),
.events-timeline :deep(.vis-label[class*='vis-group-level']) {
  padding-right: 0 !important;
  margin-left: 0 !important;
  text-indent: 0 !important;
  overflow: visible !important;
  white-space: nowrap !important;
  color: #1f2937;
  font-weight: 600;
  font-size: 10px;
  text-shadow:
    0 0 3px rgba(255, 255, 255, 0.95),
    0 0 6px rgba(255, 255, 255, 0.85),
    1px 0 0 rgba(255, 255, 255, 0.75),
    -1px 0 0 rgba(255, 255, 255, 0.75);
}
.events-timeline :deep(.vis-label.vis-group-level-0) { padding-left: 4px !important; }
.events-timeline :deep(.vis-label.vis-group-level-1) { padding-left: 10px !important; }
.events-timeline :deep(.vis-label.vis-group-level-2) { padding-left: 16px !important; }
.events-timeline :deep(.vis-label.vis-group-level-3) { padding-left: 22px !important; }
.events-timeline :deep(.vis-label.vis-group-level-4) { padding-left: 28px !important; }
.events-timeline :deep(.vis-labelset .vis-label .vis-inner),
.events-timeline :deep(.vis-label .vis-inner) {
  padding-left: 0 !important;
  padding-right: 0 !important;
  margin-left: 0 !important;
  width: auto !important;
}
/* Drop the heavy red/black default row borders that vis-timeline
 * inherits from currentColor — use the same #e5e7eb the Chart.js
 * grid uses so the swim lanes blend with the charts below. */
.events-timeline :deep(.vis-foreground .vis-group),
.events-timeline :deep(.vis-panel.vis-background .vis-group),
.events-timeline :deep(.vis-panel.vis-background .vis-horizontal),
.events-timeline :deep(.vis-timeline) {
  border-color: #e5e7eb !important;
}
.events-timeline :deep(.vis-label) {
  border-color: #e5e7eb !important;
  border-top: none !important;
  border-left: none !important;
  border-right: none !important;
}
</style>
