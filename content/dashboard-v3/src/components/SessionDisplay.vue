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
import PlayLog from '@/components/PlayLog.vue';
import BitrateChartPanelToolbar from '@/components/BitrateChartPanelToolbar.vue';
import CycleBandsRail from '@/components/CycleBandsRail.vue';
import { useChartCoordination } from '@/composables/useChartCoordination';
// Issue #474 Milestone C: session_markers retired. The severity filter
// derives its event list from `labels[]` on rows across all three
// streams (events / network / control_events) instead of a dedicated
// markers table. See `sessionEvents` computed below.
interface SessionEvent {
  ts?: string;
  event_time?: string;
  type?: string;
  info?: string;
  kind?: 'effect' | 'cause';
  priority?: 1 | 2 | 3 | 4;
  severity?: string;
  [k: string]: unknown;
}
import { usePlayer } from '@/composables/usePlayer';
import { useSessionTimeSeries } from '@/composables/useSessionTimeSeries';
import { chRowToPlayerRecord, tsOfRow } from '@/composables/chRowAdapter';
import {
  setArchivePlayer,
  type PlayerRecord,
} from '@/repo/v2-repo';

const props = defineProps<{
  /** Canonical player UUID. */
  playerId: string;
  /** Play id. Pass a specific id to lock onto one play (archive
   *  replay style); pass null to let the v3 timeseries server pick
   *  the latest play and follow rotations. */
  playId: string | null;
  /** Suppress the internal Session Details panel. The active-testing
   *  pages mount their own Session Details right before Fault Injection
   *  (above the control panels), so they pass this flag and SessionDisplay
   *  omits the duplicate panel from its stack. */
  hideSessionDetails?: boolean;
  /** SessionViewer "show before/after" toggle. When true the SSE
   *  drops the play_id filter and widens fromMs/toMs to the cached
   *  play bounds ± 5 minutes so the operator can scroll through the
   *  surrounding context for the same player. Default: false — the
   *  view is locked to this play. */
  showContext?: boolean;
  /** Initial time window. Caller (SessionViewer reading the URL)
   *  passes startMs (ms-since-epoch) and endMs. endMs = null means
   *  "follow live edge" — the SSE backfills from startMs but doesn't
   *  set a `to` bound, and the brush stays unpinned so it tracks
   *  live samples as they arrive. Both null = legacy behaviour
   *  (auto-pin brush to samples.rangeBounds when they land). */
  startMs?: number | null;
  endMs?: number | null;
  /** Live-page hint: caller is mounting on a perpetually-live page
   *  (TestingSession) with no URL time bounds. Skip the
   *  pin-to-sample-bounds fallback so the brush leaves coord.range
   *  null and panels follow the live edge on every refresh. Treat
   *  identically to startMs!=null && endMs==null but without forcing
   *  a startMs anchor. Default: false (legacy archive behaviour). */
  followLive?: boolean;
}>();

const playIdRef = computed(() => props.playId);

// Always subscribe to the live SSE. When the player is active the
// TanStack cache gets continuous PlayerRecord updates (for outside-
// SessionDisplay mutation panels — FaultRules, NetworkShaping, etc.
// in TestingSession). When the player is dead the GET 404s once and
// SSE idles — harmless. SessionDisplay's own display panels read
// from the archive store (fed by the projection watcher below), so
// this subscription only matters for mutation-side consumers.
const livePlayerIdRef = computed(() => props.playerId);
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
// Always uses an `archive:<player_id>:<play_id>` prefix so the
// brush-projection write doesn't collide with the live cache that
// outside mutation panels (FaultRules etc.) read for editing. The
// `archive:` prefix also tells usePlayer/usePlayerSSE NOT to start
// a second SSE subscription (we already have one via livePlayerIdRef
// for the live cache).
const archivePlayerId = computed(() =>
  `archive:${props.playerId}:${props.playId ?? 'all'}`,
);

// Event accordion source. The events stream isn't on the v3 timeseries
// endpoint yet (out of scope for TS6); kept here so the brush-rail
// tick markers, the priority/tier filter UI, and the prev/next nav
// keep functioning unchanged. Filters by player_id + play_id only —
// session_id retired.
// Derived event list for the severity filter — projects every
// labelled row across session_events / network_requests / control_events
// into a SessionEvent. After issue #474 Milestone C this replaces the
// useArchivedSessionMarkers fetch against the retired session_markers
// table. One synthetic event per label; ts/severity/type come straight
// from the row + label string.
const sessionEvents = computed<SessionEvent[]>(() => {
  // Trigger reactivity on each stream's version ref.
  void timeseries.events.version.value;
  void timeseries.network.version.value;
  void timeseries.control.version.value;
  const out: SessionEvent[] = [];
  function emit(row: Record<string, unknown>, kind: 'effect' | 'cause') {
    const labels = Array.isArray((row as { labels?: unknown }).labels)
      ? ((row as { labels: unknown[] }).labels as string[])
      : null;
    if (!labels || labels.length === 0) return;
    const ts = (row.ts as string | undefined) ?? '';
    for (const l of labels) {
      const eq = l.indexOf('=');
      if (eq <= 0) continue;
      const sev = l.slice(0, eq);
      let type = l.slice(eq + 1);
      // Strip the `*` synthesized-marker prefix for display — the
      // filter UI treats `*manifest_failure` and `manifest_failure`
      // as the same bucket type.
      if (type.startsWith('*')) type = type.slice(1);
      if (sev !== 'error' && sev !== 'critical' && sev !== 'warning' && sev !== 'info' && sev !== 'testing') continue;
      out.push({ ts, type, severity: sev, kind });
    }
  }
  // Cause / effect axis (post-#474 redefinition):
  //   - cause  = something the operator / proxy deliberately introduced
  //              to provoke a reaction. Every control_events row counts
  //              (fault enables, pattern toggles, shaper edits, the
  //              pattern_step ticks themselves). On network rows, only
  //              the proxy-flagged `faulted=1` responses count — those
  //              are the 1-in-10 HTTP 4xx/5xx returned by the fault
  //              rules. An organic upstream 5xx the proxy didn't fault
  //              stays an effect.
  //   - effect = the player's / network's reaction. All session_events
  //              rows (stalls, restarts, ABR shifts, errors) and the
  //              clean network rows.
  for (const r of timeseries.events.inRange(0, Number.MAX_SAFE_INTEGER)) emit(r, 'effect');
  for (const r of timeseries.network.inRange(0, Number.MAX_SAFE_INTEGER)) {
    const faulted = Number((r as { faulted?: unknown }).faulted) || 0;
    emit(r, faulted ? 'cause' : 'effect');
  }
  for (const r of timeseries.control.inRange(0, Number.MAX_SAFE_INTEGER)) emit(r, 'cause');
  return out;
});

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
// Pass the prop through directly. Caller controls follow-latest
// (null) vs locked-play (specific id) — Testing/TestingSession can
// pass null to track the active play across rotations; SessionViewer
// passes the URL play_id to lock onto one historical play. Either
// way the v3 timeseries SSE handles backfill + live deltas in the
// same stream, so an in-progress play visited from session-viewer
// will still receive live updates via the ring overlay.
/** Initial time-window source of truth, in order of preference:
 *    1. props.startMs / props.endMs   ← URL-driven (sessions.html)
 *    2. samples.rangeBounds            ← legacy auto-pin once data lands
 *
 *  windowBoundsRef holds whichever is currently the truth. URL props
 *  take precedence — when present, the brush pins to them immediately
 *  on mount (no waiting for samples). When absent, falls back to the
 *  rangeBounds watcher below. */
