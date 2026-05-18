<script setup lang="ts">
/**
 * Sessions.vue — Vue port of the legacy session picker
 * (content/shared/session-replay.js `startReplayPicker`). The data
 * source, derived health metrics, column set, badge styling, sort
 * + filter semantics, star bookmark, bundle download and auto-refresh
 * cadence all match the legacy implementation 1:1 so the v3 page
 * looks identical to /dashboard/sessions.html.
 */
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue';
import ShellLayout from '@/components/ShellLayout.vue';

interface SessionRow {
  session_id: string;
  play_id?: string;
  player_id?: string;
  group_id?: string;
  content_id?: string;
  started?: string;
  last_seen?: string;
  classification?: string;
  last_state?: string;
  last_player_error?: string;
  metric_events?: number | string;
  net_events?: number | string;
  stalls?: number;
  dropped_frames?: number;
  downshifts?: number;
  upshifts?: number;
  resolution_changes?: number;
  master_manifest_failures?: number;
  manifest_failures?: number;
  segment_failures?: number;
  all_failures?: number;
  transport_failures?: number;
  active_timeouts?: number;
  idle_timeouts?: number;
  user_marked_count?: number;
  frozen_count?: number;
  segment_stall_count?: number;
  restart_count?: number;
  error_event_count?: number;
  avg_quality_pct?: number;
  bitrate_shifts?: number;
  // derived in deriveHealth
  duration_ms?: number;
  errors_count?: number;
  faults_count?: number;
  downshifts_count?: number;
  upshifts_count?: number;
  issues_count?: number;
  health_score?: number;
  is_critical?: boolean;
  issues_breakdown?: Record<string, any>;
  health_breakdown?: Record<string, any>;
}

const RANGES = [
  { id: '15m',    label: 'Last 15 minutes', ms: 15 * 60 * 1000 },
  { id: '1h',     label: 'Last hour',       ms: 60 * 60 * 1000 },
  { id: '4h',     label: 'Last 4 hours',    ms: 4 * 60 * 60 * 1000 },
  { id: '24h',    label: 'Last 24 hours',   ms: 24 * 60 * 60 * 1000 },
  { id: '7d',     label: 'Last 7 days',     ms: 7 * 24 * 60 * 60 * 1000 },
  { id: '30d',    label: 'Last 30 days',    ms: 30 * 24 * 60 * 60 * 1000 },
  { id: 'all',    label: 'All time',        ms: 0 },
  { id: 'custom', label: 'Custom range…', ms: -1 },
] as const;
type RangeId = typeof RANGES[number]['id'];

const RANGE_KEY = 'ismSessionsRange';
const RANGE_CUSTOM_KEY = 'ismSessionsRangeCustom';

const activeRangeId = ref<RangeId>(readStoredRange());
const customFrom = ref(readStoredCustom().from);
const customTo = ref(readStoredCustom().to);

function readStoredRange(): RangeId {
  try {
    const v = localStorage.getItem(RANGE_KEY) as RangeId | null;
    if (v && RANGES.some((r) => r.id === v)) return v;
  } catch { /* ignore */ }
  return '24h';
}
function readStoredCustom(): { from: string; to: string } {
  try {
    const raw = localStorage.getItem(RANGE_CUSTOM_KEY);
    if (raw) {
      const v = JSON.parse(raw);
      return { from: v?.from ?? '', to: v?.to ?? '' };
    }
  } catch { /* ignore */ }
  return { from: '', to: '' };
}

const filters = ref<{
  player_id: string;
  group_id: string;
  content_id: string;
  play_id: string;
  classification: 'all' | 'starred' | 'interesting' | 'other';
}>({
  player_id: '', group_id: '', content_id: '', play_id: '',
  classification: 'all',
});

const rows = ref<SessionRow[]>([]);
const loading = ref(false);
const error = ref<string | null>(null);
const sortKey = ref('started');
const sortDir = ref<'asc' | 'desc'>('desc');
// Force-bump on auto-refresh so `fmtRelTime` re-renders even if the
// underlying row's `last_seen` string is unchanged but the wall clock
// moved.
const tick = ref(0);

const rangeLabel = computed(() => RANGES.find((r) => r.id === activeRangeId.value)?.label ?? '');

function computeRange(): { since: string; until: string } {
  if (activeRangeId.value === 'custom') {
    return { since: customFrom.value || '', until: customTo.value || '' };
  }
  const meta = RANGES.find((r) => r.id === activeRangeId.value);
  if (!meta || meta.ms === 0) return { since: '1970-01-01T00:00:00Z', until: '' };
  return { since: new Date(Date.now() - meta.ms).toISOString(), until: '' };
}

