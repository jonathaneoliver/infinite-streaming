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
import { useChartCoordination, DEFAULT_FOCUS_MS } from '@/composables/useChartCoordination';
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
  // play_id lifecycle (issue #486) — one range per distinct play_id
  // so the operator can see when a play starts / stops. Colored by a
  // stable hash of the id so consecutive plays alternate visibly.
  PLAY_ID:     { label: 'PLAY ID',     color: '#0891b2' },
  // iOS 18 AVMetrics raw event stream (issue #486 spike) — one point
  // per emitted AVMetric event. Hover tooltip shows the full payload.
  AVMETRICS:   { label: 'AVMETRICS',   color: '#a78bfa' },
};

/** Stable, deterministic color for a given play_id so swapping plays
 *  shows obvious contrast on the swim lane. FNV-1a → indexed palette. */
function playIdColor(id: string): string {
  const palette = ['#0891b2', '#0ea5e9', '#6366f1', '#a855f7', '#ec4899', '#f43f5e', '#f97316', '#eab308', '#84cc16', '#10b981'];
  let h = 0x811c9dc5;
  for (let i = 0; i < id.length; i++) {
    h ^= id.charCodeAt(i);
    h = (h + ((h << 1) + (h << 4) + (h << 7) + (h << 8) + (h << 24))) >>> 0;
  }
  return palette[h % palette.length];
}

/** Compact AVMetric event-type label — strip the framework prefix so
 *  the swim-lane point's tooltip header is readable at a glance. */
function shortAVMetricType(t: string): string {
  return t.replace(/^AVMetricPlayerItem/, '').replace(/^AVMetric/, '');
}

/** True when the active player has published any AVMetric events in
 *  the cached window. Used to gate the AVMETRICS lane + legend so
 *  non-iOS devices (Android, Roku, Web) don't get a permanently-
 *  empty section on screen. Issue #486. */
function hasAVMetricsActivity(): boolean {
  const stream = props.avmetricsStream;
  if (!stream) return false;
  if ((stream.rangeBounds.value?.max ?? 0) > 0) return true;
  // rangeBounds is null on a fresh stream; fall back to a cheap
  // inRange probe so we re-check even before the first delta lands.
  return stream.inRange(0, Number.MAX_SAFE_INTEGER).length > 0;
}

/** Color per AVMetric event type. Scoped to the AVMETRICS lane only —
 *  no other lane reads this. Maps from the *short* form (post-prefix-
 *  strip) so the table stays readable. Unknown types fall back to the
 *  lane's default indigo so we never render a transparent bar. */
const AVMETRICS_COLOR_BY_TYPE: Record<string, string> = {
  // ABR / variant
  VariantSwitchEvent:           '#2563eb', // blue   — completed switch
  VariantSwitchStartEvent:      '#60a5fa', // sky-blue — switch START (iOS 26)
  // Buffer readiness
  LikelyToKeepUpEvent:          '#f59e0b', // amber
  InitialLikelyToKeepUpEvent:   '#d97706', // dark amber — first-frame readiness (iOS 26)
  // Stalls (subclass of RateChange — iOS 26)
  StallEvent:                   '#dc2626', // red
  PlaybackStalledEvent:         '#dc2626', // red (legacy / variant name)
  // Playback control
  RateChangeEvent:              '#16a34a', // green
  SeekEvent:                    '#a855f7', // purple — seek START (iOS 26)
  SeekDidCompleteEvent:         '#9333ea', // purple-dark — seek COMPLETE
  // Summary / end-of-session
  PlaybackSummaryEvent:         '#6366f1', // indigo
  // Errors
  ErrorEvent:                   '#dc2626', // red
  // Network / resource fetches
  MediaResourceRequestEvent:    '#0891b2', // cyan
  HLSMediaSegmentRequestEvent:  '#0891b2', // cyan
  HLSPlaylistRequestEvent:      '#0ea5e9', // sky
  // DRM
  ContentKeyRequestEvent:       '#ea580c', // orange
};

