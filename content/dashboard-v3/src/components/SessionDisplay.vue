<script setup lang="ts">
/**
 * SessionDisplay.vue — shared display layout for both the live
 * testing-session page and the archive session-viewer page.
 *
 * Owns:
 *   - Display panels (Session Details, Player Metrics, Player State,
 *     Bandwidth/RTT/Buffer/FPS charts, Network Log)
 *   - Brush rail (skip buttons, focus window, drag-pan, click-recenter)
 *   - Event filter accordion (Effects/Causes pills + priority tier
 *     groups + per-tier and per-type eye toggles + instance drilldown)
 *   - Synchronized "selected event" cursor across all charts
 *   - Prev/next nav with scope locking (tier / type) and keyboard ,/.
 *
 * Mode-driven behaviour:
 *   - mode='archive' — brush + accordion + nav always visible. Bulk-feed
 *     drives chart history; setArchivePlayer + setQueryData prime the
 *     TanStack cache so the same display panels render archive data
 *     with no per-component refactor.
 *   - mode='live'    — brush + accordion + nav HIDDEN until the operator
 *     pauses (clicks a chart, drags the brush, or hits ⏸). Live SSE
 *     fills the chart per-sample via MetricsLineChart's player.value
 *     watcher; this component's bulk-feed only fires on initial
 *     historical preload (when sessionId+playId are known).
 *
 * Composables run unconditionally; if sessionId is empty (live mode
 * before historical resolution), they short-circuit and return empty.
 */
import { ref, computed, watch, onMounted, onBeforeUnmount } from 'vue';
import { useQueryClient } from '@tanstack/vue-query';
import CollapsibleSection from '@/components/CollapsibleSection.vue';
import SessionDetails from '@/components/SessionDetails.vue';
import PlayerMetrics from '@/components/PlayerMetrics.vue';
import EventsTimeline from '@/components/EventsTimeline.vue';
import BandwidthChart from '@/components/BandwidthChart.vue';
import RTTChart from '@/components/RTTChart.vue';
import BufferChart from '@/components/BufferChart.vue';
import FPSChart from '@/components/FPSChart.vue';
import NetworkLog from '@/components/NetworkLog.vue';
import BitrateChartPanelToolbar from '@/components/BitrateChartPanelToolbar.vue';
import { useChartCoordination } from '@/composables/useChartCoordination';
import { useArchivedSessionEvents, type SessionEvent } from '@/composables/useArchivedSessionEvents';
import { usePlayer } from '@/composables/usePlayer';
import { useSessionTimeSeries } from '@/composables/useSessionTimeSeries';
import { chRowToPlayerRecord, tsOfRow } from '@/composables/chRowAdapter';
import {
  setArchivePlayer,
  type PlayerRecord,
} from '@/repo/v2-repo';

const props = defineProps<{
  /** Canonical player UUID. Used identically in live and archive
   *  modes — SSE registration, v3 timeseries subscription, panel cache
   *  keying. (session_id retired: SSE keys by player_id only and the
   *  v3 endpoint resolves rows by player_id.) */
  playerId: string;
  /** Play id. In live mode usually null (subscribe across plays); in
   *  archive mode the specific play being replayed. */
  playId: string | null;
  /** archive = sticky brush + replay banner visible; live = brush
   *  hidden until paused. The v3 timeseries server decides actual
   *  liveness via its `meta.live` event regardless of this prop. */
  mode: 'live' | 'archive';
  /** Suppress the internal Session Details panel. The live testing
   *  pages mount their own Session Details right before Fault Injection
   *  (above the control panels), so they pass this flag and SessionDisplay
   *  omits the duplicate panel from its stack. */
  hideSessionDetails?: boolean;
}>();

/** Live vs archive switches the brush-rail visibility default and
 *  whether to extend timeRange.max with `coord.lastSampleMs` for the
 *  live edge. Driven by the `mode` prop — the page knows which one
 *  it's rendering. The v3 stream subscription works the same in both
 *  modes (player_id + optional play_id). */
const isLive = computed(() => props.mode !== 'archive');

const playIdRef = computed(() => props.playId);

// usePlayer subscribes to live SSE + the all-pool stream. In live
// mode the TanStack cache is fed by SSE; in archive mode usePlayer's
// GET will 404 (player isn't in the proxy roster) — that's fine,
// nothing reads `livePlayer.value` in archive mode.
const livePlayerIdRef = computed(() => (isLive.value ? props.playerId : ''));
const { player: livePlayer } = usePlayer(livePlayerIdRef);

// Defensive: only adopt usePlayer's canonical-case id when it
// matches props.playerId case-insensitively. The shared TanStack
// cache + all-pool SSE pool can briefly leak a record from a
// DIFFERENT player during cache invalidation/refetch races; passing
// that wrong UUID into useSessionTimeSeries would re-subscribe the
// SSE to a foreign player. Fall back to props.playerId whenever the
// live record disagrees.
const apiPlayerIdRef = computed(() => {
  const fromLive = livePlayer.value?.id;
  if (fromLive && fromLive.toLowerCase() === props.playerId.toLowerCase()) {
    return fromLive;
  }
  return props.playerId;
});

// Internal cache key for the side-channel archive store (v2-repo).
// In archive mode we synthesize an `archive:<player_id>:<play_id>`
// key so the panel cursor (set by the windowEndMs watcher below)
// doesn't collide with the live cache for the same player_id. In
// live mode the raw player_id IS the cache key.
const archivePlayerId = computed(() =>
  isLive.value
    ? props.playerId
    : `archive:${props.playerId}:${props.playId ?? 'all'}`,
);

// Event accordion source. The events stream isn't on the v3 timeseries
// endpoint yet (out of scope for TS6); kept here so the brush-rail
// tick markers, the priority/tier filter UI, and the prev/next nav
// keep functioning unchanged. Filters by player_id + play_id only —
// session_id retired.
const { events: sessionEvents } = useArchivedSessionEvents(apiPlayerIdRef, playIdRef);

// v3 unified time-series model. Single subscription per
// SessionDisplay drives:
//   - Bandwidth / RTT / Buffer / FPS charts (samples)
//   - EventsTimeline swim lanes (samples → ingest)
//   - NetworkLog waterfall (network)
//   - Focus bar / brush rail bounds (rangeBounds + coord.lastSampleMs)
//   - PlayerMetrics / SessionDetails panels in archive mode
//     (lastAt(windowEndMs) + chRowToPlayerRecord adapter feeding
//     setArchivePlayer)
// session_details bundle is requested so the snapshot-cursor row
// carries enough identity (manifest_url etc.) for the panels.
//
// Identity refs:
//   - apiPlayerIdRef is defensively stable against the shared
//     TanStack cache race (see its definition above).
//   - playIdRef is null in live mode (server returns rows across all
//     plays so the brush handles boundary moves naturally) and the
//     specific archived play in archive mode.
const timeseriesPlayId = computed<string | null>(() => isLive.value ? null : props.playId);
const timeseries = useSessionTimeSeries(
  apiPlayerIdRef,
  timeseriesPlayId,
  {
    streams: ['samples', 'network'],
    bundles: ['charts_minimal', 'lanes_v1', 'session_details', 'network'],
  },
);

// coord declared up-front so the `timeRange` computed below can read
// `coord.state.lastSampleMs` (live edge) without a temporal dead zone
// — and so any earlier reactive code (window watcher, brush clamps)
// sees a coord instance even though it gets consumed mostly later.
const coord = useChartCoordination(archivePlayerId);

/** Effective time range for the brush rail. Reads the cached
 *  rangeBounds of the samples stream as the historical span; in live
 *  mode extend `max` with `coord.lastSampleMs` so the rail's right
 *  edge tracks the live tail even when the cache hasn't received the
 *  freshest CH backfill yet. */
const timeRange = computed<{ min: number; max: number } | null>(() => {
  const ar = timeseries.samples.rangeBounds.value;
  const live = isLive.value ? (coord.state.lastSampleMs || 0) : 0;
  if (!ar && !live) return null;
  if (!ar) return { min: live, max: live };
  if (!live) return ar;
  return { min: ar.min, max: Math.max(ar.max, live) };
});