const windowBoundsRef = ref<{ min: number; max: number } | null>(null);
if (props.startMs != null && props.endMs != null) {
  windowBoundsRef.value = { min: props.startMs, max: props.endMs };
} else if (props.startMs != null && props.endMs == null) {
  // end_time=live: start is anchored, end follows live edge. The
  // window's "max" doesn't matter for the SSE (we leave toMs null
  // below); set it to startMs for now and let lastSampleMs extend it.
  windowBoundsRef.value = { min: props.startMs, max: props.startMs };
}

/** Effective play_id for the SSE subscription. `showContext = true`
 *  drops the play_id filter so rows from neighbouring plays of the
 *  same player land in the caches. */
const effectivePlayIdRef = computed<string | null>(() =>
  props.showContext ? null : playIdRef.value,
);

/** Reactive fromMs / toMs for the SSE.
 *  - Base case (showContext off): use the URL's startMs/endMs as-is.
 *    endMs=null means "follow live" — pass through as null so SSE
 *    tails the live edge.
 *  - showContext on: widen by CONTEXT_PAD_MS on each side so the
 *    operator can scroll through before/after; toMs stays null when
 *    end_time was "live". */
const CONTEXT_PAD_MS = 5 * 60 * 1000;
const fromMsRef = computed<number | null>(() => {
  const startMs = props.startMs ?? windowBoundsRef.value?.min ?? null;
  if (startMs == null) return null;
  return props.showContext ? startMs - CONTEXT_PAD_MS : startMs;
});
const toMsRef = computed<number | null>(() => {
  // end_time=live (props.endMs == null but startMs set) → no upper
  // bound on the SSE backfill, regardless of showContext.
  if (props.endMs == null) return null;
  return props.showContext ? props.endMs + CONTEXT_PAD_MS : props.endMs;
});

/* ─── Refetch-on-pan (#587) ─────────────────────────────────────────
 * When the operator pins the focus bar to a window OLDER than what's in
 * the rolling cache (evicted by the #582 recency cap), re-point the SSE
 * at that window so the panels can show the early part of a long session.
 * Returning to live (range null) drops the override and re-subscribes to
 * the live tail. Reuses the existing fromMs/toMs Ref re-subscribe path —
 * the same mechanism SessionViewer's "show context" uses — so reviewing
 * history pauses live until the operator clicks Live (or drags back to
 * the right edge). The cache-reset epoch makes the charts/timeline
 * re-drain the fetched window instead of missing it behind a stale
 * watermark. The server treats a `to` >5s in the past as a bounded
 * archive read (no live tail), so the window loads cleanly. */
const histFromRef = ref<number | null>(null);
const histToRef = ref<number | null>(null);
const HISTORY_PAD_MS = 60 * 1000;
// Live mode backfills only the recent window (cheap), NOT the whole play
// — a multi-hour play is 10k+ rows and re-streaming it on every
// return-to-live left the live edge blank for seconds. Deep history is
// loaded on demand via the pan-back override below. Refreshed whenever
// we (re)enter live so "return to live" pulls a fresh recent window.
const LIVE_BACKFILL_MS = 10 * 60 * 1000;
const liveFromRef = ref<number | null>(props.followLive ? Date.now() - LIVE_BACKFILL_MS : null);
const tsFromMs = computed<number | null>(() => histFromRef.value ?? liveFromRef.value ?? fromMsRef.value);
const tsToMs = computed<number | null>(() => histToRef.value ?? toMsRef.value);

/** Resolved [fromMs, toMs] for the CycleBandsRail. When toMsRef is
 *  null (follow-live), use Date.now() as the upper bound so bands
 *  still render. Rail renders nothing when either bound is missing
 *  (covers the initial-load case before bounds settle). */
const cycleBandsDomain = computed<{ fromMs: number; toMs: number } | null>(() => {
  const from = fromMsRef.value;
  if (from == null) return null;
  const to = toMsRef.value ?? Date.now();
  if (to <= from) return null;
  return { fromMs: from, toMs: to };
});

const timeseries = useSessionTimeSeries(
  apiPlayerIdRef,
  effectivePlayIdRef,
  {
    // 'control' added so the PlayLog and severity filter can see
    // proxy/harness action rows alongside player events + network
    // rows. The control bundle is auto-added when 'control' is in
    // streams (useSessionTimeSeries). Issue #474 Milestone C.
    streams: ['events', 'network', 'control', 'avmetrics'],
    bundles: ['charts_minimal', 'lanes_v1', 'panel_v1', 'session_details', 'network'],
    // History override (#587) takes precedence over the live window.
    fromMs: tsFromMs,
    toMs: tsToMs,
  },
);

