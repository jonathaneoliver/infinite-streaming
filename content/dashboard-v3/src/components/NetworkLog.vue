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
import { computed, nextTick, onBeforeUnmount, onMounted, ref, toRef, watch } from 'vue';
import type { NetworkLogEntry } from '@/repo/v2-repo';
import { usePlayer } from '@/composables/usePlayer';
import { useChartCoordination } from '@/composables/useChartCoordination';
import type { Stream } from '@/composables/useSessionTimeSeries';

const props = defineProps<{
  playerId: string;
  /** Network stream from the parent SessionDisplay's
   *  useSessionTimeSeries model. Supplies every per-request row for
   *  this (player, play), server-filtered to the current play_id,
   *  with the ring + CH boundary already deduplicated. */
  networkStream: Stream<Record<string, unknown>>;
}>();
const playerIdRef = toRef(props, 'playerId');
usePlayer(playerIdRef); // keep the SSE subscription warm; live state is read off `coord` below
const coord = useChartCoordination(playerIdRef);

/** Rows to highlight for the synchronized "selected event" cursor.
 *
 *  Three-tier fallback so SOMETHING always lights up when a cursor
 *  is set, since events rarely line up with a request's lifetime
 *  (player-side stalls, downshifts, etc. happen between requests):
 *
 *    1. Containing requests — any rows whose [ts, ts+duration]
 *       interval includes the cursor time. Multiple can match
 *       (overlapping partial-segment fetches); highlight all.
 *    2. Predecessor — the most recent request that started at or
 *       before the cursor. Maps "where in the request stream did
 *       this event happen?" to the obvious answer "right after this
 *       last request started."
 *    3. Successor — first row after the cursor, only when the
 *       cursor sits before any logged request (e.g. cursor at
 *       session start before traffic began).
 */
const currentRowKeys = computed<Set<string>>(() => {
  const ms = coord.state.cursorMs;
  const out = new Set<string>();
  if (ms == null || !Number.isFinite(ms)) return out;
  const arr = allRows.value;
  if (!arr.length) return out;

  let foundContaining = false;
  for (const r of arr) {
    if (r.ts <= ms && ms <= r.ts + Math.max(1, r.duration)) {
      out.add(rowKey(r));
      foundContaining = true;
    }
  }
  if (foundContaining) return out;

  // Predecessor — largest ts ≤ cursorMs. allRows isn't guaranteed
  // sorted by ts (depends on the forwarder's ORDER BY), so scan.
  let bestPred: typeof arr[number] | null = null;
  let bestPredTs = -Infinity;
  let bestSucc: typeof arr[number] | null = null;
  let bestSuccTs = Infinity;
  for (const r of arr) {
    if (r.ts <= ms) {
      if (r.ts > bestPredTs) { bestPredTs = r.ts; bestPred = r; }
    } else {
      if (r.ts < bestSuccTs) { bestSuccTs = r.ts; bestSucc = r; }
    }
  }
  if (bestPred) out.add(rowKey(bestPred));
  else if (bestSucc) out.add(rowKey(bestSucc));
  return out;
});
function rowKey(r: { ts: number; entry: NetworkLogEntry }): string {
  // Same join key as the v-for :key so the class application lines up
  // with the rendered row. URL alone isn't unique (retries), so we
  // pair with ts. (sortedRows :key is the array index, but mapping by
  // index requires the computed to know the current sort order — far
  // simpler to key by ts+url and check Set.has() in the template.)
  return r.ts + '|' + (r.entry.url ?? r.entry.path ?? '');
}

type SortCol = 'time' | 'method' | 'path' | 'bytes' | 'mbps' | 'duration' | 'status';
type SortDir = 'asc' | 'desc';

const sortCol = ref<SortCol | null>('time');
// Ascending by default so the newest request lands at the bottom of
// the table — matches the legacy waterfall + lets follow-latest scroll
// to the bottom naturally.
const sortDir = ref<SortDir>('asc');
const faultedOnly = ref(false);
const hideSuccessful = ref(false);

/** Follow-latest mirrors the page-level focus bar's Live state.
 *  When the operator drags the brush back to a historical range,
 *  range !== null and we stop auto-scrolling. See
 *  BitrateChartPanelToolbar.vue for the canonical pattern. */
const liveChecked = computed(() => coord.state.range === null);
function onLiveToggleClick() {
  coord.toggleLive();
}

const rowsScrollRef = ref<HTMLDivElement | null>(null);