const loading = computed(() => timeseries.samples.loading.value);
const error = computed(() => timeseries.samples.error.value);
const progressLabel = computed(() => loading.value ? 'Streaming snapshots…' : '');
// Approximate count of rendered samples — used in the brush-rail
// status line. The cache only grows; reading via inRange touches
// `version` so this stays reactive on every flush.
const samplesCount = computed(() => {
  void timeseries.samples.version.value;
  return timeseries.samples.inRange(0, Number.MAX_SAFE_INTEGER).length;
});

/* ─── Event filter ──────────────────────────────────────────────── */

function eventMs(ev: SessionEvent): number {
  const raw = ev.ts ?? ev.event_time;
  if (!raw) return NaN;
  const iso = typeof raw === 'string' && !raw.includes('T')
    ? raw.replace(' ', 'T') + 'Z'
    : String(raw);
  const v = Date.parse(iso);
  return Number.isFinite(v) ? v : NaN;
}

// L0 — Kind toggle (orthogonal: effect vs cause)
const enabledKind = ref<Record<'effect' | 'cause', boolean>>({
  effect: true,
  cause: false,
});

// L1 — Priority tier
type Priority = 1 | 2 | 3 | 4;
const PRIORITY_ORDER: Priority[] = [1, 2, 3, 4];
const PRIORITY_META: Record<Priority, { label: string; color: string; bg: string; border: string }> = {
  1: { label: 'Critical', color: '#dc2626', bg: '#fee2e2', border: '#fca5a5' },
  2: { label: 'High',     color: '#b45309', bg: '#fef3c7', border: '#fcd34d' },
  3: { label: 'Medium',   color: '#1d4ed8', bg: '#dbeafe', border: '#93c5fd' },
  4: { label: 'Low',      color: '#4b5563', bg: '#e5e7eb', border: '#9ca3af' },
};

const expandedTiers = ref<Record<Priority, boolean>>({
  1: true, 2: true, 3: false, 4: false,
});
function toggleTier(p: Priority) {
  expandedTiers.value[p] = !expandedTiers.value[p];
}

const visiblePriority = ref<Record<Priority, boolean>>({
  1: true, 2: true, 3: true, 4: false,
});
function togglePriorityVisibility(p: Priority, e: MouseEvent) {
  e.stopPropagation();
  const willBeVisible = !visiblePriority.value[p];
  visiblePriority.value[p] = willBeVisible;
  // Cascade to type-level eyes.
  const next = new Set(hiddenTypeKeys.value);
  for (const t of tierTypes.value[p]) {
    const k = typeKey(t.type, p);
    if (willBeVisible) next.delete(k);
    else next.add(k);
  }
  hiddenTypeKeys.value = next;
}

const hiddenTypeKeys = ref<Set<string>>(new Set());
function isTypeVisible(t: string, p: Priority): boolean {
  return !hiddenTypeKeys.value.has(typeKey(t, p));
}
function toggleTypeVisibility(t: string, p: Priority, e: MouseEvent) {
  e.stopPropagation();
  const k = typeKey(t, p);
  const next = new Set(hiddenTypeKeys.value);
  if (next.has(k)) next.delete(k); else next.add(k);
  hiddenTypeKeys.value = next;
}

const lockedPriority = ref<Priority | null>(null);
const lockedType = ref<string | null>(null);
function selectTier(p: Priority) {
  if (lockedPriority.value === p && !lockedType.value) {
    lockedPriority.value = null;
  } else {
    lockedPriority.value = p;
    lockedType.value = null;
    navCursor.value = 0;
    expandedTiers.value[p] = true;
  }
}
function selectType(t: string, p: Priority) {
  if (lockedType.value === t && lockedPriority.value === p) {
    lockedType.value = null;
  } else {
    lockedPriority.value = p;
    lockedType.value = t;
    navCursor.value = 0;
    expandedTypeKey.value = typeKey(t, p);
  }
}
function clearScope() {
  lockedPriority.value = null;
  lockedType.value = null;
}

const expandedTypeKey = ref<string | null>(null);
function typeKey(t: string, p: Priority): string { return `${p}|${t}`; }
function toggleTypeExpand(t: string, p: Priority) {
  const k = typeKey(t, p);
  expandedTypeKey.value = expandedTypeKey.value === k ? null : k;
}

function eventPriority(ev: SessionEvent): Priority {
  const p = ev.priority;
  return (p === 1 || p === 2 || p === 3 || p === 4) ? p : 3;
}
function eventKindCE(ev: SessionEvent): 'effect' | 'cause' {
  return ev.kind === 'cause' ? 'cause' : 'effect';
}

interface AnnotatedEvent extends SessionEvent {
  _ts: number;
  _p: Priority;
}

const kindFilteredEvents = computed<AnnotatedEvent[]>(() =>
  sessionEvents.value
    .filter((ev) => enabledKind.value[eventKindCE(ev)])
    .map((ev) => ({ ...ev, _ts: eventMs(ev), _p: eventPriority(ev) }))
    .filter((ev) => Number.isFinite(ev._ts))
    .sort((a, b) => a._ts - b._ts),
);

const filteredEvents = computed<AnnotatedEvent[]>(
  () => kindFilteredEvents.value.filter((ev) => {
    if (!visiblePriority.value[ev._p]) return false;
    const k = `${ev._p}|${ev.type ?? 'event'}`;
    if (hiddenTypeKeys.value.has(k)) return false;
    return true;
  }),
);

const tierCounts = computed<Record<Priority, number>>(() => {
  const c: Record<Priority, number> = { 1: 0, 2: 0, 3: 0, 4: 0 };
  for (const ev of kindFilteredEvents.value) c[ev._p]++;
  return c;
});

function kindCount(k: 'effect' | 'cause'): number {
  let n = 0;
  for (const ev of sessionEvents.value) if (eventKindCE(ev) === k) n++;
  return n;
}

const tierTypes = computed<Record<Priority, Array<{ type: string; count: number }>>>(() => {
  const buckets: Record<Priority, Map<string, number>> = {
    1: new Map(), 2: new Map(), 3: new Map(), 4: new Map(),
  };
  for (const ev of kindFilteredEvents.value) {
    const t = String(ev.type ?? 'event');
    buckets[ev._p].set(t, (buckets[ev._p].get(t) ?? 0) + 1);
  }
  const out = {} as Record<Priority, Array<{ type: string; count: number }>>;
  for (const p of PRIORITY_ORDER) {
    out[p] = [...buckets[p].entries()]
      .map(([type, count]) => ({ type, count }))
      .sort((a, b) => a.count - b.count);
  }
  return out;
});

const tierTypeInstances = computed<Record<string, AnnotatedEvent[]>>(() => {
  const out: Record<string, AnnotatedEvent[]> = {};
  for (const ev of kindFilteredEvents.value) {
    const k = `${ev._p}|${ev.type ?? 'event'}`;
    (out[k] ??= []).push(ev);
  }
  return out;
});

function selectInstance(ev: AnnotatedEvent, t: string, p: Priority) {
  if (lockedType.value !== t || lockedPriority.value !== p) {
    lockedPriority.value = p;
    lockedType.value = t;
    expandedTiers.value[p] = true;
    expandedTypeKey.value = typeKey(t, p);
  }
  const idx = navEvents.value.findIndex(
    (e) => e._ts === ev._ts && e.type === ev.type,
  );
  if (idx >= 0) {
    navCursor.value = idx;
    recenterOnNav();
  }
}

function tierPreview(p: Priority): Array<{ type: string; count: number }> {
  return tierTypes.value[p].slice(0, 5);
}
function tierPreviewMore(p: Priority): number {
  return Math.max(0, tierTypes.value[p].length - 5);
}
function pickPreviewType(t: string, p: Priority) {
  expandedTiers.value[p] = true;
  selectType(t, p);
}

const scopeLabel = computed<string>(() => {
  if (lockedType.value && lockedPriority.value) {
    return `${lockedType.value} (in ${PRIORITY_META[lockedPriority.value].label})`;
  }
  if (lockedPriority.value) {
    return `All ${PRIORITY_META[lockedPriority.value].label} events`;
  }
  return `All events (${filteredEvents.value.length})`;
});