// Fallback: when the URL didn't carry start_time/end_time, capture
// the play's bounds the first time samples arrive in archive mode
// so windowBoundsRef can drive the SSE re-subscribe on showContext
// toggles. Skipped when URL props are present — those take precedence.
watch(
  () => timeseries.events.rangeBounds.value,
  (b) => {
    if (!b) return;
    if (props.startMs != null) return;   // URL gave us the truth
    if (props.showContext) return;        // wider window, don't anchor on it
    if (props.followLive) return;         // live mode uses liveFromRef (#587)
    if (!playIdRef.value) return;         // live mode
    if (windowBoundsRef.value !== null) return;
    windowBoundsRef.value = b;
  },
  { immediate: true },
);

// coord declared up-front so the `timeRange` computed below can read
// `coord.state.lastSampleMs` (live edge) without a temporal dead zone
// — and so any earlier reactive code (window watcher, brush clamps)
// sees a coord instance even though it gets consumed mostly later.
const coord = useChartCoordination(archivePlayerId);

/* Refetch-on-pan driver (#587). Watches the committed focus range (the
 * #590 brush debounce means this fires once per gesture, not per
 * mousemove). Pinning before the cached data fetches that window;
 * returning to live drops the override. Declared after `coord` to avoid
 * a TDZ on the watch getter. Live mode only — URL-driven archive views
 * (props.startMs set) already load their bounded window. */
watch(
  () => coord.state.range,
  (range) => {
    if (props.startMs != null) return; // URL-driven archive; not live
    if (!range) {
      // Back to live — re-subscribe to a FRESH recent window (not the
      // stale one from when the page first loaded).
      histFromRef.value = null;
      histToRef.value = null;
      if (props.followLive) liveFromRef.value = Date.now() - LIVE_BACKFILL_MS;
      return;
    }
    const bounds = timeseries.events.rangeBounds.value;
    // Only refetch when the pinned window starts MEANINGFULLY before
    // what's cached; a window already in the cache needs no server
    // round-trip. The slop keeps the auto-pin fallback (bounds.min − 5s)
    // and small drag jitter from triggering a needless re-subscribe.
    const REFETCH_SLOP_MS = 15 * 1000;
    if (bounds && range.min < bounds.min - REFETCH_SLOP_MS) {
      histFromRef.value = range.min - HISTORY_PAD_MS;
      histToRef.value = range.max + HISTORY_PAD_MS;
    }
  },
);

// Pin the focus-window brush as soon as we know the time bounds.
//   - URL gave us startMs + endMs (sessions.html click): pin
//     immediately on mount, no waiting for samples.
//   - URL gave us startMs + endMs=null ("live"): leave coord.range
//     null so the brush follows the live edge.
//   - URL gave us nothing (legacy / direct URL): wait for samples
//     and pin to bounds.min - 5s when they land.
// One-shot: don't fight subsequent operator drags or the
// "show context" toggle widening.
let hasPinnedBrush = false;
function tryPinBrush(min: number | null, max: number | null) {
  if (hasPinnedBrush) return;
  if (min == null || max == null) return;
  if (props.showContext) return;
  if (coord.state.range !== null) return;
  coord.setRange({ min, max });
  hasPinnedBrush = true;
}
if (props.startMs != null && props.endMs != null) {
  // URL-driven archive range: pin immediately.
  tryPinBrush(props.startMs, props.endMs);
} else if ((props.startMs != null && props.endMs == null) || props.followLive) {
  // end_time=live OR explicit live-page hint: leave coord.range null
  // so brush follows live edge. Treat as "pinned" for the fallback
  // watcher's purposes so it doesn't auto-pin on first samples.
  hasPinnedBrush = true;
}
watch(
  () => timeseries.events.rangeBounds.value,
  (b) => {
    if (hasPinnedBrush) return;
    if (!b) return;
    if (props.showContext) return;
    if (!playIdRef.value) return;
    tryPinBrush(b.min - 5000, b.max);
  },
  { immediate: true },
);

// Live-edge anchor. `coord.lastSampleMs` is the brush's live-follow
// target (effectiveRange.max) AND the rail's right edge. It used to be
// advanced ONLY from inside the charts' drain (pushSample → noteSample),
// so while the charts backfilled asynchronously the brush sat behind the
// true live edge — "not locked at live" until the drain caught up. The
// cache's `rangeBounds.max` is the authoritative newest ts the instant
// the cache has it, so feed the live edge from there directly. The
// charts' recent-first drain keeps the visible window populated up to
// this edge, so the chart's right edge stays in sync without a blank.
watch(
  () => timeseries.events.rangeBounds.value?.max,
  (m) => { if (m != null) coord.noteSample(m); },
  { immediate: true },
);

/* True start of the CURRENT play (#587). The rail's left edge must
 * anchor to where THIS play_id began, not `current_play.started_at` —
 * that's a player-level field that goes stale when the play rotates
 * (a content switch gives a new play_id but leaves started_at pointing
 * at the prior play, so the rail stretched back hours to a play that
 * isn't loaded). Query the earliest archived event for the live play_id
 * instead; re-query whenever the play_id changes. */
const playStartMs = ref<number | null>(null);
watch(
  () => (livePlayer.value?.current_play as { play_id?: string } | null | undefined)?.play_id
    ?? playIdRef.value ?? null,
  async (pid) => {
    playStartMs.value = null;
    const player = apiPlayerIdRef.value;
    if (!pid || !player) return;
    try {
      const url = `/analytics/api/v2/events?player_id=${encodeURIComponent(player)}`
        + `&play_id=${encodeURIComponent(pid)}&order=asc&limit=1`;
      const r = await fetch(url);
      if (!r.ok) return;
      const j = await r.json();
      const ts = j?.items?.[0]?.ts as string | undefined;
      if (ts) {
        const ms = Date.parse(ts);
        // Guard against a race where the play_id changed mid-fetch.
        const curPid = (livePlayer.value?.current_play as { play_id?: string } | null | undefined)?.play_id
          ?? playIdRef.value ?? null;
        if (Number.isFinite(ms) && curPid === pid) playStartMs.value = ms;
      }
    } catch { /* network/parse failure → fall back to cache min */ }
  },
  { immediate: true },
);