/** Adapt a raw row from the /api/v2/timeseries stream (CH row
 *  shape — `ts` string in CH format, Int64 fields as JSON strings,
 *  `faulted` as 0/1) to the NetworkLogEntry shape the renderer
 *  expects. The wire shape is intentionally kept close to CH's
 *  storage; the dashboard does the conversion once on read. */
function chRowToEntry(raw: Record<string, unknown>): NetworkLogEntry {
  const tsRaw = raw.ts as string | undefined;
  const timestamp = tsRaw && tsRaw.length > 10 && tsRaw.charAt(10) === ' '
    ? tsRaw.replace(' ', 'T') + 'Z'
    : (tsRaw ?? '');
  const toNum = (v: unknown): number => {
    if (typeof v === 'number') return v;
    if (typeof v === 'string') return Number(v);
    return 0;
  };
  return {
    timestamp,
    method: (raw.method as string) ?? '',
    url: (raw.url as string) ?? '',
    upstream_url: (raw.upstream_url as string) ?? '',
    path: (raw.path as string) ?? '',
    request_kind: (raw.request_kind as string) ?? '',
    status: toNum(raw.status),
    bytes_in: toNum(raw.bytes_in),
    bytes_out: toNum(raw.bytes_out),
    content_type: (raw.content_type as string) ?? '',
    play_id: (raw.play_id as string) ?? '',
    ttfb_ms: toNum(raw.ttfb_ms),
    total_ms: toNum(raw.total_ms),
    dns_ms: toNum(raw.dns_ms),
    connect_ms: toNum(raw.connect_ms),
    tls_ms: toNum(raw.tls_ms),
    transfer_ms: toNum(raw.transfer_ms),
    client_wait_ms: toNum(raw.client_wait_ms),
    faulted: !!raw.faulted && raw.faulted !== 0,
    fault_type: (raw.fault_type as string) ?? '',
    fault_action: (raw.fault_action as string) ?? '',
    fault_category: (raw.fault_category as string) ?? '',
  } as NetworkLogEntry;
}

interface Row {
  entry: NetworkLogEntry;
  ts: number;
  duration: number;
  dns: number;
  connect: number;
  tls: number;
  wait: number;
  transfer: number;
  // labels[] off the raw CH row (issue #474). Carried alongside the
  // entry instead of on NetworkLogEntry itself because the OpenAPI-
  // generated type doesn't know about labels yet.
  labels: string[];
  // #506 — batch-derived per-row token (V_SEG(ΔP,ΔS), V_PROBE, …)
  // LEFT-JOINed onto the row by the forwarder. Empty for un-scored rows.
  token: string;
}

function num(v: unknown): number {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

function buildRow(e: NetworkLogEntry, labels: string[], token: string): Row | null {
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
  return { entry: e, ts, duration, dns, connect, tls, wait, transfer, labels, token };
}

// Per-label rendering helpers — mirror PlayLog so chips look the
// same across panels. Severity → CSS class for the tint; name strips
// the severity prefix + the `*` synthMark for display, keeping the
// raw form on hover via the title attr.
function labelSeverity(label: string): 'info' | 'warning' | 'critical' | 'error' {
  const eq = label.indexOf('=');
  if (eq <= 0) return 'info';
  const sev = label.slice(0, eq);
  if (sev === 'error' || sev === 'critical' || sev === 'warning') return sev;
  return 'info';
}
function labelName(label: string): string {
  const eq = label.indexOf('=');
  const tail = eq > 0 ? label.slice(eq + 1) : label;
  return tail.startsWith('*') ? tail.slice(1) : tail;
}

function isSuccessful(e: NetworkLogEntry): boolean {
  const s = Number(e.status ?? 0);
  return s >= 200 && s < 400 && !e.faulted;
}

/** All rows, before brush + toggles — used to feed the overview rail
 *  so the user always sees every request regardless of current filter.
 *
 *  Sourced from the parent SessionDisplay's unified time-series
 *  model (`networkStream`). The stream is server-filtered by play_id
 *  and merges ring + CH on the boundary, so the table no longer
 *  needs to dedupe or worry about stale-play leakage. Reading the
 *  stream's `version` ref registers the computed as a dep so it
 *  re-runs on every delta. */
const allRows = computed<Row[]>(() => {
  // Touch `version` so Vue tracks deltas; inRange() ALSO touches
  // the underlying array ref via tsOf, but reading version is a
  // belt-and-suspenders guarantee against shallowRef quirks.
  void props.networkStream.version.value;
  const raw = props.networkStream.inRange(0, Number.MAX_SAFE_INTEGER);
  const built: Row[] = [];
  for (const obj of raw) {
    const entry = chRowToEntry(obj);
    const labels = Array.isArray((obj as { labels?: unknown }).labels)
      ? ((obj as { labels: unknown[] }).labels).filter((x): x is string => typeof x === 'string')
      : [];
    const token = typeof (obj as { token?: unknown }).token === 'string'
      ? ((obj as { token: string }).token)
      : '';
    const r = buildRow(entry, labels, token);
    if (!r) continue;
    built.push(r);
  }
  return built;
});

const rows = computed<Row[]>(() => {
  // Follow the focus bar (issue #586): only rows within the coordinated
  // visible window, so the Network Log lines up with the charts +
  // timeline. effectiveRange is the live tail when live, or the pinned
  // window when the operator pans back. (The summary line below stays a
  // full-log tally on purpose.)
  const w = coord.effectiveRange.value;
  return allRows.value.filter((r) => {
    if (w && (r.ts < w.min || r.ts > w.max)) return false;
    if (faultedOnly.value && !r.entry.faulted) return false;
    if (hideSuccessful.value && isSuccessful(r.entry)) return false;
    return true;
  });
});

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
  // Refresh is a no-op on the streaming model — new deltas arrive
  // via SSE automatically. We keep the button for muscle memory;
  // a future iteration could expose a reconnect() that re-opens
  // the EventSource if the operator wants to force a re-backfill.
}