/* ─── Prev/next navigation ──────────────────────────────────────── */

const navEvents = computed<AnnotatedEvent[]>(() => {
  let arr = filteredEvents.value;
  if (lockedPriority.value) arr = arr.filter((e) => e._p === lockedPriority.value);
  if (lockedType.value)     arr = arr.filter((e) => e.type === lockedType.value);
  return arr;
});
const navCursor = ref(0);
const navCurrent = computed<AnnotatedEvent | null>(
  () => navEvents.value[navCursor.value] ?? null,
);
watch(navEvents, (arr) => {
  if (navCursor.value >= arr.length) navCursor.value = Math.max(0, arr.length - 1);
});
function navPrev() {
  const n = navEvents.value.length; if (n === 0) return;
  navCursor.value = (navCursor.value - 1 + n) % n;
  recenterOnNav();
}
function navNext() {
  const n = navEvents.value.length; if (n === 0) return;
  navCursor.value = (navCursor.value + 1) % n;
  recenterOnNav();
}
function recenterOnNav() {
  const ev = navCurrent.value; if (!ev) return;
  const half = (windowEndMs.value - windowStartMs.value) / 2;
  windowStartMs.value = clampStart(ev._ts - half);
  windowEndMs.value = clampEnd(ev._ts + half);
  userMovedBrush.value = true;
}

watch(navCurrent, (ev) => {
  coord.setCursorMs(ev ? ev._ts : null);
});

function onKey(e: KeyboardEvent) {
  const t = e.target as HTMLElement | null;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'SELECT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
  if (e.key === ',') { e.preventDefault(); navPrev(); }
  else if (e.key === '.') { e.preventDefault(); navNext(); }
  else if (e.key === 'Escape') {
    if (lockedType.value) lockedType.value = null;
    else if (lockedPriority.value) lockedPriority.value = null;
  }
}

const railMarkers = computed(() => {
  const r = timeRange.value;
  if (!r || !filteredEvents.value.length) return [] as Array<{ leftPct: number; color: string; opacity: number; isCurrent: boolean; title: string; ts: number; ev: AnnotatedEvent }>;
  const span = Math.max(1, r.max - r.min);
  const cur = navCurrent.value;
  return filteredEvents.value.map((ev) => {
    const pct = Math.max(0, Math.min(100, ((ev._ts - r.min) / span) * 100));
    const isCurrent = !!cur && cur._ts === ev._ts && cur.type === ev.type;
    return {
      leftPct: pct,
      color: PRIORITY_META[ev._p].color,
      opacity: eventKindCE(ev) === 'effect' ? 1 : 0.4,
      isCurrent,
      ts: ev._ts,
      ev,
      title: `${ev.type ?? 'event'} · p${ev._p} · ${fmtTime(ev._ts)}${ev.info ? ' · ' + ev.info : ''}`,
    };
  });
});

/* ─── Brush + focus window ──────────────────────────────────────── */

const windowStartMs = ref<number>(0);
const windowEndMs = ref<number>(0);
const windowSpanMs = computed(() => Math.max(1, windowEndMs.value - windowStartMs.value));

const DEFAULT_FOCUS_MS = 10 * 60 * 1000;
const userMovedBrush = ref(false);

/** Same checked rule as the chart-toolbar Live toggles — the brush
 *  rail's Live button reflects the shared `coord.state.viewport`. */
const brushLiveChecked = computed(() => coord.state.viewport === null);
watch(timeRange, (r) => {
  if (!r) return;
  const fullSpan = r.max - r.min;
  // When the brush is tracking the live edge (!userMovedBrush) we
  // preserve whatever WIDTH it already has — so if the operator
  // resized to 3 min and then dropped at live, the live-tracking
  // window stays 3 min wide. Only fall back to DEFAULT_FOCUS_MS on
  // the very first paint when no width exists yet.
  if (!userMovedBrush.value) {
    // Live-tracking: span is driven by coord.liveSpanMs (set by chart
    // Alt+wheel) or DEFAULT_FOCUS_MS. Never carry forward the previous
    // tick's currentSpan — that's what got the brush stuck at 30 s
    // when a fresh session started narrow and we never grew past it.
    // Capped at fullSpan so a very young session (1 s of data) doesn't
    // try to show empty area before r.min.
    const liveSpan = coord.state.liveSpanMs;
    const targetSpan = liveSpan != null
      ? Math.min(liveSpan, fullSpan)
      : Math.min(DEFAULT_FOCUS_MS, fullSpan);
    const newStart = Math.max(r.min, r.max - targetSpan);
    windowStartMs.value = newStart;
    windowEndMs.value = r.max;
    return;
  }
  if (windowStartMs.value < r.min) windowStartMs.value = r.min;
  if (windowEndMs.value > r.max) windowEndMs.value = r.max;
  if (windowStartMs.value >= windowEndMs.value) {
    const span = Math.min(DEFAULT_FOCUS_MS, fullSpan);
    windowStartMs.value = Math.max(r.min, r.max - span);
    windowEndMs.value = r.max;
  }
});

// Chart Alt+wheel updates coord.liveSpanMs to narrow the visible
// span around the live edge. The brush width tracks chart zoom
// unconditionally — zooming on a chart IS a request to resize the
// focus rail, regardless of whether the brush is currently pinned.
// Position-pinning (userMovedBrush) governs WHERE the window lives,
// not how wide it is.
watch(
  () => coord.state.liveSpanMs,
  (span) => {
    const r = timeRange.value;
    if (!r) return;
    const targetSpan = span ?? Math.min(DEFAULT_FOCUS_MS, r.max - r.min);
    // Anchor at the CURRENT right edge when the user has parked the
    // brush off live — Alt+wheel on a chart should only resize the
    // span, not rip the window back to "now". When at live (or never
    // moved off), anchor at r.max so the live-tracking semantics hold.
    const anchorRight = userMovedBrush.value ? windowEndMs.value : r.max;
    console.log('[BR] liveSpanMs watch span=' + (span ?? 'null') + ' → brush ' + Math.round(targetSpan/1000) + 's, anchor=' + (userMovedBrush.value ? 'current' : 'live'));
    windowStartMs.value = Math.max(r.min, anchorRight - targetSpan);
    windowEndMs.value = anchorRight;
  },
);

function clampStart(v: number) {
  const r = timeRange.value; if (!r) return v;
  return Math.max(r.min, Math.min(v, windowEndMs.value - 1000));
}
function clampEnd(v: number) {
  const r = timeRange.value; if (!r) return v;
  return Math.min(r.max, Math.max(v, windowStartMs.value + 1000));
}

/* ─── Brush drag handling ───────────────────────────────────────── */

const railRef = ref<HTMLElement | null>(null);
const dragState = ref<{ mode: 'pan' | 'resize-left' | 'resize-right'; startX: number; startStart: number; startEnd: number } | null>(null);

function railFracToMs(frac: number): number {
  const r = timeRange.value; if (!r) return 0;
  return r.min + Math.max(0, Math.min(1, frac)) * (r.max - r.min);
}
function pxToMs(px: number): number {
  const w = railRef.value?.clientWidth ?? 1;
  const r = timeRange.value; if (!r || w <= 0) return 0;
  return (px / w) * (r.max - r.min);
}

