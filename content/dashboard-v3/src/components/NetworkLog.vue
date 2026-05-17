<script setup lang="ts">
/**
 * NetworkLog.vue — per-player HAR-shaped request waterfall. Columns
 * match the legacy testing-session.html network log:
 *   Time · Flags · Method · Path · KB · Mbps · Dur · Status · timing bar
 *
 * Flags column:
 *   ↻  retry
 *   !  faulted (with category glyph appended)
 *   ⏰ slow segment (transfer > 6s; HLS-target-duration heuristic)
 *
 * The timing bar shows phase segments (dns / connect / tls / wait /
 * transfer) sized by their reported durations, positioned within the
 * visible time window (defaults to the span of currently-loaded rows).
 *
 * Polled at 2 Hz; SSE-driven feed lands in a follow-up.
 */
import { computed, nextTick, ref, toRef, watch } from 'vue';
import { useQuery } from '@tanstack/vue-query';
import * as repo from '@/repo/v2-repo';
import type { NetworkLogEntry } from '@/repo/v2-repo';
import { usePlayer } from '@/composables/usePlayer';
import NetworkLogBrush, { type BrushTick } from './NetworkLogBrush.vue';

const props = defineProps<{ playerId: string }>();
const playerIdRef = toRef(props, 'playerId');
const { sseState } = usePlayer(playerIdRef);

type SortCol = 'time' | 'method' | 'path' | 'bytes' | 'mbps' | 'duration' | 'status';
type SortDir = 'asc' | 'desc';

const sortCol = ref<SortCol | null>('time');
// Ascending by default so the newest request lands at the bottom of
// the table — matches the legacy waterfall + lets follow-latest scroll
// to the bottom naturally.
const sortDir = ref<SortDir>('asc');
const followLatest = ref(true);
const faultedOnly = ref(false);
const hideSuccessful = ref(false);
const paused = ref(false);

const rowsScrollRef = ref<HTMLDivElement | null>(null);

const brushStartMs = ref<number | null>(null);
const brushEndMs = ref<number | null>(null);
function onBrushChange(v: { startMs: number; endMs: number } | null) {
  if (v) {
    brushStartMs.value = v.startMs;
    brushEndMs.value = v.endMs;
  } else {
    brushStartMs.value = null;
    brushEndMs.value = null;
  }
}

const query = useQuery({
  queryKey: computed(() => ['network', playerIdRef.value] as const),
  queryFn: () => repo.getPlayerNetworkLog(playerIdRef.value, 500),
  // Stop polling once the server has rejected this player_id outright
  // (400 / 404). Otherwise refetch every 2s while not paused.
  refetchInterval: (q: any) => {
    const s = (q?.state?.error as any)?.status;
    if (typeof s === 'number' && s >= 400 && s < 500) return false;
    return paused.value ? false : 2_000;
  },
  refetchIntervalInBackground: false,
  staleTime: 1_000,
  retry: (n, err: any) => {
    const s = err?.status;
    if (typeof s === 'number' && s >= 400 && s < 500) return false;
    return n < 1;
  },
  refetchOnMount: (q: any) => {
    const s = (q?.state?.error as any)?.status;
    return !(typeof s === 'number' && s >= 400 && s < 500);
  },
  refetchOnWindowFocus: (q: any) => {
    const s = (q?.state?.error as any)?.status;
    return !(typeof s === 'number' && s >= 400 && s < 500);
  },
  refetchOnReconnect: (q: any) => {
    const s = (q?.state?.error as any)?.status;
    return !(typeof s === 'number' && s >= 400 && s < 500);
  },
});

interface Row {
  entry: NetworkLogEntry;
  ts: number;
  duration: number;
  dns: number;
  connect: number;
  tls: number;
  wait: number;
  transfer: number;
}