// Follow-latest: auto-scroll to the newest row as deltas land —
// gated by the page-level focus bar's Live state. When the operator
// drags the brush back (range !== null), we stop yanking the scroll
// out from under their inspection.
watch(
  () => sortedRows.value.length,
  () => {
    if (!liveChecked.value) return;
    nextTick(() => {
      const el = rowsScrollRef.value;
      if (!el) return;
      // Desc sort (default) keeps newest at top; asc keeps at bottom.
      el.scrollTop = sortDir.value === 'asc' ? el.scrollHeight : 0;
    });
  },
);

// Position the highlighted row inside the .rows scroll container
// without touching the OUTER page scroll. scrollIntoView() would
// walk up every scroll ancestor (panel → CollapsibleSection → page)
// and yank the page to the network log — annoying when the operator
// is mid-scroll elsewhere. Instead, set scrollTop directly so only
// the inner container moves; if the row is already inside the
// visible band, do nothing (scrollIntoView's `block: 'nearest'`
// equivalent without the side effect).
watch(
  () => coord.state.cursorMs,
  () => {
    if (currentRowKeys.value.size === 0) return;
    nextTick(() => {
      const el = rowsScrollRef.value;
      if (!el) return;
      const target = el.querySelector('.row.cursor-current') as HTMLElement | null;
      if (!target) return;
      const containerTop = el.scrollTop;
      const containerBottom = containerTop + el.clientHeight;
      const rowTop = target.offsetTop;
      const rowBottom = rowTop + target.offsetHeight;
      if (rowTop < containerTop) {
        // Above the viewport — scroll up so the row sits at the top.
        el.scrollTop = rowTop;
      } else if (rowBottom > containerBottom) {
        // Below — scroll down just enough to land the row at bottom.
        el.scrollTop = rowBottom - el.clientHeight;
      }
      // Otherwise it's already visible; leave scrollTop alone.
    });
  },
);

/* ─── Wheel hijack ─────────────────────────────────────────────────
 *
 * Plain mouse-wheel inside the network-log row container should scroll
 * the PAGE, not the inner overflow box. Trapping wheel events on a
 * long inner-scroll region is the legacy session-shell.js parity
 * behaviour — operators expect the page to keep scrolling when they
 * roll past the rail, and only opt into row scrolling explicitly.
 *
 *   - plain wheel  → preventDefault + window.scrollBy (page moves)
 *   - Alt+wheel    → native overflow scroll inside the rows container
 *
 * Capture-phase listener so we run before the browser's default
 * overflow handler, with `passive: false` so preventDefault is allowed.
 */
// `.rows` is only rendered when `sortedRows.length > 0`, so on first
// mount the ref is null. Watch the ref and (re)attach on every
// appearance — covers the empty→populated transition AND any future
// teardown if the table is fully filtered out and then refills.
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
  if (e.altKey) return; // operator opted in — let the rows scroll
  e.preventDefault();
  window.scrollBy({ top: e.deltaY, left: e.deltaX, behavior: 'auto' });
}
</script>