function onBrushMouseDown(e: MouseEvent, mode: 'pan' | 'resize-left' | 'resize-right') {
  e.preventDefault();
  e.stopPropagation();
  userMovedBrush.value = true;
  dragState.value = {
    mode,
    startX: e.clientX,
    startStart: windowStartMs.value,
    startEnd: windowEndMs.value,
  };
  window.addEventListener('mousemove', onDragMove);
  window.addEventListener('mouseup', onDragEnd, { once: true });
}
function onDragMove(e: MouseEvent) {
  const d = dragState.value; if (!d) return;
  const r = timeRange.value; if (!r) return;
  const dms = pxToMs(e.clientX - d.startX);
  const MIN_WINDOW_MS = 1000;
  if (d.mode === 'pan') {
    const span = d.startEnd - d.startStart;
    let s = d.startStart + dms;
    let f = s + span;
    if (s < r.min) { s = r.min; f = s + span; }
    if (f > r.max) { f = r.max; s = f - span; }
    windowStartMs.value = s;
    windowEndMs.value = f;
  } else if (d.mode === 'resize-left') {
    let s = d.startStart + dms;
    if (s < r.min) s = r.min;
    if (s > d.startEnd - MIN_WINDOW_MS) s = d.startEnd - MIN_WINDOW_MS;
    windowStartMs.value = s;
  } else if (d.mode === 'resize-right') {
    let f = d.startEnd + dms;
    if (f > r.max) f = r.max;
    if (f < d.startStart + MIN_WINDOW_MS) f = d.startStart + MIN_WINDOW_MS;
    windowEndMs.value = f;
  }
}
function onDragEnd() {
  dragState.value = null;
  window.removeEventListener('mousemove', onDragMove);
  const r = timeRange.value;
  // RIGHT EDGE position determines the pinned-vs-following state.
  // Drop within 2 s of the live sample → following live (charts and
  // brush re-anchor at r.max via the watchers below).
  // Drop away from live → pinned to the brush window.
  const atLiveEdge =
    !!r
    && props.mode !== 'archive'
    && !coord.state.paused
    && r.max - windowEndMs.value <= 2000;
  const dropSpan = windowEndMs.value - windowStartMs.value;
  if (atLiveEdge) userMovedBrush.value = false;

  // BRUSH WIDTH on release becomes the new liveSpanMs — operator's
  // intent regardless of where the right edge ended up. Pinned drops
  // store the span so it survives the round trip when they later
  // click Live to return; live drops update the span immediately so
  // every other chart's live-tracker uses the same width.
  if (isLive.value && dropSpan > 0 && coord.state.liveSpanMs !== dropSpan) {
    coord.setLiveSpanMs(dropSpan);
  }

  // Reconcile viewport with the new brush window. !userMovedBrush →
  // clears viewport (charts follow live). userMovedBrush → pins
  // viewport to the brush range.
  applyWindowToViewport();
}
/** Alt+wheel on the brush rail zooms the focus-window duration.
 *
 *   - AT LIVE (right edge ≈ r.max): left-edge-only — right stays
 *     glued to live, span shrinks/grows from the left.
 *   - OFF LIVE: mouse-anchored — the time under the cursor stays
 *     fixed while both edges move. If the resulting right edge
 *     reaches live, snap the brush back to live tracking
 *     (userMovedBrush=false) so further zooms take the left-only
 *     path.
 *
 *  Plain wheel falls through to native page scroll. */
function onRailWheel(e: WheelEvent) {
  if (!e.altKey) return;
  e.preventDefault();
  e.stopPropagation();
  const rail = railRef.value;
  const r = timeRange.value;
  if (!rail || !r) return;
  const fullSpan = Math.max(1, r.max - r.min);
  const currentSpan = Math.max(1, windowEndMs.value - windowStartMs.value);
  const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
  const MIN_SPAN_MS = 1_000;
  const nextSpan = Math.max(MIN_SPAN_MS, Math.min(fullSpan, currentSpan * factor));
  if (nextSpan === currentSpan) return;
  const atLive = isLive.value && r.max - windowEndMs.value <= 2000;

  let newStart: number;
  let newEnd: number;
  if (atLive) {
    newEnd = r.max;
    newStart = Math.max(r.min, newEnd - nextSpan);
  } else {
    // Mouse-anchored: keep the timestamp under the cursor at the same
    // x position in the rail after the zoom.
    const rect = rail.getBoundingClientRect();
    const frac = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5;
    const anchorTime = r.min + frac * fullSpan;
    const anchorFracInWindow = (anchorTime - windowStartMs.value) / currentSpan;
    newStart = anchorTime - anchorFracInWindow * nextSpan;
    newEnd = newStart + nextSpan;
    if (newStart < r.min) { newStart = r.min; newEnd = newStart + nextSpan; }
    if (newEnd > r.max) { newEnd = r.max; newStart = newEnd - nextSpan; }
  }

  windowStartMs.value = newStart;
  windowEndMs.value = newEnd;
  // Snap back to live tracking if the zoom landed at the live edge.
  const endedAtLive = isLive.value && r.max - newEnd <= 2000;
  userMovedBrush.value = !endedAtLive;
}

function onRailMouseDown(e: MouseEvent) {
  if (e.defaultPrevented) return;
  if (!(e.target instanceof HTMLElement)) return;
  if (e.target.closest('.brush-window, .brush-tick')) return;
  const rail = railRef.value; if (!rail) return;
  const rect = rail.getBoundingClientRect();
  if (rect.width <= 0) return;
  const frac = (e.clientX - rect.left) / rect.width;
  const target = railFracToMs(frac);
  const span = windowEndMs.value - windowStartMs.value;
  const r = timeRange.value; if (!r) return;
  let s = target - span / 2;
  let f = s + span;
  if (s < r.min) { s = r.min; f = s + span; }
  if (f > r.max) { f = r.max; s = f - span; }
  windowStartMs.value = s;
  windowEndMs.value = f;
  userMovedBrush.value = true;
}

/* ─── Brush-driven panel cursor ─────────────────────────────────── */

const qc = useQueryClient();
function playerKey(id: string) {
  return ['player', id] as const;
}

watch([windowStartMs, windowEndMs], () => {
  // Brush WIDTH drives coord.liveSpanMs in live-tracking mode so
  // chart Alt+wheel zoom and brush drag stay in sync. We do NOT feed
  // back into coord.windowMs anymore — doing so capped the wheel
  // anchor's zoom-out at the current brush width and trips the
  // "snap to null" branch, jumping from e.g. 1 min straight back to
  // 10 min. windowMs stays the slow-changing "max default rolling
  // window" (10 min); liveSpanMs is the active zoom span.
  //
  // Auto-feedback to liveSpanMs only when we're NOT actively dragging
  // — the brush watcher fires on every windowStart/End change, and
  // mid-drag the right edge can briefly be within the 2 s live
  // tolerance even when the operator is on their way OFF live. That
  // would flip userMovedBrush back to false and the liveSpanMs
  // watcher would yank the brush right edge to r.max, fighting the
  // drag. onDragEnd handles the "released AT live → update span"
  // case explicitly instead.
  const focusSpan = windowEndMs.value - windowStartMs.value;
  if (
    focusSpan > 0
    && isLive.value
    && !userMovedBrush.value
    && coord.state.liveSpanMs !== focusSpan
  ) {
    coord.setLiveSpanMs(focusSpan);
  }
  applyWindowToViewport();
});

function applyWindowToViewport() {
  // Three states for the chart viewport:
  //  1. Operator has explicitly moved the brush — pin charts to that
  //     window (frozen view, even on live mode).
  //  2. Paused — same: pin charts to the window the operator pinned
  //     by hitting pause.
  //  3. Neither, AND we're in live mode — hand the viewport back to
  //     coord by clearing it. `effectiveViewport` then follows the
  //     live edge with the coord's `windowMs` span, so the charts
  //     keep advancing as new samples arrive. This is what `>>` and
  //     drag-to-live-edge both want.
  //
  // Archive mode never falls into the "hand back to coord" branch —
  // there is no live edge to follow, and `effectiveViewport`'s
  // live-edge fallback would snap the charts to `[lastSample -
  // windowMs, lastSample]`, throwing away the brush window.
  if (props.mode !== 'archive' && !userMovedBrush.value && !coord.state.paused) {
    if (coord.state.viewport != null) coord.setViewport(null);
    return;
  }
  if (windowStartMs.value && windowEndMs.value && windowStartMs.value < windowEndMs.value) {
    coord.setViewport({ min: windowStartMs.value, max: windowEndMs.value });
  }
}

