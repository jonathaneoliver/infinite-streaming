<script setup lang="ts">
/**
 * PlayLog.vue — time-multiplexed scroll of three sources on one
 * chronological view:
 *
 *   - snapshot  (session_snapshots rows, via timeseries.samples)
 *   - network   (network_requests rows,  via timeseries.network)
 *   - event     (typed events,           via timeseries.events)
 *
 * Operator-facing differences from NetworkLog: no timing bar (per
 * user request), source-toggle checkboxes, and uniform columns
 * (time / source / player_id / play_id / restart_id / name / info)
 * that work for all three row shapes.
 *
 * No new server fetch — re-uses the three Streams the parent
 * SessionDisplay already holds from useSessionTimeSeries. Row build
 * is a reactive computed over `version` so the table grows live as
 * SSE deltas land.
 */
import { computed, nextTick, onBeforeUnmount, ref, toRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import type { Stream } from '@/composables/useSessionTimeSeries';

const props = defineProps<{
  playerId: string;
  /** play_id from the URL, used as the fallback `play_id` value for
   *  event rows — the events SSE doesn't yet project play_id (the
   *  derivation in events_query.go doesn't carry it through the
   *  UNION ALL branches). Within a SessionViewer scope this is the
   *  same id every event would have anyway.
   *  In live mode this is null, in which case event rows show '—'. */
  playId: string | null;
  samplesStream: Stream<Record<string, unknown>>;
  networkStream: Stream<Record<string, unknown>>;
  eventsStream: Stream<Record<string, unknown>>;
}>();

const playerIdRef = toRef(props, 'playerId');
const { sseState } = usePlayer(playerIdRef);

const showSnapshots = ref(true);
const showNetwork = ref(true);
const showEvents = ref(true);
const paused = ref(false);
const followLatest = ref(true);

/** Display mode for SNAPSHOT rows only. Snapshots are dense
 *  state-dumps where most fields stay constant across heartbeats;
 *  showing every field on every row drowns out the actual deltas.
 *  Network + event rows are inherently distinct per row so they
 *  always render every field regardless of this setting.
 *
 *  - "all": every non-empty key=value pair on the snapshot row.
 *  - "changed": only the keys whose value differs from the
 *    previous chronologically-earlier snapshot row. Lets the
 *    operator scan state transitions on a thrashing player. */
type DisplayMode = 'all' | 'changed';
const displayMode = ref<DisplayMode>('all');

type SortCol = 'time' | 'source';
type SortDir = 'asc' | 'desc';
const sortCol = ref<SortCol | null>('time');
const sortDir = ref<SortDir>('asc');

const rowsScrollRef = ref<HTMLDivElement | null>(null);

type Source = 'snapshot' | 'network' | 'event';
interface Row {
  ts: number;          // epoch ms
  source: Source;
  playerId: string;
  playId: string;
  restartId: string;
  /** Original payload as it came off the stream — used to compute
   *  the per-row field list (and to diff against the previous row
   *  of the same source for "Changed fields" mode). */
  raw: Record<string, unknown>;
  /** Event rows: the value of `raw.type` lifted to a top-level
   *  badge so the event name is always visible regardless of the
   *  chip ordering / diff filter. Empty for snapshot + network
   *  rows. */
  eventName?: string;
}

interface DisplayedField {
  name: string;
  value: string;
}

/** Field keys handled by the identity columns; skip in the kv panel
 *  to avoid duplicating what's already visible on every row. Also
 *  skips monotonic-noise fields whose change-on-every-row would
 *  dominate the "Changed fields" view. */
const SKIP_KEYS = new Set([
  'ts', 'timestamp', 'event_time',     // rendered as _time
  'player_id', 'id',                   // rendered as player_id
  'play_id',                           // rendered as play_id
  'restart_id',                        // rendered as restart_id
  'revision',                          // monotonic counter
  'server_received_at_ms',             // monotonic counter (server clock)
  'entry_fingerprint',                 // CH dedupe key
]);

function tsOf(raw: Record<string, unknown>): number {
  const v = raw.ts ?? raw.timestamp;
  if (typeof v === 'number' && Number.isFinite(v)) return v;
  if (typeof v !== 'string') return NaN;
  // ClickHouse "YYYY-MM-DD HH:MM:SS.fff" → RFC3339 by swapping the
  // space for 'T' and appending 'Z'. Already-RFC3339 strings pass
  // through Date.parse unchanged.
  const normalised = v.length > 10 && v.charAt(10) === ' '
    ? v.replace(' ', 'T') + 'Z'
    : v;
  const ms = Date.parse(normalised);
  return Number.isFinite(ms) ? ms : NaN;
}

function asStr(v: unknown): string {
  if (typeof v === 'string') return v;
  if (typeof v === 'number') return String(v);
  return '';
}

/** Extract the path tail from a HAR URL, dropping the content-name
 *  prefix that's identical for every request on a play. */
function pathTail(url: string): string {
  if (!url) return '';
  const idx = url.indexOf('/go-live/');
  if (idx < 0) return url;
  // skip "/go-live/<content_id>/"
  const after = url.slice(idx + '/go-live/'.length);
  const slash = after.indexOf('/');
  return slash >= 0 ? after.slice(slash + 1) : after;
}

function buildSnapshotRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  return {
    ts,
    source: 'snapshot',
    playerId: asStr(raw.player_id ?? props.playerId),
    playId: asStr(raw.play_id),
    restartId: asStr(raw.restart_id),
    raw,
  };
}

function buildNetworkRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  // Derive the operator-friendly summary fields the legacy NetworkLog
  // panel surfaces (KB, Mbps, dur) and graft them onto the row so
  // they appear in the kv chip list — both 'all' and 'changed' modes
  // pick them up naturally, and the alphabetical chip order keeps
  // them visually adjacent to status / bytes_out / transfer_ms.
  const bytesOut = numOrZero(raw.bytes_out);
  const transferMs = numOrZero(raw.transfer_ms);
  const totalMs = numOrZero(raw.total_ms);
  const summed = numOrZero(raw.dns_ms) + numOrZero(raw.connect_ms)
    + numOrZero(raw.tls_ms) + numOrZero(raw.ttfb_ms) + transferMs;
  const durMs = totalMs > 0 ? totalMs : summed;
  const enriched: Record<string, unknown> = { ...raw };
  if (bytesOut > 0) enriched.KB = (bytesOut / 1024);
  if (transferMs > 0 && bytesOut > 0) {
    enriched.Mbps = (bytesOut * 8) / (transferMs * 1000);
  }
  if (durMs > 0) enriched.dur = fmtMs(durMs);
  return {
    ts,
    source: 'network',
    playerId: asStr(raw.player_id ?? props.playerId),
    playId: asStr(raw.play_id),
    restartId: asStr(raw.restart_id),
    raw: enriched,
  };
}

function numOrZero(v: unknown): number {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

/** Same human-readable ms/s formatter the NetworkLog panel uses, so
 *  durations in the Play Log line up with the waterfall above it. */
function fmtMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '';
  if (ms < 1) return ms.toFixed(2) + ' ms';
  if (ms < 1000) return ms.toFixed(0) + ' ms';
  return (ms / 1000).toFixed(2) + ' s';
}

function buildEventRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  // event rows from /api/v2/timeseries don't currently project
  // play_id / restart_id (events_query.go derives events on the fly
  // from snapshots + network_requests via UNION ALL, and the columns
  // don't ride through). Fall back to the URL's play_id — within
  // SessionViewer scope they're always identical, and live mode
  // shows '—' which is the honest answer.
  return {
    ts,
    source: 'event',
    playerId: props.playerId,
    playId: props.playId || '',
    restartId: '',
    raw,
    eventName: asStr(raw.type),
  };
}

/** Render a single field value to a short display string. JSON-stringify
 *  nested values; trim floats; tolerate everything else. */