function deriveHealth(r: SessionRow): void {
  const n = (k: keyof SessionRow) => Number((r as any)[k]) || 0;
  const stalls = n('stalls');
  const drops = n('dropped_frames');
  const downshifts = n('downshifts');
  const upshifts = n('upshifts');
  const resChanges = n('resolution_changes');
  const errors = r.last_player_error && r.last_player_error.length > 0 ? 1 : 0;
  const faults = n('master_manifest_failures') + n('manifest_failures') + n('segment_failures')
    + n('all_failures') + n('transport_failures')
    + n('active_timeouts') + n('idle_timeouts');
  const dropBlocks = Math.ceil(drops / 100);

  r.errors_count = errors;
  r.faults_count = faults;
  r.downshifts_count = downshifts;
  r.upshifts_count = upshifts;

  r.user_marked_count = n('user_marked_count');
  r.frozen_count = n('frozen_count');
  r.segment_stall_count = n('segment_stall_count');
  r.restart_count = n('restart_count');
  r.error_event_count = n('error_event_count');
  r.is_critical = (r.user_marked_count > 0)
    || (r.frozen_count > 0)
    || (r.error_event_count > 0)
    || (errors > 0)
    || (n('master_manifest_failures') > 0)
    || (n('all_failures') > 0);

  r.issues_count = stalls + errors * 5 + faults + downshifts + dropBlocks;
  r.issues_breakdown = {
    stalls, errors, faults, downshifts, drops, dropBlocks,
    resolution_changes: resChanges,
    upshifts, player_error: r.last_player_error || '',
  };

  const deductStalls = stalls * 2;
  const deductErrors = errors * 25;
  const deductFaults = Math.min(faults * 1, 10);
  const deductDrops = Math.min(dropBlocks, 20);
  const deductShifts = downshifts * 1;
  r.health_score = Math.max(0, 100 - (deductStalls + deductErrors + deductFaults + deductDrops + deductShifts));
  r.health_breakdown = {
    stalls: deductStalls, errors: deductErrors,
    faults: deductFaults, drops: deductDrops, downshifts: deductShifts,
  };
}

let reloadInFlight = false;
async function loadRows(silent = false) {
  if (reloadInFlight) return;
  reloadInFlight = true;
  if (!silent) loading.value = true;
  error.value = null;
  try {
    const { since, until } = computeRange();
    const qs = new URLSearchParams();
    if (since) qs.set('since', since);
    if (until) qs.set('until', until);
    const resp = await fetch('/analytics/api/sessions' + (qs.toString() ? '?' + qs : ''));
    if (!resp.ok) throw new Error(`HTTP ${resp.status} (analytics forwarder reachable?)`);
    const body = await resp.text();
    const fresh: SessionRow[] = [];
    for (const line of body.split('\n')) {
      if (!line) continue;
      try { fresh.push(JSON.parse(line)); } catch { /* skip */ }
    }
    for (const r of fresh) {
      const t0 = Date.parse(r.started ?? '');
      const t1 = Date.parse(r.last_seen ?? '');
      r.duration_ms = (Number.isFinite(t0) && Number.isFinite(t1) && t1 >= t0) ? (t1 - t0) : 0;
      deriveHealth(r);
    }
    rows.value = fresh;
  } catch (e: any) {
    error.value = String(e?.message ?? e);
  } finally {
    if (!silent) loading.value = false;
    reloadInFlight = false;
  }
}

const isInterestingRow = (r: SessionRow): boolean =>
  Number(r.user_marked_count) > 0
  || Number(r.frozen_count) > 0
  || Number(r.error_event_count) > 0
  || Number(r.segment_stall_count) > 0
  || Number(r.restart_count) > 0;

function matchesClassification(r: SessionRow): boolean {
  switch (filters.value.classification) {
    case 'all': return true;
    case 'starred': return String(r.classification || '') === 'favourite';
    case 'interesting': return isInterestingRow(r) || String(r.classification || '') === 'favourite';
    case 'other': return !isInterestingRow(r) && String(r.classification || '') !== 'favourite';
  }
}

function matches(r: SessionRow): boolean {
  const f = filters.value;
  return (!f.player_id || r.player_id === f.player_id)
    && (!f.group_id || r.group_id === f.group_id)
    && (!f.content_id || r.content_id === f.content_id)
    && (!f.play_id || r.play_id === f.play_id)
    && matchesClassification(r);
}

const filtered = computed(() => rows.value.filter(matches));

const sorted = computed(() => {
  const list = filtered.value.slice();
  const key = sortKey.value;
  const dir = sortDir.value === 'asc' ? 1 : -1;
  const isNumberCol = COLUMNS.find((c) => c.key === key)?.type === 'number';
  list.sort((a, b) => {
    const av = (a as any)[key];
    const bv = (b as any)[key];
    if (isNumberCol) {
      return ((Number(av) || 0) - (Number(bv) || 0)) * dir;
    }
    const as = String(av || '');
    const bs = String(bv || '');
    if (as < bs) return -1 * dir;
    if (as > bs) return 1 * dir;
    return 0;
  });
  return list;
});

