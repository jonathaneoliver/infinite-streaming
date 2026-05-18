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

type SortCol = 'time' | 'source' | 'name';
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
  name: string;
  info: string;
}

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
  // The samples stream wraps the raw CH row in a PlayerRecord-shaped
  // projection — fields land at the top level (player_id, play_id,
  // restart_id) AND under player_metrics for some derived fields.
  const lastEvent = asStr(raw.last_event ?? (raw.player_metrics as Record<string, unknown> | undefined)?.last_event);
  const state = asStr(raw.player_state ?? (raw.player_metrics as Record<string, unknown> | undefined)?.state);
  return {
    ts,
    source: 'snapshot',
    playerId: asStr(raw.player_id ?? props.playerId),
    playId: asStr(raw.play_id),
    restartId: asStr(raw.restart_id),
    name: lastEvent || 'snapshot',
    info: state ? `state=${state}` : '',
  };
}

function buildNetworkRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  const method = asStr(raw.method) || 'GET';
  const path = pathTail(asStr(raw.url) || asStr(raw.path));
  const status = asStr(raw.status);
  const faulted = !!raw.faulted && raw.faulted !== 0;
  const faultTag = faulted ? ` ⚠${asStr(raw.fault_category) || asStr(raw.fault_type) || ''}` : '';
  return {
    ts,
    source: 'network',
    playerId: asStr(raw.player_id ?? props.playerId),
    playId: asStr(raw.play_id),
    restartId: asStr(raw.restart_id),
    name: `${method} ${path}`,
    info: `status=${status || '—'}${faultTag}`,
  };
}

function buildEventRow(raw: Record<string, unknown>): Row | null {
  const ts = tsOf(raw);
  if (!Number.isFinite(ts)) return null;
  const type = asStr(raw.type) || 'event';
  const info = asStr(raw.info);
  const priority = asStr(raw.priority);
  const kind = asStr(raw.kind);
  // event rows from /api/v2/timeseries don't currently project
  // play_id / restart_id (events_query.go derives events on the fly
  // from snapshots + network_requests via UNION ALL, and the play_id
  // column doesn't ride through). Fall back to the URL's play_id —
  // within SessionViewer scope they're always identical, and live
  // mode shows '—' which is the honest answer.
  const playIdFallback = props.playId || '';
  return {
    ts,
    source: 'event',
    playerId: props.playerId,
    playId: playIdFallback,
    restartId: '',
    name: type,
    info: [info, kind && `kind=${kind}`, priority && `p${priority}`].filter(Boolean).join(' · '),
  };
}

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
    case 'name': return r.name;
  }
}

const sortedRows = computed<Row[]>(() => {
  const list = allRows.value.slice();
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
        <div class="cell c-name sortable" @click="clickSort('name')">event_name<span class="arr">{{ arrow('name') }}</span></div>
        <div class="cell c-info">info</div>
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
          <div class="cell c-name" :title="r.name">{{ r.name }}</div>
          <div class="cell c-info" :title="r.info">{{ r.info }}</div>
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

.row {
  display: grid;
  grid-template-columns:
    var(--c-time, 96px)
    var(--c-source, 76px)
    var(--c-player, 90px)
    var(--c-play, 90px)
    var(--c-restart, 90px)
    var(--c-name, minmax(160px, 1.2fr))
    var(--c-info, minmax(160px, 1.6fr));
  gap: 8px;
  padding: 3px 8px;
  font-size: 11px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  align-items: center;
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
</style>