/** Effective time range for the brush rail. Reads the cached
 *  rangeBounds of the samples stream as the historical span; always
 *  extends `max` with `coord.lastSampleMs` so the rail's right edge
 *  tracks the latest sample even when the cache hasn't received the
 *  freshest CH backfill yet. For a dead play `lastSampleMs` is 0
 *  (or the last archived sample's ts) and the rail bounds come
 *  entirely from `rangeBounds`. */
const timeRange = computed<{ min: number; max: number } | null>(() => {
  const ar = timeseries.events.rangeBounds.value;
  const live = coord.state.lastSampleMs || 0;
  // Extend the rail's LEFT edge to THIS play's true start (#587) so the
  // operator can drag the focus bar into windows the recency cap has
  // evicted from the cache — panning there fires the refetch watcher
  // above. Prefer the CLIENT-supplied, play-scoped current_play.start_time
  // (rotates with play_id); fall back to the play_id's earliest archived
  // event (playStartMs) for non-instrumented clients; then the cache min.
  // NEVER the stale player-level current_play.started_at.
  const clientStartStr = (livePlayer.value?.current_play as { start_time?: string | null } | null | undefined)?.start_time;
  const clientStart = clientStartStr ? Date.parse(clientStartStr) : NaN;
  const playStart = (Number.isFinite(clientStart) && clientStart > 0) ? clientStart : playStartMs.value;
  const haveStart = playStart != null && Number.isFinite(playStart) && playStart > 0;
  if (!ar && !live && !haveStart) return null;
  let min = ar?.min ?? 0;
  if (haveStart && (min === 0 || (playStart as number) < min)) min = playStart as number;
  let max = Math.max(ar?.max ?? 0, live);
  if (!min) min = max;
  if (!max) max = min;
  if (!min && !max) return null;
  return { min, max };
});