watch(
  () => coord.state.viewport,
  (v) => {
    console.log('[BR] coord.viewport watch', v ? `[span=${Math.round((v.max-v.min)/1000)}s]` : 'null', 'userMoved=' + userMovedBrush.value);
    if (v) {
      if (windowStartMs.value === v.min && windowEndMs.value === v.max) return;
      console.log('[BR] coord.viewport set → brush ' + Math.round((v.max-v.min)/1000) + 's, userMovedBrush=true');
      windowStartMs.value = v.min;
      windowEndMs.value = v.max;
      userMovedBrush.value = true;
      return;
    }
    // viewport=null = explicit return to following live (via the Live
    // toggle's checked → unchecked path or snap-back-to-live on zoom).
    // Snap brush right edge to live and ALWAYS clear userMovedBrush
    // so charts AND brush move together. Span preference order:
    //   1. liveSpanMs — set by chart Alt+wheel; the operator picked it
    //   2. currentSpan — whatever the brush was when pinned (e.g. a
    //      manual 5-min drag); preserves their inspection width
    //   3. DEFAULT_FOCUS_MS — last-resort fallback on a fresh session
    //      where neither has ever been set
    if (!isLive.value) return;
    const r = timeRange.value; if (!r) return;
    const fullSpan = r.max - r.min;
    const liveSpan = coord.state.liveSpanMs;
    const currentSpan = windowEndMs.value - windowStartMs.value;
    let targetSpan: number;
    if (liveSpan != null) targetSpan = Math.min(liveSpan, fullSpan);
    else if (currentSpan > 0) targetSpan = Math.min(currentSpan, fullSpan);
    else targetSpan = Math.min(DEFAULT_FOCUS_MS, fullSpan);
    const newStart = Math.max(r.min, r.max - targetSpan);
    windowStartMs.value = newStart;
    windowEndMs.value = r.max;
    userMovedBrush.value = false;
  },
);

// Panel-cursor sync: the SessionDetails / PlayerMetrics panels read
// from the v2-archive store (or live cache in live mode). For archive
// mode we project the row at the brush's right edge — adapted from
// CH via chRowToPlayerRecord — into the archive store so panels stay
// in sync with the focus window.
watch(
  [windowEndMs, () => timeseries.samples.version.value],
  () => {
    if (isLive.value) return;
    const row = timeseries.samples.lastAt(windowEndMs.value);
    if (!row) return;
    const adapted = chRowToPlayerRecord(row);
    setArchivePlayer(archivePlayerId.value, adapted);
    qc.setQueryData(playerKey(archivePlayerId.value), { player: adapted, etag: undefined });
  },
);

onMounted(() => window.addEventListener('keydown', onKey));
onBeforeUnmount(() => window.removeEventListener('keydown', onKey));

/* ─── Misc helpers ──────────────────────────────────────────────── */