function formatValue(v: unknown): string {
  if (v == null) return '';
  if (typeof v === 'string') return v;
  if (typeof v === 'number') {
    // Trim long float tails — 3 sig figs is enough for buffer/bitrate
    // ranges that dominate the diff view. Ints pass through unchanged.
    if (Number.isInteger(v)) return String(v);
    if (!Number.isFinite(v)) return String(v);
    return Number(v.toFixed(3)).toString();
  }
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  try { return JSON.stringify(v); } catch { return String(v); }
}

/** Walk the raw row's top-level keys (skipping identity + noise
 *  fields), emit name=value pairs sorted alphabetically. Empty
 *  values are dropped — they'd just be visual noise.
 *
 *  `extraSkip` lets a caller hide keys it's already rendering
 *  elsewhere — used for event rows to omit `type` (lifted to the
 *  eventName badge) so it doesn't appear twice. */
function fieldsFromRaw(raw: Record<string, unknown>, extraSkip?: Set<string>): DisplayedField[] {
  const out: DisplayedField[] = [];
  const keys = Object.keys(raw).sort();
  for (const k of keys) {
    if (SKIP_KEYS.has(k)) continue;
    if (extraSkip && extraSkip.has(k)) continue;
    const v = raw[k];
    if (v == null) continue;
    const formatted = formatValue(v);
    if (formatted === '') continue;
    out.push({ name: k, value: formatted });
  }
  return out;
}

const EVENT_SKIP = new Set(['type']);

const allRows = computed<Row[]>(() => {
  // Touch each stream's version so Vue re-runs the computed on every
  // delta, even though inRange() also touches the underlying ref.
  void props.samplesStream.version.value;
  void props.networkStream.version.value;
  void props.eventsStream.version.value;
  const built: Row[] = [];
  if (showSnapshots.value) {
    for (const raw of props.samplesStream.inRange(0, Number.MAX_SAFE_INTEGER)) {
      const r = buildSnapshotRow(raw);
      if (r) built.push(r);
    }
  }
  if (showNetwork.value) {
    for (const raw of props.networkStream.inRange(0, Number.MAX_SAFE_INTEGER)) {
      const r = buildNetworkRow(raw);
      if (r) built.push(r);
    }
  }
  if (showEvents.value) {
    for (const raw of props.eventsStream.inRange(0, Number.MAX_SAFE_INTEGER)) {
      const r = buildEventRow(raw);
      if (r) built.push(r);
    }
  }
  return built;
});

function sortValue(r: Row, c: SortCol): number | string {
  switch (c) {
    case 'time': return r.ts;
    case 'source': return r.source;
  }
}

/** Row + the field list to render. Computed in chronological order
 *  so the "Changed fields" diff against the previous same-source row
 *  is well-defined regardless of the display sort. */
interface RowWithFields extends Row {
  fields: DisplayedField[];
}

const rowsWithFields = computed<RowWithFields[]>(() => {
  // Build chronological copy so the diff against the previous
  // snapshot is well-defined regardless of the display sort
  // direction the operator picks below.
  const chrono = allRows.value.slice().sort((a, b) => a.ts - b.ts);
  const mode = displayMode.value;
  // Only snapshots participate in the diff — every network /
  // event row is unique by construction so a per-row diff is
  // either uninformative (everything different every time) or
  // misleading (status=200 chip vanishes because the previous
  // request was also 200, hiding the steady-state success).
  let prevSnapshot: Record<string, unknown> | null = null;
  const out: RowWithFields[] = new Array(chrono.length);
  for (let i = 0; i < chrono.length; i++) {
    const r = chrono[i];
    let fields: DisplayedField[];
    const skip = r.source === 'event' ? EVENT_SKIP : undefined;
    if (r.source === 'snapshot' && mode === 'changed' && prevSnapshot) {
      const prevByKey = new Map<string, string>();
      for (const f of fieldsFromRaw(prevSnapshot)) prevByKey.set(f.name, f.value);
      fields = fieldsFromRaw(r.raw, skip).filter((f) => prevByKey.get(f.name) !== f.value);
    } else {
      // Snapshot in 'all' mode, first-ever snapshot in 'changed'
      // mode (no prior to diff against), or any non-snapshot row.
      fields = fieldsFromRaw(r.raw, skip);
    }
    out[i] = { ...r, fields };
    if (r.source === 'snapshot') prevSnapshot = r.raw;
  }
  return out;
});