<template>
  <div class="net-log">
    <div class="toolbar">
      <button class="btn" type="button" @click="refresh">Refresh</button>
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
      <button
        type="button"
        class="btn live-toggle"
        :class="{ checked: liveChecked }"
        @click="onLiveToggleClick"
        :title="liveChecked
          ? 'Pause auto-scroll at the current row. Drops follow-latest until you toggle back.'
          : 'Resume following live — drops any pinned brush window.'"
      >
        {{ liveChecked ? '●' : '○' }} Live
      </button>
    </div>

    <!-- In-panel brush retired in the timeseries migration —
         SessionDisplay's page-level focus bar is the single brush
         surface. NetworkLog now shows whatever the network stream
         delivers for the current play. -->


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
        <div class="cell c-labels">Labels</div>
        <div class="cell c-token">Token</div>
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
          :class="[rowFaultClass(r), { 'cursor-current': currentRowKeys.has(rowKey(r)) }]"
          :title="tooltipFor(r)"
        >
          <div class="cell c-time">{{ fmtTime(r.ts) }}</div>
          <div class="cell c-flags" :style="{ color: flagsFor(r).color }">{{ flagsFor(r).text }}</div>
          <div class="cell c-labels">
            <span
              v-for="l in r.labels"
              :key="l"
              class="nl-label-chip"
              :class="'label-' + labelSeverity(l)"
              :title="l"
            >{{ labelName(l) }}</span>
            <span v-if="!r.labels.length" class="dash">—</span>
          </div>
          <div class="cell c-token" :title="r.token">
            <span v-if="r.token" class="nl-token">{{ r.token }}</span>
            <span v-else class="dash">—</span>
          </div>
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

/* Live toggle — same scheme as BitrateChartPanelToolbar /
 * MetricsLineChart / EventsTimeline so all the panel toggles in
 * the session card match visually. margin-left: auto pushes the
 * button to the right edge, same screen position as the chart
 * panels' Live buttons. */
.btn.live-toggle {
  margin-left: auto;
}
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
    var(--c-labels, minmax(120px, 1fr))
    var(--c-token, minmax(110px, 0.8fr))
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
  /* position:relative makes this the offsetParent so the cursor
     auto-scroll's target.offsetTop is measured against THIS container. */
  position: relative;
  max-height: 480px;
  overflow-y: auto;
}

.row:hover { background: #f9fafb; }

/* "Selected event" cursor — synchronized with the chart cursor. A
 * row is highlighted when its [ts, ts+duration] interval contains
 * cursorMs. Visual treatment is intentionally loud: a 2px dashed
 * blue line top + bottom, saturated blue background tint, a 4px
 * left-edge gutter in solid blue, and z-index lift so the borders
 * aren't visually cropped by the next row's top-border. Multiple
 * rows can light up simultaneously (overlapping partial fetches),
 * which correctly conveys "everything in flight at that moment." */
.row.cursor-current {
  position: relative;
  background: rgba(29, 78, 216, 0.14);
  border-top: 2px dashed #1d4ed8;
  border-bottom: 2px dashed #1d4ed8;
  box-shadow: inset 4px 0 0 #1d4ed8;
  z-index: 2;
}
.row.cursor-current:hover { background: rgba(29, 78, 216, 0.20); }

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
.c-token { overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
.nl-token {
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  font-size: 10px;
  color: #3730a3;
  background: #eef2ff;
  border: 1px solid #e0e7ff;
  border-radius: 3px;
  padding: 0 4px;
}
.c-labels {
  display: flex; flex-wrap: wrap; gap: 3px;
  align-items: center; min-width: 0;
}
.nl-label-chip {
  display: inline-block;
  padding: 0 5px;
  border-radius: 8px;
  font: 600 10px system-ui;
  line-height: 1.5;
  border: 1px solid transparent;
  white-space: nowrap;
}
.nl-label-chip.label-info     { background: #f0fdf4; color: #1f2937; border-color: #a7f3d0; }
.nl-label-chip.label-warning  { background: #fef3c7; color: #854d0e; border-color: #fcd34d; }
.nl-label-chip.label-critical { background: #fee2e2; color: #7f1d1d; border-color: #fca5a5; }
.nl-label-chip.label-error    { background: #ffedd5; color: #7c2d12; border-color: #fdba74; }
.dash { color: #9ca3af; }
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