function fmtTime(ms: number): string {
  if (!Number.isFinite(ms) || !ms) return '—';
  const d = new Date(ms);
  return d.toLocaleString('en-US', { hour12: false }) + '.' +
    String(d.getMilliseconds()).padStart(3, '0');
}
function fmtDur(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  const s = Math.floor(ms / 1000);
  const hh = Math.floor(s / 3600);
  const mm = Math.floor((s % 3600) / 60);
  const ss = s % 60;
  return hh ? `${hh}h ${mm}m ${ss}s` : `${mm}m ${ss}s`;
}
function fmtDurShort(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

const scrubMin = computed(() => timeRange.value?.min ?? 0);
const scrubMax = computed(() => timeRange.value?.max ?? 0);

function skipToStart() {
  const r = timeRange.value; if (!r) return;
  const span = Math.max(1000, windowEndMs.value - windowStartMs.value);
  windowStartMs.value = r.min;
  windowEndMs.value = Math.min(r.max, r.min + span);
  userMovedBrush.value = true;
}
function skipToEnd() {
  const r = timeRange.value; if (!r) return;
  const span = Math.max(1000, windowEndMs.value - windowStartMs.value);
  windowEndMs.value = r.max;
  windowStartMs.value = Math.max(r.min, r.max - span);
  userMovedBrush.value = false;
}

</script>

<template>
  <div class="session-display">
    <!-- Brush + event filter + nav-bar live in a persistent fold so
         the operator can collapse them out of the way without losing
         the live view to a pause-state toggle. Pause-gated visibility
         was disruptive — the section appearing / disappearing under
         the panels caused layout jumps. Now always rendered; operator
         hides via the fold chevron. -->
    <CollapsibleSection title="Focus Window" :open="true" persist-key="focus-window">
    <!-- Empty-state placeholder: a fresh session has no archived
         snapshots until the forwarder ingests a few seconds of SSE
         data. The brush rail without any rows is a misleading
         full-width green bar with `—` time labels, so swap it out
         for a short status message until rows arrive. -->
    <div v-if="!samplesCount" class="brush-empty">
      <span class="brush-empty-title">No archived snapshots yet</span>
      <span class="brush-empty-detail">
        <template v-if="loading">streaming history…</template>
        <template v-else-if="error">error: {{ error }}</template>
        <template v-else>Live samples stream into the charts below; the focus rail will fill in as the forwarder ingests.</template>
      </span>
    </div>
    <div v-else class="brush">
      <div class="brush-row">
        <button
          type="button"
          class="brush-skip"
          @click="skipToStart"
          :disabled="!timeRange"
          title="Jump focus window to start"
        >⏮</button>

        <div
          class="brush-rail"
          ref="railRef"
          @mousedown="onRailMouseDown"
          @wheel="onRailWheel"
        >
          <button
            v-for="(m, i) in railMarkers"
            :key="i"
            type="button"
            class="brush-tick"
            :class="{ current: m.isCurrent }"
            :style="{
              left: m.leftPct + '%',
              background: m.color,
              opacity: m.opacity,
              '--tick-color': m.color,
            }"
            :data-title="m.title"
            :title="m.title"
            @click.stop="selectInstance(m.ev, String(m.ev.type ?? 'event'), m.ev._p)"
          />
          <div
            v-if="timeRange"
            class="brush-window"
            :class="{ dragging: dragState }"
            :style="{
              left: ((windowStartMs - scrubMin) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
              right: ((scrubMax - windowEndMs) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
            }"
            @mousedown.stop="onBrushMouseDown($event, 'pan')"
          >
            <div class="brush-handle left" @mousedown.stop="onBrushMouseDown($event, 'resize-left')" />
            <div class="brush-handle right" @mousedown.stop="onBrushMouseDown($event, 'resize-right')" />
          </div>
        </div>

        <button
          type="button"
          class="brush-skip"
          @click="skipToEnd"
          :disabled="!timeRange"
          title="Jump focus window to end"
        >⏭</button>
      </div>

      <div class="brush-labels-row">
        <span class="rail-edge-label">{{ fmtTime(scrubMin) }}</span>
        <span
          v-if="timeRange"
          class="rail-focus-label"
          :style="{
            left: ((windowStartMs - scrubMin) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
            right: ((scrubMax - windowEndMs) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
          }"
        >
          <span class="focus-pill">{{ fmtDurShort(windowSpanMs) }} · at end</span>
          <span class="focus-pill subtle">{{ samplesCount.toLocaleString() }} rendered</span>
        </span>
        <span class="rail-edge-label">{{ fmtTime(scrubMax) }}</span>
      </div>

      <div class="event-filter" v-if="sessionEvents.length">
        <div class="kind-row">
          <span class="chips-label">Show:</span>
          <button
            type="button"
            class="kind-pill effect"
            :class="{ off: !enabledKind.effect }"
            @click="enabledKind.effect = !enabledKind.effect"
            title="Effects — what the player or user saw"
          >
            {{ enabledKind.effect ? '✓' : '○' }} Effects · {{ kindCount('effect') }}
          </button>
          <button
            type="button"
            class="kind-pill cause"
            :class="{ off: !enabledKind.cause }"
            @click="enabledKind.cause = !enabledKind.cause"
            title="Causes — proxy/system actions"
          >
            {{ enabledKind.cause ? '✓' : '○' }} Causes · {{ kindCount('cause') }}
          </button>
        </div>

        <div
          v-for="p in PRIORITY_ORDER"
          :key="p"
          class="tier-group"
          :class="{
            expanded: expandedTiers[p],
            dim: !tierCounts[p],
            'tier-active': lockedPriority === p && !lockedType,
          }"
          :style="{
            '--tier-bg': PRIORITY_META[p].bg,
            '--tier-border': PRIORITY_META[p].border,
            '--tier-color': PRIORITY_META[p].color,
          }"
        >
          <div class="tier-header">
            <button
              type="button"
              class="tier-chevron-btn"
              @click="toggleTier(p)"
              :title="expandedTiers[p] ? 'Collapse' : 'Expand'"
            >
              {{ expandedTiers[p] ? '▾' : '▸' }}
            </button>
            <button
              type="button"
              class="tier-name-btn"
              @click="selectTier(p); expandedTiers[p] = true"
              :title="`Walk all ${tierCounts[p]} ${PRIORITY_META[p].label} event(s) with prev/next`"
              :disabled="!tierCounts[p]"
            >
              <span class="tier-dot" />
              <span class="tier-name">{{ PRIORITY_META[p].label }}</span>
              <span class="tier-count-pill">{{ tierCounts[p] }}</span>
            </button>
            <button
              type="button"
              class="tier-eye-btn"
              :class="{ off: !visiblePriority[p] }"
              @click="togglePriorityVisibility(p, $event)"
              :title="visiblePriority[p] ? `Hide ${PRIORITY_META[p].label} events from the rail` : `Show ${PRIORITY_META[p].label} events on the rail`"
              :disabled="!tierCounts[p]"
            >
              <svg v-if="visiblePriority[p]" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                <path d="M10 12a2 2 0 100-4 2 2 0 000 4z"/>
                <path fill-rule="evenodd" d="M.458 10C1.732 5.943 5.522 3 10 3s8.268 2.943 9.542 7c-1.274 4.057-5.064 7-9.542 7S1.732 14.057.458 10zM14 10a4 4 0 11-8 0 4 4 0 018 0z" clip-rule="evenodd"/>
              </svg>
              <svg v-else viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                <path fill-rule="evenodd" d="M3.707 2.293a1 1 0 00-1.414 1.414l14 14a1 1 0 001.414-1.414l-1.473-1.473A10.014 10.014 0 0019.542 10C18.268 5.943 14.478 3 10 3a9.958 9.958 0 00-4.512 1.074l-1.78-1.781zm4.261 4.26l1.514 1.515a2.003 2.003 0 012.45 2.45l1.514 1.514a4 4 0 00-5.478-5.478z" clip-rule="evenodd"/>
                <path d="M12.454 16.697L9.75 13.992a4 4 0 01-3.742-3.741L2.335 6.578A9.98 9.98 0 00.458 10c1.274 4.057 5.065 7 9.542 7 .847 0 1.669-.105 2.454-.303z"/>
              </svg>
            </button>
            <div class="tier-preview-chips" v-if="!expandedTiers[p] && tierCounts[p]">
              <button
                v-for="t in tierPreview(p)"
                :key="t.type"
                type="button"
                class="preview-chip"
                @click="pickPreviewType(t.type, p)"
                :title="`Walk ${t.count} ${t.type} event(s)`"
              >
                {{ t.type }} · {{ t.count }}
              </button>
              <span v-if="tierPreviewMore(p)" class="preview-more">
                +{{ tierPreviewMore(p) }} more
              </span>
            </div>
          </div>

          <div class="tier-body" v-if="expandedTiers[p] && tierCounts[p]">
            <div
              v-for="t in tierTypes[p]"
              :key="t.type"
              class="type-row-wrap"
            >
              <div
                class="type-row"
                :class="{
                  active: lockedPriority === p && lockedType === t.type,
                  'instances-open': expandedTypeKey === typeKey(t.type, p),
                  hidden: !isTypeVisible(t.type, p),
                }"
              >
                <button
                  type="button"
                  class="type-chevron-btn"
                  @click="toggleTypeExpand(t.type, p)"
                  :title="expandedTypeKey === typeKey(t.type, p) ? 'Hide instances' : 'Show instances'"
                >
                  {{ expandedTypeKey === typeKey(t.type, p) ? '▾' : '▸' }}
                </button>
                <button
                  type="button"
                  class="type-name-btn-row"
                  @click="selectType(t.type, p)"
                  :title="`Walk ${t.count} ${t.type} event(s) with prev/next`"
                >
                  <span class="type-name">{{ t.type }}</span>
                  <span class="type-count">{{ t.count }}</span>
                </button>
                <button
                  type="button"
                  class="type-eye-btn"
                  :class="{ off: !isTypeVisible(t.type, p) }"
                  @click="toggleTypeVisibility(t.type, p, $event)"
                  :title="isTypeVisible(t.type, p) ? `Hide ${t.type} events from the rail` : `Show ${t.type} events on the rail`"
                >
                  <svg v-if="isTypeVisible(t.type, p)" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                    <path d="M10 12a2 2 0 100-4 2 2 0 000 4z"/>
                    <path fill-rule="evenodd" d="M.458 10C1.732 5.943 5.522 3 10 3s8.268 2.943 9.542 7c-1.274 4.057-5.064 7-9.542 7S1.732 14.057.458 10zM14 10a4 4 0 11-8 0 4 4 0 018 0z" clip-rule="evenodd"/>
                  </svg>
                  <svg v-else viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
                    <path fill-rule="evenodd" d="M3.707 2.293a1 1 0 00-1.414 1.414l14 14a1 1 0 001.414-1.414l-1.473-1.473A10.014 10.014 0 0019.542 10C18.268 5.943 14.478 3 10 3a9.958 9.958 0 00-4.512 1.074l-1.78-1.781zm4.261 4.26l1.514 1.515a2.003 2.003 0 012.45 2.45l1.514 1.514a4 4 0 00-5.478-5.478z" clip-rule="evenodd"/>
                    <path d="M12.454 16.697L9.75 13.992a4 4 0 01-3.742-3.741L2.335 6.578A9.98 9.98 0 00.458 10c1.274 4.057 5.065 7 9.542 7 .847 0 1.669-.105 2.454-.303z"/>
                  </svg>
                </button>
              </div>
              <div class="instances" v-if="expandedTypeKey === typeKey(t.type, p)">
                <button
                  v-for="(ev, idx) in tierTypeInstances[typeKey(t.type, p)] ?? []"
                  :key="idx"
                  type="button"
                  class="instance-row"
                  :class="{
                    current: navCurrent
                      && lockedPriority === p
                      && lockedType === t.type
                      && navCurrent._ts === ev._ts,
                  }"
                  @click="selectInstance(ev, t.type, p)"
                  :title="`Jump to this event at ${fmtTime(ev._ts)}`"
                >
                  <span class="instance-marker">▸</span>
                  <span class="instance-time">{{ fmtTime(ev._ts) }}</span>
                  <span class="instance-info" v-if="ev.info">{{ ev.info }}</span>
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div class="nav-bar" v-if="navEvents.length">
        <span class="nav-scope">
          Showing: <strong>{{ scopeLabel }}</strong>
          <span class="nav-detail" v-if="navCurrent">
            · current {{ fmtTime(navCurrent._ts) }}<template v-if="navCurrent.info"> · {{ navCurrent.info }}</template>
          </span>
        </span>
        <button class="nav-btn" type="button" @click="navPrev" :disabled="navEvents.length < 2" title="Previous (,)">‹ prev</button>
        <span class="nav-counter">{{ navCursor + 1 }} / {{ navEvents.length }}</span>
        <button class="nav-btn" type="button" @click="navNext" :disabled="navEvents.length < 2" title="Next (.)">next ›</button>
        <button
          v-if="lockedPriority || lockedType"
          class="btn-mini"
          type="button"
          @click="clearScope"
          title="Clear scope — walk every event"
        >Clear scope</button>
      </div>

      <div class="brush-actions">
        <button
          type="button"
          class="btn live-toggle"
          :class="{ checked: brushLiveChecked }"
          @click="coord.togglePause()"
          :title="brushLiveChecked ? 'Pause at current live edge' : 'Resume following live'"
        >
          {{ brushLiveChecked ? '●' : '○' }} Live
        </button>
        <span class="brush-status">
          <template v-if="loading">streaming · {{ samplesCount.toLocaleString() }} snapshots</template>
          <template v-else-if="error"><span class="brush-status-err">error: {{ error }}</span></template>
          <template v-else>complete · {{ samplesCount.toLocaleString() }} snapshots</template>
        </span>
      </div>

      <div v-if="loading" class="thin-progress">
        <div class="thin-progress-shimmer" />
      </div>
    </div>
    </CollapsibleSection>

    <!-- Display panels — same components in both modes; data routes
         through usePlayer/getPlayer per the playerId prefix. -->
    <CollapsibleSection v-if="!hideSessionDetails" title="Session Details" persist-key="session-details">
      <SessionDetails :player-id="archivePlayerId" />
    </CollapsibleSection>

    <CollapsibleSection title="Player Metrics" persist-key="player-metrics">
      <PlayerMetrics :player-id="archivePlayerId" />
    </CollapsibleSection>

    <CollapsibleSection title="Player State" :open="true" eager persist-key="player-state">
      <EventsTimeline :player-id="archivePlayerId" :samples-stream="timeseries.samples" />
    </CollapsibleSection>

    <CollapsibleSection title="Bitrate Chart etc" :open="true" eager persist-key="bitrate-chart">
      <BitrateChartPanelToolbar :player-id="archivePlayerId" />
      <div class="chart-stack">
        <BandwidthChart :player-id="archivePlayerId" :samples-stream="timeseries.samples" />
        <RTTChart :player-id="archivePlayerId" :samples-stream="timeseries.samples" />
        <BufferChart :player-id="archivePlayerId" :samples-stream="timeseries.samples" />
        <FPSChart :player-id="archivePlayerId" :samples-stream="timeseries.samples" />
      </div>
    </CollapsibleSection>

    <CollapsibleSection title="Network Log" persist-key="network-log">
      <!-- The page-level brush in SessionDisplay is the only scrub
           surface — archive shows it always, live shows it once
           paused. NetworkLog's own in-panel brush would duplicate it
           (or worse, show a brush in live-not-paused when nothing
           else does), so always opt out of it here. -->
      <NetworkLog :player-id="archivePlayerId" :network-stream="timeseries.network" />
    </CollapsibleSection>
  </div>