const loading = computed(() => timeseries.events.loading.value);
const error = computed(() => timeseries.events.error.value);
const progressLabel = computed(() => loading.value ? 'Streaming snapshots…' : '');
// Approximate count of rendered samples — used in the brush-rail
// status line. The cache only grows; reading via inRange touches
// `version` so this stays reactive on every flush.
const samplesCount = computed(() => {
  void timeseries.events.version.value;
  return timeseries.events.inRange(0, Number.MAX_SAFE_INTEGER).length;
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

// L1 — Severity tier (issue #473, replaces numeric Priority 1..4).
// String tiers, worst-first. Same vocabulary the forwarder writes to
// session_events.labels[] and network_requests.labels[] so a single
// filter UI sweeps both. Critical leads (user-visible playback
// breakage like qoe_stall_severe_midplay / frozen / restart_auto_recovery); Error
// sits next for system-detected error states (player_error).
type Severity = 'error' | 'critical' | 'warning' | 'info' | 'testing';
const SEVERITY_ORDER: Severity[] = ['critical', 'error', 'warning', 'info', 'testing'];
const SEVERITY_META: Record<Severity, { label: string; color: string; bg: string; border: string }> = {
  // Critical wears the red palette (worst-looking — user-visible
  // playback breakage); Error wears the orange palette. Swapped
  // from the original assignment so the two tiers' visuals match
  // operator intuition. The whole palette pair moves together so
  // each tier keeps internal bg/border/text harmony.
  error:    { label: 'Error',    color: '#7c2d12', bg: '#ffedd5', border: '#fdba74' },
  critical: { label: 'Critical', color: '#7f1d1d', bg: '#fee2e2', border: '#fca5a5' },
  warning:  { label: 'Warning',  color: '#854d0e', bg: '#fef3c7', border: '#fcd34d' },
  info:     { label: 'Info',     color: '#1f2937', bg: '#f0fdf4', border: '#a7f3d0' },
  // Testing wears a muted slate palette (visually recessive — it's
  // test-harness metadata, not playback signal). Lowest in the order
  // and hidden by default. See the forwarder's SevTesting (#571).
  testing:  { label: 'Testing',  color: '#475569', bg: '#f1f5f9', border: '#cbd5e1' },
};

const expandedTiers = ref<Record<Severity, boolean>>({
  error: true, critical: true, warning: false, info: false, testing: false,
});
function toggleTier(p: Severity) {
  expandedTiers.value[p] = !expandedTiers.value[p];
}

const visiblePriority = ref<Record<Severity, boolean>>({
  error: true, critical: true, warning: true, info: false, testing: false,
});
function togglePriorityVisibility(p: Severity, e: MouseEvent) {
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
function isTypeVisible(t: string, p: Severity): boolean {
  return !hiddenTypeKeys.value.has(typeKey(t, p));
}
function toggleTypeVisibility(t: string, p: Severity, e: MouseEvent) {
  e.stopPropagation();
  const k = typeKey(t, p);
  const next = new Set(hiddenTypeKeys.value);
  if (next.has(k)) next.delete(k); else next.add(k);
  hiddenTypeKeys.value = next;
}

const lockedPriority = ref<Severity | null>(null);
const lockedType = ref<string | null>(null);
function selectTier(p: Severity) {
  if (lockedPriority.value === p && !lockedType.value) {
    lockedPriority.value = null;
  } else {
    lockedPriority.value = p;
    lockedType.value = null;
    navCursor.value = 0;
    expandedTiers.value[p] = true;
  }
}
function selectType(t: string, p: Severity) {
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
function typeKey(t: string, p: Severity): string { return `${p}|${t}`; }
function toggleTypeExpand(t: string, p: Severity) {
  const k = typeKey(t, p);
  expandedTypeKey.value = expandedTypeKey.value === k ? null : k;
}

function eventSeverity(ev: SessionEvent): Severity {
  // Prefer the string `severity` field (post-#473 markers carry it).
  // Fall back to the legacy numeric `priority` for one release while
  // older forwarder builds + historical rows roll out.
  const sev = (ev as { severity?: string }).severity;
  if (sev === 'error' || sev === 'critical' || sev === 'warning' || sev === 'info' || sev === 'testing') return sev;
  switch (ev.priority) {
    case 1: return 'error';
    case 2: return 'critical';
    case 3: return 'warning';
    case 4: return 'info';
  }
  return 'warning';
}
function eventKindCE(ev: SessionEvent): 'effect' | 'cause' {
  return ev.kind === 'cause' ? 'cause' : 'effect';
}

interface AnnotatedEvent extends SessionEvent {
  _ts: number;
  _p: Severity;
}

const kindFilteredEvents = computed<AnnotatedEvent[]>(() =>
  sessionEvents.value
    .filter((ev) => enabledKind.value[eventKindCE(ev)])
    .map((ev) => ({ ...ev, _ts: eventMs(ev), _p: eventSeverity(ev) }))
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

const tierCounts = computed<Record<Severity, number>>(() => {
  const c: Record<Severity, number> = { error: 0, critical: 0, warning: 0, info: 0, testing: 0 };
  for (const ev of kindFilteredEvents.value) c[ev._p]++;
  return c;
});

function kindCount(k: 'effect' | 'cause'): number {
  let n = 0;
  for (const ev of sessionEvents.value) if (eventKindCE(ev) === k) n++;
  return n;
}

const tierTypes = computed<Record<Severity, Array<{ type: string; count: number }>>>(() => {
  const buckets: Record<Severity, Map<string, number>> = {
    error: new Map(), critical: new Map(), warning: new Map(), info: new Map(), testing: new Map(),
  };
  for (const ev of kindFilteredEvents.value) {
    const t = String(ev.type ?? 'event');
    buckets[ev._p].set(t, (buckets[ev._p].get(t) ?? 0) + 1);
  }
  const out = {} as Record<Severity, Array<{ type: string; count: number }>>;
  for (const p of SEVERITY_ORDER) {
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

function selectInstance(ev: AnnotatedEvent, t: string, p: Severity) {
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

function tierPreview(p: Severity): Array<{ type: string; count: number }> {
  return tierTypes.value[p].slice(0, 5);
}
function tierPreviewMore(p: Severity): number {
  return Math.max(0, tierTypes.value[p].length - 5);
}
function pickPreviewType(t: string, p: Severity) {
  expandedTiers.value[p] = true;
  selectType(t, p);
}

const scopeLabel = computed<string>(() => {
  if (lockedType.value && lockedPriority.value) {
    return `${lockedType.value} (in ${SEVERITY_META[lockedPriority.value].label})`;
  }
  if (lockedPriority.value) {
    return `All ${SEVERITY_META[lockedPriority.value].label} events`;
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
  const current = brushRange.value;
  const half = (current.max - current.min) / 2;
  const newStart = clampStart(ev._ts - half);
  const newEnd = clampEnd(ev._ts + half);
  coord.setRange({ min: newStart, max: newEnd });
}

watch(navCurrent, (ev) => {
  if (!ev) { coord.setCursor(null, null); return; }
  // Compose a short label for the cursor hover tooltip. `type` is the
  // event class (e.g. `restart`, `stall_start`, `fault_on`); `info`
  // is the per-event detail string when available. Severity is
  // appended in parens so the hover surface tells the operator
  // everything they need without reopening the navigator. Issue #486.
  const parts: string[] = [];
  if (ev.type) parts.push(String(ev.type));
  if (ev.info) parts.push(String(ev.info));
  const sev = (ev.severity ?? '').toString();
  const label = parts.join(' · ') + (sev ? ` (${sev})` : '');
  coord.setCursor(ev._ts, label || 'event');
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
      color: SEVERITY_META[ev._p].color,
      opacity: eventKindCE(ev) === 'effect' ? 1 : 0.4,
      isCurrent,
      ts: ev._ts,
      ev,
      title: `${ev.type ?? 'event'} · p${ev._p} · ${fmtTime(ev._ts)}${ev.info ? ' · ' + ev.info : ''}`,
    };
  });
});

/* ─── Brush + focus window ──────────────────────────────────────── */

/** Brush position derives directly from `coord.effectiveRange`. Drag
 *  handlers compute new ranges from mouse deltas and write via
 *  `coord.setRange`; there are no local windowStart/windowEnd refs,
 *  no userMovedBrush flag. "Paused / pinned" IS `coord.state.range
 *  !== null`. "Live edge" IS `coord.isAtLiveEdge(brushRange.max)`.
 *
 *  Fresh-session auto-grow happens automatically: as samples land,
 *  `coord.state.lastSampleMs` advances, so `effectiveRange.max`
 *  advances, so the brush widens up to `coord.state.liveSpan`
 *  (default 10 min) without any auto-feedback watcher that could get
 *  stuck. */
const brushRange = computed(() => coord.effectiveRange.value);

/** Draft focus window during an active brush gesture (issue #590). While
 *  the operator drags/resizes the rail (or wheel-zooms it), the rail
 *  renders from this draft so it tracks the cursor live — but the
 *  coordinated range the heavy panels read is NOT updated until the
 *  gesture settles (mouse-release for drag/resize, ~160 ms quiet for
 *  wheel). So charts/logs/timeline re-render once on commit instead of
 *  on every mousemove/wheel tick. */
const draftRange = ref<{ min: number; max: number } | null>(null);
/** What the rail visual + focus pill render: the in-flight draft while
 *  gesturing, else the committed coordinated range. */
const railRange = computed(() => draftRange.value ?? coord.effectiveRange.value);
const windowSpanMs = computed(() => Math.max(1, railRange.value.max - railRange.value.min));
/** Is the focus window parked at the live edge? Drives the rail pill —
 *  it used to be hardcoded "· at end", which lied once the operator
 *  pinned to (or panned into) a historical window. Reads railRange so it
 *  tracks the in-flight draft during a drag too. */
const atLiveEdge = computed(() => coord.isAtLiveEdge(railRange.value.max));

/** Live toggle checked rule — same across every surface. */
const brushLiveChecked = computed(() => coord.state.range === null);

function clampStart(v: number) {
  const r = timeRange.value; if (!r) return v;
  return Math.max(r.min, Math.min(v, brushRange.value.max - 1000));
}
function clampEnd(v: number) {
  const r = timeRange.value; if (!r) return v;
  return Math.min(r.max, Math.max(v, brushRange.value.min + 1000));
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
  // Snapshot the start range BEFORE pinning — if the brush was
  // following live, effectiveRange was advancing with each sample.
  // Pinning stops that so the drag operates on a stable baseline.
  const start = brushRange.value;
  dragState.value = {
    mode,
    startX: e.clientX,
    startStart: start.min,
    startEnd: start.max,
  };
  // Pin coord once so live-follow stops during the drag, and seed the
  // draft so the rail tracks the cursor live. onDragMove updates only
  // the draft; coord (and thus the panels) commits on release (#590).
  coord.setRange({ min: start.min, max: start.max });
  draftRange.value = { min: start.min, max: start.max };
  window.addEventListener('mousemove', onDragMove);
  window.addEventListener('mouseup', onDragEnd, { once: true });
}
function onDragMove(e: MouseEvent) {
  const d = dragState.value; if (!d) return;
  const r = timeRange.value; if (!r) return;
  const dms = pxToMs(e.clientX - d.startX);
  const MIN_WINDOW_MS = 1000;
  let s = d.startStart;
  let f = d.startEnd;
  if (d.mode === 'pan') {
    const span = d.startEnd - d.startStart;
    s = d.startStart + dms;
    f = s + span;
    if (s < r.min) { s = r.min; f = s + span; }
    if (f > r.max) { f = r.max; s = f - span; }
  } else if (d.mode === 'resize-left') {
    s = d.startStart + dms;
    if (s < r.min) s = r.min;
    if (s > d.startEnd - MIN_WINDOW_MS) s = d.startEnd - MIN_WINDOW_MS;
    f = d.startEnd;
  } else if (d.mode === 'resize-right') {
    f = d.startEnd + dms;
    if (f > r.max) f = r.max;
    if (f < d.startStart + MIN_WINDOW_MS) f = d.startStart + MIN_WINDOW_MS;
    s = d.startStart;
  }
  // Update only the draft during the drag (#590) — the rail moves live,
  // the panels stay parked at the pinned start until release.
  draftRange.value = { min: s, max: f };
}
function onDragEnd() {
  // Commit the draft (the final dragged window) to coord on release.
  const ended = draftRange.value ?? brushRange.value;
  draftRange.value = null;
  dragState.value = null;
  window.removeEventListener('mousemove', onDragMove);
  // BRUSH WIDTH on release becomes the new liveSpan — operator's
  // intent regardless of where the right edge ended up. Pinned drops
  // store the span so it survives the round trip when they later
  // click Live to return; live drops update the span immediately so
  // every other chart's live-tracker uses the same width.
  const dropSpan = ended.max - ended.min;
  if (dropSpan > 0) coord.setLiveSpan(dropSpan);
  // RIGHT EDGE within 2 s of the latest sample → snap to live (charts
  // AND brush follow the right edge as new samples arrive). Otherwise
  // commit the dragged window to coord — onDragMove only moved the
  // draft, so coord is still parked at the drag's start until here (#590).
  if (coord.isAtLiveEdge(ended.max)) coord.setRange(null);
  else coord.setRange({ min: ended.min, max: ended.max });
}

/** Alt+wheel on the brush rail zooms the focus-window duration. Same
 *  semantics as Alt+wheel on the chart toolbars:
 *    - AT LIVE (brush.max ≈ lastSampleMs): grow/shrink span, keep
 *      right edge glued to live. Updates liveSpan, range stays null.
 *    - OFF LIVE: mouse-anchored — the time under the cursor stays
 *      fixed while both edges move. If the new right edge reaches
 *      live, snap to live tracking.
 *
 *  Plain wheel falls through to native page scroll. */
/** Rail-wheel debounce (#590). Wheel events update only the draft (rail
 *  moves live); the coordinated range commits ~160 ms after the wheel
 *  goes quiet, so the heavy panels render once per gesture instead of
 *  per tick. */
let railWheelTimer: number | null = null;
function scheduleRailCommit() {
  if (railWheelTimer != null) clearTimeout(railWheelTimer);
  railWheelTimer = window.setTimeout(commitRailDraft, 160);
}
function commitRailDraft() {
  if (railWheelTimer != null) { clearTimeout(railWheelTimer); railWheelTimer = null; }
  const d = draftRange.value;
  if (!d) return;
  draftRange.value = null;
  const span = d.max - d.min;
  if (span > 0) coord.setLiveSpan(span);
  // Right edge at live → follow live with the new span; else pin.
  if (coord.isAtLiveEdge(d.max)) coord.setRange(null);
  else coord.setRange({ min: d.min, max: d.max });
}

function onRailWheel(e: WheelEvent) {
  const rail = railRef.value;
  const r = timeRange.value;
  if (!rail || !r) return;
  // Base the next window on the in-flight draft so successive wheel
  // ticks compound before the debounced commit (#590).
  // Horizontal pan: trackpad two-finger swipe (deltaX) OR Shift+wheel
  // (the mouse way to scroll horizontally; magnitude lands on deltaX or
  // deltaY depending on browser). deltaX/railWidth maps directly to
  // fraction-of-full-data-range so a one-rail-width swipe pans by the
  // entire visible data span. The brush is CLAMPED to [r.min, r.max] so
  // it never leaves the rail. See gh#461.
  if (!e.altKey && (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY))) {
    e.preventDefault();
    e.stopPropagation();
    const widthPx = rail.clientWidth;
    if (widthPx <= 0) return;
    const current = railRange.value;
    const span = current.max - current.min;
    const delta = Math.abs(e.deltaX) >= Math.abs(e.deltaY) ? e.deltaX : e.deltaY;
    const dms = (delta / widthPx) * (r.max - r.min);
    let s = current.min + dms;
    let f = current.max + dms;
    if (s < r.min) { s = r.min; f = s + span; }
    if (f > r.max) { f = r.max; s = f - span; }
    draftRange.value = { min: s, max: f };
    scheduleRailCommit();
    return;
  }
  if (!e.altKey) return;
  e.preventDefault();
  e.stopPropagation();
  const fullSpan = Math.max(1, r.max - r.min);
  const current = railRange.value;
  const currentSpan = Math.max(1, current.max - current.min);
  const factor = e.deltaY < 0 ? 0.9 : 1 / 0.9;
  const MIN_SPAN_MS = 1_000;
  const nextSpan = Math.max(MIN_SPAN_MS, Math.min(fullSpan, currentSpan * factor));
  if (nextSpan === currentSpan) return;

  if (coord.isAtLiveEdge(current.max)) {
    // At live: keep the right edge glued to live, grow/shrink leftward.
    draftRange.value = { min: current.max - nextSpan, max: current.max };
    scheduleRailCommit();
    return;
  }
  // Mouse-anchored: keep the timestamp under the cursor at the same
  // x position in the rail after the zoom.
  const rect = rail.getBoundingClientRect();
  const frac = rect.width > 0 ? (e.clientX - rect.left) / rect.width : 0.5;
  const anchorTime = r.min + frac * fullSpan;
  const anchorFracInWindow = (anchorTime - current.min) / currentSpan;
  let newStart = anchorTime - anchorFracInWindow * nextSpan;
  let newEnd = newStart + nextSpan;
  if (newStart < r.min) { newStart = r.min; newEnd = newStart + nextSpan; }
  if (newEnd > r.max) { newEnd = r.max; newStart = newEnd - nextSpan; }
  draftRange.value = { min: newStart, max: newEnd };
  scheduleRailCommit();
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
  const current = brushRange.value;
  const span = current.max - current.min;
  const r = timeRange.value; if (!r) return;
  let s = target - span / 2;
  let f = s + span;
  if (s < r.min) { s = r.min; f = s + span; }
  if (f > r.max) { f = r.max; s = f - span; }
  coord.setRange({ min: s, max: f });
}

/* ─── Brush-driven panel cursor ─────────────────────────────────── */

const qc = useQueryClient();
function playerKey(id: string) {
  return ['player', id] as const;
}

// Brush-end-row projection — runs in ALL contexts so SessionDisplay's
// display panels (PlayerMetrics, SessionDetails, charts) always
// reflect the state at the brush's right edge. When at the live
// edge, lastAt(brush.max) returns the latest sample → panels show
// essentially-current state. When brush moves back, panels show
// that past moment. The cache key is the prefixed archive id so
// this never collides with the live cache that outside mutation
// panels (FaultRules etc.) read.
watch(
  [() => brushRange.value.max, () => timeseries.events.version.value],
  ([endMs]) => {
    const row = timeseries.events.lastAt(endMs);
    if (!row) return;
    // Pass the events stream's min-bound as the play's first_seen_at
    // so SessionDetails' "First Request" + "Session Duration" tiles
    // render the play's true start, not the brush-cursor row's ts.
    const bounds = timeseries.events.rangeBounds.value;
    const minMs = bounds?.min;
    // ISO-with-Z so SessionDetails' fmtDate parses it as UTC across
    // all browsers (matches chRowAdapter.toISOWithZ normalisation
    // applied to last_seen_at; same format on both ends keeps
    // fmtDuration honest).
    const firstSeenAt = (minMs != null && Number.isFinite(minMs))
      ? new Date(minMs).toISOString()
      : undefined;
    // Max control_revision + max attempt_id across the whole play.
    // attempt_id is the recovery counter (1 = no recovery, 2 = one
    // restart, etc.); SessionDetails shows it as the "Attempt" tile.
    // Both pulled from the same single inRange() walk.
    let maxControlRevision: string | undefined;
    let maxAttemptId: number | undefined;
    if (bounds && Number.isFinite(bounds.min) && Number.isFinite(bounds.max)) {
      const rows = timeseries.events.inRange(bounds.min, bounds.max);
      // control_revision is RFC3339Nano post type-change-in-place;
      // string-compare gives chronological order for ISO timestamps.
      let crStr: string | undefined;
      let att = 0;
      for (const r of rows) {
        const rec = r as Record<string, unknown>;
        const candidate = typeof rec.control_revision === 'string' ? rec.control_revision : '';
        if (candidate && (!crStr || candidate > crStr)) crStr = candidate;
        const a = Number(rec.attempt_id ?? 0);
        if (Number.isFinite(a) && a > att) att = a;
      }
      maxControlRevision = crStr;
      if (att > 0) maxAttemptId = att;
    }
    const adapted = chRowToPlayerRecord(row, { firstSeenAt, maxControlRevision, maxAttemptId });
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
  const current = brushRange.value;
  const span = Math.max(1000, current.max - current.min);
  const newStart = r.min;
  const newEnd = Math.min(r.max, r.min + span);
  coord.setRange({ min: newStart, max: newEnd });
}
function skipToEnd() {
  const r = timeRange.value; if (!r) return;
  const current = brushRange.value;
  const span = Math.max(1000, current.max - current.min);
  // Snap to live — clear range, set liveSpan to current brush width.
  if (span > 0) coord.setLiveSpan(span);
  coord.setRange(null);
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
      <div class="brush-top-row">
        <button
          type="button"
          class="btn live-toggle brush-live-toggle"
          :class="{ checked: brushLiveChecked }"
          @click="coord.toggleLive()"
          :title="brushLiveChecked ? 'Pause at current live edge' : 'Resume following live'"
        >
          {{ brushLiveChecked ? '●' : '○' }} Live
        </button>
      </div>
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
              left: Math.max(0, (railRange.min - scrubMin) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
              right: Math.max(0, (scrubMax - railRange.max) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
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
            left: Math.max(0, (railRange.min - scrubMin) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
            right: Math.max(0, (scrubMax - railRange.max) / Math.max(1, scrubMax - scrubMin) * 100) + '%',
          }"
        >
          <span class="focus-pill">{{ fmtDurShort(windowSpanMs) }}{{ atLiveEdge ? ' · at end' : ' · ends ' + fmtTime(railRange.max) }}</span>
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
          v-for="p in SEVERITY_ORDER"
          :key="p"
          class="tier-group"
          :class="{
            expanded: expandedTiers[p],
            dim: !tierCounts[p],
            'tier-active': lockedPriority === p && !lockedType,
          }"
          :style="{
            '--tier-bg': SEVERITY_META[p].bg,
            '--tier-border': SEVERITY_META[p].border,
            '--tier-color': SEVERITY_META[p].color,
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
              :title="`Walk all ${tierCounts[p]} ${SEVERITY_META[p].label} event(s) with prev/next`"
              :disabled="!tierCounts[p]"
            >
              <span class="tier-dot" />
              <span class="tier-name">{{ SEVERITY_META[p].label }}</span>
              <span class="tier-count-pill">{{ tierCounts[p] }}</span>
            </button>
            <button
              type="button"
              class="tier-eye-btn"
              :class="{ off: !visiblePriority[p] }"
              @click="togglePriorityVisibility(p, $event)"
              :title="visiblePriority[p] ? `Hide ${SEVERITY_META[p].label} events from the rail` : `Show ${SEVERITY_META[p].label} events on the rail`"
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
      <!-- Cycle-band overlay — visible only when control_events for
           this play include at least one label_changed row carrying
           a cycle_id (characterization runs only). Aligned with the
           SSE backfill window. -->
      <CycleBandsRail
        v-if="cycleBandsDomain"
        :control-stream="timeseries.control"
        :from-ms="cycleBandsDomain.fromMs"
        :to-ms="cycleBandsDomain.toMs"
      />
      <EventsTimeline :player-id="archivePlayerId" :events-stream="timeseries.events" :avmetrics-stream="timeseries.avmetrics" />
    </CollapsibleSection>

    <CollapsibleSection title="Bitrate Chart etc" :open="true" eager persist-key="bitrate-chart">
      <BitrateChartPanelToolbar :player-id="archivePlayerId" />
      <div class="chart-stack">
        <BandwidthChart :player-id="archivePlayerId" :events-stream="timeseries.events" :avmetrics-stream="timeseries.avmetrics" />
        <BufferChart :player-id="archivePlayerId" :events-stream="timeseries.events" />
        <RTTChart :player-id="archivePlayerId" :events-stream="timeseries.events" />
        <FPSChart :player-id="archivePlayerId" :events-stream="timeseries.events" />
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

    <CollapsibleSection title="Play Log" persist-key="play-log">
      <!-- Time-multiplexed view of snapshots + network rows + events
           interleaved on one chronological scroll, with checkbox
           filters per source. The three streams come from the same
           timeseries SSE pool the other panels use; PlayLog merges
           them on the dashboard side rather than asking the
           forwarder for a pre-joined view. -->
      <PlayLog
        :player-id="archivePlayerId"
        :play-id="playIdRef"
        :events-stream="timeseries.events"
        :network-stream="timeseries.network"
        :control-stream="timeseries.control"
        :avmetrics-stream="timeseries.avmetrics"
      />
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

/* Top row above the scrub rail — right-aligned Live toggle. Own row
 * so it never overlaps the ⏮/⏭ buttons regardless of rail width. */
.brush-top-row {
  display: flex;
  justify-content: flex-end;
  margin-bottom: 6px;
}
.brush-live-toggle {
  font-size: 11px;
  padding: 3px 10px;
  border-radius: 4px;
  border: 1px solid #d1d5db;
  cursor: pointer;
  font-weight: 500;
}
.brush-live-toggle.checked {
  background: #10b981;
  border-color: #059669;
  color: white;
  font-weight: 600;
}
.brush-live-toggle.checked:hover { background: #059669; }
.brush-live-toggle:not(.checked) {
  background: #f3f4f6;
  color: #6b7280;
}
.brush-live-toggle:not(.checked):hover { background: #e5e7eb; color: #374151; }

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
  /* overflow VISIBLE so the brush window can extend above/below the rail
   * as a taller grab target (those strips are tick-free, so they're a
   * clean drag zone). Horizontal bleed past the rail edges onto the
   * ⏮/⏭ buttons is instead prevented by clamping the window's left/right
   * to ≥0 in the template binding. */
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
  /* Extend above and below the 30px rail so the grab target is taller
   * than the rail itself — those strips are tick-free, so you can always
   * grab the window (or its handles) for dragging without landing on a
   * marker. */
  top: -9px;
  bottom: -9px;
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
  /* Match the window's vertical extent so the resize grips are equally
   * tall and easy to grab above/below the rail. */
  top: -9px;
  bottom: -9px;
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
  /* Two-line: edge labels (scrubMin/scrubMax) on the first line,
     focus-label pills on the second so the centered pill cannot
     overlap the date+time edge strings when the rail is narrow
     or the focus window is near an edge. */
  height: 34px;
  font-size: 10.5px;
  color: #6b7280;
  font-family: ui-monospace, monospace;
}
.rail-edge-label { position: absolute; top: 0; }
.rail-edge-label:first-child { left: 0; }
.rail-edge-label:last-child  { right: 0; }
.rail-focus-label {
  position: absolute;
  top: 16px;
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
/* count hugs the name instead of being pushed to the far edge by flex:1
   (mirrors the Sessions label-filter fix). */
.type-name {
  flex: 0 1 auto;
  min-width: 0;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.type-count { flex: none; font-variant-numeric: tabular-nums; color: #6b7280; font-size: 10.5px; }
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