function avMetricsColor(eventType: string): string {
  const short = shortAVMetricType(eventType);
  return AVMETRICS_COLOR_BY_TYPE[short] ?? EVENT_LANES.AVMETRICS.color;
}

function escapeHTML(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/** Format the AVMetric event's Obj-C property dump as HTML for the
 *  vis-timeline tooltip overlay. One field per `<div>`, sorted
 *  alphabetically, with values HTML-escaped so AVAssetVariant
 *  `<...>` notation renders as text. Length-bounded so a pathological
 *  payload doesn't make the tooltip unscrollable. Issue #486. */
function formatAVMetricRawHTML(raw: unknown): string {
  if (typeof raw !== 'string' || raw.length === 0) return '';
  let parsed: unknown;
  try { parsed = JSON.parse(raw); }
  catch {
    const trunc = raw.length > 4000 ? raw.slice(0, 4000) + '…' : raw;
    return `<div>${escapeHTML(trunc)}</div>`;
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return `<div>${escapeHTML(raw)}</div>`;
  }
  const obj = parsed as Record<string, unknown>;
  const lines: string[] = [];
  let total = 0;
  const LIMIT = 6000;
  for (const k of Object.keys(obj).sort()) {
    const v = obj[k];
    if (v == null) continue;
    const valueStr = typeof v === 'string' ? v : JSON.stringify(v);
    if (valueStr === '' || valueStr === '""') continue;
    if (total + valueStr.length + k.length > LIMIT) {
      lines.push('<div style="color:#9ca3af;margin-top:6px;">…(truncated)</div>');
      break;
    }
    lines.push(
      `<div style="margin-bottom:4px;line-height:1.4;">` +
      `<b style="color:#4338ca;">${escapeHTML(k)}</b>: ` +
      `<span style="color:#1f2937;">${escapeHTML(valueStr)}</span>` +
      `</div>`,
    );
    total += valueStr.length + k.length;
  }
  return lines.join('');
}

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
  eventsStream: Stream<Record<string, unknown>>;
  /** iOS 18 AVMetrics raw event stream (issue #486). One point per
   *  emitted AVMetric event on the AVMETRICS lane. Optional so other
   *  consumers (live testing.html) can skip wiring it without
   *  breaking the component. */
  avmetricsStream?: Stream<Record<string, unknown>>;
}>();
const coord = useChartCoordination(toRef(props, 'playerId'));

/** Adapter — map a CH session_snapshots row (wire shape from the v3
 *  /api/v2/timeseries endpoint) to the small subset of fields ingest()
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
    // #550 Phase 1 soft cutover: prefer the new canonical column
    // names; fall back to legacy ones during the deprecation window
    // so the timeline still renders for historical rows that pre-date
    // the rename.
    stalls: num(row.stalling_count) ?? num(row.stall_count),
    droppedFrames: num(row.frames_dropped),
    error: String(row.player_error ?? ''),
    firstFrameTimeS: num(row.video_first_frame_time_ms) != null
      ? (num(row.video_first_frame_time_ms) as number) / 1000
      : num(row.video_first_frame_time_s),
    videoStartTimeS: num(row.video_start_time_ms) != null
      ? (num(row.video_start_time_ms) as number) / 1000
      : num(row.video_start_time_s),
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
  type: 'PLAYERSTATE' | 'DISPLAY_RES' | 'VARIANT' | 'PLAY_ID';
  // PLAYERSTATE
  state?: string;
  reason?: string;
  // DISPLAY_RES
  resolution?: string;
  // VARIANT
  mbps?: number;
  variantRes?: string;
  variantKey?: string;
  // PLAY_ID — the play_id active on the heartbeat that produced
  // this event. Coalesce into a range; new id starts a new range.
  playId?: string;
}

const statefulEvents: StatefulEvent[] = [];
/** Backstop cap on retained stateful events (issue #582). This array was
 *  appended on every heartbeat and never trimmed within a session, so it
 *  grew unbounded — and renderStatefulLanes walks + sorts it on each
 *  render. We normally trim it to the events cache's retained window (so
 *  the Player State lanes cover the SAME span as the charts on pan-back);
 *  this count is only a fallback when the cache bounds aren't known yet.
 *  Set high (well above the cache cap × a few events/heartbeat) so it
 *  never trims tighter than the cache window. */
