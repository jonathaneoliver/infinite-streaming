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
import { useQuery, useMutation, useQueryClient } from '@tanstack/vue-query';
import ShellLayout from '@/components/ShellLayout.vue';
import ChatPanel from '@/components/chat/ChatPanel.vue';
import { sessionViewerURL } from '@/composables/urlTimeFormat';
import { listPlays, patchPlayClassification, type PlaySummary } from '@/repo/v2-repo';

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
  // Issue #474: per-play label histogram from session_events +
  // network_requests + control_events. `labels_total` is the count of
  // labelled rows; `labels_distinct_count` is how many distinct label
  // strings appeared; `labels` is the [label, count] tuples for the
  // hover tooltip.
  labels_total?: number;
  labels_distinct_count?: number;
  labels?: [string, number][];
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
  // Test-harness origin filter. 'all' = no constraint, 'only' = keep
  // plays stamped by the characterization framework (have an
  // info=run_id_… label), 'hide' = exclude them so manual sessions
  // surface without test-noise. See Characterization.vue for how
  // the framework stamps these labels at the start of each run.
  harness: 'all' | 'only' | 'hide';
  // Tristate label filter:
  //   - labels:        AND-required INCLUDES (row.labels must contain every entry)
  //   - labelsExclude: AND-required EXCLUDES (row.labels must contain NONE of these)
  // Empty arrays = no constraint on that side. Combine for queries
  // like "has http_404 AND has-not fault_rule_enabled".
  labels: string[];
  labelsExclude: string[];
}>({
  player_id: '', group_id: '', content_id: '', play_id: '',
  classification: 'all',
  harness: 'all',
  labels: [], labelsExclude: [],
});

// Test-harness label extraction. The characterization framework
// stamps each play it runs with:
//   info=run_id_<utc-compact>   e.g. info=run_id_20260524T070148Z
//   info=platform_<name>        e.g. info=platform_ipad-sim
//   info=test_<name>            e.g. info=test_rampup
// These three label PREFIXES (with their value tail) are how
// Characterization.vue groups plays into runs; this page lifts the
// same convention to surface "this play came from the harness" on
// the picker.
function findLabelTail(r: SessionRow, prefix: string): string | null {
  const pairs = Array.isArray(r.labels) ? r.labels : [];
  for (const [label] of pairs) {
    const s = String(label);
    if (s.startsWith(prefix)) return s.slice(prefix.length);
  }
  return null;
}
function harnessRunId(r: SessionRow): string | null { return findLabelTail(r, 'info=run_id_'); }
function harnessPlatform(r: SessionRow): string | null { return findLabelTail(r, 'info=platform_'); }
function harnessTest(r: SessionRow): string | null { return findLabelTail(r, 'info=test_'); }
function isHarnessRow(r: SessionRow): boolean { return harnessRunId(r) !== null; }