const sortedRows = computed<RowWithFields[]>(() => {
  const list = rowsWithFields.value.slice();
  const col = sortCol.value;
  if (!col) return list;
  const sign = sortDir.value === 'asc' ? 1 : -1;
  list.sort((a, b) => {
    const av = sortValue(a, col);
    const bv = sortValue(b, col);
    if (typeof av === 'string' || typeof bv === 'string') {
      return String(av).localeCompare(String(bv)) * sign;
    }
    return (Number(av) - Number(bv)) * sign;
  });
  return list;
});

const counts = computed(() => {
  let snap = 0, net = 0, evt = 0;
  for (const r of allRows.value) {
    if (r.source === 'snapshot') snap++;
    else if (r.source === 'network') net++;
    else evt++;
  }
  return { snap, net, evt, total: allRows.value.length };
});

function clickSort(col: SortCol) {
  if (sortCol.value !== col) {
    sortCol.value = col;
    sortDir.value = col === 'time' ? 'asc' : 'asc';
  } else if (sortDir.value === 'asc') {
    sortDir.value = 'desc';
  } else {
    sortCol.value = null;
    sortDir.value = 'asc';
  }
}

function arrow(col: SortCol): string {
  if (sortCol.value !== col) return '';
  return sortDir.value === 'asc' ? ' ▲' : ' ▼';
}

function fmtTime(ts: number): string {
  const d = new Date(ts);
  return (
    String(d.getHours()).padStart(2, '0') + ':' +
    String(d.getMinutes()).padStart(2, '0') + ':' +
    String(d.getSeconds()).padStart(2, '0') + '.' +
    String(d.getMilliseconds()).padStart(3, '0')
  );
}

/** Short 8-char prefix of a UUID for display. Returns '—' on empty. */
function shortId(id: string): string {
  if (!id) return '—';
  return id.length > 8 ? id.slice(0, 8) : id;
}

function togglePause() {
  paused.value = !paused.value;
}

// Follow-latest: snap the inner scroll container to the end when new
// rows arrive (mirrors NetworkLog.vue's behaviour).
watch(
  () => sortedRows.value.length,
  () => {
    if (!followLatest.value) return;
    if (paused.value) return;
    nextTick(() => {
      const el = rowsScrollRef.value;
      if (!el) return;
      el.scrollTop = sortDir.value === 'asc' ? el.scrollHeight : 0;
    });
  },
);

/* ─── Wheel hijack ─────────────────────────────────────────────────
 * Plain wheel inside the inner scroll container moves the PAGE; only
 * Alt+wheel scrolls the rows. Matches NetworkLog.vue so operators
 * don't have to learn two different wheel behaviours for similar
 * tables in the same panel. */
let attachedRowsEl: HTMLDivElement | null = null;
watch(rowsScrollRef, (el) => {
  if (attachedRowsEl && attachedRowsEl !== el) {
    attachedRowsEl.removeEventListener('wheel', onRowsWheel, { capture: true } as EventListenerOptions);
    attachedRowsEl = null;
  }
  if (el && el !== attachedRowsEl) {
    el.addEventListener('wheel', onRowsWheel, { capture: true, passive: false });
    attachedRowsEl = el;
  }
}, { immediate: true });
onBeforeUnmount(() => {
  if (attachedRowsEl) {
    attachedRowsEl.removeEventListener('wheel', onRowsWheel, { capture: true } as EventListenerOptions);
    attachedRowsEl = null;
  }
});
function onRowsWheel(e: WheelEvent) {
  if (e.altKey) return;
  e.preventDefault();
  window.scrollBy({ top: e.deltaY, left: e.deltaX, behavior: 'auto' });
}
</script>