const STATEFUL_EVENTS_CAP = 40000;
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
// markers-stream watcher uses this to consume only NEW rows on each
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

/** Reverse of `manifestResolutionForBitrate` — find the variant's
 *  peak bandwidth (Mbps) given its resolution. Used to colour the
 *  DISPLAY_RES lane in lock-step with the VARIANT lane so the same
 *  resolution always reads with the same swatch. Issue #486. */
function bandwidthMbpsForResolution(
  variants: IngestRow['manifestVariants'],
  resolution: string,
): number | undefined {
  if (!resolution || !variants || variants.length === 0) return undefined;
  const target = resolution.trim().toLowerCase();
  for (const v of variants) {
    const r = String(v?.resolution ?? '').trim().toLowerCase();
    if (r && r === target) {
      const bw = Number(v?.bandwidth ?? 0);
      if (Number.isFinite(bw) && bw > 0) return bw / 1_000_000;
    }
  }
  return undefined;
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
  const groups: any[] = [
    { id: 'PLAYER_SECTION', content: 'PLAYER', nestedGroups: [
      'PLAY_ID',
      ...variantGroups.map((g) => g.id),
      'DISPLAY_RES', 'PLAYERSTATE', 'PLAYBACK', 'IMPAIRMENT',
    ] },
    { id: 'PLAY_ID', content: EVENT_LANES.PLAY_ID.label },
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
  // AVMetrics section only when the active player has actually
  // published AVMetric events (issue #486). Heuristic: any rows on
  // the avmetrics stream within the cached window. Hides the lane
  // entirely for Android/Roku/Web players that don't have the
  // framework. Re-evaluated on every rebuildGroups call so an iOS
  // player joining later wakes the lane up automatically.
  if (hasAVMetricsActivity()) {
    groups.push(
      { id: 'AVMETRICS_SECTION', content: 'AVMETRICS', nestedGroups: ['AVMETRICS'] },
      { id: 'AVMETRICS', content: EVENT_LANES.AVMETRICS.label },
    );
  }
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
        // HTML tooltips (issue #486): the native browser `title` attr
        // is plain-text only and the OS clips long content. vis-timeline's
        // own tooltip overlay respects HTML in item.title when XSS
        // protection is disabled — needed so AVMetric event payloads
        // render as multi-line, vertically-spread fields. `overflowMethod`
        // flips the tooltip when it would clip the timeline edge.
        tooltip: { followMouse: true, overflowMethod: 'flip' },
        xss: { disabled: true },
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
      // Left-click on a blank part of the timeline = toggle live
      // (issue #486). Mirrors the canvas-click behaviour on the line
      // charts. `what === 'background'` skips item clicks, lane-
      // header clicks, axis clicks — only the empty strip counts.
      timeline.on('click', (ev: any) => {
        if (ev?.what === 'background') {
          coord.toggleLive();
        }
      });
      timeline.on('rangechanged', (rc: any) => {
        userInteracting = false;
        // Re-flow AVMetric bar widths to the new viewport regardless
        // of who triggered the change — zoom in, zoom out, pan, or a
        // programmatic setWindow all change ms-per-pixel. Cheap diff
        // update; safe on every fire. Issue #486.
        reflowAVMetricsDurations();
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
        if (coord.isAtLiveEdge(b)) {
          coord.setLiveSpan(b - a);
          coord.setRange(null);
          return;
        }
        coord.setRange({ min: a, max: b });
      });
      installLiveWheelAnchor();
      installCursorHoverTooltip();
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

  // PLAY_ID lane — one stateful tick per heartbeat with the active
  // play_id. The renderer coalesces consecutive runs of the same id
  // into a range, so each play shows up as one bar with a colour
  // derived from the id. New play_id (rotation / reload) starts a
  // new range automatically. Issue #486.
  if (r.playId) {
    statefulEvents.push({ ts: t, type: 'PLAY_ID', playId: r.playId });
  }

  // DISPLAY_RES — the decoded video resolution being displayed.
  // Stamp the matching variant's Mbps onto the event so the lane can
  // colour-match the VARIANT row (which is keyed by bitrate bucket).
  // Operator preference: a single resolution should read with the
  // same colour everywhere on the chart. Issue #486.
  if (r.videoResolution) {
    const matchMbps = bandwidthMbpsForResolution(r.manifestVariants, r.videoResolution);
    statefulEvents.push({
      ts: t,
      type: 'DISPLAY_RES',
      resolution: r.videoResolution,
      mbps: matchMbps,
    });
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
    // Colour by the matching variant's Mbps when available so
    // 1920x1080 here reads with the same swatch as 1920x1080 on
    // the VARIANT lane. Pre-manifest heartbeats (no match) fall
    // back to the legacy resolution-bucket palette. Issue #486.
    (e) => (e.mbps && e.mbps > 0
      ? variantColor(e.mbps)
      : displayResColor(e.resolution ?? null)),
  );
  coalesce(
    'VARIANT',
    (e) => e.variantKey ?? 'VARIANT',
    () => '', // ranges are blank bars; label sits on the group header
    (e) => variantColor(e.mbps ?? 0),
  );

  // PLAY_ID — one range per contiguous run of the same play_id.
  // Issue #486. Tooltip shows the full id; the bar shows a short
  // 8-char prefix so the lane stays readable even with stacked
  // narrow ranges.
  coalesce(
    'PLAY_ID',
    () => 'PLAY_ID',
    (e) => (e.playId ? e.playId.slice(0, 8) : '—'),
    (e) => (e.playId ? playIdColor(e.playId) : '#9ca3af'),
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
  const raw = props.eventsStream.inRange(
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
      // Pause buffer is only for rows arriving PAST the pinned
      // window — see MetricsLineChart.vue:679 for the reasoning.
      // Archive replay pins the brush before backfill starts; those
      // rows belong inside the range and must reach the timeline.
      const range = coord.state.range;
      if (range !== null && row.ts > range.max) {
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
  // Bound the stateful-event history (issue #582) WITHOUT trimming
  // tighter than what the charts show. Trim to the events cache's
  // retained window (rangeBounds.min) so the Player State lanes fill in
  // over exactly the same span the charts do when the operator pans the
  // focus window back. Appended in ascending ts order, so the oldest are
  // at the front. The fixed cap is only a fallback before bounds exist.
  const minKeep = props.eventsStream.rangeBounds.value?.min;
  if (minKeep != null) {
    let drop = 0;
    while (drop < statefulEvents.length && statefulEvents[drop].ts < minKeep) drop++;
    if (drop > 0) statefulEvents.splice(0, drop);
  } else if (statefulEvents.length > STATEFUL_EVENTS_CAP) {
    statefulEvents.splice(0, statefulEvents.length - STATEFUL_EVENTS_CAP);
  }
}

watch(
  () => props.eventsStream.version.value,
  () => { void drainNewRows(); },
  { immediate: true },
);

/* ─── AVMetrics drain (issue #486) ─────────────────────────────────
 * Parallel to drainNewRows but consumes the avmetricsStream and emits
 * one POINT item per event on the AVMETRICS lane. No coalescing — each
 * event is its own marker. Hover tooltip carries the full payload.
 */
const pendingLiveAV: Record<string, unknown>[] = [];
let lastAVIngestedMs = -Infinity;
let avDrainToken = 0;

/** Synthetic duration for AVMetric items — viewport-aware (issue #486).
 *
 * AVMetric events are instantaneous in the SDK, but a single tick of
 * width is unclickable on any zoom level. We want a constant on-screen
 * footprint (~AVMETRICS_TARGET_PIXELS px wide) regardless of whether
 * the operator is looking at a 10-min window or a 10-hour archive
 * replay. Duration is computed from the current vis-timeline window
 * and container width, clamped to a sane minimum/maximum so a deeply-
 * zoomed-in view doesn't shrink to sub-millisecond and a 10-hr replay
 * doesn't push individual events past the next one.
 *
 * Bars are sized at ingest time AND re-flowed on every `rangechanged`
 * (pan/zoom) — see `reflowAVMetricsDurations`. */
const AVMETRICS_TARGET_PIXELS = 8;       // clickable minimum on screen
const AVMETRICS_MIN_DURATION_MS = 250;   // floor at deep zoom-in
const AVMETRICS_MAX_DURATION_MS = 60_000; // ceiling at deep zoom-out

function computeAVMetricsDurationMs(): number {
  if (!timeline || !container.value) return 1000;
  const win = timeline.getWindow();
  const startMs = (win.start instanceof Date) ? win.start.getTime() : Number(win.start);
  const endMs   = (win.end   instanceof Date) ? win.end.getTime()   : Number(win.end);
  const winMs = endMs - startMs;
  if (!Number.isFinite(winMs) || winMs <= 0) return 1000;
  // Reserve space for the group-header column; the actual content
  // area is narrower than the full container. The exact value isn't
  // critical — being off by ±50 px just shifts the clickable footprint
  // by a fraction of a pixel.
  const containerWidth = container.value.clientWidth || 1000;
  const contentWidth = Math.max(400, containerWidth - 220);
  const msPerPixel = winMs / contentWidth;
  const target = AVMETRICS_TARGET_PIXELS * msPerPixel;
  return Math.min(AVMETRICS_MAX_DURATION_MS, Math.max(AVMETRICS_MIN_DURATION_MS, target));
}

/** Time-cost of one screen pixel under the current viewport. Used
 *  both as the visual gap between adjacent events and as the absolute
 *  floor for an event's width (so even tightly-clustered events keep
 *  a 1px-visible mark). */
function msPerPixel(): number {
  if (!timeline || !container.value) return 1;
  const win = timeline.getWindow();
  const startMs = (win.start instanceof Date) ? win.start.getTime() : Number(win.start);
  const endMs   = (win.end   instanceof Date) ? win.end.getTime()   : Number(win.end);
  const winMs = endMs - startMs;
  if (!Number.isFinite(winMs) || winMs <= 0) return 1;
  const containerWidth = container.value.clientWidth || 1000;
  const contentWidth = Math.max(400, containerWidth - 220);
  return winMs / contentWidth;
}

/** Walk every AVMetric range in the DataSet (in chronological order)
 *  and update its end to give each bar the target pixel width — but
 *  never overlapping into the next event's start. Result: each event
 *  is as wide as the target *or* as wide as the gap to its neighbor,
 *  whichever is smaller, minus a 2-px visual gap. Called from the
 *  rangechanged handler so widths track pan / zoom, and at the end of
 *  every AVMetrics drain so newly-arrived events get sized correctly
 *  (and the neighbor they pulled in next to gets re-clamped). */
function reflowAVMetricsDurations() {
  if (!itemsDS) return;
  const target = computeAVMetricsDurationMs();
  const mpp = msPerPixel();
  const gapMs = Math.max(20, 2 * mpp);   // 2 px visual gap between neighbours
  const floorMs = Math.max(1, mpp);      // never less than 1 px wide

  const ranges: { id: any; ts0: number; end: number }[] = [];
  itemsDS.forEach((it: any) => {
    if (it.group !== 'AVMETRICS') return;
    if (typeof it.ts0 !== 'number') return;
    ranges.push({ id: it.id, ts0: it.ts0, end: it.end });
  });
  ranges.sort((a, b) => a.ts0 - b.ts0);

  const updates: any[] = [];
  for (let i = 0; i < ranges.length; i++) {
    const cur = ranges[i];
    const next = ranges[i + 1];
    let dur = target;
    if (next) {
      const room = next.ts0 - cur.ts0 - gapMs;
      if (room < dur) dur = room;
    }
    if (dur < floorMs) dur = floorMs;
    const newEnd = cur.ts0 + dur;
    if (cur.end !== newEnd) updates.push({ id: cur.id, end: newEnd });
  }
  if (updates.length) itemsDS.update(updates);
}

function ingestAVMetric(row: Record<string, unknown>) {
  const t = tsOfRow(row);
  if (!Number.isFinite(t)) return;
  const eventType = String(row.event_type ?? '').trim() || 'AVMetric';
  const short = shortAVMetricType(eventType);
  const formatted = formatAVMetricRawHTML(row.raw_json);
  const id = nextId++;
  // HTML tooltip (issue #486). Constrained max-width so very long
  // unbroken value strings (e.g. fromVariant's full AVAssetVariant
  // dump) wrap rather than push the tooltip off-screen.
  const header =
    `<div style="font-weight:600;margin-bottom:2px;color:#1e1b4b;">${escapeHTML(eventType)}</div>` +
    `<div style="color:#6b7280;font-size:11px;margin-bottom:8px;">${fmtTime(t)}</div>`;
  const title = `<div class="avmetrics-tooltip" style="max-width:720px;font-family:ui-monospace,'SF Mono',Menlo,monospace;font-size:11px;white-space:normal;word-break:break-all;">${header}${formatted}</div>`;
  const color = avMetricsColor(eventType);
  const item: TimelineRangeItem = {
    id,
    group: 'AVMETRICS',
    content: short,
    start: t,
    end: t + computeAVMetricsDurationMs(),
    ts0: t,
    type: 'range',
    title,
    style: `background-color: ${color}; border-color: ${color}; color: #1e1b4b;`,
  };
  items.push(item);
  itemsDS?.add(item);
}

async function drainNewAVMetricsRows() {
  const stream = props.avmetricsStream;
  if (!stream) return;
  const raw = stream.inRange(
    lastAVIngestedMs === -Infinity ? 0 : lastAVIngestedMs + 1,
    Number.MAX_SAFE_INTEGER,
  );
  if (!raw.length) return;
  await ensureTimeline();
  const myToken = ++avDrainToken;
  const CHUNK = 500;
  let highWater = lastAVIngestedMs;
  for (let start = 0; start < raw.length; start += CHUNK) {
    if (myToken !== avDrainToken) return;
    const end = Math.min(start + CHUNK, raw.length);
    for (let i = start; i < end; i++) {
      const row = raw[i];
      const t = tsOfRow(row);
      if (!Number.isFinite(t) || t <= lastAVIngestedMs) continue;
      const range = coord.state.range;
      if (range !== null && t > range.max) {
        pendingLiveAV.push(row);
      } else {
        ingestAVMetric(row);
      }
      if (t > highWater) highWater = t;
    }
    if (end < raw.length) {
      await new Promise<void>((r) => setTimeout(r, 0));
    }
  }
  lastAVIngestedMs = highWater;
  // After the batch lands, re-flow widths so the just-arrived events
  // get sized to the current viewport AND their neighbours get
  // re-clamped to not overlap. Cheap — itemsDS.update() with the
  // diff path is sub-ms for the volumes we deal with.
  reflowAVMetricsDurations();
}

watch(
  () => props.avmetricsStream?.version.value ?? 0,
  () => { void drainNewAVMetricsRows(); },
  { immediate: true },
);

// Toggle the AVMETRICS lane's visibility when activity appears or
// disappears (issue #486). Single boolean memo so we only call
// rebuildGroups on the actual transition — every other version
// tick is a no-op.
let avMetricsLaneVisible = false;
watch(
  () => props.avmetricsStream?.version.value ?? 0,
  () => {
    const nowHas = hasAVMetricsActivity();
    if (nowHas !== avMetricsLaneVisible) {
      avMetricsLaneVisible = nowHas;
      rebuildGroups();
    }
  },
  { immediate: true },
);

/** Reactive equivalent of hasAVMetricsActivity() for the template —
 *  the AVMETRICS legend strip uses this to hide on non-iOS devices. */
const hasAVMetricsForLegend = computed(() => {
  void props.avmetricsStream?.version.value;
  return hasAVMetricsActivity();
});

// Resume drain — feed any buffered rows through `ingest()` in arrival
// order so the coalescing logic produces the same lane segments it
// would have produced live.
watch(
  () => coord.state.range,
  (range) => {
    if (range !== null) return;
    if (pendingLive.length) {
      const drained = pendingLive.splice(0, pendingLive.length);
      for (const r of drained) ingest(r);
    }
    if (pendingLiveAV.length) {
      const drained = pendingLiveAV.splice(0, pendingLiveAV.length);
      for (const r of drained) ingestAVMetric(r);
    }
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
    pendingLiveAV.length = 0;
    lastIngestedMs = -Infinity;
    lastAVIngestedMs = -Infinity;
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
      // Horizontal scroll → pan. Same semantics as MetricsLineChart;
      // see gh#461.
      if (!e.altKey && Math.abs(e.deltaX) > Math.abs(e.deltaY)) {
        e.preventDefault();
        e.stopPropagation();
        const widthPx = el.clientWidth;
        if (widthPx <= 0) return;
        const current = coord.effectiveRange.value;
        const span = current.max - current.min;
        const dms = (e.deltaX / widthPx) * span;
        coord.setRange({ min: current.min + dms, max: current.max + dms });
        return;
      }
      if (!e.altKey) return;
      e.preventDefault();
      e.stopPropagation();
      const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
      const MIN_SPAN_MS = 1_000;
      const MAX_SPAN_MS = 24 * 3600 * 1000;
      const vp = coord.state.range;

      if (vp == null) {
        const currentSpan = coord.state.liveSpan;
        const nextSpan = Math.max(MIN_SPAN_MS, Math.min(DEFAULT_FOCUS_MS, currentSpan * factor));
        coord.setLiveSpan(nextSpan);
        return;
      }

      const currentSpan = vp.max - vp.min;
      const nextSpan = Math.max(MIN_SPAN_MS, Math.min(MAX_SPAN_MS, currentSpan * factor));
      const rect = el.getBoundingClientRect();
      const frac = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5;
      const anchorTime = vp.min + frac * currentSpan;
      let newStart = anchorTime - frac * nextSpan;
      let newEnd = newStart + nextSpan;
      if (coord.isAtLiveEdge(newEnd)) {
        coord.setLiveSpan(nextSpan);
        coord.setRange(null);
        return;
      }
      coord.setRange({ min: newStart, max: newEnd });
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
  coord.toggleLive();
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

/** Custom-DOM cursor tooltip state for the EventsTimeline strip
 *  (issue #486). Driven by a mousemove handler on the strip: when
 *  the mouse is within ~6 px of the rendered `.vis-custom-time`
 *  line, show the tooltip near the cursor with cursorLabel text. */
const cursorTooltipVisible = ref(false);
const cursorTooltipX = ref(0);
const cursorTooltipY = ref(0);

/** Install the strip-level mousemove + mouseleave handlers. Called
 *  once after the timeline is created. The vis-timeline custom-time
 *  DOM element is the source of truth for the line's screen X; we
 *  read its bounding rect each move so pan/zoom is handled
 *  automatically. */
function installCursorHoverTooltip() {
  const c = container.value;
  if (!c) return;
  c.addEventListener('mousemove', (e) => {
    const label = coord.state.cursorLabel;
    if (!label || coord.state.cursorMs == null) {
      if (cursorTooltipVisible.value) cursorTooltipVisible.value = false;
      return;
    }
    const lineEl = c.querySelector('.vis-custom-time') as HTMLElement | null;
    if (!lineEl) {
      if (cursorTooltipVisible.value) cursorTooltipVisible.value = false;
      return;
    }
    const cRect = c.getBoundingClientRect();
    const lRect = lineEl.getBoundingClientRect();
    const lineX = lRect.left + lRect.width / 2 - cRect.left;
    const mx = e.clientX - cRect.left;
    const my = e.clientY - cRect.top;
    if (Math.abs(mx - lineX) > 6) {
      if (cursorTooltipVisible.value) cursorTooltipVisible.value = false;
      return;
    }
    cursorTooltipX.value = Math.min(lineX + 6, c.clientWidth - 240);
    cursorTooltipY.value = Math.max(4, my - 28);
    cursorTooltipVisible.value = true;
  });
  c.addEventListener('mouseleave', () => {
    cursorTooltipVisible.value = false;
  });
}

watch(
  [() => coord.state.cursorMs, () => coord.state.cursorLabel],
  async ([ms]) => {
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
          @click="coord.toggleLive(); userInteracted = false"
          :title="liveChecked ? 'Pause at current live edge' : 'Resume following live (drops zoom and pan)'"
        >
          {{ liveChecked ? '●' : '○' }} Live
        </button>
        <span class="hint">Alt+scroll · drag pan</span>
      </div>
    </div>

    <div class="strip-wrap" :class="expandedClass">
      <div ref="container" class="strip" />
      <!-- Cursor hover tooltip (issue #486). The vis-timeline
           `.vis-custom-time` line is too thin to reliably hit with
           native browser tooltips, AND vis sets pointer-events:none
           on it. Drive a custom-DOM tooltip from a mousemove handler
           on the strip instead — same UX as the line charts. -->
      <div
        v-if="cursorTooltipVisible"
        class="cursor-tooltip"
        :style="{ left: cursorTooltipX + 'px', top: cursorTooltipY + 'px' }"
      >{{ coord.state.cursorLabel }}</div>
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

    <!-- AVMetrics colour key (issue #486) — its own strip with a
         section header so it reads as the parallel observation stream
         it is, not as a continuation of the heartbeat-derived colours
         above. Entries mirror AVMETRICS_COLOR_BY_TYPE in the script. -->
    <div v-if="hasAVMetricsForLegend" class="legend legend-avmetrics">
      <span class="legend-label">AVMETRICS:</span>
      <span class="key"><span class="sw" style="background:#2563eb"/>VariantSwitch</span>
      <span class="key"><span class="sw" style="background:#60a5fa"/>VariantSwitchStart</span>
      <span class="key"><span class="sw" style="background:#f59e0b"/>LikelyToKeepUp</span>
      <span class="key"><span class="sw" style="background:#d97706"/>InitialLikelyToKeepUp</span>
      <span class="key"><span class="sw" style="background:#dc2626"/>Stall / Error</span>
      <span class="key"><span class="sw" style="background:#16a34a"/>RateChange</span>
      <span class="key"><span class="sw" style="background:#a855f7"/>Seek</span>
      <span class="key"><span class="sw" style="background:#9333ea"/>SeekDidComplete</span>
      <span class="key"><span class="sw" style="background:#6366f1"/>PlaybackSummary</span>
      <span class="key"><span class="sw" style="background:#0891b2"/>MediaResource / HLS Segment</span>
      <span class="key"><span class="sw" style="background:#0ea5e9"/>HLS Playlist</span>
      <span class="key"><span class="sw" style="background:#ea580c"/>ContentKey (DRM)</span>
      <span class="key"><span class="sw" style="background:#a78bfa"/>other</span>
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
/* AVMetrics row sits below the heartbeat legend with a hairline rule
 * + a leading section label so the two palettes don't read as one. */
.legend-avmetrics {
  border-top: 1px solid #e5e7eb;
  padding-top: 4px;
}
.legend-label {
  font-weight: 600;
  color: #4338ca;
  letter-spacing: 0.4px;
  text-transform: uppercase;
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

/* Cursor hover tooltip (issue #486) — mirrors MetricsLineChart's
 * tooltip styling so the surface reads identically across all
 * synchronized charts. */
.cursor-tooltip {
  position: absolute;
  z-index: 4;
  background: #1e3a8a;
  color: #fff;
  font-size: 11px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  padding: 4px 8px;
  border-radius: 4px;
  pointer-events: none;
  white-space: nowrap;
  box-shadow: 0 2px 6px rgba(0, 0, 0, 0.18);
  max-width: 240px;
  overflow: hidden;
  text-overflow: ellipsis;
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