// Cascading distinct-values for the four selects: each select's
// option set is filtered by the SELECTIONS to its left.
function distinctFor(key: 'player_id' | 'group_id' | 'content_id' | 'play_id'): string[] {
  const f = filters.value;
  const pool = rows.value.filter((r) => {
    if (key === 'player_id') return true;
    if (!f.player_id || r.player_id === f.player_id) {
      if (key === 'group_id') return true;
      if (!f.group_id || r.group_id === f.group_id) {
        if (key === 'content_id') return true;
        return !f.content_id || r.content_id === f.content_id;
      }
    }
    return false;
  });
  return Array.from(new Set(pool.map((r) => (r as any)[key] || '').filter(Boolean))).sort();
}
const playerOptions = computed(() => distinctFor('player_id'));
const groupOptions = computed(() => distinctFor('group_id'));
const contentOptions = computed(() => distinctFor('content_id'));
const playOptions = computed(() => distinctFor('play_id'));

// When a parent filter narrows enough that the current value is no
// longer in the visible set, clear it. Mirrors `fillSelect` in the
// legacy code.
watch(playerOptions, (opts) => { if (filters.value.player_id && !opts.includes(filters.value.player_id)) filters.value.player_id = ''; });
watch(groupOptions, (opts) => { if (filters.value.group_id && !opts.includes(filters.value.group_id)) filters.value.group_id = ''; });
watch(contentOptions, (opts) => { if (filters.value.content_id && !opts.includes(filters.value.content_id)) filters.value.content_id = ''; });
watch(playOptions, (opts) => { if (filters.value.play_id && !opts.includes(filters.value.play_id)) filters.value.play_id = ''; });

function clearFilters() {
  filters.value.player_id = '';
  filters.value.group_id = '';
  filters.value.content_id = '';
  filters.value.play_id = '';
  filters.value.classification = 'all';
}

function onRangeChange() {
  try { localStorage.setItem(RANGE_KEY, activeRangeId.value); } catch { /* ignore */ }
  if (activeRangeId.value !== 'custom') void loadRows();
}
function applyCustomRange() {
  customFrom.value = localToIso(customFromInput.value);
  customTo.value = localToIso(customToInput.value);
  try {
    localStorage.setItem(RANGE_CUSTOM_KEY, JSON.stringify({ from: customFrom.value, to: customTo.value }));
  } catch { /* ignore */ }
  void loadRows();
}

const customFromInput = ref(isoToLocal(customFrom.value));
const customToInput = ref(isoToLocal(customTo.value));