// Pretty-print the UTC compact run_id (20260524T070148Z) as local
// hh:mm. Falls back to the raw string if it doesn't match the
// expected shape (older runs, manual stamps, etc.).
function shortRunId(runID: string | null): string {
  if (!runID) return '';
  const m = runID.match(/^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/);
  if (!m) return runID;
  const utc = new Date(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`);
  if (!Number.isFinite(utc.getTime())) return runID;
  return utc.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

// Compact "harness pill" payload — what's shown on the row when the
// play was stamped by the test framework. Null when the play isn't a
// harness run.
function harnessPill(r: SessionRow): { runId: string; platform: string; test: string; tooltip: string } | null {
  const runID = harnessRunId(r);
  if (!runID) return null;
  const platform = harnessPlatform(r) ?? 'unknown';
  const test = harnessTest(r) ?? '—';
  return {
    runId: shortRunId(runID),
    platform,
    test,
    tooltip: `Test harness run\nrun_id: ${runID}\nplatform: ${platform}\ntest: ${test}`,
  };
}

// rows / loading / error are computeds backed by playsQuery; declared
// further down once the query is constructed.
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

// Normalise one PlaySummary into a SessionRow: aliases v2 field
// names (started_at/last_seen_at, label_histogram) to the v1 names the
// rest of this page reads, derives health metrics, and computes
// duration. Runs once per fetch inside the queryFn so the cache holds
// processed rows and the render path stays cheap.
function normalisePlay(p: PlaySummary): SessionRow {
  const r: SessionRow = { ...(p as any), session_id: (p as any).session_id ?? '' };
  if (!r.started && p.started_at) r.started = p.started_at;
  if (!r.last_seen && p.last_seen_at) r.last_seen = p.last_seen_at;
  if (!r.labels && Array.isArray(p.label_histogram)) r.labels = p.label_histogram;
  const t0 = Date.parse(r.started ?? '');
  const t1 = Date.parse(r.last_seen ?? '');
  r.duration_ms = (Number.isFinite(t0) && Number.isFinite(t1) && t1 >= t0) ? (t1 - t0) : 0;
  deriveHealth(r);
  return r;
}

// Stable query key that refetches when the time range changes. The
// range tuple is the only server-side filter; player/group/content/
// classification/labels are post-fetch client-side filters.
const playsQueryKey = computed(() => {
  const { since, until } = computeRange();
  return ['plays', since, until] as const;
});

const qc = useQueryClient();
const playsQuery = useQuery<SessionRow[]>({
  queryKey: playsQueryKey,
  queryFn: async () => {
    const { since, until } = computeRange();
    const items = await listPlays({ from: since, to: until, limit: 5000 });
    return items.map(normalisePlay);
  },
  // Match the legacy 5s cadence. TanStack pauses the interval while
  // the tab is backgrounded — same effective behaviour as before.
  refetchInterval: 5000,
  // Keep prior rows visible while the refetch runs so the picker
  // doesn't blank out between ticks.
  placeholderData: (prev) => prev,
});

// Surfaces previously held by manual refs. computeds keep the rest of
// the template trusting `rows`, `loading`, `error` unchanged.
const rows = computed<SessionRow[]>(() => playsQuery.data.value ?? []);
const loading = computed<boolean>(() => playsQuery.isLoading.value);
const error = computed<string | null>(() => {
  const e = playsQuery.error.value as Error | null;
  return e ? String(e.message ?? e) : null;
});

// Stand-in for the old `void loadRows()` calls in user-driven range
// changes. queryKey reactivity handles the refetch automatically when
// activeRangeId / customFrom / customTo change; this is the explicit
// "fetch now" for the rare cases the key didn't shift (eg. clicking
// the same range button to force a refresh).
function refreshPlays() {
  void playsQuery.refetch();
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

function matchesLabels(r: SessionRow): boolean {
  const inc = filters.value.labels;
  const exc = filters.value.labelsExclude;
  if (!inc.length && !exc.length) return true;
  const have = Array.isArray(r.labels) ? r.labels : [];
  const haveSet = new Set<string>();
  for (const [label] of have) haveSet.add(String(label));
  // INCLUDE = AND: every requested label must be present.
  for (const want of inc) {
    if (!haveSet.has(want)) return false;
  }
  // EXCLUDE = AND: none of the excluded labels may be present.
  for (const ban of exc) {
    if (haveSet.has(ban)) return false;
  }
  return true;
}

function matchesHarness(r: SessionRow): boolean {
  switch (filters.value.harness) {
    case 'all':  return true;
    case 'only': return isHarnessRow(r);
    case 'hide': return !isHarnessRow(r);
  }
}

function matches(r: SessionRow): boolean {
  const f = filters.value;
  return (!f.player_id || r.player_id === f.player_id)
    && (!f.group_id || r.group_id === f.group_id)
    && (!f.content_id || r.content_id === f.content_id)
    && (!f.play_id || r.play_id === f.play_id)
    && matchesClassification(r)
    && matchesHarness(r)
    && matchesLabels(r);
}

// Hierarchical label filter — mirrors the SessionDisplay event-filter
// accordion. One tier per severity (error → critical → warning →
// info), each containing the distinct labels seen at that tier with
// occurrence counts. Click a label chip to toggle inclusion;
// click the tier header to toggle ALL labels in that tier. Issue
// #474 follow-up.
type Severity = 'error' | 'critical' | 'warning' | 'info';
// User-facing ordering: Critical leads (the "🚨 something's actually
// wrong" tier), then Error (player-error transitions), Warning, Info.
const SEVERITY_ORDER: Severity[] = ['critical', 'error', 'warning', 'info'];
const SEVERITY_META: Record<Severity, { label: string; bg: string; border: string; color: string }> = {
  // Critical wears red (worst-looking — user-visible playback
  // breakage); Error wears orange. Whole palette pair moves
  // together so each tier stays internally consistent. Mirrors
  // SessionDisplay.vue.
  error:    { label: 'Error',    bg: '#ffedd5', border: '#fdba74', color: '#7c2d12' },
  critical: { label: 'Critical', bg: '#fee2e2', border: '#fca5a5', color: '#7f1d1d' },
  warning:  { label: 'Warning',  bg: '#fef3c7', border: '#fcd34d', color: '#854d0e' },
  info:     { label: 'Info',     bg: '#f0fdf4', border: '#a7f3d0', color: '#1f2937' },
};

interface LabelEntry {
  value: string;     // raw `<severity>=<event>` string
  name: string;      // event portion (with `*` synthMark preserved)
  count: number;     // total occurrences across all loaded rows
}
interface LabelTier {
  sev: Severity;
  entries: LabelEntry[];
  total: number;     // sum of entry.count
}

const labelTiers = computed<LabelTier[]>(() => {
  const acc: Record<Severity, Map<string, number>> = {
    error: new Map(), critical: new Map(), warning: new Map(), info: new Map(),
  };
  for (const r of rows.value) {
    const pairs = Array.isArray(r.labels) ? r.labels : [];
    for (const p of pairs) {
      const label = String(p[0] ?? '');
      const n = Number(p[1]) || 0;
      if (!label || n <= 0) continue;
      const eq = label.indexOf('=');
      if (eq <= 0) continue;
      const sev = label.slice(0, eq) as Severity;
      if (!(sev in acc)) continue;
      acc[sev].set(label, (acc[sev].get(label) ?? 0) + n);
    }
  }
  const out: LabelTier[] = [];
  for (const sev of SEVERITY_ORDER) {
    const entries: LabelEntry[] = [];
    let total = 0;
    for (const [value, count] of acc[sev]) {
      const eq = value.indexOf('=');
      const name = eq > 0 ? value.slice(eq + 1) : value;
      entries.push({ value, name, count });
      total += count;
    }
    entries.sort((a, b) => b.count - a.count || a.name.localeCompare(b.name));
    out.push({ sev, entries, total });
  }
  return out;
});

// Expand state per tier. Defaults: error + critical open; warning +
// info collapsed (matches SessionDisplay's expandedTiers initial
// state — the user usually wants to scan the worst tiers first).
const expandedLabelTiers = ref<Record<Severity, boolean>>({
  error: true, critical: true, warning: false, info: false,
});
function toggleLabelTier(sev: Severity) {
  expandedLabelTiers.value[sev] = !expandedLabelTiers.value[sev];
}

// Per-label tristate. Click cycles:
//   none  →  include  →  exclude  →  none
// `include` requires the row to have this label; `exclude` requires
// the row to NOT have it. Both are AND'd within their respective sets
// and combine to support queries like "has http_404 AND has-not
// fault_rule_enabled".
type LabelState = 'none' | 'include' | 'exclude';
function labelState(value: string): LabelState {
  if (filters.value.labels.includes(value)) return 'include';
  if (filters.value.labelsExclude.includes(value)) return 'exclude';
  return 'none';
}
function cycleLabelFilter(value: string) {
  const inc = filters.value.labels;
  const exc = filters.value.labelsExclude;
  const cur = labelState(value);
  // Strip existing entries first so we always end up in a clean state.
  filters.value.labels = inc.filter((v) => v !== value);
  filters.value.labelsExclude = exc.filter((v) => v !== value);
  if (cur === 'none')        filters.value.labels = [...filters.value.labels, value];
  else if (cur === 'include') filters.value.labelsExclude = [...filters.value.labelsExclude, value];
  // cur === 'exclude' falls through to clean state (= 'none').
}
function isLabelSelected(value: string): boolean {
  return labelState(value) !== 'none';
}
// Tier-level cycle — same none → include → exclude → none as per-label
// but applied across every label currently visible in the tier. Click
// computes the consensus state (everything-included, everything-
// excluded, or mixed/none) and advances the whole group.
function toggleTierAll(sev: Severity, e: MouseEvent) {
  e.stopPropagation();
  const tier = labelTiers.value.find((t) => t.sev === sev);
  if (!tier || !tier.entries.length) return;
  const tierValues = tier.entries.map((x) => x.value);
  const state = tierSelectionState(sev);
  // Always strip everything in this tier from both sets first; then
  // re-add to the target based on the next state.
  const incOther = filters.value.labels.filter((v) => !tierValues.includes(v));
  const excOther = filters.value.labelsExclude.filter((v) => !tierValues.includes(v));
  switch (state) {
    case 'none':
    case 'partial':
      filters.value.labels = [...incOther, ...tierValues];
      filters.value.labelsExclude = excOther;
      break;
    case 'all-include':
      filters.value.labels = incOther;
      filters.value.labelsExclude = [...excOther, ...tierValues];
      break;
    case 'all-exclude':
    default:
      filters.value.labels = incOther;
      filters.value.labelsExclude = excOther;
      break;
  }
}
// Per-tier consensus state. Lets the tier dot show the same
// has/has-not affordance as per-label rows.
type TierState = 'none' | 'partial' | 'all-include' | 'all-exclude';
function tierSelectionState(sev: Severity): TierState {
  const tier = labelTiers.value.find((t) => t.sev === sev);
  if (!tier || !tier.entries.length) return 'none';
  let inc = 0, exc = 0;
  for (const e of tier.entries) {
    const s = labelState(e.value);
    if (s === 'include') inc++;
    else if (s === 'exclude') exc++;
  }
  const total = tier.entries.length;
  if (inc === total) return 'all-include';
  if (exc === total) return 'all-exclude';
  if (inc === 0 && exc === 0) return 'none';
  return 'partial';
}
function clearLabelFilter() {
  filters.value.labels = [];
  filters.value.labelsExclude = [];
}
// Total selected (include + exclude) — drives the Clear button count.
const labelFilterCount = computed(() =>
  filters.value.labels.length + filters.value.labelsExclude.length);
// True iff any label is currently displayed (even an empty tier).
const hasAnyLabels = computed(() => labelTiers.value.some((t) => t.total > 0));

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
  filters.value.harness = 'all';
  filters.value.labels = [];
  filters.value.labelsExclude = [];
}

function onRangeChange() {
  try { localStorage.setItem(RANGE_KEY, activeRangeId.value); } catch { /* ignore */ }
  // Query key is reactive on (since, until); switching range triggers
  // a refetch automatically. The "custom" branch waits for the user
  // to fill in both inputs before applyCustomRange() flips since/until.
  if (activeRangeId.value !== 'custom') refreshPlays();
}
function applyCustomRange() {
  customFrom.value = localToIso(customFromInput.value);
  customTo.value = localToIso(customToInput.value);
  try {
    localStorage.setItem(RANGE_CUSTOM_KEY, JSON.stringify({ from: customFrom.value, to: customTo.value }));
  } catch { /* ignore */ }
  refreshPlays();
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

/** ClickHouse-aware ISO parser. ClickHouse emits timestamps as
 *  "YYYY-MM-DD HH:MM:SS.fff" (space separator, no zone). `Date.parse`
 *  of that form is implementation-defined and returns NaN on Safari /
 *  older WebKit, which previously caused `endTimeFor` to fall through
 *  to `'live'` for every row regardless of how old the session was.
 *  Already-RFC3339 strings (with `T`) pass through unchanged. */
function parseChIsoMs(v: string | null | undefined): number {
  if (!v) return NaN;
  const normalised = v.length > 10 && v.charAt(10) === ' '
    ? v.replace(' ', 'T') + 'Z'
    : v;
  return Date.parse(normalised);
}

/** A play is "still live" — i.e. the viewer should follow the live
 *  edge rather than pin to a fixed end_time — if its last_seen is
 *  within LIVE_TAIL_MS of now AND no terminal-state marker is set.
 *  Threshold is generous (60 s) so a heartbeat that's a few seconds
 *  late doesn't kick us into archive mode. */
const LIVE_TAIL_MS = 60_000;
function endTimeFor(r: SessionRow): string {
  const lastSeen = parseChIsoMs(r.last_seen);
  if (!Number.isFinite(lastSeen)) return 'live';
  if (Date.now() - lastSeen < LIVE_TAIL_MS) return 'live';
  return new Date(lastSeen).toISOString();
}

function viewerHref(r: SessionRow): string {
  if (!r.player_id) return '#';
  // Time bounds so the viewer scopes its initial brush + SSE
  // backfill to this play's range instead of inferring from samples.
  // toMs=null when the play looks still-active — the viewer drops
  // the upper bound and follows the live edge.
  const startMs = r.started ? parseChIsoMs(r.started) : NaN;
  const lastSeen = r.last_seen ? parseChIsoMs(r.last_seen) : NaN;
  const stillLive = Number.isFinite(lastSeen) && (Date.now() - lastSeen) < LIVE_TAIL_MS;
  return sessionViewerURL({
    playerId: r.player_id,
    playId: r.play_id && r.play_id !== '—' ? r.play_id : undefined,
    fromMs: Number.isFinite(startMs) ? startMs : undefined,
    toMs: stillLive ? undefined : (Number.isFinite(lastSeen) ? lastSeen : undefined),
  });
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
// Per-label chip data for the Labels column. One chip per distinct
// label, count baked in. Sorted by count desc (most-frequent first)
// so the row reads "what happened most" left-to-right. Issue #474.
interface LabelChip {
  label: string;     // raw `<severity>=<event>` string
  name: string;      // event portion (with the `*` synthMark intact)
  count: number;
  cls: 'info' | 'warning' | 'critical' | 'error';
}
function labelChips(r: SessionRow): LabelChip[] {
  const pairs = Array.isArray(r.labels) ? r.labels : [];
  const out: LabelChip[] = [];
  for (const p of pairs) {
    const label = String(p[0] ?? '');
    const count = Number(p[1]) || 0;
    if (!label || count <= 0) continue;
    const eq = label.indexOf('=');
    const sev = eq > 0 ? label.slice(0, eq) : '';
    const name = eq > 0 ? label.slice(eq + 1) : label;
    const cls: LabelChip['cls'] =
      sev === 'error' ? 'error'
      : sev === 'critical' ? 'critical'
      : sev === 'warning' ? 'warning'
      : 'info';
    out.push({ label, name, count, cls });
  }
  out.sort((a, b) =>
    b.count - a.count
    || a.label.localeCompare(b.label),
  );
  return out;
}

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
  { key: 'labels_total',     label: 'Labels',     type: 'number' as const,  sortable: true },
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

// Star/unstar via the standard TanStack mutation contract used
// throughout v3 (see composables/usePlayer.ts § makeGroupMutation):
//   onMutate  — cancel in-flight refetches, snapshot, optimistic write
//   onError   — restore the snapshot
//   onSuccess — write the server-settled value back
// cancelQueries is the key step that fixes the race the homegrown
// loadRows() had: without it, the 5s refetch could land before the
// ClickHouse ALTER UPDATE propagated and visibly revert the star.
type ClassValue = 'favourite' | 'interesting' | 'other' | 'auto';
type StarVars = { playId: string; target: ClassValue; optimistic: string };

// Predicate matches every ['plays', since, until] cache entry — covers
// past time-window selections that the user might switch back to.
const playsQueryPredicate = { predicate: (q: any) => q.queryKey?.[0] === 'plays' };

function applyClassificationToCaches(playId: string, value: string) {
  qc.setQueriesData<SessionRow[]>(playsQueryPredicate, (old) =>
    old?.map((r) => (r.play_id === playId ? { ...r, classification: value } : r)),
  );
}

const starMutation = useMutation({
  mutationFn: ({ playId, target }: StarVars) => patchPlayClassification(playId, target),
  onMutate: async ({ playId, optimistic }) => {
    // Pause every plays-keyed in-flight refetch so the optimistic
    // write isn't stomped before the PATCH settles.
    await qc.cancelQueries(playsQueryPredicate);
    const prev = qc.getQueriesData<SessionRow[]>(playsQueryPredicate);
    applyClassificationToCaches(playId, optimistic);
    return { prev };
  },
  onError: (err: any, _vars, ctx) => {
    if (ctx?.prev) {
      for (const [key, snapshot] of ctx.prev) {
        qc.setQueryData(key, snapshot);
      }
    }
    window.alert(`Star toggle failed: ${err?.message ?? err}`);
  },
  onSuccess: (settled, { playId }) => {
    // Server tells us the settled classification ('auto' resolves to
    // interesting/other server-side). Write it into every cached
    // plays-list so the chip + filter agree without another refetch.
    if (typeof settled?.classification === 'string') {
      applyClassificationToCaches(playId, settled.classification);
    }
  },
});

function toggleStar(r: SessionRow, ev: MouseEvent) {
  ev.stopPropagation();
  ev.preventDefault();
  if (!r.play_id || r.play_id === '—') {
    window.alert('Cannot star a row without a play_id (legacy pre-stamp row).');
    return;
  }
  const wasStarred = String(r.classification || '') === 'favourite';
  // For unstar: empty string optimistically (UI checks === 'favourite');
  // server's auto-classifier will resolve to 'interesting' or 'other'
  // and onSuccess writes the real value back.
  starMutation.mutate({
    playId: r.play_id,
    target: wasStarred ? 'auto' : 'favourite',
    optimistic: wasStarred ? '' : 'favourite',
  });
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

let clockTimer: number | undefined;
onMounted(() => {
  // 1s clock tick so "1s ago" → "2s ago" updates without a fetch.
  // The play list itself refetches every 5s via useQuery.refetchInterval.
  clockTimer = window.setInterval(() => { tick.value++; }, 1000);
});
onBeforeUnmount(() => {
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

            <div class="class-chip-wrap" title="Filter by test-harness origin (plays stamped with info=run_id_… by the characterization framework)">
              <span class="ctrl-label-text">Harness:</span>
              <button
                v-for="c in ([
                  { value: 'all',  label: 'All' },
                  { value: 'only', label: '🧪 Only' },
                  { value: 'hide', label: 'Hide' }
                ] as const)"
                :key="c.value"
                type="button"
                class="class-chip"
                :class="{ active: filters.harness === c.value }"
                @click="filters.harness = c.value"
              >{{ c.label }}</button>
            </div>

            <button type="button" class="btn btn-secondary" @click="clearFilters">Clear filters</button>
            <span class="match-count">{{ matchCount }}</span>
          </div>

          <!-- Hierarchical labels filter (issue #474 follow-up).
               Mirrors SessionDisplay's Focus Window event-filter
               accordion: severity tier headers → expand → distinct
               labels at that tier with click-to-toggle inclusion.
               Multi-select OR semantics. -->
          <div class="label-filter-accordion" v-if="hasAnyLabels">
            <div class="label-filter-head">
              <span class="ctrl-label-text">Filter by label:</span>
              <button
                v-if="labelFilterCount"
                type="button"
                class="btn btn-secondary label-clear"
                @click="clearLabelFilter"
              >Clear ({{ labelFilterCount }})</button>
            </div>
            <div
              v-for="tier in labelTiers"
              :key="tier.sev"
              class="lf-tier"
              :class="{
                expanded: expandedLabelTiers[tier.sev],
                dim: !tier.total,
                'tier-active': tierSelectionState(tier.sev) !== 'none',
              }"
              :style="{
                '--tier-bg': SEVERITY_META[tier.sev].bg,
                '--tier-border': SEVERITY_META[tier.sev].border,
                '--tier-color': SEVERITY_META[tier.sev].color,
              }"
            >
              <div class="lf-tier-head">
                <button
                  type="button"
                  class="lf-chevron"
                  @click="toggleLabelTier(tier.sev)"
                  :title="expandedLabelTiers[tier.sev] ? 'Collapse' : 'Expand'"
                  :disabled="!tier.total"
                >{{ expandedLabelTiers[tier.sev] ? '▾' : '▸' }}</button>
                <button
                  type="button"
                  class="lf-tier-name"
                  @click="toggleTierAll(tier.sev, $event)"
                  :disabled="!tier.total"
                  :title="tier.total
                    ? `Cycle ALL ${tier.entries.length} ${SEVERITY_META[tier.sev].label} labels:  none → include → exclude → none`
                    : ''"
                >
                  <!-- Four-state dot:
                         none         hollow ring
                         all-include  solid fill
                         all-exclude  solid fill + ⊘ overlay
                         partial      half-fill -->
                  <span
                    class="lf-tier-dot"
                    :class="'sel-' + tierSelectionState(tier.sev)"
                    aria-hidden="true"
                  />
                  <span class="lf-tier-text">{{ SEVERITY_META[tier.sev].label }}</span>
                  <span class="lf-tier-pill">{{ tier.total }}</span>
                </button>
                <!-- Collapsed preview: first 5 labels as compact chips.
                     Mirrors SessionDisplay's tierPreview. -->
                <div class="lf-preview" v-if="!expandedLabelTiers[tier.sev] && tier.total">
                  <button
                    v-for="e in tier.entries.slice(0, 5)"
                    :key="e.value"
                    type="button"
                    class="lf-preview-chip"
                    :class="'state-' + labelState(e.value)"
                    @click.stop="cycleLabelFilter(e.value)"
                    :title="`${e.value}\nclick: none → include → exclude → none`"
                  >{{ e.name }} · {{ e.count }}</button>
                  <span v-if="tier.entries.length > 5" class="lf-preview-more">
                    +{{ tier.entries.length - 5 }} more
                  </span>
                </div>
              </div>
              <div class="lf-tier-body" v-if="expandedLabelTiers[tier.sev] && tier.total">
                <button
                  v-for="e in tier.entries"
                  :key="e.value"
                  type="button"
                  class="lf-label-row"
                  :class="'state-' + labelState(e.value)"
                  @click="cycleLabelFilter(e.value)"
                  :title="`${e.value}\nclick: none → include → exclude → none`"
                >
                  <!-- Tristate glyph:
                         none     hollow ring     ○
                         include  filled check    ✓
                         exclude  circled slash   ⊘ -->
                  <span class="lf-label-check">{{
                    labelState(e.value) === 'include' ? '✓'
                    : labelState(e.value) === 'exclude' ? '⊘'
                    : '○'
                  }}</span>
                  <span class="lf-label-name">{{ e.name }}</span>
                  <span class="lf-label-count">{{ e.count }}</span>
                </button>
              </div>
            </div>
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
                  <td>
                    <div>{{ r.content_id || '' }}</div>
                    <div v-if="harnessPill(r)" class="harness-pill" :title="harnessPill(r)!.tooltip">
                      🧪
                      <span class="harness-test">{{ harnessPill(r)!.test }}</span>
                      <span class="harness-sep">·</span>
                      <span class="harness-platform">{{ harnessPill(r)!.platform }}</span>
                      <span class="harness-sep">·</span>
                      <span class="harness-run">{{ harnessPill(r)!.runId }}</span>
                    </div>
                  </td>
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
                  <td class="cell-labels">
                    <span
                      v-for="chip in labelChips(r)"
                      :key="chip.label"
                      class="label-chip"
                      :class="'label-' + chip.cls"
                      :title="chip.label"
                    >{{ chip.count }}× {{ chip.name }}</span>
                    <span v-if="labelChips(r).length === 0" class="dash">—</span>
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

    <!-- AI chat side panel (#497). Default collapsed so the page
         lays out unchanged; expand via the ◀ button. -->
    <Teleport to="body">
      <div class="chat-dock">
        <ChatPanel
          :scope="{ kind: 'fleet' }"
          scope-key="sessions:fleet"
          variant="panel"
          :start-collapsed="true"
        />
      </div>
    </Teleport>
  </ShellLayout>
</template>

<style>
/* Unscoped — Teleport-to-body element needs the parent style applied
   directly. Pinned to the right edge, full viewport height. */
.chat-dock {
  position: fixed;
  top: var(--header-height, 64px);
  right: 0;
  bottom: 0;
  z-index: 50;
  box-shadow: var(--shadow-md);
  background: #fff;
}
</style>

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

/* Harness origin pill — sub-line under the content cell when a
   play was stamped by the characterization framework. Compact;
   reuses the colour palette of the test-runner page so the visual
   tie between Sessions ↔ Characterization is obvious. */
.harness-pill {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  margin-top: 2px;
  padding: 1px 6px;
  border-radius: 10px;
  background: #ecfdf5;
  border: 1px solid #6ee7b7;
  color: #065f46;
  font: 500 10px ui-monospace, SFMono-Regular, monospace;
  white-space: nowrap;
}
.harness-test { font-weight: 700; }
.harness-platform { color: #047857; }
.harness-run { color: #065f46; opacity: 0.85; }
.harness-sep { color: #34d399; }

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

/* Per-label chips (issue #474). One chip per distinct label,
 * count baked in. Severity-tinted, mirroring SessionDisplay's
 * SEVERITY_META palette. */
.cell-labels { min-width: 220px; max-width: 360px; }
.label-chip {
  display: inline-block;
  padding: 1px 6px;
  margin: 0 3px 2px 0;
  border-radius: 10px;
  font: 600 11px system-ui;
  line-height: 1.4;
  white-space: nowrap;
  border: 1px solid transparent;
}
.label-info     { background: #f0fdf4; color: #1f2937; border-color: #a7f3d0; }
.label-warning  { background: #fef3c7; color: #854d0e; border-color: #fcd34d; }
.label-critical { background: #fee2e2; color: #7f1d1d; border-color: #fca5a5; }
.label-error    { background: #ffedd5; color: #7c2d12; border-color: #fdba74; }

/* Hierarchical labels filter — mirrors SessionDisplay's Focus Window
 * event-filter accordion. One row per severity tier with a clickable
 * header (toggle ALL labels in that tier), a chevron (collapse), and
 * an expanded body listing individual label rows with check toggles. */
.label-filter-accordion {
  display: flex; flex-direction: column; gap: 4px;
  padding: 4px 0;
}
.label-filter-head {
  display: flex; align-items: center; gap: 8px; padding-bottom: 2px;
}
.label-clear { margin-left: auto; }

.lf-tier {
  border: 1px solid var(--tier-border);
  border-radius: 8px;
  background: var(--tier-bg);
  padding: 4px 6px;
}
.lf-tier.dim { opacity: 0.45; }
.lf-tier.tier-active { box-shadow: 0 0 0 1.5px var(--tier-color) inset; }

.lf-tier-head {
  display: flex; align-items: center; gap: 6px; min-height: 26px;
}
.lf-chevron {
  background: transparent; border: 0;
  width: 18px; height: 18px;
  cursor: pointer;
  color: var(--tier-color);
  font-size: 13px;
  line-height: 1;
}
.lf-chevron:disabled { cursor: default; opacity: 0.4; }
.lf-tier-name {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 2px 8px;
  border-radius: 12px;
  border: 0;
  background: transparent;
  color: var(--tier-color);
  font: 600 12px system-ui;
  cursor: pointer;
}
.lf-tier-name:hover:not(:disabled) { background: rgba(0,0,0,0.05); }
.lf-tier-name:disabled { cursor: default; }
/* Tristate (+ partial) selection dot for tier headers. Visual states:
 *   .sel-none        — hollow ring
 *   .sel-partial     — half-fill (mixed include/exclude/none)
 *   .sel-all-include — solid fill
 *   .sel-all-exclude — solid fill + ⊘ slash overlay */
.lf-tier-dot {
  position: relative;
  display: inline-block;
  width: 12px; height: 12px;
  border-radius: 50%;
  box-sizing: border-box;
  border: 1.5px solid var(--tier-color);
  background: transparent;
}
.lf-tier-dot.sel-all-include {
  background: var(--tier-color);
}
/* Exclude: leave the dot hollow (matches sel-none ring) but draw a
 * diagonal slash across it. Reads as "not this" — same affordance as
 * a "no entry" sign. */
.lf-tier-dot.sel-all-exclude {
  background: transparent;
}
.lf-tier-dot.sel-all-exclude::after {
  content: '';
  position: absolute;
  left: 50%;
  top: -3px;
  bottom: -3px;
  width: 1.5px;
  background: var(--tier-color);
  transform: translateX(-50%) rotate(45deg);
}
.lf-tier-dot.sel-partial {
  background: conic-gradient(var(--tier-color) 0 50%, transparent 50% 100%);
}
.lf-tier-pill {
  background: rgba(0,0,0,0.08);
  color: var(--tier-color);
  border-radius: 8px;
  padding: 0 6px;
  font: 700 10px ui-monospace, Menlo, monospace;
  min-width: 18px;
  text-align: center;
}

.lf-preview {
  display: flex; flex-wrap: wrap; gap: 4px; margin-left: 8px; flex: 1;
}
.lf-preview-chip {
  padding: 1px 7px;
  border-radius: 10px;
  font: 500 11px system-ui;
  border: 1px solid var(--tier-border);
  background: rgba(255,255,255,0.55);
  color: var(--tier-color);
  cursor: pointer;
  line-height: 1.4;
}
.lf-preview-chip.state-include {
  background: var(--tier-color);
  color: #fff;
  border-color: var(--tier-color);
}
/* Exclude: keep the chip hollow (same baseline as 'none') and draw a
 * diagonal slash across it so the chip reads as "not this". Reuses
 * the same "no entry" semantic as the tier dot's exclude state.
 * Border stays the tier color and slightly heavier so the chip still
 * registers as a filter target. */
.lf-preview-chip.state-exclude {
  background: rgba(255,255,255,0.55);
  color: var(--tier-color);
  border-color: var(--tier-color);
  border-width: 1.5px;
  position: relative;
  text-decoration: line-through;
  text-decoration-thickness: 1.5px;
  text-decoration-color: var(--tier-color);
}
.lf-preview-more {
  font: 500 11px system-ui;
  color: var(--tier-color);
  opacity: 0.7;
  align-self: center;
}

.lf-tier-body {
  display: flex; flex-direction: column; gap: 2px;
  padding: 4px 4px 2px 26px;     /* indent under the chevron */
}
.lf-label-row {
  display: grid;
  grid-template-columns: 18px 1fr auto;
  gap: 6px;
  align-items: center;
  padding: 2px 8px;
  border: 1px solid transparent;
  border-radius: 6px;
  background: rgba(255,255,255,0.55);
  color: var(--tier-color);
  font: 500 12px system-ui;
  text-align: left;
  cursor: pointer;
}
.lf-label-row:hover { background: rgba(255,255,255,0.85); }
.lf-label-row.state-include {
  background: var(--tier-color);
  color: #fff;
  border-color: var(--tier-color);
}
/* Exclude: hollow background like the 'none' baseline, just a slash
 * across the row plus a heavier border so the inverted state reads
 * at a glance. The lf-label-check glyph (⊘) reinforces the same
 * "not this" affordance. */
.lf-label-row.state-exclude {
  background: rgba(255,255,255,0.55);
  color: var(--tier-color);
  border-color: var(--tier-color);
  border-width: 1.5px;
  text-decoration: line-through;
  text-decoration-thickness: 1.5px;
  text-decoration-color: var(--tier-color);
}
.lf-label-check { font-weight: 700; }
/* Tighten the ⊘ glyph — many fonts render it overly wide. */
.lf-label-row.state-exclude .lf-label-check { letter-spacing: -1px; }
.lf-label-name {
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  font: 500 12px ui-monospace, Menlo, monospace;
}
.lf-label-count {
  font: 600 11px ui-monospace, Menlo, monospace;
  opacity: 0.85;
}

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