</template>

<style scoped>
.session-display { display: contents; }

/* Empty-state for the Focus Window before archive snapshots land. */
.brush-empty {
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  padding: 14px 16px;
  margin-bottom: 14px;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.brush-empty-title {
  font-size: 13px;
  font-weight: 600;
  color: #374151;
}
.brush-empty-detail {
  font-size: 12px;
  color: #6b7280;
}

/* Brush block — joins onto the page header above (no gap, shared
 * borders) for a single unified top panel. */
.brush {
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 0 0 8px 8px;
  padding: 12px 14px 10px;
  margin-bottom: 14px;
  position: relative;
}
.brush-row { display: flex; align-items: stretch; gap: 8px; }

.brush-skip {
  width: 32px;
  background: #fff;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  cursor: pointer;
  font-size: 14px;
  color: #4b5563;
  display: flex;
  align-items: center;
  justify-content: center;
}
.brush-skip:hover:not(:disabled) { background: #f3f4f6; color: #111827; }
.brush-skip:disabled { opacity: 0.4; cursor: not-allowed; }

.brush-rail {
  flex: 1;
  position: relative;
  height: 30px;
  background: #d1fae5;
  border-radius: 6px;
  overflow: visible;
  cursor: crosshair;
}
.brush-rail .brush-tick {
  position: absolute;
  top: 0;
  bottom: 0;
  width: 3px;
  border: 0;
  padding: 0;
  transform: translateX(-1px);
  border-radius: 1px;
  opacity: 0.9;
  z-index: 3;
  cursor: pointer;
}
.brush-rail .brush-tick:hover {
  opacity: 1;
  width: 5px;
  top: -3px;
  bottom: -3px;
  box-shadow: 0 0 0 1px #fff, 0 1px 4px rgba(0,0,0,0.25);
}
.brush-rail .brush-tick:hover::after {
  content: attr(data-title);
  position: absolute;
  bottom: calc(100% + 4px);
  left: 50%;
  transform: translateX(-50%);
  background: #1f2937;
  color: #fff;
  padding: 3px 8px;
  border-radius: 4px;
  font-size: 11px;
  font-family: ui-monospace, monospace;
  white-space: nowrap;
  pointer-events: none;
  z-index: 10;
  box-shadow: 0 2px 6px rgba(0,0,0,0.25);
}
.brush-rail .brush-tick.current {
  width: 5px;
  top: -3px;
  bottom: -3px;
  outline: 2px solid #fff;
  outline-offset: -1px;
  box-shadow: 0 0 0 1px var(--tick-color, #1d4ed8), 0 0 0 3px rgba(29,78,216,0.25);
  z-index: 3;
}

.brush-window {
  position: absolute;
  top: 0;
  bottom: 0;
  background: rgba(29, 78, 216, 0.18);
  border: 0;
  border-radius: 6px;
  box-shadow:
    inset 0 0 0 1px rgba(29, 78, 216, 0.45),
    inset 0 0 0 2px rgba(255, 255, 255, 0.4);
  cursor: grab;
  box-sizing: border-box;
  z-index: 2;
}
.brush-window.dragging { cursor: grabbing; }
.brush-handle {
  position: absolute;
  top: -1px;
  bottom: -1px;
  width: 8px;
  background: #1d4ed8;
  border-radius: 3px;
  cursor: ew-resize;
  box-shadow: 0 1px 3px rgba(29, 78, 216, 0.4);
}
.brush-handle.left  { left: -4px; }
.brush-handle.right { right: -4px; }

.brush-labels-row {
  position: relative;
  margin: 4px 40px 0;
  height: 16px;
  font-size: 10.5px;
  color: #6b7280;
  font-family: ui-monospace, monospace;
}
.rail-edge-label { position: absolute; top: 0; }
.rail-edge-label:first-child { left: 0; }
.rail-edge-label:last-child  { right: 0; }
.rail-focus-label {
  position: absolute;
  top: 0;
  display: inline-flex;
  align-items: baseline;
  gap: 6px;
  justify-content: center;
  min-width: 60px;
  pointer-events: none;
}
.focus-pill {
  background: rgba(29, 78, 216, 0.08);
  color: #1d4ed8;
  font-weight: 600;
  padding: 1px 6px;
  border-radius: 3px;
  white-space: nowrap;
}
.focus-pill.subtle { background: transparent; color: #6b7280; font-weight: 500; padding: 1px 0; }

/* Event filter ─── L0 / L1 / L2 / L3 */
.chips-label { font-size: 11px; color: #6b7280; font-weight: 500; margin-right: 2px; }
.kind-row { display: flex; align-items: center; flex-wrap: wrap; gap: 6px; margin: 10px 0 0; }
.kind-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 11px;
  font-weight: 600;
  padding: 3px 10px;
  border-radius: 999px;
  border: 1px solid;
  cursor: pointer;
  line-height: 1.4;
}
.kind-pill.effect { background: #dbeafe; border-color: #93c5fd; color: #1d4ed8; }
.kind-pill.cause  { background: #fed7aa; border-color: #fdba74; color: #7c2d12; }
.kind-pill.off    { opacity: 0.4; background: #f3f4f6; border-color: #d1d5db; color: #6b7280; }

.event-filter {
  margin: 10px 0 0;
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  overflow: hidden;
}
.tier-group {
  --tier-bg: #f3f4f6;
  --tier-border: #d1d5db;
  --tier-color: #4b5563;
  border-top: 1px solid #e5e7eb;
}
.tier-group:first-of-type { border-top: none; }
.tier-group.dim { opacity: 0.5; }
.tier-header {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 4px 8px 4px 4px;
  background: transparent;
  font-size: 12px;
}
.tier-group.expanded .tier-header { background: var(--tier-bg); }
.tier-group.tier-active .tier-header { background: var(--tier-bg); }
.tier-chevron-btn {
  width: 22px;
  height: 22px;
  font-size: 10px;
  color: var(--tier-color);
  background: transparent;
  border: 0;
  border-radius: 4px;
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}
.tier-chevron-btn:hover { background: rgba(0,0,0,0.05); }
.tier-name-btn {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 10px;
  background: transparent;
  border: 0;
  border-radius: 6px;
  font-size: 12px;
  font-weight: 600;
  color: #1f2937;
  cursor: pointer;
  text-align: left;
  flex-shrink: 0;
}
.tier-name-btn:hover:not(:disabled) { background: rgba(0,0,0,0.05); }
.tier-name-btn:disabled { cursor: default; opacity: 0.5; }
.tier-group.tier-active .tier-name-btn { box-shadow: inset 0 0 0 2px var(--tier-color); }
.tier-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--tier-color);
  flex-shrink: 0;
}
.tier-name { color: var(--tier-color); }
.tier-count-pill {
  font-variant-numeric: tabular-nums;
  font-weight: 600;
  color: var(--tier-color);
  background: rgba(255,255,255,0.7);
  border-radius: 999px;
  padding: 0 7px;
  min-width: 22px;
  text-align: center;
  font-size: 11px;
}

.tier-eye-btn {
  width: 24px;
  height: 24px;
  background: transparent;
  border: 0;
  border-radius: 4px;
  cursor: pointer;
  color: var(--tier-color);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  padding: 0;
  opacity: 0.75;
}
.tier-eye-btn svg { width: 16px; height: 16px; display: block; }
.tier-eye-btn:hover:not(:disabled) { background: rgba(0,0,0,0.06); opacity: 1; }
.tier-eye-btn.off { opacity: 0.45; color: #6b7280; }
.tier-eye-btn:disabled { opacity: 0.2; cursor: not-allowed; }

.type-eye-btn {
  width: 22px;
  height: 22px;
  background: transparent;
  border: 0;
  border-radius: 4px;
  cursor: pointer;
  color: var(--tier-color);
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  padding: 0;
  margin-right: 6px;
  opacity: 0;
  transition: opacity 0.1s;
}
.type-eye-btn svg { width: 14px; height: 14px; display: block; }
.type-row:hover .type-eye-btn,
.type-row .type-eye-btn.off { opacity: 0.75; }
.type-row .type-eye-btn:hover { background: rgba(0,0,0,0.06); opacity: 1; }
.type-row .type-eye-btn.off { color: #6b7280; }
.type-row.hidden .type-name-btn-row { opacity: 0.45; }

.tier-preview-chips {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 4px;
  margin-left: 6px;
  flex: 1;
  min-width: 0;
}
.preview-chip {
  font-size: 10.5px;
  font-family: ui-monospace, monospace;
  font-weight: 500;
  padding: 1px 8px;
  border: 1px solid var(--tier-border);
  background: #fff;
  color: var(--tier-color);
  border-radius: 999px;
  cursor: pointer;
  line-height: 1.5;
  white-space: nowrap;
}
.preview-chip:hover { background: var(--tier-color); color: #fff; border-color: var(--tier-color); }
.preview-more { font-size: 10px; color: #6b7280; font-style: italic; margin-left: 2px; }

.tier-body { max-height: 240px; overflow-y: auto; border-top: 1px solid #e5e7eb; background: #fff; }
.type-row-wrap {}
.type-row {
  display: flex;
  align-items: stretch;
  width: 100%;
  background: transparent;
  font-family: ui-monospace, monospace;
  border-left: 3px solid transparent;
}
.type-row.active { background: var(--tier-bg); border-left-color: var(--tier-color); }
.type-row.instances-open { background: rgba(0,0,0,0.02); }
.type-chevron-btn {
  width: 28px;
  background: transparent;
  border: 0;
  font-size: 10px;
  color: var(--tier-color);
  cursor: pointer;
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
}
.type-chevron-btn:hover { background: rgba(0,0,0,0.05); }
.type-name-btn-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 1;
  padding: 4px 10px 4px 4px;
  background: transparent;
  border: 0;
  font-size: 11.5px;
  color: #374151;
  cursor: pointer;
  text-align: left;
  font-family: inherit;
}
.type-name-btn-row:hover { background: #f9fafb; }
.type-row.active .type-name-btn-row { font-weight: 700; color: var(--tier-color); }
.type-name { flex: 1; }
.type-count { font-variant-numeric: tabular-nums; color: #6b7280; font-size: 10.5px; }
.type-row.active .type-count { color: var(--tier-color); }

.instances {
  background: #fafafa;
  border-top: 1px dashed #e5e7eb;
  border-bottom: 1px dashed #e5e7eb;
  max-height: 200px;
  overflow-y: auto;
}
.instance-row {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  padding: 3px 10px 3px 60px;
  background: transparent;
  border: 0;
  font-size: 11px;
  font-family: ui-monospace, monospace;
  color: #4b5563;
  cursor: pointer;
  text-align: left;
}
.instance-row:hover { background: rgba(29, 78, 216, 0.06); }
.instance-row.current {
  background: rgba(29, 78, 216, 0.12);
  color: #1d4ed8;
  font-weight: 700;
  box-shadow: inset 3px 0 0 #1d4ed8;
}
.instance-marker { width: 10px; font-size: 9px; color: #9ca3af; text-align: center; }
.instance-row.current .instance-marker { color: #1d4ed8; }
.instance-time { font-variant-numeric: tabular-nums; flex-shrink: 0; }
.instance-info {
  color: #6b7280;
  font-size: 10.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.instance-row.current .instance-info { color: #1d4ed8; }

.nav-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 8px 0 0;
  padding: 6px 10px;
  background: #f9fafb;
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  font-size: 11px;
  color: #374151;
  flex-wrap: wrap;
}
.nav-scope { flex: 1; min-width: 0; }
.nav-scope strong { color: #111827; }
.nav-detail { color: #1d4ed8; font-family: ui-monospace, monospace; font-size: 10.5px; }
.nav-btn {
  font-size: 11px;
  font-weight: 600;
  padding: 3px 10px;
  border: 1px solid #d1d5db;
  background: #fff;
  border-radius: 4px;
  cursor: pointer;
  color: #1f2937;
}
.nav-btn:hover:not(:disabled) { background: #f3f4f6; }
.nav-btn:disabled { opacity: 0.4; cursor: not-allowed; }
.nav-counter {
  font-variant-numeric: tabular-nums;
  font-weight: 700;
  color: #6b7280;
  padding: 0 4px;
  min-width: 60px;
  text-align: center;
}

.brush-actions {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 8px;
  flex-wrap: wrap;
  font-size: 11px;
  color: #4b5563;
}
.btn-mini {
  font-size: 11px;
  padding: 3px 8px;
  border: 1px solid #d1d5db;
  background: #fff;
  border-radius: 4px;
  cursor: pointer;
}
.btn-mini:hover { background: #f3f4f6; }
/* Live toggle in the brush rail — matches the chart-toolbar style
 * (filled green when checked, muted when unchecked) so all four
 * Live toggles on the page look the same and update in lockstep. */
.brush-actions .btn.live-toggle {
  font-size: 11px;
  padding: 3px 8px;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  background: #f3f4f6;
  color: #6b7280;
  cursor: pointer;
}
.brush-actions .btn.live-toggle:hover { background: #e5e7eb; color: #374151; }
.brush-actions .btn.live-toggle.checked {
  background: #10b981;
  border-color: #059669;
  color: #fff;
  font-weight: 600;
}
.brush-actions .btn.live-toggle.checked:hover { background: #059669; }
.brush-status {
  margin-left: auto;
  font-size: 11px;
  color: #6b7280;
  font-family: ui-monospace, monospace;
}
.brush-status-err { color: #b91c1c; }

.thin-progress {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  height: 2px;
  overflow: hidden;
  border-radius: 0 0 8px 8px;
}
.thin-progress-shimmer {
  position: absolute;
  inset: 0;
  background: repeating-linear-gradient(90deg,
    transparent 0 6px,
    rgba(59, 130, 246, 0.6) 6px 12px);
  animation: viewer-shimmer 0.8s linear infinite;
}
@keyframes viewer-shimmer {
  0% { transform: translateX(0); }
  100% { transform: translateX(12px); }
}

.chart-stack { display: grid; gap: 12px; }
</style>