function localToIso(v: string): string {
  if (!v) return '';
  const d = new Date(v);
  return Number.isFinite(d.getTime()) ? d.toISOString() : '';
}
function isoToLocal(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A play is "still live" — i.e. the viewer should follow the live
 *  edge rather than pin to a fixed end_time — if its last_seen is
 *  within LIVE_TAIL_MS of now AND no terminal-state marker is set.
 *  Threshold is generous (60 s) so a heartbeat that's a few seconds
 *  late doesn't kick us into archive mode. */
const LIVE_TAIL_MS = 60_000;
function endTimeFor(r: SessionRow): string {
  const lastSeen = Date.parse(r.last_seen ?? '');
  if (!Number.isFinite(lastSeen)) return 'live';
  if (Date.now() - lastSeen < LIVE_TAIL_MS) return 'live';
  return new Date(lastSeen).toISOString();
}

function viewerHref(r: SessionRow): string {
  if (!r.player_id) return '#';
  const qs = new URLSearchParams({ player_id: r.player_id });
  if (r.play_id && r.play_id !== '—') qs.set('play_id', r.play_id);
  // Pass the play's time bounds so the viewer can scope its initial
  // brush + SSE backfill to this play's range instead of inferring
  // it from samples landing. end_time=live means "follow live edge"
  // and is set when the play looks still-active.
  if (r.started) {
    const startMs = Date.parse(r.started);
    if (Number.isFinite(startMs)) {
      qs.set('start_time', new Date(startMs).toISOString());
    }
  }
  qs.set('end_time', endTimeFor(r));
  return '/dashboard/v3/session-viewer.html?' + qs.toString();
}
function bundleHref(r: SessionRow): string {
  if (!r.player_id) return '#';
  const qs = new URLSearchParams({ player_id: r.player_id });
  if (r.play_id && r.play_id !== '—') qs.set('play_id', r.play_id);
  return '/analytics/api/session_bundle?' + qs.toString();
}

function fmtDur(ms?: number): string {
  if (!ms) return '—';
  const s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h) return `${h}h ${m}m ${sec}s`;
  if (m) return `${m}m ${sec}s`;
  return `${sec}s`;
}
function fmtStarted(iso?: string): string {
  if (!iso) return '—';
  const t = Date.parse(iso.includes('T') ? iso : iso.replace(' ', 'T') + 'Z');
  if (!Number.isFinite(t)) return iso;
  const d = new Date(t);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} `
    + `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${String(d.getMilliseconds()).padStart(3, '0')}`;
}
interface RelTime { label: string; tip: string; recent: boolean; medium: boolean }
function fmtRelTime(iso?: string): RelTime {
  // Touch tick so this re-evaluates on auto-refresh.
  void tick.value;
  if (!iso) return { label: '—', tip: '', recent: false, medium: false };
  const t = Date.parse(iso.includes('T') ? iso : iso.replace(' ', 'T') + 'Z');
  if (!Number.isFinite(t)) return { label: '—', tip: '', recent: false, medium: false };
  const ageSec = Math.max(0, (Date.now() - t) / 1000);
  let label: string;
  if (ageSec < 60) label = `${Math.round(ageSec)}s ago`;
  else if (ageSec < 3600) label = `${Math.round(ageSec / 60)}m ago`;
  else if (ageSec < 86400) label = `${Math.round(ageSec / 3600)}h ago`;
  else label = `${Math.round(ageSec / 86400)}d ago`;
  return { label, tip: iso, recent: ageSec < 30, medium: ageSec >= 30 && ageSec < 300 };
}
interface IssueBadge { count: number; cls: 'ok' | 'warn' | 'bad'; tip: string }
function fmtIssuesBadge(r: SessionRow): IssueBadge {
  const c = Number(r.issues_count) || 0;
  const cls: IssueBadge['cls'] = c === 0 ? 'ok' : c <= 4 ? 'warn' : 'bad';
  const b = r.issues_breakdown || {};
  const parts: string[] = [];
  if (b.stalls) parts.push(`${b.stalls} stall${b.stalls === 1 ? '' : 's'}`);
  if (b.errors) parts.push(`player error: ${b.player_error || 'yes'}`);
  if (b.faults) parts.push(`${b.faults} injected fault${b.faults === 1 ? '' : 's'}`);
  if (b.downshifts) parts.push(`${b.downshifts} ABR downshift${b.downshifts === 1 ? '' : 's'}`);
  if (b.drops) parts.push(`${b.drops} dropped frames (~${b.dropBlocks} blocks)`);
  if (b.resolution_changes) parts.push(`${b.resolution_changes} resolution changes`);
  return { count: c, cls, tip: parts.join(' · ') || 'no noteworthy events' };
}
interface HealthBadge { score: number; cls: 'ok' | 'warn' | 'bad'; tip: string }
function fmtHealthBadge(r: SessionRow): HealthBadge {
  const s = Number(r.health_score) || 0;
  const cls: HealthBadge['cls'] = s >= 90 ? 'ok' : s >= 70 ? 'warn' : 'bad';
  const b = r.health_breakdown || {};
  const tip = `100 − stalls:${b.stalls || 0} − errors:${b.errors || 0} − faults:${b.faults || 0} − drops:${b.drops || 0} − downshifts:${b.downshifts || 0}`;
  return { score: s, cls, tip };
}
interface CountCell { n: number; color: string; bold: boolean }
function fmtCount(v: number | undefined, warn: number, bad: number): CountCell {
  const n = Number(v) || 0;
  let color = '#065f46';
  if (n >= bad) color = '#991b1b';
  else if (n >= warn) color = '#92400e';
  else if (n === 0) color = '#9ca3af';
  return { n, color, bold: n !== 0 };
}
interface PctCell { label: string; color: string }
function fmtPct(v?: number): PctCell {
  const n = Number(v);
  if (!Number.isFinite(n) || n === 0) return { label: '—', color: '#9ca3af' };
  let color = '#065f46';
  if (n < 60) color = '#991b1b';
  else if (n < 85) color = '#92400e';
  return { label: `${n.toFixed(1)}%`, color };
}

const FLAG_DEFS = [
  { key: 'user_marked_count' as const,   icon: '🚨', label: '911 / user flag',  color: '#dc2626' },
  { key: 'frozen_count' as const,        icon: '❄️',  label: 'frozen',           color: '#7c3aed' },
  { key: 'error_event_count' as const,   icon: '⛔',        label: 'error event',      color: '#b91c1c' },
  { key: 'segment_stall_count' as const, icon: '⏸',         label: 'segment stall',    color: '#c2410c' },
  { key: 'restart_count' as const,       icon: '🔄', label: 'restart',          color: '#b45309' },
];
function flagChips(r: SessionRow): { icon: string; label: string; tip: string; color: string; count: number }[] {
  const out: { icon: string; label: string; tip: string; color: string; count: number }[] = [];
  for (const f of FLAG_DEFS) {
    const c = Number((r as any)[f.key]) || 0;
    if (c <= 0) continue;
    out.push({ icon: f.icon, label: f.label, color: f.color, count: c, tip: `${c} ${f.label}${c === 1 ? '' : 's'}` });
  }
  return out;
}

// Column metadata drives header rendering + sort.
const COLUMNS = [
  { key: '__star',           label: '',           type: 'string' as const,  sortable: false },
  { key: 'started',          label: 'Started',    type: 'string' as const,  sortable: true },
  { key: 'last_seen',        label: 'Last updated', type: 'string' as const, sortable: true },
  { key: 'duration_ms',      label: 'Duration',   type: 'number' as const,  sortable: true },
  { key: 'player_id',        label: 'Player',     type: 'string' as const,  sortable: true },
  { key: 'content_id',       label: 'Content',    type: 'string' as const,  sortable: true },
  { key: 'play_id',          label: 'Play ID',    type: 'string' as const,  sortable: true },
  { key: 'last_state',       label: 'State',      type: 'string' as const,  sortable: true },
  { key: 'issues_count',     label: 'Issues',     type: 'number' as const,  sortable: true },
  { key: '__flags',          label: 'Flags',      type: 'string' as const,  sortable: false },
  { key: 'health_score',     label: 'Health',     type: 'number' as const,  sortable: true },
  { key: 'stalls',           label: 'Stalls',     type: 'number' as const,  sortable: true },
  { key: 'errors_count',     label: 'Errors',     type: 'number' as const,  sortable: true },
  { key: 'faults_count',     label: 'Faults',     type: 'number' as const,  sortable: true },
  { key: 'downshifts_count', label: 'Downshifts', type: 'number' as const,  sortable: true },
  { key: 'dropped_frames',   label: 'Drops',      type: 'number' as const,  sortable: true },
  { key: 'avg_quality_pct',  label: 'Avg Q%',     type: 'number' as const,  sortable: true },
  { key: 'metric_events',    label: 'Metrics',    type: 'number' as const,  sortable: true },
  { key: 'net_events',       label: 'HAR',        type: 'number' as const,  sortable: true },
  { key: '__bundle',         label: '',           type: 'string' as const,  sortable: false },
];

function onHeaderClick(col: typeof COLUMNS[number]) {
  if (!col.sortable) return;
  if (sortKey.value === col.key) {
    sortDir.value = sortDir.value === 'asc' ? 'desc' : 'asc';
  } else {
    sortKey.value = col.key;
    sortDir.value = col.type === 'number' ? 'desc' : 'asc';
  }
}

async function toggleStar(r: SessionRow, ev: MouseEvent) {
  ev.stopPropagation();
  ev.preventDefault();
  const wasStarred = String(r.classification || '') === 'favourite';
  const pid = (r.play_id && r.play_id !== '—') ? r.play_id : '—';
  const url = `/analytics/api/sessions/${encodeURIComponent(r.session_id)}/${encodeURIComponent(pid)}/star`;
  const method = wasStarred ? 'DELETE' : 'POST';
  // Optimistic flip.
  r.classification = wasStarred ? '' : 'favourite';
  try {
    const resp = await fetch(url, { method });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
  } catch (err: any) {
    r.classification = wasStarred ? 'favourite' : '';
    window.alert(`Star toggle failed: ${err?.message ?? err}`);
  }
}

function onRowClick(r: SessionRow, ev: MouseEvent) {
  const tgt = ev.target as HTMLElement | null;
  // Star, bundle and explicit anchors handle themselves.
  if (tgt?.closest('[data-star-cell]')) return;
  if (tgt?.closest('[data-bundle-link]')) return;
  if (tgt?.closest('a:not([data-row-fallback])')) return;
  if (ev.metaKey || ev.ctrlKey || ev.shiftKey) return;
  const href = viewerHref(r);
  if (href !== '#') window.location.href = href;
}

let autoRefreshTimer: number | undefined;
let clockTimer: number | undefined;
onMounted(() => {
  void loadRows();
  // Auto-refresh every 5s (silent).
  autoRefreshTimer = window.setInterval(() => void loadRows(true), 5000);
  // 1s clock tick so "1s ago" → "2s ago" updates without a fetch.
  clockTimer = window.setInterval(() => { tick.value++; }, 1000);
});
onBeforeUnmount(() => {
  if (autoRefreshTimer) clearInterval(autoRefreshTimer);
  if (clockTimer) clearInterval(clockTimer);
});

const heading = computed(() =>
  `${rows.value.length} playback episodes archived (${rangeLabel.value.toLowerCase()}). Filter then pick one to replay.`,
);
const matchCount = computed(() => `${filtered.value.length} matching`);
const showCustomInputs = computed(() => activeRangeId.value === 'custom');
</script>

<template>
  <ShellLayout active-page="sessions">
    <template #header>
      <div class="page-title-bar">Sessions</div>
    </template>

    <main class="ism-content-wide">
      <div class="page-header">
        <div class="page-title">Sessions</div>
        <div class="page-subtitle">Browse archived streaming sessions. Click one to open it in the Session Viewer.</div>
      </div>

      <div class="panel">
        <div class="panel-header">
          <div class="panel-title">Active Sessions <span v-if="loading" class="status-message">loading…</span></div>
        </div>

        <div class="picker-wrap">
          <div class="picker-heading">{{ heading }}</div>

          <div class="range-row">
            <label class="ctrl-label">
              <span>Time range:</span>
              <select v-model="activeRangeId" @change="onRangeChange" class="ctrl-input">
                <option v-for="r in RANGES" :key="r.id" :value="r.id">{{ r.label }}</option>
              </select>
            </label>
            <span v-if="showCustomInputs" class="custom-range">
              <span>from</span>
              <input type="datetime-local" v-model="customFromInput" class="ctrl-input" />
              <span>to</span>
              <input type="datetime-local" v-model="customToInput" class="ctrl-input" />
              <button type="button" class="btn btn-secondary" @click="applyCustomRange">Apply</button>
            </span>
            <span v-if="error" class="error">{{ error }}</span>
          </div>

          <div class="filter-row">
            <label class="ctrl-label">
              <span>Player:</span>
              <select v-model="filters.player_id" class="ctrl-input">
                <option value="">all ({{ playerOptions.length }})</option>
                <option v-for="v in playerOptions" :key="v" :value="v">{{ v }}</option>
              </select>
            </label>
            <label class="ctrl-label">
              <span>Group:</span>
              <select v-model="filters.group_id" class="ctrl-input">
                <option value="">all ({{ groupOptions.length }})</option>
                <option v-for="v in groupOptions" :key="v" :value="v">{{ v }}</option>
              </select>
            </label>
            <label class="ctrl-label">
              <span>Content:</span>
              <select v-model="filters.content_id" class="ctrl-input">
                <option value="">all ({{ contentOptions.length }})</option>
                <option v-for="v in contentOptions" :key="v" :value="v">{{ v }}</option>
              </select>
            </label>
            <label class="ctrl-label">
              <span>Play:</span>
              <select v-model="filters.play_id" class="ctrl-input">
                <option value="">all ({{ playOptions.length }})</option>
                <option v-for="v in playOptions" :key="v" :value="v">{{ v }}</option>
              </select>
            </label>

            <div class="class-chip-wrap">
              <span class="ctrl-label-text">Show:</span>
              <button
                v-for="c in ([
                  { value: 'all',         label: 'All' },
                  { value: 'starred',     label: '★ Starred' },
                  { value: 'interesting', label: 'Interesting' },
                  { value: 'other',       label: 'Other' }
                ] as const)"
                :key="c.value"
                type="button"
                class="class-chip"
                :class="{ active: filters.classification === c.value }"
                @click="filters.classification = c.value"
              >{{ c.label }}</button>
            </div>

            <button type="button" class="btn btn-secondary" @click="clearFilters">Clear filters</button>
            <span class="match-count">{{ matchCount }}</span>
          </div>

          <div class="table-wrap">
            <table class="picker-table">
              <thead>
                <tr>
                  <th
                    v-for="col in COLUMNS"
                    :key="col.key"
                    :class="{ sortable: col.sortable }"
                    :title="col.sortable ? `Sort by ${col.label}` : ''"
                    @click="onHeaderClick(col)"
                  >
                    {{ col.label }}<template v-if="col.sortable">
                      <span v-if="sortKey === col.key">{{ sortDir === 'asc' ? ' ▲' : ' ▼' }}</span>
                      <span v-else class="sort-idle"> ⇅</span>
                    </template>
                  </th>
                </tr>
              </thead>
              <tbody>
                <tr v-if="sorted.length === 0">
                  <td :colspan="COLUMNS.length" class="empty">No sessions match the current filters.</td>
                </tr>
                <tr
                  v-for="r in sorted"
                  :key="r.session_id + ':' + (r.play_id ?? '')"
                  :class="{ 'row-critical': r.is_critical }"
                  @click="onRowClick(r, $event)"
                >
                  <td class="cell-star">
                    <span
                      data-star-cell
                      class="star"
                      :class="{ starred: r.classification === 'favourite' }"
                      :title="r.classification === 'favourite'
                        ? 'Starred — kept forever (click to unstar)'
                        : 'Click to star — kept forever and exempt from TTL'"
                      @click="toggleStar(r, $event)"
                    >{{ r.classification === 'favourite' ? '★' : '☆' }}</span>
                  </td>
                  <td>{{ fmtStarted(r.started) }}</td>
                  <td>
                    <span
                      :class="{ 'rel-recent': fmtRelTime(r.last_seen).recent, 'rel-medium': fmtRelTime(r.last_seen).medium, 'rel-old': !fmtRelTime(r.last_seen).recent && !fmtRelTime(r.last_seen).medium }"
                      :title="fmtRelTime(r.last_seen).tip"
                    >
                      <span v-if="fmtRelTime(r.last_seen).recent" class="rel-dot"></span>
                      {{ fmtRelTime(r.last_seen).label }}
                    </span>
                  </td>
                  <td>{{ fmtDur(r.duration_ms) }}</td>
                  <td>{{ r.player_id || '' }}</td>
                  <td>{{ r.content_id || '' }}</td>
                  <td class="cell-play-id">
                    <a v-if="r.play_id && r.player_id" :href="viewerHref(r)" class="play-id-link">{{ r.play_id }}</a>
                    <template v-else>{{ r.play_id || '' }}</template>
                  </td>
                  <td>{{ r.last_state || '' }}</td>
                  <td>
                    <span class="issue-badge" :class="'issue-' + fmtIssuesBadge(r).cls" :title="fmtIssuesBadge(r).tip">{{ fmtIssuesBadge(r).count }}</span>
                  </td>
                  <td>
                    <span v-for="(f, i) in flagChips(r)" :key="i" class="flag-chip" :style="{ background: f.color }" :title="f.tip">{{ f.icon }} {{ f.count }}</span>
                    <span v-if="flagChips(r).length === 0" class="dash">—</span>
                  </td>
                  <td>
                    <span class="health-badge" :class="'issue-' + fmtHealthBadge(r).cls" :title="fmtHealthBadge(r).tip">{{ fmtHealthBadge(r).score }}</span>
                  </td>
                  <td><span :style="{ color: fmtCount(r.stalls, 1, 5).color, fontWeight: fmtCount(r.stalls, 1, 5).bold ? 600 : 400 }">{{ fmtCount(r.stalls, 1, 5).n }}</span></td>
                  <td><span :style="{ color: fmtCount(r.errors_count, 1, 1).color, fontWeight: fmtCount(r.errors_count, 1, 1).bold ? 600 : 400 }">{{ fmtCount(r.errors_count, 1, 1).n }}</span></td>
                  <td><span :style="{ color: fmtCount(r.faults_count, 1, 10).color, fontWeight: fmtCount(r.faults_count, 1, 10).bold ? 600 : 400 }">{{ fmtCount(r.faults_count, 1, 10).n }}</span></td>
                  <td><span :style="{ color: fmtCount(r.downshifts_count, 1, 5).color, fontWeight: fmtCount(r.downshifts_count, 1, 5).bold ? 600 : 400 }">{{ fmtCount(r.downshifts_count, 1, 5).n }}</span></td>
                  <td><span :style="{ color: fmtCount(r.dropped_frames, 100, 1000).color, fontWeight: fmtCount(r.dropped_frames, 100, 1000).bold ? 600 : 400 }">{{ fmtCount(r.dropped_frames, 100, 1000).n }}</span></td>
                  <td><span :style="{ color: fmtPct(r.avg_quality_pct).color }">{{ fmtPct(r.avg_quality_pct).label }}</span></td>
                  <td>{{ r.metric_events || 0 }}</td>
                  <td>{{ r.net_events || 0 }}</td>
                  <td>
                    <a
                      v-if="r.player_id"
                      data-bundle-link
                      :href="bundleHref(r)"
                      download
                      title="Download session bundle (.zip)"
                      class="bundle-btn"
                    >📥</a>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </main>
  </ShellLayout>
</template>

<style scoped>
.page-title-bar { font-size: 16px; font-weight: 600; }
.ism-content-wide { width: 100%; padding: 16px 24px; box-sizing: border-box; }
.page-header { margin-bottom: 20px; }
.page-title { font-size: 32px; font-weight: 400; margin-bottom: 8px; color: var(--text-primary); }
.page-subtitle { font-size: 14px; color: var(--text-secondary); }

.panel {
  background: var(--bg-primary);
  border-radius: 12px;
  box-shadow: var(--shadow-md);
  padding: 16px;
  margin-bottom: 18px;
}
.panel-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 12px;
  gap: 12px;
  flex-wrap: wrap;
}
.panel-title { font-size: 16px; font-weight: 600; color: var(--text-primary); }
.status-message { font-size: 12px; color: var(--text-secondary); font-weight: 400; margin-left: 8px; }

.picker-wrap { display: flex; flex-direction: column; gap: 10px; }
.picker-heading { font-size: 13px; color: var(--text-secondary); }

.range-row, .filter-row {
  display: flex; flex-wrap: wrap; gap: 10px; align-items: center;
}
.ctrl-label {
  display: flex; align-items: center; gap: 6px;
  font-size: 12px; color: var(--text-secondary); font-weight: 500;
}
.ctrl-label-text {
  font-size: 12px; color: var(--text-secondary); font-weight: 500;
}
.ctrl-input {
  padding: 4px 8px;
  font: 13px system-ui;
  background: var(--bg-primary);
  color: var(--text-primary);
  border: 1px solid var(--border-color, #d1d5db);
  border-radius: 4px;
}
.custom-range { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: var(--text-secondary); }
.error { color: #b91c1c; font-size: 12px; }
.match-count { margin-left: auto; font-size: 12px; color: var(--text-secondary); }

.class-chip-wrap { display: flex; align-items: center; gap: 6px; }
.class-chip {
  padding: 4px 10px;
  border-radius: 14px;
  border: 1px solid var(--border-color, #d1d5db);
  background: var(--bg-secondary, #f3f4f6);
  color: var(--text-primary, #111827);
  font: 500 12px system-ui;
  cursor: pointer;
}
.class-chip.active {
  background: #1d4ed8;
  color: #fff;
  font-weight: 700;
  border-color: #1d4ed8;
}

.btn {
  display: inline-block;
  padding: 4px 10px;
  font-size: 12px;
  border: 1px solid #d1d5db;
  background: #fff;
  border-radius: 4px;
  text-decoration: none;
  color: #1f2937;
  cursor: pointer;
}
.btn:hover { background: #f3f4f6; }
.btn-secondary { background: var(--bg-secondary, #f9fafb); }

.table-wrap {
  max-height: 60vh;
  overflow: auto;
  background: var(--bg-primary);
  border: 1px solid var(--border-color, #e5e7eb);
  border-radius: 6px;
}
.picker-table {
  width: 100%;
  border-collapse: collapse;
  font: 12px ui-monospace, Menlo, monospace;
}
.picker-table thead tr {
  background: var(--bg-secondary, #f5f5f5);
  position: sticky;
  top: 0;
  z-index: 3;
}
.picker-table th {
  padding: 8px 10px;
  text-align: left;
  border-bottom: 1px solid var(--border-color, #e5e7eb);
  font-weight: 600;
  color: var(--text-primary);
  white-space: nowrap;
  user-select: none;
}
.picker-table th.sortable { cursor: pointer; }
.sort-idle { color: #9ca3af; }
.picker-table td {
  padding: 5px 8px;
  border-bottom: 1px solid var(--border-color, #f3f4f6);
  position: relative;
}
.picker-table tbody tr {
  cursor: pointer;
  border-left: 4px solid transparent;
}
.picker-table tbody tr:hover { background: var(--bg-hover, #f9fafb); }
.picker-table tbody tr.row-critical { border-left: 4px solid #dc2626; }
.picker-table tbody tr.row-critical:hover { background: #fef2f2; }
.empty {
  padding: 16px;
  text-align: center;
  color: var(--text-secondary);
}

.cell-star { width: 28px; padding-left: 6px; padding-right: 0; }
.star {
  cursor: pointer;
  font-size: 18px;
  line-height: 1;
  color: #9ca3af;
  display: inline-block;
  padding: 0 6px;
  user-select: none;
}
.star.starred { color: #f59e0b; }

.cell-play-id { font-weight: 600; color: #1d4ed8; }
.play-id-link { color: #1d4ed8; text-decoration: none; font-weight: 600; }
.play-id-link:hover { text-decoration: underline; }

.issue-badge, .health-badge {
  display: inline-block;
  min-width: 28px;
  text-align: center;
  padding: 2px 8px;
  border-radius: 10px;
  font-weight: 700;
  font-family: system-ui;
}
.health-badge { border-radius: 4px; min-width: 36px; }
.issue-ok { background: #d1fae5; color: #065f46; }
.issue-warn { background: #fef3c7; color: #92400e; }
.issue-bad { background: #fee2e2; color: #991b1b; }

.flag-chip {
  display: inline-block;
  padding: 1px 6px;
  margin: 0 2px 0 0;
  border-radius: 10px;
  color: #fff;
  font: 600 11px system-ui;
  line-height: 1.4;
}
.dash { color: #9ca3af; }

.rel-recent { color: #16a34a; font-weight: 600; display: inline-flex; align-items: center; gap: 6px; }
.rel-medium { color: var(--text-primary); }
.rel-old { color: var(--text-secondary); }
.rel-dot {
  display: inline-block; width: 6px; height: 6px;
  border-radius: 50%; background: #16a34a;
  animation: rel-pulse 1.2s ease-in-out infinite;
}
@keyframes rel-pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.35; } }

.bundle-btn {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 4px;
  background: var(--bg-secondary, #f3f4f6);
  color: var(--text-primary, #111827);
  text-decoration: none;
  font-size: 13px;
  line-height: 1.2;
}
.bundle-btn:hover { background: var(--bg-hover, #e5e7eb); }
</style>