<template>
  <div class="play-log">
    <div class="toolbar">
      <label class="opt"><input type="checkbox" v-model="showSnapshots" /> Snapshots ({{ counts.snap }})</label>
      <label class="opt"><input type="checkbox" v-model="showNetwork" /> Network ({{ counts.net }})</label>
      <label class="opt"><input type="checkbox" v-model="showEvents" /> Events ({{ counts.evt }})</label>
      <button class="btn" type="button" @click="togglePause">
        {{ paused ? '▶ Live' : '⏸ Pause' }}
      </button>
      <label class="opt">
        <input type="checkbox" v-model="followLatest" />
        Follow latest
      </label>
      <span class="count">{{ counts.total }} row{{ counts.total === 1 ? '' : 's' }}</span>
      <span class="sse" :data-state="sseState">{{ sseState }}</span>
    </div>

    <div class="toolbar mode-row">
      <span class="mode-label">Snapshot fields:</span>
      <label class="opt"><input type="radio" value="all" v-model="displayMode" /> All</label>
      <label class="opt"><input type="radio" value="changed" v-model="displayMode" /> Changed only (vs previous snapshot)</label>
      <span class="mode-hint">Network &amp; event rows always show every field.</span>
    </div>

    <p class="note">
      Time-ordered merge of three sources. <strong>Snapshot</strong> = one
      `session_snapshots` row (player heartbeat or state-change post).
      <strong>Network</strong> = one `network_requests` row. <strong>Event</strong>
      = one derived row from the typed event taxonomy. `restart_id` on
      event rows is blank — the events SSE doesn't yet project it (see
      `events_query.go`); within a single SessionViewer scope the
      `play_id` shown is the URL's play_id.
    </p>

    <div v-if="!sortedRows.length" class="empty">No rows yet.</div>
    <div v-else class="table-wrap">
      <div class="row head">
        <div class="cell c-time sortable" @click="clickSort('time')">_time<span class="arr">{{ arrow('time') }}</span></div>
        <div class="cell c-source sortable" @click="clickSort('source')">source<span class="arr">{{ arrow('source') }}</span></div>
        <div class="cell c-player">player_id</div>
        <div class="cell c-play">play_id</div>
        <div class="cell c-restart">restart_id</div>
        <div class="cell c-fields">fields</div>
      </div>

      <div class="rows" ref="rowsScrollRef">
        <div
          v-for="(r, i) in sortedRows"
          :key="i"
          class="row"
          :class="`src-${r.source}`"
        >
          <div class="cell c-time">{{ fmtTime(r.ts) }}</div>
          <div class="cell c-source">
            <span class="src-tag" :class="`tag-${r.source}`">{{ r.source }}</span>
          </div>
          <div class="cell c-player" :title="r.playerId">{{ shortId(r.playerId) }}</div>
          <div class="cell c-play" :title="r.playId">{{ shortId(r.playId) }}</div>
          <div class="cell c-restart" :title="r.restartId">{{ shortId(r.restartId) }}</div>
          <div class="cell c-fields">
            <span
              v-if="r.eventName"
              class="event-name"
              :title="'event_name=' + r.eventName"
            >{{ r.eventName }}</span>
            <span v-if="r.fields.length === 0 && !r.eventName" class="kv-empty">—</span>
            <span
              v-for="f in r.fields"
              :key="f.name"
              class="kv"
              :title="f.name + '=' + f.value"
            ><span class="kv-name">{{ f.name }}</span>=<span class="kv-value">{{ f.value }}</span></span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.play-log {
  display: grid;
  gap: 8px;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 12px;
  color: #6b7280;
  flex-wrap: wrap;
}
.btn {
  background: #f3f4f6;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  padding: 4px 10px;
  font-size: 12px;
  cursor: pointer;
}
.btn:hover { background: #e5e7eb; }
.opt { display: inline-flex; align-items: center; gap: 4px; cursor: pointer; }
.count { font-variant-numeric: tabular-nums; }
.sse {
  text-transform: uppercase;
  padding: 2px 6px;
  border-radius: 8px;
  font-weight: 600;
  font-size: 10px;
  margin-left: auto;
}
.sse[data-state='open'] { background: #d1fae5; color: #065f46; }
.sse[data-state='connecting'] { background: #fef3c7; color: #92400e; }
.sse[data-state='closed'] { background: #fee2e2; color: #991b1b; }

.empty {
  font-size: 13px;
  color: #9ca3af;
  padding: 24px;
  text-align: center;
}

.note {
  font-size: 12px;
  background: #eff6ff;
  border: 1px solid #bfdbfe;
  color: #1e3a8a;
  padding: 8px 10px;
  border-radius: 6px;
  margin: 0;
  line-height: 1.4;
}

.table-wrap {
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  overflow: hidden;
  background: #fff;
}

.mode-row {
  margin-top: -2px;
}
.mode-label {
  font-weight: 600;
  color: #374151;
}
.mode-hint {
  color: #9ca3af;
  font-size: 11px;
}

.row {
  display: grid;
  grid-template-columns:
    var(--c-time, 96px)
    var(--c-source, 76px)
    var(--c-player, 90px)
    var(--c-play, 90px)
    var(--c-restart, 90px)
    var(--c-fields, minmax(320px, 4fr));
  gap: 8px;
  padding: 4px 8px;
  font-size: 11px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  align-items: start;
  border-top: 1px solid #f3f4f6;
}

.row.head {
  background: #f3f4f6;
  font-family: system-ui;
  font-weight: 600;
  color: #4b5563;
  text-transform: uppercase;
  font-size: 10px;
  letter-spacing: 0.4px;
  padding: 6px 8px;
  border-top: none;
  position: sticky;
  top: 0;
  z-index: 1;
}

.rows {
  max-height: 480px;
  overflow-y: auto;
}

.row:hover { background: #f9fafb; }

.row.src-snapshot { background: #fafafa; }
.row.src-snapshot:hover { background: #f3f4f6; }
.row.src-network  { background: #ffffff; }
.row.src-event    { background: #fef9c3; }
.row.src-event:hover { background: #fef08a; }

.cell {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.c-time { color: #6b7280; }
.c-player, .c-play, .c-restart {
  color: #374151;
  font-variant-numeric: tabular-nums;
}

.src-tag {
  display: inline-block;
  padding: 1px 6px;
  border-radius: 8px;
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  border: 1px solid transparent;
}
.tag-snapshot { background: #e5e7eb; color: #374151; border-color: #d1d5db; }
.tag-network  { background: #dbeafe; color: #1e3a8a; border-color: #bfdbfe; }
.tag-event    { background: #fde68a; color: #92400e; border-color: #fcd34d; }

.sortable { cursor: pointer; user-select: none; }
.sortable:hover { color: #1f2937; }
.arr { font-size: 9px; margin-left: 2px; }

/* Field column — flowing kv chips. Each chip wraps when the row is
 * narrow; long values clamp on overflow so the row stays one
 * "paragraph" of fields rather than smearing across the layout. */
.c-fields {
  white-space: normal;
  overflow: hidden;
  display: flex;
  flex-wrap: wrap;
  gap: 3px 8px;
  line-height: 1.55;
}
.kv {
  background: #f3f4f6;
  border: 1px solid #e5e7eb;
  border-radius: 4px;
  padding: 0 5px;
  font-size: 10px;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: inline-block;
}
.kv-name {
  color: #4b5563;
  font-weight: 600;
}
.kv-value {
  color: #111827;
}
.kv-empty {
  color: #9ca3af;
  font-size: 10px;
}

/* Event name — leading badge so the operator always sees "stall"
 * "downshift" etc. without scanning the kv chips for `type=`. */
.event-name {
  background: #f59e0b;
  color: #fff;
  border-radius: 4px;
  padding: 1px 8px;
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.3px;
}

/* Source-specific tints on the kv chips so a glance at a row tells
 * you which table it came from even when the fields column is the
 * dominant visual area. */
.row.src-network .kv {
  background: #eff6ff;
  border-color: #bfdbfe;
}
.row.src-network .kv-name { color: #1e3a8a; }

.row.src-event .kv {
  background: #fef3c7;
  border-color: #fcd34d;
}
.row.src-event .kv-name { color: #92400e; }
</style>
