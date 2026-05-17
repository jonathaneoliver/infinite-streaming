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
import { usePlayer } from '@/composables/usePlayer';
import { ensureVisTimeline } from '@/composables/useChartJs';
import { useChartCoordination } from '@/composables/useChartCoordination';
import type { PlayerRecord } from '@/repo/v2-repo';

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

const props = defineProps<{ playerId: string }>();
const { player } = usePlayer(toRef(props, 'playerId'));
const coord = useChartCoordination(props.playerId);

const container = ref<HTMLDivElement | null>(null);

let vis: any = null;
let timeline: any = null;
let itemsDS: any = null;
let groupsDS: any = null;
let nextId = 1;
let suppressNextRangeChange = false;
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
let prevPlayerRestarts: number | null = null;
let prevLoopServer: number | null = null;
let prevControlRev: string | null = null;
let prevError: string | null = null;
let prevPlayId: string | null = null;
let prevFirstFrame: number | null = null;
let prevVideoStart: number | null = null;
let prevVariantMbps: number | null = null;

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
function manifestResolutionForBitrate(p: PlayerRecord, targetMbps: number): string {
  if (!Number.isFinite(targetMbps)) return '';
  const variants =
    (p as any)?.current_play?.manifest?.variants ??
    (p as any)?.raw_session?.manifest_variants ??
    null;
  if (!Array.isArray(variants) || variants.length === 0) return '';
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

function tsFor(p: PlayerRecord): number {
  if (p.player_metrics?.event_time) {
    const v = Date.parse(p.player_metrics.event_time);
    if (Number.isFinite(v)) return v;
  }
  if (p.last_seen_at) {
    const v = Date.parse(p.last_seen_at);
    if (Number.isFinite(v)) return v;
  }
  return Date.now();
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
      const vp = coord.effectiveViewport.value;
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
      // Click on the strip toggles pause / live, matching the
      // line-chart canvas behaviour (MetricsLineChart). vis-timeline's
      // 'click' event only fires on a real click — not after a drag —
      // so panning doesn't accidentally toggle.
      timeline.on('click', () => {
        coord.togglePause();
      });
      timeline.on('rangechanged', (rc: any) => {
        if (suppressNextRangeChange) { suppressNextRangeChange = false; return; }
        if (!rc?.byUser) return;
        userInteracted = true;
        // With the Alt+wheel live-anchor listener below capturing
        // wheel events in live mode, the only user-driven rangechange
        // left here is PAN (drag). Pan auto-pauses, mirroring the
        // chartjs pan handler. In paused mode the Alt+wheel listener
        // falls through to vis-timeline's mouse-anchored zoom, which
        // also fires rangechanged → we set the sticky viewport in
        // both branches.
        if (!coord.state.paused) coord.setPaused(true);
        const a = rc.start instanceof Date ? rc.start.getTime() : Date.parse(rc.start);
        const b = rc.end instanceof Date ? rc.end.getTime() : Date.parse(rc.end);
        if (Number.isFinite(a) && Number.isFinite(b)) coord.setViewport({ min: a, max: b });
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
function ingest(p: PlayerRecord) {
  const pm = p.player_metrics;
  const t = tsFor(p);
  coord.noteSample(t);

  // Track play_id transitions for the POINT-event diff trackers (so
  // e.g. STALL counters reset per-play). We DO NOT wipe
  // `statefulEvents` here — the synthetic play_id can flicker when
  // the proxy projects different variant manifest URLs between
  // metric ticks, and that flicker would otherwise erase the whole
  // PLAYERSTATE / VARIANT / DISPLAY_RES history (visible as the
  // PLAYERSTATE bar suddenly vanishing). Real "new play" transitions
  // surface naturally via `player_metrics.state` going through
  // idle/loading/playing — that's what drives the lane segmentation.
  const playId = p.current_play?.id ?? null;
  if (playId !== prevPlayId) {
    prevPlayId = playId;
    prevStalls = prevDropped = prevPlayerRestarts = null;
    prevLoopServer = null;
    prevError = null;
    prevFirstFrame = prevVideoStart = null;
  }

  // STATEFUL LANES — push every heartbeat as an event into a flat
  // array. The renderer coalesces runs of same-label entries below.
  // This is the legacy session-shell.js pattern, robust against the
  // empty-vs-null / case-flicker / play_id-resync issues that broke
  // the incremental open-range approach.
  const stateNorm = String(pm?.state ?? '').trim();
  const reasonNorm = String(pm?.waiting_reason ?? '').trim();
  if (stateNorm) {
    statefulEvents.push({ ts: t, type: 'PLAYERSTATE', state: stateNorm, reason: reasonNorm });
  }

  // DISPLAY_RES — the decoded video resolution being displayed (what
  // the user actually sees on-screen as video content). Sourced from
  // `pm.video_resolution`, matching the legacy session-shell.js:2352
  // — the lane label is "DISPLAY RES" but the value is the active
  // variant's decoded size. (`pm.display_resolution` is the player's
  // window/viewport size and is reported on its own field, not used
  // for this lane.)
  const videoResNorm = String(pm?.video_resolution ?? '').trim();
  if (videoResNorm) {
    statefulEvents.push({ ts: t, type: 'DISPLAY_RES', resolution: videoResNorm });
  }

  // VARIANT — same heartbeat-push pattern. Keyed on the MANIFEST's
  // canonical resolution for the rung (legacy parity, via
  // `manifestResolutionForBitrate()`), so iPad's churn through
  // intermediate `video_resolution` reports during a switch doesn't
  // create phantom lanes for one rung.
  const mbpsRaw = pm?.video_bitrate_mbps ?? null;
  if (mbpsRaw != null && Number.isFinite(mbpsRaw) && mbpsRaw > 0) {
    const mbpsRounded = Math.round(mbpsRaw * 10) / 10;
    // Only emit when the manifest has a matching variant — that's the
    // canonical (resolution, bitrate) pair. Falling back to the player's
    // reported `video_resolution` here would seed phantom lanes during
    // ABR transitions (player reports an intermediate decoded size like
    // 2560x1440 while the active rung is actually 3840x2160 · 29.9
    // Mbps), and those phantom lanes would never collapse even after
    // the manifest loads. Skip emission until we have the manifest.
    const variantRes = manifestResolutionForBitrate(p, mbpsRounded);
    if (variantRes) {
      // Emit PLAYBACK SHIFT_UP / SHIFT_DOWN as POINT events on real
      // rung changes — incremental tracker is fine here since these
      // are points, not coalesced ranges.
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

  // IMPAIRMENT — STALL on stall counter increments; ERROR when error
  // string changes; FROZEN on dropped-frame surge (heuristic).
  if (pm?.stalls != null && prevStalls != null && pm.stalls > prevStalls) {
    const delta = pm.stalls - prevStalls;
    pushPoint('IMPAIRMENT', t, 'STALL', '#000000', `\n+${delta} (total ${pm.stalls})`);
  }
  if (pm?.dropped_frames != null && prevDropped != null && pm.dropped_frames > prevDropped + 10) {
    pushPoint('IMPAIRMENT', t, 'FROZEN', '#4c1d95', `\n+${pm.dropped_frames - prevDropped} dropped`);
  }
  if (pm?.error && pm.error !== prevError) {
    pushPoint('IMPAIRMENT', t, 'ERROR', '#e11d48', `\n${pm.error}`);
    prevError = pm.error;
  }
  if (pm?.stalls != null) prevStalls = pm.stalls;
  if (pm?.dropped_frames != null) prevDropped = pm.dropped_frames;

  // PLAYBACK — RESTART on player_restarts increments; FIRST_FRAME +
  // PLAYBACK_START once at the play boundary.
  if (pm?.player_restarts != null && prevPlayerRestarts != null && pm.player_restarts > prevPlayerRestarts) {
    const delta = pm.player_restarts - prevPlayerRestarts;
    pushPoint('PLAYBACK', t, 'RESTART', '#a855f7', `\n+${delta}`);
  }
  if (pm?.player_restarts != null) prevPlayerRestarts = pm.player_restarts;
  if (pm?.first_frame_time_s != null && pm.first_frame_time_s > 0 && prevFirstFrame !== pm.first_frame_time_s) {
    pushPoint('PLAYBACK', t, 'FIRST FRAME', '#14b8a6', `\n${pm.first_frame_time_s.toFixed(3)}s`);
    prevFirstFrame = pm.first_frame_time_s;
  }
  if (pm?.video_start_time_s != null && pm.video_start_time_s > 0 && prevVideoStart !== pm.video_start_time_s) {
    pushPoint('PLAYBACK', t, 'START TIME', '#15803d', `\n${pm.video_start_time_s.toFixed(3)}s`);
    prevVideoStart = pm.video_start_time_s;
  }

  // SERVER — LOOP increments
  const loop = p.loop_count_server ?? null;
  if (loop != null && prevLoopServer != null && loop > prevLoopServer) {
    const delta = loop - prevLoopServer;
    pushPoint('LOOP_SERVER', t, 'LOOP', '#84cc16', `\n+${delta} (total ${loop})`);
  }
  if (loop != null) prevLoopServer = loop;

  // CONTROL — record any control_revision change.
  const rev = p.control_revision ?? null;
  if (rev && rev !== prevControlRev) {
    if (prevControlRev != null) {
      pushPoint('CONTROL', t, 'CONTROL CHANGE', '#7c3aed', `\n${prevControlRev} → ${rev}`);
    }
    prevControlRev = rev;
  }

  // Rebuild stateful-lane items from the events array. Cheap because
  // events typically number in the hundreds even for hour-long sessions.
  renderStatefulLanes(t);
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

watch(
  () => player.value,
  async (p) => {
    if (!p) return;
    await ensureTimeline();
    ingest(p);
  },
  { immediate: true },
);

watch(
  () => [coord.state.version, coord.state.paused, coord.state.lastSampleMs] as const,
  () => {
    if (!timeline) return;
    const vp = coord.effectiveViewport.value;
    suppressNextRangeChange = true;
    timeline.setWindow(vp.min, vp.max, { animation: false });
  },
);

/**
 * Live-edge wheel zoom anchor — port of legacy session-shell.js
 * `ensureVisTimelineLiveWheelAnchor`. Same rule as the chartjs charts:
 *
 *   - LIVE  (not paused): Alt+wheel updates `coord.liveSpanMs`, the
 *     timeline tracks the live edge with the new span (cursor position
 *     ignored). preventDefault + stopPropagation so vis-timeline's own
 *     wheel handler doesn't double-zoom.
 *   - PAUSED: fall through to vis-timeline's default mouse-anchored
 *     wheel zoom (which fires `rangechanged` and refreshes the sticky
 *     viewport).
 *
 * Capture-phase listener on the timeline container so we run before
 * vis-timeline's own handler. */
function installLiveWheelAnchor() {
  const el = container.value;
  if (!el) return;
  el.addEventListener(
    'wheel',
    (e: WheelEvent) => {
      if (!e.altKey) return;
      if (coord.state.paused) return;
      e.preventDefault();
      e.stopPropagation();
      const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
      const windowMs = coord.state.windowMs;
      const currentSpan =
        coord.state.liveSpanMs != null ? coord.state.liveSpanMs : windowMs;
      const MIN_SPAN_MS = 1_000;
      const nextSpan = Math.max(
        MIN_SPAN_MS,
        Math.min(windowMs, currentSpan * factor),
      );
      coord.setLiveSpanMs(nextSpan >= windowMs ? null : nextSpan);
    },
    { capture: true, passive: false },
  );
}

const expandedClass = computed(() => (coord.state.expanded ? 'expanded' : ''));
const pauseLabel = computed(() => (coord.state.paused ? '▶ Live' : '⏸ Pause'));
const zoomActive = computed(() => coord.state.viewport !== null || userInteracted);

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
        <button class="btn btn-expand" type="button" :class="{ active: coord.state.expanded }"
          @click="coord.toggleExpanded()"
          :title="coord.state.expanded ? 'Restore default chart height' : 'Double this chart\'s height for a closer look'">
          <span class="chart-expand-icon">⤢</span>
          {{ coord.state.expanded ? 'Collapse' : 'Expand' }}
        </button>
        <button class="btn" type="button" :class="{ active: zoomActive }"
          @click="coord.resetZoom(); userInteracted = false" title="Snap back to live edge">
          Reset Zoom
        </button>
        <button class="btn" type="button" :class="{ live: coord.state.paused }"
          @click="coord.togglePause()">
          {{ pauseLabel }}
        </button>
        <span class="hint">Alt+scroll · drag pan</span>
      </div>
    </div>

    <!-- Static legend strip — every section's colour key. -->
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

    <div class="strip-wrap" :class="expandedClass">
      <div ref="container" class="strip" />
      <div v-if="coord.state.paused" class="paused-badge">PAUSED</div>
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
.btn.live { background: #10b981; border-color: #059669; color: white; }
.btn.live:hover { background: #059669; }
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
.paused-badge {
  position: absolute;
  top: 4px;
  /* `right: 6px` would place it past the reserved gutter — shift in
   * by 60+6 so it sits inside the plot area instead. */
  right: 66px;
  background: rgba(31, 41, 55, 0.85);
  color: #fde68a;
  padding: 1px 6px;
  border-radius: 4px;
  font-size: 10px;
  font-weight: 700;
  letter-spacing: 1px;
  pointer-events: none;
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