function num(v: unknown): number {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

function buildRow(e: NetworkLogEntry): Row | null {
  const ts = e.timestamp ? Date.parse(e.timestamp) : NaN;
  if (!Number.isFinite(ts)) return null;
  const dns = Math.max(0, num(e.dns_ms));
  const connect = Math.max(0, num(e.connect_ms));
  const tls = Math.max(0, num(e.tls_ms));
  // Legacy "wait" = upstream TTFB minus connect+tls (or client_wait_ms if
  // explicitly reported). Clamp at 0 so visual phases stay non-negative.
  const ttfb = Math.max(0, num(e.ttfb_ms));
  const clientWait = num(e.client_wait_ms);
  const wait = clientWait > 0 ? clientWait : Math.max(0, ttfb - dns - connect - tls);
  const transfer = Math.max(0, num(e.transfer_ms));
  let duration = num(e.total_ms);
  if (duration <= 0) duration = dns + connect + tls + wait + transfer;
  return { entry: e, ts, duration, dns, connect, tls, wait, transfer };
}

function isSuccessful(e: NetworkLogEntry): boolean {
  const s = Number(e.status ?? 0);
  return s >= 200 && s < 400 && !e.faulted;
}

/** All rows, before brush + toggles — used to feed the overview rail
 *  so the user always sees every request regardless of current filter. */
const allRows = computed<Row[]>(() => {
  const items = (query.data.value ?? []) as NetworkLogEntry[];
  const built: Row[] = [];
  for (const e of items) {
    const r = buildRow(e);
    if (!r) continue;
    built.push(r);
  }
  return built;
});

const rows = computed<Row[]>(() => {
  return allRows.value.filter((r) => {
    if (faultedOnly.value && !r.entry.faulted) return false;
    if (hideSuccessful.value && isSuccessful(r.entry)) return false;
    if (brushStartMs.value != null && brushEndMs.value != null) {
      const reqStart = r.ts;
      const reqEnd = r.ts + r.duration;
      // Keep any row whose extent overlaps the brush range.
      if (reqEnd < brushStartMs.value) return false;
      if (reqStart > brushEndMs.value) return false;
    }
    return true;
  });
});

const brushTicks = computed<BrushTick[]>(() =>
  allRows.value.map((r) => ({
    ts: r.ts,
    status: Number(r.entry.status ?? 0),
    faulted: !!r.entry.faulted,
  })),
);

/** Summary line: total request count + breakdowns by kind / fault /
 *  slow + lifetime bytes-in. Matches the legacy `.netwf-summary`. */
const summary = computed(() => {
  const all = allRows.value;
  let segments = 0;
  let manifests = 0;
  let master = 0;
  let other = 0;
  let faults = 0;
  let slow = 0;
  let bytes = 0;
  for (const r of all) {
    const k = String(r.entry.request_kind ?? '').toLowerCase();
    if (k === 'master_manifest') master++;
    else if (k === 'manifest' || k === 'audio_manifest') manifests++;
    else if (k === 'segment' || k === 'partial' || k === 'init' || k === 'audio_segment') segments++;
    else other++;
    if (r.entry.faulted) faults++;
    if (isSlowSegment(r)) slow++;
    bytes += num(r.entry.bytes_out);
  }
  return { total: all.length, segments, manifests, master, other, faults, slow, bytes };
});

function fmtBytesShort(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / 1024 / 1024).toFixed(2)} MB`;
  return `${(n / 1024 / 1024 / 1024).toFixed(2)} GB`;
}

function sortValue(r: Row, c: SortCol): number | string {
  switch (c) {
    case 'time': return r.ts;
    case 'method': return r.entry.method ?? '';
    case 'path': return r.entry.path ?? r.entry.url ?? '';
    case 'bytes': return num(r.entry.bytes_out);
    case 'mbps': return r.transfer > 0 ? (num(r.entry.bytes_out) * 8) / (r.transfer * 1000) : 0;
    case 'duration': return r.duration;
    case 'status': return num(r.entry.status);
  }
}

const sortedRows = computed<Row[]>(() => {
  const list = rows.value.slice();
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

const winRange = computed(() => {
  if (!sortedRows.value.length) return { start: 0, end: 0, span: 1 };
  let start = Infinity;
  let end = -Infinity;
  for (const r of sortedRows.value) {
    if (r.ts < start) start = r.ts;
    const e = r.ts + Math.max(50, r.duration);
    if (e > end) end = e;
  }
  return { start, end, span: Math.max(50, end - start) };
});

function clickSort(col: SortCol) {
  if (sortCol.value !== col) {
    sortCol.value = col;
    sortDir.value = 'desc';
  } else if (sortDir.value === 'desc') {
    sortDir.value = 'asc';
  } else {
    sortCol.value = null;
    sortDir.value = 'desc';
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

function fmtKB(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '—';
  return (n / 1024).toFixed(n < 1024 * 100 ? 1 : 0);
}

function fmtMbps(r: Row): string {
  if (r.transfer <= 0) return '—';
  const v = (num(r.entry.bytes_out) * 8) / (r.transfer * 1000);
  if (!Number.isFinite(v)) return '—';
  if (v < 1) return v.toFixed(2);
  if (v < 100) return v.toFixed(1);
  return v.toFixed(0);
}

function fmtMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  if (ms < 1) return ms.toFixed(2) + ' ms';
  if (ms < 1000) return ms.toFixed(0) + ' ms';
  return (ms / 1000).toFixed(2) + ' s';
}

function statusClass(s: number): string {
  if (!s) return 'status-none';
  if (s >= 500) return 'status-5xx';
  if (s >= 400) return 'status-4xx';
  if (s >= 300) return 'status-3xx';
  return 'status-2xx';
}

function isSlowSegment(r: Row): boolean {
  const kind = String(r.entry.request_kind ?? '').toLowerCase();
  if (!/segment|init/.test(kind)) return false;
  if (/manifest/.test(kind)) return false;
  return r.transfer > 6_000; // HLS target duration default
}

function flagsFor(r: Row): { text: string; color: string } {
  const cat = String(r.entry.fault_category ?? '').toLowerCase();
  let glyph = '';
  if (r.entry.faulted) {
    if (cat === 'socket') glyph = '!✂';
    else if (cat === 'transfer_timeout') glyph = '!⏱';
    else if (cat === 'client_disconnect') glyph = '!↩';
    else glyph = '!';
  }
  const slow = isSlowSegment(r) ? '⏰' : '';
  const text = glyph + slow;
  const color = r.entry.faulted ? '#7f1d1d' : (slow ? '#92400e' : '');
  return { text, color };
}

function rowFaultClass(r: Row): string {
  if (r.entry.faulted) return 'row-faulted';
  if (isSlowSegment(r)) return 'row-slow';
  return '';
}

function tooltipFor(r: Row): string {
  const lines: string[] = [];
  lines.push(`${r.entry.method ?? 'GET'} ${r.entry.url || r.entry.path || ''}`);
  if (r.entry.fault_type) lines.push(`fault: ${r.entry.fault_type}${r.entry.fault_category ? ` (${r.entry.fault_category})` : ''}`);
  if (r.entry.fault_action) lines.push(`action: ${r.entry.fault_action}`);
  if (r.dns) lines.push(`DNS ${r.dns.toFixed(0)} ms`);
  if (r.connect) lines.push(`connect ${r.connect.toFixed(0)} ms`);
  if (r.tls) lines.push(`TLS ${r.tls.toFixed(0)} ms`);
  if (r.wait) lines.push(`wait ${r.wait.toFixed(0)} ms`);
  if (r.transfer) lines.push(`transfer ${r.transfer.toFixed(0)} ms`);
  if (r.entry.content_type) lines.push(`type: ${r.entry.content_type}`);
  if (r.entry.bytes_out) lines.push(`bytes out: ${r.entry.bytes_out}`);
  if (r.entry.bytes_in) lines.push(`bytes in: ${r.entry.bytes_in}`);
  return lines.join('\n');
}

interface PhaseStyle {
  left: string;
  width: string;
  segments: { key: string; flex: number }[];
  durationLeft?: string;
  durationRight?: string;
}

function phaseStyle(r: Row): PhaseStyle | null {
  const w = winRange.value;
  if (w.span <= 0) return null;
  const reqStart = r.ts;
  const reqEnd = r.ts + Math.max(50, r.duration);
  if (reqEnd < w.start || reqStart > w.end) return null;
  const leftPct = Math.max(0, ((reqStart - w.start) / w.span) * 100);
  const widthPct = Math.max(0.2, ((reqEnd - reqStart) / w.span) * 100);
  const phases = [
    { key: 'dns', value: r.dns },
    { key: 'connect', value: r.connect },
    { key: 'tls', value: r.tls },
    { key: 'wait', value: r.wait },
    { key: 'transfer', value: r.transfer },
  ];
  const total = phases.reduce((a, p) => a + Math.max(0, p.value), 0) || 1;
  const segments = phases
    .filter((p) => p.value > 0)
    .map((p) => ({ key: p.key, flex: p.value / total }));
  const rightEdge = leftPct + widthPct;
  return {
    left: leftPct.toFixed(3) + '%',
    width: widthPct.toFixed(3) + '%',
    segments,
    durationLeft: rightEdge < 90 ? `calc(${rightEdge.toFixed(3)}% + 6px)` : undefined,
    durationRight: rightEdge >= 90 ? `${(100 - rightEdge + 0.5).toFixed(3)}%` : undefined,
  };
}

function refresh() {
  query.refetch();
}

function togglePause() {
  paused.value = !paused.value;
  if (!paused.value) refresh();
}

// Follow-latest: when on, scroll the row list to the latest row each
// refresh. We watch the sorted rows length and trigger after the next
// DOM tick so the row is laid out before we scroll.
watch(
  () => sortedRows.value.length,
  () => {
    if (!followLatest.value) return;
    if (paused.value) return;
    nextTick(() => {
      const el = rowsScrollRef.value;
      if (!el) return;
      // Desc sort (default) keeps newest at top; asc keeps at bottom.
      el.scrollTop = sortDir.value === 'asc' ? el.scrollHeight : 0;
    });
  },
);
</script>

<template>
  <div class="net-log">
    <div class="toolbar">
      <button class="btn" type="button" @click="refresh">Refresh</button>
      <button class="btn" type="button" @click="togglePause">
        {{ paused ? '▶ Live' : '⏸ Pause' }}
      </button>
      <label class="opt">
        <input type="checkbox" v-model="followLatest" />
        Follow latest
      </label>
      <label class="opt">
        <input type="checkbox" v-model="hideSuccessful" />
        Hide successful
      </label>
      <label class="opt">
        <input type="checkbox" v-model="faultedOnly" />
        Faulted only
      </label>
      <span class="count">
        {{ sortedRows.length }} request{{ sortedRows.length === 1 ? '' : 's' }}
      </span>
      <span class="sse" :data-state="sseState">{{ sseState }}</span>
    </div>

    <NetworkLogBrush
      v-if="brushTicks.length"
      :ticks="brushTicks"
      :brush-start-ms="brushStartMs"
      :brush-end-ms="brushEndMs"
      @update:brush="onBrushChange"
    />

    <!-- Summary line: count breakdown + lifetime bytes (matches legacy
         .netwf-summary). Shown beneath the brush so it picks up the
         tally for the *full* request log, not the filtered/brushed
         table. -->
    <div v-if="summary.total" class="summary">
      <span class="sm-tag total">{{ summary.total }} requests</span>
      <span v-if="summary.segments" class="sm-tag seg">{{ summary.segments }} seg</span>
      <span v-if="summary.manifests" class="sm-tag man">{{ summary.manifests }} man</span>
      <span v-if="summary.master" class="sm-tag mst">{{ summary.master }} master</span>
      <span v-if="summary.other" class="sm-tag oth">{{ summary.other }} other</span>
      <span v-if="summary.faults" class="sm-tag flt">{{ summary.faults }} faulted</span>
      <span v-if="summary.slow" class="sm-tag slow">{{ summary.slow }} slow</span>
      <span class="sm-tag bytes">{{ fmtBytesShort(summary.bytes) }} out</span>
    </div>

    <p class="warning">
      Transfer timings and derived Mbps are approximate, measured
      <strong>downstream</strong> — from when go-proxy starts writing the
      response back to the client device until the last byte is flushed
      (proxy → player). They do <strong>not</strong> include the upstream
      fetch from go-proxy to go-live. Numbers are most reliable when the
      network is slow and transfers are large (especially video segments);
      short responses transfer in &lt;1 ms and round to noise.
    </p>

    <div v-if="!sortedRows.length" class="empty">No requests to plot yet.</div>
    <div v-else class="table-wrap">
      <div class="row head">
        <div class="cell c-time sortable" @click="clickSort('time')">Time<span class="arr">{{ arrow('time') }}</span></div>
        <div class="cell c-flags">⚑</div>
        <div class="cell c-method sortable" @click="clickSort('method')">M<span class="arr">{{ arrow('method') }}</span></div>
        <div class="cell c-path sortable" @click="clickSort('path')">Path<span class="arr">{{ arrow('path') }}</span></div>
        <div class="cell c-bytes sortable" @click="clickSort('bytes')">KB<span class="arr">{{ arrow('bytes') }}</span></div>
        <div class="cell c-mbps sortable" @click="clickSort('mbps')">Mbps<span class="arr">{{ arrow('mbps') }}</span></div>
        <div class="cell c-dur sortable" @click="clickSort('duration')">Dur<span class="arr">{{ arrow('duration') }}</span></div>
        <div class="cell c-status sortable" @click="clickSort('status')">Status<span class="arr">{{ arrow('status') }}</span></div>
        <div class="cell c-bar">Timing</div>
      </div>

      <div class="rows" ref="rowsScrollRef">
        <div
          v-for="(r, i) in sortedRows"
          :key="i"
          class="row"
          :class="rowFaultClass(r)"
          :title="tooltipFor(r)"
        >
          <div class="cell c-time">{{ fmtTime(r.ts) }}</div>
          <div class="cell c-flags" :style="{ color: flagsFor(r).color }">{{ flagsFor(r).text }}</div>
          <div class="cell c-method">{{ r.entry.method ?? '?' }}</div>
          <div class="cell c-path" :title="r.entry.url ?? r.entry.path ?? ''">
            {{ r.entry.path || r.entry.url || '—' }}
          </div>
          <div class="cell c-bytes">{{ fmtKB(num(r.entry.bytes_out)) }}</div>
          <div class="cell c-mbps">{{ fmtMbps(r) }}</div>
          <div class="cell c-dur">{{ fmtMs(r.duration) }}</div>
          <div class="cell c-status" :class="statusClass(num(r.entry.status))">
            {{ r.entry.status || '—' }}
          </div>
          <div class="cell c-bar">
            <div class="track">
              <template v-if="phaseStyle(r)">
                <div
                  class="bar"
                  :style="{ left: phaseStyle(r)!.left, width: phaseStyle(r)!.width }"
                >
                  <div
                    v-for="seg in phaseStyle(r)!.segments"
                    :key="seg.key"
                    class="phase"
                    :class="`ph-${seg.key}`"
                    :style="{ flex: `${seg.flex} 0 0` }"
                  />
                </div>
                <div
                  class="duration-text"
                  :style="{
                    left: phaseStyle(r)!.durationLeft,
                    right: phaseStyle(r)!.durationRight,
                  }"
                >
                  {{ fmtMs(r.duration) }}
                </div>
              </template>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.net-log {
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

.warning {
  font-size: 12px;
  background: #fef7e0;
  border: 1px solid #fde68a;
  color: #92400e;
  padding: 8px 10px;
  border-radius: 6px;
  margin: 0;
  line-height: 1.4;
}

.summary {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 11px;
}
.sm-tag {
  background: #f3f4f6;
  color: #374151;
  border: 1px solid #e5e7eb;
  padding: 2px 8px;
  border-radius: 10px;
  font-variant-numeric: tabular-nums;
}
.sm-tag.total  { background: #e0e7ff; color: #312e81; border-color: #c7d2fe; font-weight: 600; }
.sm-tag.seg    { background: #d1fae5; color: #065f46; border-color: #a7f3d0; }
.sm-tag.man    { background: #dbeafe; color: #1e3a8a; border-color: #bfdbfe; }
.sm-tag.mst    { background: #ede9fe; color: #5b21b6; border-color: #ddd6fe; }
.sm-tag.flt    { background: #fee2e2; color: #991b1b; border-color: #fca5a5; font-weight: 600; }
.sm-tag.slow   { background: #fef3c7; color: #92400e; border-color: #fcd34d; }
.sm-tag.bytes  { background: #f1f5f9; color: #475569; border-color: #cbd5e1; }
.sm-tag.oth    { background: #f3f4f6; color: #6b7280; }

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
    var(--c-flags, 28px)
    var(--c-method, 44px)
    var(--c-path, minmax(140px, 1fr))
    var(--c-bytes, 64px)
    var(--c-mbps, 56px)
    var(--c-dur, 72px)
    var(--c-status, 60px)
    var(--c-bar, minmax(220px, 1.4fr));
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
.row-faulted { background: #fef2f2; }
.row-faulted:hover { background: #fee2e2; }
.row-slow { background: #fffbeb; }

.cell {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.c-time { color: #6b7280; }
.c-flags { text-align: center; font-weight: 700; }
.c-path { color: #111827; }
.c-bytes, .c-mbps, .c-dur { text-align: right; }
.c-status {
  text-align: center;
  padding: 0 6px;
  border-radius: 3px;
}
.status-2xx { background: #d1fae5; color: #065f46; }
.status-3xx { background: #dbeafe; color: #1e40af; }
.status-4xx { background: #fef3c7; color: #92400e; }
.status-5xx { background: #fee2e2; color: #991b1b; }
.status-none { color: #9ca3af; }

.sortable { cursor: pointer; user-select: none; }
.sortable:hover { color: #1f2937; }
.arr { font-size: 9px; margin-left: 2px; }

.c-bar { padding: 0; }
.track {
  position: relative;
  height: 14px;
  background: #f9fafb;
  border-radius: 3px;
  overflow: hidden;
}
.bar {
  position: absolute;
  top: 1px;
  bottom: 1px;
  display: flex;
  border-radius: 2px;
  overflow: hidden;
  min-width: 2px;
}
.phase { height: 100%; }
.ph-dns      { background: #8b5cf6; }
.ph-connect  { background: #f59e0b; }
.ph-tls      { background: #10b981; }
.ph-wait     { background: #3b82f6; }
.ph-transfer { background: #6366f1; }

.duration-text {
  position: absolute;
  top: 0;
  font-size: 10px;
  color: #6b7280;
  line-height: 14px;
  font-family: ui-monospace, monospace;
  pointer-events: none;
}
</style>
