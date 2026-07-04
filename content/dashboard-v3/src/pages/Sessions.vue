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
import LabelFrequencyTable from '@/components/LabelFrequencyTable.vue';
import ChatPanel from '@/components/chat/ChatPanel.vue';
import { sessionViewerURL, type CompareMember } from '@/composables/urlTimeFormat';
import { listPlays, patchPlayClassification, type PlaySummary, type Scenario } from '@/repo/v2-repo';

interface SessionRow {
  session_id: string;
  play_id?: string;
  player_id?: string;
  group_id?: string;
  content_id?: string;
  // #673 — Scenario (run-identity) source fields. These typed columns are
  // already on the play summary (find.go final SELECT, #550 Phase 4) and
  // reach us via PlaySummary's index signature; the Scenario column surfaces
  // them. test / platform / run_id have no column and come from the testing=
  // label tier instead (see scenario()).
  device_class?: string;
  device_model?: string;
  player_tech?: string;
  app_version?: string;
  os_version_major?: number | string;
  os_version_minor?: number | string;
  // #678 — typed run-identity object from /api/v2/plays (built server-side
  // from the columns above + testing= label tails). Preferred by scenario();
  // the loose columns remain for the rollout fallback.
  scenario?: Scenario;
  started?: string;
  last_seen?: string;
  classification?: string;
  last_state?: string;
  last_player_error?: string;
  // #550 Phase 2 outcome (argMax'd onto the play summary) — how the play
  // ended. Rendered as the "Outcome" column via endOutcome().
  playback_status?: string;
  playback_reason?: string;
  metric_events?: number | string;
  net_events?: number | string;
  stalls?: number;
  frames_dropped?: number;
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
  // #563 — derived QoE rate metrics (read-time, from the #550 residency
  // accumulators). undefined when there's no playing time to normalise
  // against, so the cell renders "—" rather than a misleading 0.
  rebuffer_ratio?: number;   // stalling / (stalling + playing)
  buffering_ratio?: number;  // buffering / (buffering + playing)
  drop_ratio?: number;       // dropped / (dropped + displayed)
  stalls_per_hr?: number;
  mean_stall_ms?: number;    // stalling_time_ms / stalls
  shifts_per_min?: number;   // bitrate shifts / playing minute
  downshifts_per_min?: number;
  errors_per_hr?: number;
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
  play_id: string;
  classification: 'all' | 'starred' | 'interesting' | 'other';
  // Session origin filter. 'all' = no constraint; otherwise keep only plays of
  // that origin — 'manual' (user-initiated, no harness markers), 'sweep' (sweep
  // framework, testing=sweep_/exp_id_), or 'automated' (characterization runs,
  // testing=test_/run_id_). See sessionSource().
  source: 'all' | 'manual' | 'sweep' | 'automated';
  // Tristate label filter:
  //   - labels:        AND-required INCLUDES (row.labels must contain every entry)
  //   - labelsExclude: AND-required EXCLUDES (row.labels must contain NONE of these)
  // Empty arrays = no constraint on that side. Combine for queries
  // like "has http_404 AND has-not fault_rule_enabled".
  labels: string[];
  labelsExclude: string[];
  // Tristate scenario facet filter (same shape as labels). Values are encoded
  // `<fieldKey>=<value>` (e.g. `os=26.5`, `variant=ll`). AND across different
  // keys, OR within one key; exclude = must-not-match.
  scenario: string[];
  scenarioExclude: string[];
  // 4-category facet (docs/EVENT_TAXONOMY.md). Empty = no constraint; else
  // OR-semantics: keep plays that contain ≥1 label in any selected category.
  categories: Category[];
}>({
  player_id: '', group_id: '', play_id: '',
  classification: 'all',
  source: 'all',
  labels: [], labelsExclude: [],
  scenario: [], scenarioExclude: [],
  categories: [],
});

// Test-harness label extraction. The characterization framework
// stamps each play it runs with these keys (#571 moved them from the
// info= prefix to the testing= tier):
//   testing=run_id_<utc-compact>   e.g. testing=run_id_20260524T070148Z
//   testing=platform_<name>        e.g. testing=platform_ipad-sim
//   testing=test_<name>            e.g. testing=test_rampup
// These keys (with their value tail) are how Characterization.vue groups
// plays into runs; this page lifts the same convention to surface "this
// play came from the harness" on the picker. We match the testing= tier
// first, then fall back to the legacy info= prefix for plays written
// before #571 (still present within the ≤30-day 'other' TTL).
function findLabelTail(r: SessionRow, key: string): string | null {
  const pairs = Array.isArray(r.labels) ? r.labels : [];
  for (const prefix of [`testing=${key}`, `info=${key}`]) {
    for (const [label] of pairs) {
      const s = String(label);
      if (s.startsWith(prefix)) return s.slice(prefix.length);
    }
  }
  return null;
}
function harnessRunId(r: SessionRow): string | null { return findLabelTail(r, 'run_id_'); }
function harnessPlatform(r: SessionRow): string | null { return findLabelTail(r, 'platform_'); }
function harnessTest(r: SessionRow): string | null { return findLabelTail(r, 'test_'); }
// Session origin — how the play was initiated. Order matters: sweep plays carry
// sweep_/exp_id_ but NO run_id, so check them first; characterization runs carry
// test_/run_id_; anything with none of those is a user-initiated manual play.
type SessionSource = 'manual' | 'sweep' | 'automated';
function sessionSource(r: SessionRow): SessionSource {
  if (findLabelTail(r, 'sweep_') !== null || findLabelTail(r, 'exp_id_') !== null) return 'sweep';
  if (findLabelTail(r, 'test_') !== null || rowRunId(r) !== '') return 'automated';
  return 'manual';
}
const SOURCE_LABEL: Record<SessionSource, string> = {
  manual: '👤 manual', sweep: '🌙 sweep', automated: '🧪 automated',
};
// Click the Categories-cell source chip to drive the Source picker (toggle off
// to 'all' if it's already the active origin).
function filterBySource(r: SessionRow) {
  const s = sessionSource(r);
  filters.value.source = filters.value.source === s ? 'all' : s;
}
// #678 — scenario-aware accessors: prefer the server `scenario` object, fall
// back to the testing= label tails. Used by the Platform/Test facets so they
// agree with the Scenario cell regardless of which source populated it.
function rowTest(r: SessionRow): string { return r.scenario?.test ?? harnessTest(r) ?? ''; }
function rowPlatform(r: SessionRow): string { return r.scenario?.platform ?? harnessPlatform(r) ?? ''; }
function rowRunId(r: SessionRow): string { return r.scenario?.run_id ?? harnessRunId(r) ?? ''; }

// Pretty-print the UTC compact run_id (20260524T070148Z) as local
// hh:mm. Falls back to the raw string if it doesn't match the
// expected shape (older runs, manual stamps, etc.).
function shortRunId(runID: string | null): string {
  if (!runID) return '';
  const m = runID.match(/^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/);
  if (!m) return runID;
  const utc = new Date(`${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`);
  if (!Number.isFinite(utc.getTime())) return runID;
  // hour12: false so the harness pill matches the "Started" column's
  // 24-hour format (which is built from getHours() / pad()). Without
  // this, en-US users would see 12-hour ("12:01 AM") on the pill but
  // 24-hour ("00:01") on the row — visually disorienting.
  return utc.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}

// #673/#678 — Scenario (run-identity) cell. A play's *dimensions* — what it
// IS (test, platform, device, content versions) — as opposed to the event
// labels, which are what HAPPENED during it (stalls, faults, breaches, with
// counts). They were intermingled in the testing= label tier; this surfaces
// the identity fields as their own structured cell.
//
// #678 moved the assembly server-side (forwarder /api/v2/plays now returns a
// typed `scenario` object). We prefer that; scenarioFromRow() is the rollout
// fallback for when the frontend ships ahead of the forwarder (separate
// deploys) or for cached rows predating the field. Both paths feed the same
// renderer, scenarioView(). Content has its own column, so content_id from
// the object is intentionally not shown here.
interface ScenarioField { key: string; label: string; value: string }
// Render order + display labels, keyed by the typed Scenario field name.
const SCENARIO_FIELDS: { src: keyof Scenario; key: string; label: string }[] = [
  { src: 'test',         key: 'test',     label: 'test' },
  { src: 'platform',     key: 'platform', label: 'platform' },
  { src: 'device_model', key: 'device',   label: 'device' },
  { src: 'player_tech',  key: 'player',   label: 'player' },
  { src: 'player_tech_version', key: 'player_ver', label: 'player ver' },
  { src: 'app_version',  key: 'app',      label: 'app' },
  { src: 'os_version',   key: 'os',       label: 'os' },
  { src: 'manifest_variant', key: 'variant', label: 'variant' }, // #679
  { src: 'server_build', key: 'build',    label: 'build' },      // #679
  { src: 'run_id',       key: 'run',      label: 'run' },
];
// Build the render model from a typed Scenario object (server-supplied, #678).
function scenarioView(sc: Scenario): { fields: ScenarioField[]; tooltip: string } | null {
  const fields: ScenarioField[] = [];
  for (const f of SCENARIO_FIELDS) {
    let value = String(sc[f.src] ?? '');
    if (!value && f.src === 'device_model') value = String(sc.device_class ?? '');
    if (f.src === 'run_id' && value) value = shortRunId(value);
    if (value) fields.push({ key: f.key, label: f.label, value });
  }
  if (fields.length === 0) return null;
  const tooltip = fields.map((f) => `${f.label}: ${f.value}`).join('\n')
    + (sc.run_id ? `\nrun_id: ${sc.run_id}` : '');
  return { fields, tooltip };
}
// Rollout fallback: assemble a Scenario from the raw row (typed columns +
// testing= label tails) the way #673 did client-side, for rows the forwarder
// hasn't enriched yet.
function scenarioFromRow(r: SessionRow): Scenario {
  const col = (k: string) => {
    const v = (r as any)[k];
    return v === undefined || v === null || v === '' ? '' : String(v);
  };
  const osMaj = col('os_version_major');
  const osMin = col('os_version_minor');
  return {
    test: harnessTest(r) ?? '',
    platform: harnessPlatform(r) ?? '',
    run_id: harnessRunId(r) ?? '',
    device_class: col('device_class'),
    device_model: col('device_model'),
    player_tech: col('player_tech'),
    app_version: col('app_version'),
    os_version: osMaj ? (osMin ? `${osMaj}.${osMin}` : osMaj) : '',
  };
}
function scenario(r: SessionRow): { fields: ScenarioField[]; tooltip: string } | null {
  return scenarioView(r.scenario ?? scenarioFromRow(r));
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
  // Reads any field off the raw PlaySummary (normalisePlay spreads the
  // whole summary onto the row), so this also reaches the #550
  // accumulators that aren't in the SessionRow interface.
  const n = (k: string) => Number((r as any)[k]) || 0;
  const stalls = n('stalls');
  const drops = n('frames_dropped');
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

  // #659: health_score is computed below, AFTER the normalized rates — it's
  // re-based on those (length-unbiased) + a breach penalty, not raw counts.

  // #563 — derived QoE rate metrics, normalised against playing time
  // (Conviva-style). undefined when the denominator is zero so cells
  // show "—". All inputs are cumulative #550 columns on the summary.
  // Minimum sample before a rate is trustworthy. A couple of events over
  // a few seconds otherwise normalises to an absurd per-hour/per-minute
  // value (1 stall over 2s ≈ 1800/hr). Below the floor the cell renders
  // "—" (insufficient sample) rather than noise. Per-time rates gate on
  // playing time; the time-ratios gate on their own (engaged-time)
  // denominator so a stall-heavy SHORT play still reports a real
  // rebuffer %; the drop ratio gates on a minimum frame count.
  const RATE_MIN_PLAYING_MS = 30_000; // ≥30s of playback for per-time rates
  const RATIO_MIN_MS = 30_000;        // ≥30s engaged time for the time ratios
  const RATIO_MIN_FRAMES = 300;       // ≥~5–10s of frames for the drop ratio
  const playingMs = n('playing_time_ms');
  const stallingMs = n('stalling_time_ms');
  const bufferingMs = n('buffering_time_ms');
  const displayed = n('frames_displayed');
  const playingHrs = playingMs / 3_600_000;
  const playingMin = playingMs / 60_000;
  const rebufDenom = stallingMs + playingMs;
  const bufDenom = bufferingMs + playingMs;
  const frameDenom = drops + displayed;
  const rateOk = playingMs >= RATE_MIN_PLAYING_MS;
  r.rebuffer_ratio = rebufDenom >= RATIO_MIN_MS ? stallingMs / rebufDenom : undefined;
  r.buffering_ratio = bufDenom >= RATIO_MIN_MS ? bufferingMs / bufDenom : undefined;
  r.drop_ratio = frameDenom >= RATIO_MIN_FRAMES ? drops / frameDenom : undefined;
  r.stalls_per_hr = rateOk ? stalls / playingHrs : undefined;
  r.mean_stall_ms = stalls > 0 ? stallingMs / stalls : undefined;
  r.shifts_per_min = rateOk ? n('bitrate_shifts') / playingMin : undefined;
  r.downshifts_per_min = rateOk ? downshifts / playingMin : undefined;
  r.errors_per_hr = rateOk ? n('error_count') / playingHrs : undefined;

  // #659: health from normalized QoE rates (length-unbiased) + a hard
  // penalty for critical/error QoE labels, replacing the raw-count formula.
  // Below the rate sample floor a play has no trustworthy rates, so it's
  // scored on hard-failure presence instead of rate noise.
  const breach = breachHealthPenalty(r);
  if (rateOk) {
    const rebufPct = (r.rebuffer_ratio ?? 0) * 100;
    const dropPct = (r.drop_ratio ?? 0) * 100;
    const dRebuf = Math.min(rebufPct * 4, 50);
    const dStalls = Math.min((r.stalls_per_hr ?? 0) * 3, 20);
    const dDown = Math.min((r.downshifts_per_min ?? 0) * 5, 15);
    const dDrop = Math.min(dropPct * 2, 15);
    const dErr = Math.min((r.errors_per_hr ?? 0) * 5, 20);
    r.health_score = Math.max(0, 100 - (dRebuf + dStalls + dDown + dDrop + dErr + breach));
    const r3 = (x: number) => Math.round(x * 1000) / 1000;
    r.health_breakdown = {
      rebuffer_pct: r3(rebufPct),
      stalls_per_hr: r3(r.stalls_per_hr ?? 0),
      downshifts_per_min: r3(r.downshifts_per_min ?? 0),
      drop_pct: r3(dropPct),
      errors_per_hr: r3(r.errors_per_hr ?? 0),
      breach,
    };
  } else {
    const hardFail = errors > 0 || faults > 0 || n('frozen_count') > 0;
    r.health_score = Math.max(0, 100 - (hardFail ? 40 : 0) - breach);
    r.health_breakdown = { short_play: true, hard_failure: hardFail, breach };
  }
}

// #659: health penalty for the worst QoE labels — critical (×20) and error
// (×10) tiers (the *_breach family etc.), capped at 40. Makes a real breach
// dent the score that the raw-count formula ignored.
function breachHealthPenalty(r: SessionRow): number {
  const pairs = Array.isArray(r.labels) ? r.labels : [];
  let p = 0;
  for (const pr of pairs) {
    const label = String((pr as any)[0] ?? '');
    const eq = label.indexOf('=');
    const sev = eq > 0 ? label.slice(0, eq) : '';
    if (sev === 'critical') p += 20;
    else if (sev === 'error') p += 10;
  }
  return Math.min(p, 40);
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

function matchesSource(r: SessionRow): boolean {
  const s = filters.value.source;
  return s === 'all' || sessionSource(r) === s;
}

// Category facet — OR-semantics: with any category selected, keep plays
// that contain ≥1 label in any of them. Empty selection = no constraint.
function matchesCategories(r: SessionRow): boolean {
  const cats = filters.value.categories;
  if (!cats.length) return true;
  const byCat = chipsByCategory(r);
  return cats.some((c) => byCat[c].length > 0);
}
function toggleCategoryFilter(c: Category) {
  const cur = filters.value.categories;
  filters.value.categories = cur.includes(c) ? cur.filter((x) => x !== c) : [...cur, c];
}

function matches(r: SessionRow): boolean {
  const f = filters.value;
  return (!f.player_id || r.player_id === f.player_id)
    && (!f.group_id || r.group_id === f.group_id)
    && (!f.play_id || r.play_id === f.play_id)
    && matchesClassification(r)
    && matchesSource(r)
    && matchesCategories(r)
    && matchesScenario(r)
    && matchesLabels(r);
}

// Hierarchical label filter — mirrors the SessionDisplay event-filter
// accordion. One tier per severity (critical → error → warning →
// info), each containing the distinct labels seen at that tier with
// occurrence counts. Click a label chip to toggle inclusion; click the
// tier header to toggle ALL labels in that tier. Issue #474 follow-up.
// The test-harness `testing=` KV tier used to live here too; it moved
// to the scenario/config panel — it's play *configuration* (platform /
// mode / recipe / …), not a playback event. See HARNESS_FACET_KEYS.
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

// --- Scenario facet filter (OS / device / segment / build / player / …) ------
// The scenario object is populated from player metrics on ~every play, so these
// dimensions filter manual + harness plays alike. platform/test keep their
// dropdowns above; this hierarchical panel (same tristate as the label filter)
// covers the rest.
const SCENARIO_FACET_KEYS = ['platform', 'test', 'os', 'device', 'variant', 'build', 'app', 'player', 'player_ver', 'content', 'run'] as const;
const SCENARIO_FACET_LABEL: Record<string, string> = {
  platform: 'Platform', test: 'Test', os: 'OS', device: 'Device', variant: 'Segment',
  build: 'Build', app: 'App', player: 'Player', player_ver: 'Player ver',
  content: 'Content', run: 'Run',
};
// Test-harness config facets — the `testing=<key>_<value>` KV tier the
// characterization / sweep framework stamps on the plays it runs. This is
// play *configuration* (platform / mode / recipe / …), so it filters here in
// the scenario/config panel rather than in the label (event) filter. Keys with
// a dedicated control elsewhere are intentionally omitted: platform + test have
// their own dropdowns, run_id is the 'run' scenario facet above. Each value is
// the `testing=<key>_…` tail (findLabelTail); the underscore split is why the
// keys must be enumerated here, not derived.
const HARNESS_FACET_KEYS = ['mode', 'recipe', 'class', 'kind', 'arm', 'sweep', 'exp_id', 'group', 'verdict', 'why', 'protocol'] as const;
const HARNESS_FACET_LABEL: Record<string, string> = {
  mode: 'Mode', recipe: 'Recipe', class: 'Class', kind: 'Kind', arm: 'Arm',
  sweep: 'Sweep', exp_id: 'Experiment', group: 'Group', verdict: 'Verdict',
  why: 'Why', protocol: 'Protocol',
};
const HARNESS_FACET_SET: ReadonlySet<string> = new Set(HARNESS_FACET_KEYS);
function scenarioValueFor(r: SessionRow, key: string): string {
  // platform/test prefer the scenario object but fall back to the testing=
  // tail (rowPlatform/rowTest); content reads the row's content_id (it has no
  // scenario chip — the Content column owns it).
  if (key === 'platform') return rowPlatform(r);
  if (key === 'test') return rowTest(r);
  if (key === 'content') return String(r.content_id ?? '');
  if (HARNESS_FACET_SET.has(key)) return findLabelTail(r, `${key}_`) ?? '';
  const fld = SCENARIO_FIELDS.find((f) => f.key === key);
  if (!fld) return '';
  const sc = r.scenario ?? scenarioFromRow(r);
  let v = String(sc[fld.src] ?? '');
  if (!v && fld.src === 'device_model') v = String(sc.device_class ?? '');
  if (fld.src === 'run_id' && v) v = shortRunId(v);
  return v;
}
interface ScenarioFacet { key: string; label: string; entries: { value: string; count: number }[]; total: number }
function buildFacets(keys: readonly string[], labels: Record<string, string>): ScenarioFacet[] {
  const counts = new Map<string, Map<string, number>>();
  for (const k of keys) counts.set(k, new Map());
  for (const r of rows.value) {
    for (const k of keys) {
      const v = scenarioValueFor(r, k);
      if (!v) continue;
      const m = counts.get(k)!;
      m.set(v, (m.get(v) ?? 0) + 1);
    }
  }
  return keys
    .map((k) => {
      const entries = [...counts.get(k)!.entries()]
        .map(([value, count]) => ({ value, count }))
        .sort((a, b) => b.count - a.count || a.value.localeCompare(b.value));
      return { key: k, label: labels[k], entries, total: entries.length };
    })
    .filter((g) => g.total > 0);
}
const scenarioFacets = computed<ScenarioFacet[]>(() => buildFacets(SCENARIO_FACET_KEYS, SCENARIO_FACET_LABEL));
const harnessFacets = computed<ScenarioFacet[]>(() => buildFacets(HARNESS_FACET_KEYS, HARNESS_FACET_LABEL));
const hasAnyScenario = computed(() => scenarioFacets.value.length > 0 || harnessFacets.value.length > 0);
const hasAnyHarness = computed(() => harnessFacets.value.length > 0);
// Group lookups span both facet lists — scenario + harness share the same
// `key=value` encoding and tristate machinery, so state helpers resolve a key
// against either.
function findFacet(key: string): ScenarioFacet | undefined {
  return scenarioFacets.value.find((x) => x.key === key) ?? harnessFacets.value.find((x) => x.key === key);
}
const expandedScenario = ref<Record<string, boolean>>({ os: true, device: true, variant: true, mode: true, recipe: true });
function toggleScenarioGroup(key: string) { expandedScenario.value[key] = !expandedScenario.value[key]; }
function scenarioEnc(key: string, value: string): string { return `${key}=${value}`; }
function scenarioState(key: string, value: string): LabelState {
  const enc = scenarioEnc(key, value);
  if (filters.value.scenario.includes(enc)) return 'include';
  if (filters.value.scenarioExclude.includes(enc)) return 'exclude';
  return 'none';
}
function cycleScenarioFilter(key: string, value: string) {
  const enc = scenarioEnc(key, value);
  const cur = scenarioState(key, value);
  filters.value.scenario = filters.value.scenario.filter((v) => v !== enc);
  filters.value.scenarioExclude = filters.value.scenarioExclude.filter((v) => v !== enc);
  if (cur === 'none') filters.value.scenario = [...filters.value.scenario, enc];
  else if (cur === 'include') filters.value.scenarioExclude = [...filters.value.scenarioExclude, enc];
}
function scenarioGroupState(key: string): TierState {
  const g = findFacet(key);
  if (!g || !g.entries.length) return 'none';
  let inc = 0, exc = 0;
  for (const e of g.entries) {
    const s = scenarioState(key, e.value);
    if (s === 'include') inc++; else if (s === 'exclude') exc++;
  }
  if (inc === g.entries.length) return 'all-include';
  if (exc === g.entries.length) return 'all-exclude';
  if (inc === 0 && exc === 0) return 'none';
  return 'partial';
}
function toggleScenarioAll(key: string, e: MouseEvent) {
  e.stopPropagation();
  const g = findFacet(key);
  if (!g || !g.entries.length) return;
  const encs = g.entries.map((x) => scenarioEnc(key, x.value));
  const state = scenarioGroupState(key);
  const incOther = filters.value.scenario.filter((v) => !encs.includes(v));
  const excOther = filters.value.scenarioExclude.filter((v) => !encs.includes(v));
  if (state === 'none' || state === 'partial') {
    filters.value.scenario = [...incOther, ...encs];
    filters.value.scenarioExclude = excOther;
  } else if (state === 'all-include') {
    filters.value.scenario = incOther;
    filters.value.scenarioExclude = [...excOther, ...encs];
  } else {
    filters.value.scenario = incOther;
    filters.value.scenarioExclude = excOther;
  }
}
const scenarioFilterCount = computed(() =>
  filters.value.scenario.length + filters.value.scenarioExclude.length);
function clearScenarioFilter() {
  filters.value.scenario = [];
  filters.value.scenarioExclude = [];
}
// Click-to-filter from the table cells. A left click on the Player cell scopes
// the list to that player_id (toggle off if already scoped). A left click on a
// Categories chip sets that dimension's filter — platform/test drive their own
// string fields; every other key is a scenario/harness facet (toggle include).
function filterByPlayer(pid: string) {
  if (!pid) return;
  filters.value.player_id = filters.value.player_id === pid ? '' : pid;
}
function filterByCategory(key: string, value: string) {
  if (!value) return;
  const enc = scenarioEnc(key, value);
  const wasInc = filters.value.scenario.includes(enc);
  filters.value.scenario = filters.value.scenario.filter((v) => v !== enc);
  filters.value.scenarioExclude = filters.value.scenarioExclude.filter((v) => v !== enc);
  if (!wasInc) filters.value.scenario = [...filters.value.scenario, enc];
}
function isCategoryActive(key: string, value: string): boolean {
  return filters.value.scenario.includes(scenarioEnc(key, value));
}
function matchesScenario(r: SessionRow): boolean {
  const inc = filters.value.scenario;
  const exc = filters.value.scenarioExclude;
  if (inc.length) {
    // AND across keys, OR within a key.
    const byKey = new Map<string, string[]>();
    for (const enc of inc) {
      const i = enc.indexOf('='); if (i < 0) continue;
      const k = enc.slice(0, i);
      const arr = byKey.get(k) ?? []; arr.push(enc.slice(i + 1)); byKey.set(k, arr);
    }
    for (const [k, vals] of byKey) {
      if (!vals.includes(scenarioValueFor(r, k))) return false;
    }
  }
  for (const enc of exc) {
    const i = enc.indexOf('='); if (i < 0) continue;
    if (scenarioValueFor(r, enc.slice(0, i)) === enc.slice(i + 1)) return false;
  }
  return true;
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
function distinctFor(key: 'player_id' | 'group_id' | 'play_id'): string[] {
  const f = filters.value;
  const pool = rows.value.filter((r) => {
    if (key === 'player_id') return true;
    if (!f.player_id || r.player_id === f.player_id) {
      if (key === 'group_id') return true;
      return !f.group_id || r.group_id === f.group_id;
    }
    return false;
  });
  return Array.from(new Set(pool.map((r) => (r as any)[key] || '').filter(Boolean))).sort();
}
const playerOptions = computed(() => distinctFor('player_id'));
const groupOptions = computed(() => distinctFor('group_id'));
const playOptions = computed(() => distinctFor('play_id'));

// When a parent filter narrows enough that the current value is no
// longer in the visible set, clear it. Mirrors `fillSelect` in the
// legacy code.
watch(playerOptions, (opts) => { if (filters.value.player_id && !opts.includes(filters.value.player_id)) filters.value.player_id = ''; });
watch(groupOptions, (opts) => { if (filters.value.group_id && !opts.includes(filters.value.group_id)) filters.value.group_id = ''; });
watch(playOptions, (opts) => { if (filters.value.play_id && !opts.includes(filters.value.play_id)) filters.value.play_id = ''; });

function clearFilters() {
  filters.value.player_id = '';
  filters.value.group_id = '';
  filters.value.play_id = '';
  filters.value.classification = 'all';
  filters.value.source = 'all';
  filters.value.labels = [];
  filters.value.labelsExclude = [];
  filters.value.scenario = [];
  filters.value.scenarioExclude = [];
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

/** A play is "still live" — viewer follows the live edge instead of
 *  pinning to a fixed end_time — only if its last_seen is within
 *  LIVE_TAIL_MS of now AND it has not reached a terminal outcome.
 *  A play_end (any non-in_progress playback_status) means the play is
 *  OVER: clamp to [from,to] no matter how recently it ended, otherwise
 *  a just-finished archive opens following live (the 60 s tail alone
 *  misclassifies it for its first minute). Mirrors endOutcome's
 *  in_progress test. */
function isTerminal(r: SessionRow): boolean {
  const status = String(r.playback_status ?? '').trim();
  return status !== '' && status !== 'in_progress';
}
function isStillLive(r: SessionRow): boolean {
  if (isTerminal(r)) return false;
  const lastSeen = parseChIsoMs(r.last_seen);
  return Number.isFinite(lastSeen) && (Date.now() - lastSeen) < LIVE_TAIL_MS;
}
function endTimeFor(r: SessionRow): string {
  const lastSeen = parseChIsoMs(r.last_seen);
  if (!Number.isFinite(lastSeen)) return 'live';
  if (isStillLive(r)) return 'live';
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
  const stillLive = isStillLive(r);
  return sessionViewerURL({
    playerId: r.player_id,
    playId: r.play_id && r.play_id !== '—' ? r.play_id : undefined,
    fromMs: Number.isFinite(startMs) ? startMs : undefined,
    toMs: stillLive ? undefined : (Number.isFinite(lastSeen) ? lastSeen : undefined),
  });
}
// #736 — grouped plays by group_id (born-grouped runs carry a unique
// G<num> from row 1). One play per player_id; tag = the session number so
// the viewer's overlay legend reads S1/S2/S3. Computed once per render so
// the per-row "Compare group" check stays O(rows), not O(rows²).
const groupsById = computed(() => {
  const byGroup = new Map<string, CompareMember[]>();
  const seen = new Map<string, Set<string>>();
  for (const r of rows.value) {
    const gid = r.group_id;
    if (!gid || !r.player_id || !r.play_id || r.play_id === '—') continue;
    if (!byGroup.has(gid)) { byGroup.set(gid, []); seen.set(gid, new Set()); }
    const players = seen.get(gid)!;
    if (players.has(r.player_id)) continue;
    players.add(r.player_id);
    byGroup.get(gid)!.push({ playerId: r.player_id, playId: r.play_id, tag: r.session_id });
  }
  return byGroup;
});
function groupMembers(r: SessionRow): CompareMember[] {
  return (r.group_id ? groupsById.value.get(r.group_id) : undefined) ?? [];
}
function canCompareGroup(r: SessionRow): boolean {
  return groupMembers(r).length >= 2;
}
// Open the session-viewer in compare mode over every play in this row's
// group, windowed to the union of all members' bounds so no sibling's data
// is clipped.
function compareGroupHref(r: SessionRow): string {
  const members = groupMembers(r);
  if (members.length < 2 || !r.player_id) return '#';
  let minStart = Infinity, maxEnd = -Infinity, anyLive = false;
  for (const m of rows.value) {
    if (m.group_id !== r.group_id) continue;
    const s = m.started ? parseChIsoMs(m.started) : NaN;
    const e = m.last_seen ? parseChIsoMs(m.last_seen) : NaN;
    if (Number.isFinite(s)) minStart = Math.min(minStart, s);
    if (Number.isFinite(e)) maxEnd = Math.max(maxEnd, e);
    if (isStillLive(m)) anyLive = true;
  }
  return sessionViewerURL({
    playerId: r.player_id,
    playId: r.play_id && r.play_id !== '—' ? r.play_id : undefined,
    fromMs: Number.isFinite(minStart) ? minStart : undefined,
    toMs: anyLive ? undefined : (Number.isFinite(maxEnd) ? maxEnd : undefined),
    compare: members,
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
  const raw = Number(r.health_score) || 0;
  const s = Math.round(raw * 100) / 100; // #659: 2dp display
  const cls: HealthBadge['cls'] = raw >= 90 ? 'ok' : raw >= 70 ? 'warn' : 'bad';
  const b = r.health_breakdown || {};
  const tip = b.short_play
    ? `short play — ${b.hard_failure ? 'hard failure' : 'no failures'}${b.breach ? `, breach −${b.breach}` : ''}`
    : `rebuffer ${b.rebuffer_pct ?? 0}% · stalls ${b.stalls_per_hr ?? 0}/hr · downshift ${b.downshifts_per_min ?? 0}/min · drop ${b.drop_pct ?? 0}% · err ${b.errors_per_hr ?? 0}/hr${b.breach ? ` · breach −${b.breach}` : ''}`;
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

// #563 rate-cell formatters. Unlike fmtPct (quality: higher = better),
// these are "bad-high" — green when low, amber/red as they rise — and
// render "—" when the value is undefined (no playing time to normalise).
function fmtRatioPct(v: number | undefined, warn: number, bad: number): PctCell {
  if (v === undefined || !Number.isFinite(v)) return { label: '—', color: '#9ca3af' };
  const pct = v * 100;
  let color = '#065f46';
  if (v >= bad) color = '#991b1b';
  else if (v >= warn) color = '#92400e';
  return { label: `${pct.toFixed(3)}%`, color };
}
function fmtRate(v: number | undefined, warn: number, bad: number): PctCell {
  if (v === undefined || !Number.isFinite(v)) return { label: '—', color: '#9ca3af' };
  let color = '#065f46';
  if (v >= bad) color = '#991b1b';
  else if (v >= warn) color = '#92400e';
  return { label: v.toFixed(3), color };
}
function fmtMsDur(v: number | undefined): PctCell {
  if (v === undefined || !Number.isFinite(v) || v <= 0) return { label: '—', color: '#9ca3af' };
  const label = v >= 1000 ? `${(v / 1000).toFixed(1)}s` : `${Math.round(v)}ms`;
  let color = '#065f46';
  if (v >= 2000) color = '#991b1b';
  else if (v >= 1000) color = '#92400e';
  return { label, color };
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
  cls: 'info' | 'warning' | 'critical' | 'error' | 'testing';
}
// All chips for a row, incl. the testing tier — severity-ranked. Internal;
// the column uses labelChips() (testing filtered out) and the Test-meta pill
// uses testingMeta().
function allLabelChips(r: SessionRow): LabelChip[] {
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
      : sev === 'testing' ? 'testing'
      : 'info';
    out.push({ label, name, count, cls });
  }
  // #653: rank by severity tier first (so real errors lead, not the
  // high-frequency info churn), then count-desc within a tier.
  const sevRank: Record<LabelChip['cls'], number> = {
    critical: 0, error: 1, warning: 2, info: 3, testing: 4,
  };
  out.sort((a, b) =>
    sevRank[a.cls] - sevRank[b.cls]
    || b.count - a.count
    || a.label.localeCompare(b.label),
  );
  return out;
}

// Playback-signal chips for the Labels column. #658/#673: the testing= KV
// tier is structured run metadata, not events — it's pulled out of the chip
// flow (here) and rendered in the dedicated Scenario column (scenario()) so
// it stops competing with the error/warning chips.
function labelChips(r: SessionRow): LabelChip[] {
  return allLabelChips(r).filter((c) => c.cls !== 'testing');
}

// 4-category classification — mirrors the viewer's axis (see
// docs/EVENT_TAXONOMY.md): Actions / Injected / Conditions / Reactions.
// The list only has label strings (no per-row `fault_category`), so the
// synthesized failure rollups (segment/manifest) are ambiguous; they're
// disambiguated by a co-occurring unambiguous sibling on the same play
// (every `*segment_failure` row also carries an http_*/timeout label,
// and both land in the histogram). The `*` synthMark prefix is stripped.
type Category = 'action' | 'injected' | 'condition' | 'reaction';
const CAT_ACTION_RE    = /^(fault_on|fault_off|fault_rule|pattern_|shaper_|timeouts_changed|loop_server|session_start|session_end|server_start|content_changed|label_changed|control_change)/;
const CAT_CONDITION_RE = /^(fault_timeout|transfer_active_timeout|transfer_idle_timeout|slow_request|slow_segment|qoe_ttfb_breach|qoe_transfer_stall)/;
const CAT_INJECTED_RE  = /^(http_4xx|http_5xx|fault_other|fault_incomplete|corrupted|transport_socket)/;
const CAT_AMBIGUOUS_RE = /^(segment_failure|manifest_failure|master_manifest_failure)/;
function baseLabelName(name: string): string { return name.startsWith('*') ? name.slice(1) : name; }
function labelCategory(name: string, playTypes: Set<string>): Category {
  const n = baseLabelName(name);
  if (CAT_ACTION_RE.test(n)) return 'action';
  if (CAT_CONDITION_RE.test(n)) return 'condition';
  if (CAT_INJECTED_RE.test(n)) return 'injected';
  if (CAT_AMBIGUOUS_RE.test(n)) {
    for (const t of playTypes) if (CAT_INJECTED_RE.test(t)) return 'injected';
    for (const t of playTypes) if (CAT_CONDITION_RE.test(t)) return 'condition';
    return 'injected'; // a failure with no disambiguating sibling
  }
  return 'reaction';
}
const CATEGORY_ORDER: Category[] = ['action', 'injected', 'condition', 'reaction'];
const CATEGORY_TAG: Record<Category, { tag: string; title: string }> = {
  action:    { tag: 'act',  title: 'Actions — operator/proxy/harness config (faults, patterns, shaper, lifecycle)' },
  injected:  { tag: 'inj',  title: 'Injected faults — fabricated/destroyed responses (4xx/5xx, corrupted, socket)' },
  condition: { tag: 'cond', title: 'Conditions & results — transfer timeouts, slow segments, QoE network breaches' },
  reaction:  { tag: 'rxn',  title: 'Reactions — player behaviour (QoE, stalls, shifts)' },
};
function playTypeSet(r: SessionRow): Set<string> {
  const s = new Set<string>();
  for (const c of labelChips(r)) s.add(baseLabelName(c.name));
  return s;
}
function chipsByCategory(r: SessionRow): Record<Category, LabelChip[]> {
  const types = playTypeSet(r);
  const out: Record<Category, LabelChip[]> = { action: [], injected: [], condition: [], reaction: [] };
  for (const c of labelChips(r)) out[labelCategory(c.name, types)].push(c);
  return out;
}

// #653/#656: per-group cap before the "+N more" toggle collapses the tail.
const LABEL_CHIP_CAP = 6;

// Per-row "show all labels" state, keyed by play_id. Collapsed by default.
const expandedLabelRows = ref<Set<string>>(new Set());
function toggleLabelRow(playId: string) {
  const s = new Set(expandedLabelRows.value);
  if (s.has(playId)) s.delete(playId); else s.add(playId);
  expandedLabelRows.value = s;
}
// Visible head of one category group: all when expanded, else capped.
function visibleCategoryChips(r: SessionRow, cat: Category): LabelChip[] {
  const all = chipsByCategory(r)[cat];
  return expandedLabelRows.value.has(String(r.play_id)) ? all : all.slice(0, LABEL_CHIP_CAP);
}
// Combined hidden count across all category groups (drives "+N more").
function hiddenLabelCount(r: SessionRow): number {
  const byCat = chipsByCategory(r);
  return CATEGORY_ORDER.reduce((sum, c) => sum + Math.max(0, byCat[c].length - LABEL_CHIP_CAP), 0);
}

// #563 — human-readable "how did this play end?" from the Phase 2
// outcome fields. playback_status is the verb; playback_reason refines
// a user_stopped into the state it was in at exit (the iOS client's
// refineTerminalReason + the #556 proxy inactive_timeout). Returns a
// short label, a colour, and a tooltip carrying the raw status·reason.
function endOutcome(r: SessionRow): { label: string; color: string; tip: string } {
  const status = String(r.playback_status ?? '').trim();
  const reason = String(r.playback_reason ?? '').trim();
  const grey = '#9ca3af', green = '#065f46', amber = '#92400e', red = '#991b1b', neutral = '#374151';
  const tip = status ? `${status}${reason ? ' · ' + reason : ''}` : 'in progress';
  if (!status || status === 'in_progress') return { label: 'Playing', color: grey, tip };
  if (status === 'completed') return { label: 'Completed', color: green, tip };
  if (status === 'start_failure') return { label: 'Startup failed', color: red, tip };
  if (status === 'mid_stream_failure') return { label: 'Mid-stream fail', color: red, tip };
  if (status === 'abandoned_start') return { label: 'Abandoned start', color: amber, tip };
  if (status === 'user_stopped') {
    if (reason.startsWith('ended_stalling')) return { label: 'Quit (stalled)', color: amber, tip };
    if (reason.startsWith('ended_buffering')) return { label: 'Quit (buffering)', color: amber, tip };
    if (reason === 'app_backgrounded') return { label: 'Backgrounded', color: neutral, tip };
    if (reason === 'app_terminated') return { label: 'App killed', color: neutral, tip };
    if (reason === 'inactive_timeout') return { label: 'Timed out', color: amber, tip };
    if (reason === 'next_content_selected') return { label: 'Switched', color: neutral, tip };
    return { label: 'User quit', color: neutral, tip };
  }
  if (reason === 'inactive_timeout') return { label: 'Timed out', color: amber, tip };
  return { label: status.replace(/_/g, ' '), color: neutral, tip };
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
  { key: 'scenario',         label: 'Categories', type: 'string' as const,  sortable: false },
  { key: 'play_id',          label: 'Play ID',    type: 'string' as const,  sortable: true },
  { key: 'group_id',         label: 'Group',      type: 'string' as const,  sortable: true },
  { key: 'last_state',       label: 'State',      type: 'string' as const,  sortable: true },
  { key: 'playback_status',  label: 'Outcome',    type: 'string' as const,  sortable: true },
  { key: 'issues_count',     label: 'Issues',     type: 'number' as const,  sortable: true },
  { key: '__flags',          label: 'Flags',      type: 'string' as const,  sortable: false },
  { key: 'labels_total',     label: 'Labels',     type: 'number' as const,  sortable: true },
  { key: 'health_score',     label: 'Health',     type: 'number' as const,  sortable: true },
  // Raw counts — superseded for cross-play comparison by the rate/ratio
  // twins below, so they're demoted to the Detail toggle. Faults has no
  // rate twin (operator-driven), so it stays a default column.
  { key: 'stalls',           label: 'Stalls',     type: 'number' as const,  sortable: true, detail: true },
  { key: 'errors_count',     label: 'Errors',     type: 'number' as const,  sortable: true, detail: true },
  { key: 'faults_count',     label: 'Faults',     type: 'number' as const,  sortable: true },
  { key: 'downshifts_count', label: 'Downshifts', type: 'number' as const,  sortable: true, detail: true },
  { key: 'frames_dropped',   label: 'Drops',      type: 'number' as const,  sortable: true, detail: true },
  { key: 'avg_quality_pct',  label: 'Avg Q%',     type: 'number' as const,  sortable: true },
  { key: 'metric_events',    label: 'Metrics',    type: 'number' as const,  sortable: true },
  { key: 'net_events',       label: 'HAR',        type: 'number' as const,  sortable: true },
  // #563 — derived QoE rates. The high-signal ones are shown by default
  // (a ratio = time-impact, a frequency, + the reliability/quality
  // essentials); the niche rates (avg stall duration, total shift churn)
  // ride the Detail toggle alongside the raw counts they normalise.
  { key: 'rebuffer_ratio',     label: 'Rebuf %',   type: 'number' as const, sortable: true },
  { key: 'stalls_per_hr',      label: 'Stalls/hr', type: 'number' as const, sortable: true },
  { key: 'downshifts_per_min', label: 'Downsh/min', type: 'number' as const, sortable: true },
  { key: 'drop_ratio',         label: 'Drop %',    type: 'number' as const, sortable: true },
  { key: 'errors_per_hr',      label: 'Err/hr',    type: 'number' as const, sortable: true },
  { key: 'mean_stall_ms',      label: 'Stall avg', type: 'number' as const, sortable: true, detail: true },
  { key: 'shifts_per_min',     label: 'Shifts/min', type: 'number' as const, sortable: true, detail: true },
  { key: '__bundle',         label: '',           type: 'string' as const,  sortable: false },
];

// #563 — the high-signal rates are on by default; the "Detail" toggle
// reveals the raw counts they supersede + the niche rates. Persisted so
// an operator's choice sticks.
const DETAIL_KEY = 'ismSessionsShowDetail';
const showDetail = ref<boolean>((() => {
  try { return localStorage.getItem(DETAIL_KEY) === '1'; } catch { return false; }
})());
watch(showDetail, (v) => {
  try { localStorage.setItem(DETAIL_KEY, v ? '1' : '0'); } catch { /* ignore */ }
});
// Header loop iterates this so detail columns appear/disappear with the
// toggle; the body cells gate on showDetail in the same position.
const visibleColumns = computed(() => COLUMNS.filter((c) => showDetail.value || !(c as any).detail));

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

      <LabelFrequencyTable style="margin-bottom: 1rem;" />

      <div class="panel">
        <div class="panel-header">
          <div class="panel-title">All Sessions <span v-if="loading" class="status-message">loading…</span></div>
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

            <div class="class-chip-wrap" title="Filter by session origin — Manual (user-initiated), Sweep (overnight sweep experiments, testing=sweep_/exp_id_), or Automated (characterization runs, testing=test_/run_id_).">
              <span class="ctrl-label-text">Source:</span>
              <button
                v-for="c in ([
                  { value: 'all',       label: 'All' },
                  { value: 'manual',    label: '👤 Manual' },
                  { value: 'sweep',     label: '🌙 Sweep' },
                  { value: 'automated', label: '🧪 Automated' }
                ] as const)"
                :key="c.value"
                type="button"
                class="class-chip"
                :class="{ active: filters.source === c.value }"
                @click="filters.source = c.value"
              >{{ c.label }}</button>
            </div>

            <div class="class-chip-wrap" title="Filter by event kind (docs/EVENT_TAXONOMY.md): act=actions, inj=injected faults, cond=conditions/results, rxn=player reactions. Multi-select; keeps plays with ≥1 label in any selected kind.">
              <span class="ctrl-label-text">Event kind:</span>
              <button
                v-for="cat in CATEGORY_ORDER"
                :key="cat"
                type="button"
                class="class-chip"
                :class="['cat-chip-' + cat, { active: filters.categories.includes(cat) }]"
                :title="CATEGORY_TAG[cat].title"
                @click="toggleCategoryFilter(cat)"
              >{{ CATEGORY_TAG[cat].tag }}</button>
            </div>

            <!-- #673 — platform / test / content are now facets in the
                 Categorical filter panel below (folded out of these dropdowns). -->

            <button type="button" class="btn btn-secondary" @click="clearFilters">Clear filters</button>
            <span class="match-count">{{ matchCount }}</span>
            <!-- #563 — high-signal rates show by default; this reveals the
                 raw counts they supersede + the niche rates (stall avg,
                 total shift churn). -->
            <label class="ctrl-label rates-toggle" title="Show raw counts (stalls, errors, downshifts, drops) and the niche rates (stall avg, shifts/min)">
              <input type="checkbox" v-model="showDetail" />
              <span class="ctrl-label-text">Detail</span>
            </label>
          </div>

          <!-- Hierarchical labels filter (issue #474 follow-up).
               Mirrors SessionDisplay's Focus Window event-filter
               accordion: severity tier headers → expand → distinct
               labels at that tier with click-to-toggle inclusion.
               Multi-select OR semantics. -->
          <!-- Scenario facet filter (OS / device / segment / build / …). Same
               tristate accordion as the label filter; values come from each
               play's scenario object (player metrics), so it covers manual and
               harness plays alike. -->
          <div class="label-filter-accordion" v-if="hasAnyScenario">
            <div class="label-filter-head">
              <span class="ctrl-label-text">Categorical filter:</span>
              <button
                v-if="scenarioFilterCount"
                type="button"
                class="btn btn-secondary label-clear"
                @click="clearScenarioFilter"
              >Clear ({{ scenarioFilterCount }})</button>
            </div>
            <div
              v-for="g in scenarioFacets"
              :key="g.key"
              class="lf-tier"
              :class="{ expanded: expandedScenario[g.key], 'tier-active': scenarioGroupState(g.key) !== 'none' }"
              :style="{ '--tier-bg': '#f1f5f9', '--tier-border': '#cbd5e1', '--tier-color': '#334155' }"
            >
              <div class="lf-tier-head">
                <button
                  type="button"
                  class="lf-chevron"
                  @click="toggleScenarioGroup(g.key)"
                  :title="expandedScenario[g.key] ? 'Collapse' : 'Expand'"
                >{{ expandedScenario[g.key] ? '▾' : '▸' }}</button>
                <button
                  type="button"
                  class="lf-tier-name"
                  @click="toggleScenarioAll(g.key, $event)"
                  :title="`Cycle ALL ${g.entries.length} ${g.label} values:  none → include → exclude → none`"
                >
                  <span class="lf-tier-dot" :class="'sel-' + scenarioGroupState(g.key)" aria-hidden="true" />
                  <span class="lf-tier-text">{{ g.label }}</span>
                  <span class="lf-tier-pill">{{ g.total }}</span>
                </button>
                <div class="lf-preview" v-if="!expandedScenario[g.key] && g.total">
                  <button
                    v-for="e in g.entries.slice(0, 5)"
                    :key="e.value"
                    type="button"
                    class="lf-preview-chip"
                    :class="'state-' + scenarioState(g.key, e.value)"
                    @click.stop="cycleScenarioFilter(g.key, e.value)"
                    :title="`${g.label}=${e.value}\nclick: none → include → exclude → none`"
                  >{{ e.value }} · {{ e.count }}</button>
                  <span v-if="g.entries.length > 5" class="lf-preview-more">
                    +{{ g.entries.length - 5 }} more
                  </span>
                </div>
              </div>
              <div class="lf-tier-body" v-if="expandedScenario[g.key] && g.total">
                <button
                  v-for="e in g.entries"
                  :key="e.value"
                  type="button"
                  class="lf-label-row"
                  :class="'state-' + scenarioState(g.key, e.value)"
                  @click="cycleScenarioFilter(g.key, e.value)"
                  :title="`${g.label}=${e.value}\nclick: none → include → exclude → none`"
                >
                  <span class="lf-label-check">{{
                    scenarioState(g.key, e.value) === 'include' ? '✓'
                    : scenarioState(g.key, e.value) === 'exclude' ? '⊘'
                    : '○'
                  }}</span>
                  <span class="lf-label-name">{{ e.value }}</span>
                  <span class="lf-label-count">{{ e.count }}</span>
                </button>
              </div>
            </div>
            <div class="lf-subhead" v-if="hasAnyHarness">Test config <span class="lf-subhead-hint">(harness-run plays)</span></div>
            <div
              v-for="g in harnessFacets"
              :key="g.key"
              class="lf-tier"
              :class="{ expanded: expandedScenario[g.key], 'tier-active': scenarioGroupState(g.key) !== 'none' }"
              :style="{ '--tier-bg': '#eef2ff', '--tier-border': '#c7d2fe', '--tier-color': '#3730a3' }"
            >
              <div class="lf-tier-head">
                <button
                  type="button"
                  class="lf-chevron"
                  @click="toggleScenarioGroup(g.key)"
                  :title="expandedScenario[g.key] ? 'Collapse' : 'Expand'"
                >{{ expandedScenario[g.key] ? '▾' : '▸' }}</button>
                <button
                  type="button"
                  class="lf-tier-name"
                  @click="toggleScenarioAll(g.key, $event)"
                  :title="`Cycle ALL ${g.entries.length} ${g.label} values:  none → include → exclude → none`"
                >
                  <span class="lf-tier-dot" :class="'sel-' + scenarioGroupState(g.key)" aria-hidden="true" />
                  <span class="lf-tier-text">{{ g.label }}</span>
                  <span class="lf-tier-pill">{{ g.total }}</span>
                </button>
                <div class="lf-preview" v-if="!expandedScenario[g.key] && g.total">
                  <button
                    v-for="e in g.entries.slice(0, 5)"
                    :key="e.value"
                    type="button"
                    class="lf-preview-chip"
                    :class="'state-' + scenarioState(g.key, e.value)"
                    @click.stop="cycleScenarioFilter(g.key, e.value)"
                    :title="`${g.label}=${e.value}\nclick: none → include → exclude → none`"
                  >{{ e.value }} · {{ e.count }}</button>
                  <span v-if="g.entries.length > 5" class="lf-preview-more">
                    +{{ g.entries.length - 5 }} more
                  </span>
                </div>
              </div>
              <div class="lf-tier-body" v-if="expandedScenario[g.key] && g.total">
                <button
                  v-for="e in g.entries"
                  :key="e.value"
                  type="button"
                  class="lf-label-row"
                  :class="'state-' + scenarioState(g.key, e.value)"
                  @click="cycleScenarioFilter(g.key, e.value)"
                  :title="`${g.label}=${e.value}\nclick: none → include → exclude → none`"
                >
                  <span class="lf-label-check">{{
                    scenarioState(g.key, e.value) === 'include' ? '✓'
                    : scenarioState(g.key, e.value) === 'exclude' ? '⊘'
                    : '○'
                  }}</span>
                  <span class="lf-label-name">{{ e.value }}</span>
                  <span class="lf-label-count">{{ e.count }}</span>
                </button>
              </div>
            </div>
          </div>

          <div class="label-filter-accordion" v-if="hasAnyLabels">
            <div class="label-filter-head">
              <span class="ctrl-label-text">Results filter:</span>
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
                    v-for="col in visibleColumns"
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
                  :class="[{ 'row-critical': r.is_critical }, 'row-health-' + fmtHealthBadge(r).cls]"
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
                  <td class="cell-player">
                    <span
                      v-if="r.player_id"
                      class="player-filter"
                      :class="{ 'filter-active': filters.player_id === r.player_id }"
                      :title="filters.player_id === r.player_id ? 'Filtering to this player — click to clear' : 'Click to filter the list to this player_id'"
                      @click.stop="filterByPlayer(r.player_id)"
                    >{{ r.player_id }}</span>
                  </td>
                  <td>
                    <div>{{ r.content_id || '' }}</div>
                  </td>
                  <td class="cell-scenario">
                    <span
                      class="scenario-chip scenario-source"
                      :class="'source-' + sessionSource(r) + (filters.source === sessionSource(r) ? ' source-active' : '')"
                      :title="`source=${sessionSource(r)}\nclick: filter to ${sessionSource(r)} plays (click again to clear)`"
                      @click.stop="filterBySource(r)"
                    ><span class="scenario-key">src</span>{{ SOURCE_LABEL[sessionSource(r)] }}</span>
                    <template v-if="scenario(r)">
                      <span
                        v-for="f in scenario(r)!.fields"
                        :key="f.key"
                        class="scenario-chip"
                        :class="['scenario-' + f.key, { 'chip-active': isCategoryActive(f.key, f.value) }]"
                        :title="`${f.label}=${f.value}\nclick: filter to this ${f.label} (click again to clear)`"
                        @click.stop="filterByCategory(f.key, f.value)"
                      ><span class="scenario-key">{{ f.label }}</span>{{ f.value }}</span>
                    </template>
                  </td>
                  <td class="cell-play-id">
                    <a v-if="r.play_id && r.player_id" :href="viewerHref(r)" class="play-id-link">{{ r.play_id }}</a>
                    <template v-else>{{ r.play_id || '' }}</template>
                  </td>
                  <td class="cell-group-id">
                    <a
                      v-if="canCompareGroup(r)"
                      :href="compareGroupHref(r)"
                      class="group-compare-link"
                      :title="`Open all ${groupMembers(r).length} grouped plays in compare mode`"
                      @click.stop
                    >{{ r.group_id }}</a>
                    <span v-else-if="r.group_id" class="group-id-plain" :title="r.group_id">{{ r.group_id }}</span>
                    <span v-else class="dash">—</span>
                  </td>
                  <td>{{ r.last_state || '' }}</td>
                  <td><span :style="{ color: endOutcome(r).color, fontWeight: 600 }" :title="endOutcome(r).tip">{{ endOutcome(r).label }}</span></td>
                  <td>
                    <span class="issue-badge" :class="'issue-' + fmtIssuesBadge(r).cls" :title="fmtIssuesBadge(r).tip">{{ fmtIssuesBadge(r).count }}</span>
                  </td>
                  <td>
                    <span v-for="(f, i) in flagChips(r)" :key="i" class="flag-chip" :style="{ background: f.color }" :title="f.tip">{{ f.icon }} {{ f.count }}</span>
                    <span v-if="flagChips(r).length === 0" class="dash">—</span>
                  </td>
                  <td class="cell-labels">
                    <template v-for="cat in CATEGORY_ORDER" :key="cat">
                      <template v-if="visibleCategoryChips(r, cat).length">
                        <span class="label-group-tag" :class="'cat-tag-' + cat" :title="CATEGORY_TAG[cat].title">{{ CATEGORY_TAG[cat].tag }}</span>
                        <span
                          v-for="chip in visibleCategoryChips(r, cat)"
                          :key="cat + '-' + chip.label"
                          class="label-chip"
                          :class="['label-' + chip.cls, 'cat-' + cat]"
                          :title="chip.label"
                        >{{ chip.count }}× {{ chip.name }}</span>
                      </template>
                    </template>
                    <button
                      v-if="hiddenLabelCount(r) > 0 && !expandedLabelRows.has(String(r.play_id))"
                      type="button"
                      class="label-more"
                      title="Show all labels"
                      @click.stop="toggleLabelRow(String(r.play_id))"
                    >+{{ hiddenLabelCount(r) }} more</button>
                    <button
                      v-else-if="expandedLabelRows.has(String(r.play_id))"
                      type="button"
                      class="label-more"
                      title="Show fewer"
                      @click.stop="toggleLabelRow(String(r.play_id))"
                    >show less</button>
                    <span v-if="labelChips(r).length === 0" class="dash">—</span>
                  </td>
                  <td>
                    <span class="health-badge" :class="'issue-' + fmtHealthBadge(r).cls" :title="fmtHealthBadge(r).tip">{{ fmtHealthBadge(r).score }}</span>
                  </td>
                  <td v-if="showDetail"><span :style="{ color: fmtCount(r.stalls, 1, 5).color, fontWeight: fmtCount(r.stalls, 1, 5).bold ? 600 : 400 }">{{ fmtCount(r.stalls, 1, 5).n }}</span></td>
                  <td v-if="showDetail"><span :style="{ color: fmtCount(r.errors_count, 1, 1).color, fontWeight: fmtCount(r.errors_count, 1, 1).bold ? 600 : 400 }">{{ fmtCount(r.errors_count, 1, 1).n }}</span></td>
                  <td><span :style="{ color: fmtCount(r.faults_count, 1, 10).color, fontWeight: fmtCount(r.faults_count, 1, 10).bold ? 600 : 400 }">{{ fmtCount(r.faults_count, 1, 10).n }}</span></td>
                  <td v-if="showDetail"><span :style="{ color: fmtCount(r.downshifts_count, 1, 5).color, fontWeight: fmtCount(r.downshifts_count, 1, 5).bold ? 600 : 400 }">{{ fmtCount(r.downshifts_count, 1, 5).n }}</span></td>
                  <td v-if="showDetail"><span :style="{ color: fmtCount(r.frames_dropped, 100, 1000).color, fontWeight: fmtCount(r.frames_dropped, 100, 1000).bold ? 600 : 400 }">{{ fmtCount(r.frames_dropped, 100, 1000).n }}</span></td>
                  <td><span :style="{ color: fmtPct(r.avg_quality_pct).color }">{{ fmtPct(r.avg_quality_pct).label }}</span></td>
                  <td>{{ r.metric_events || 0 }}</td>
                  <td>{{ r.net_events || 0 }}</td>
                  <td><span :style="{ color: fmtRatioPct(r.rebuffer_ratio, 0.002, 0.004).color }" :title="'Rebuffering ratio — fraction of engaged time spent stalling'">{{ fmtRatioPct(r.rebuffer_ratio, 0.002, 0.004).label }}</span></td>
                  <td><span :style="{ color: fmtRate(r.stalls_per_hr, 1, 5).color }" :title="'Stalls per playing-hour'">{{ fmtRate(r.stalls_per_hr, 1, 5).label }}</span></td>
                  <td><span :style="{ color: fmtRate(r.downshifts_per_min, 1, 3).color }" :title="'ABR downshifts per playing-minute'">{{ fmtRate(r.downshifts_per_min, 1, 3).label }}</span></td>
                  <td><span :style="{ color: fmtRatioPct(r.drop_ratio, 0.05, 0.2).color }" :title="'Dropped-frame ratio'">{{ fmtRatioPct(r.drop_ratio, 0.05, 0.2).label }}</span></td>
                  <td><span :style="{ color: fmtRate(r.errors_per_hr, 0.5, 2).color }" :title="'Errors per playing-hour'">{{ fmtRate(r.errors_per_hr, 0.5, 2).label }}</span></td>
                  <td v-if="showDetail"><span :style="{ color: fmtMsDur(r.mean_stall_ms).color }" :title="'Mean stall duration'">{{ fmtMsDur(r.mean_stall_ms).label }}</span></td>
                  <td v-if="showDetail"><span :style="{ color: fmtRate(r.shifts_per_min, 2, 6).color }" :title="'Total bitrate shifts per playing-minute'">{{ fmtRate(r.shifts_per_min, 2, 6).label }}</span></td>
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
.ism-content-wide {
  width: 100%;
  padding: 16px 24px;
  box-sizing: border-box;
  /* min-width: 0 + overflow-x: hidden so the inner table-wrap +
     table can shrink with the available width when the AI panel
     widens. Without this, a child that picked a min-content width
     bigger than the shrunk parent (e.g. the picker-table when many
     columns are present) pushes ism-content-wide past its parent
     and into the AI dock area. Same class of bug as
     SessionViewer.vue's .content; same fix. */
  min-width: 0;
  overflow-x: hidden;
}
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

/* #673 Scenario column — one small "key value" chip per run-identity
   field. Reuses the green test-runner palette so the visual tie between
   Sessions ↔ Characterization carries over from the old harness pill. */
.cell-scenario { min-width: 180px; max-width: 280px; line-height: 1.7; }
.scenario-chip {
  display: inline-flex;
  align-items: baseline;
  gap: 4px;
  margin: 0 4px 2px 0;
  padding: 1px 6px;
  border-radius: 10px;
  background: #ecfdf5;
  border: 1px solid #6ee7b7;
  color: #065f46;
  font: 500 10px ui-monospace, SFMono-Regular, monospace;
  white-space: nowrap;
  cursor: pointer;
}
.scenario-chip:hover { filter: brightness(0.96); box-shadow: 0 0 0 1px #6ee7b7; }
.scenario-chip.chip-active {
  background: #059669;
  border-color: #047857;
  color: #ecfdf5;
}
.scenario-chip.chip-active .scenario-key { color: #d1fae5; opacity: 0.9; }
/* Origin chip in the Categories cell — distinct tint per source. */
.scenario-source.source-manual    { background: #f8fafc; border-color: #e2e8f0; color: #475569; }
.scenario-source.source-manual .scenario-key    { color: #64748b; }
.scenario-source.source-sweep     { background: #eef2ff; border-color: #c7d2fe; color: #3730a3; }
.scenario-source.source-sweep .scenario-key     { color: #4f46e5; }
.scenario-source.source-automated { background: #faf5ff; border-color: #e9d5ff; color: #6b21a8; }
.scenario-source.source-automated .scenario-key { color: #9333ea; }
.scenario-source.source-active { box-shadow: 0 0 0 2px currentColor inset; font-weight: 700; }
/* Player cell — click to scope the list to one player_id. */
.cell-player { max-width: 260px; }
.player-filter {
  cursor: pointer;
  font: 500 11px ui-monospace, SFMono-Regular, monospace;
  border-bottom: 1px dotted #94a3b8;
}
.player-filter:hover { color: #2563eb; border-bottom-color: #2563eb; }
.player-filter.filter-active {
  color: #1d4ed8;
  font-weight: 700;
  border-bottom: 1px solid #1d4ed8;
}
.scenario-key {
  font-weight: 700;
  text-transform: uppercase;
  font-size: 8px;
  letter-spacing: 0.04em;
  color: #047857;
  opacity: 0.85;
}
/* The two harness-stamped identity fields lead — tint them so test/platform
   read as the primary "which run is this" signal vs. the device/version tail. */
.scenario-test  { background: #d1fae5; border-color: #34d399; }
.scenario-run   { opacity: 0.85; }

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
  /* Fill the viewport below the page chrome (header + filter bar) instead of
     the old fixed 60vh, so far more rows are visible before scrolling. */
  max-height: calc(100vh - 180px);
  min-height: 320px;
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
  padding: 5px 10px;
  text-align: left;
  border-bottom: 1px solid var(--border-color, #e5e7eb);
  font-weight: 600;
  color: var(--text-primary);
  white-space: nowrap;
  user-select: none;
}
.picker-table th.sortable { cursor: pointer; }
.sort-idle { color: #9ca3af; }
/* #659: compact rows — tighter vertical padding + line-height so more rows
   are visible (the #654 label cap already shrank cell height). */
.picker-table td {
  padding: 2px 8px;
  line-height: 1.25;
  border-bottom: 1px solid var(--border-color, #f3f4f6);
  position: relative;
}
.picker-table tbody tr {
  cursor: pointer;
  border-left: 4px solid transparent;
}
.picker-table tbody tr:hover { background: var(--bg-hover, #f9fafb); }
/* #659: full-row wash + left bar by re-based health band (rate + breach).
   ok is kept very faint so healthy rows stay calm and warn/bad pop. */
.picker-table tbody tr.row-health-ok   { border-left-color: #16a34a; background: #f3faf5; }
.picker-table tbody tr.row-health-warn { border-left-color: #d97706; background: #fef6e7; }
.picker-table tbody tr.row-health-bad  { border-left-color: #dc2626; background: #fdeeee; }
.picker-table tbody tr.row-health-ok:hover   { background: #e8f5ec; }
.picker-table tbody tr.row-health-warn:hover { background: #fdeed2; }
.picker-table tbody tr.row-health-bad:hover  { background: #fbe2e2; }
.picker-table tbody tr.row-critical { border-left: 4px solid #dc2626; }
.picker-table tbody tr.row-critical:hover { background: #fbe2e2; }
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
.cell-group-id { max-width: 170px; }
.group-compare-link {
  display: inline-block; max-width: 100%; padding: 1px 6px; border-radius: 4px;
  font-family: ui-monospace, monospace; font-size: 0.72rem; font-weight: 600;
  color: #6d28d9; background: #ede9fe; text-decoration: none;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap; vertical-align: bottom;
}
.group-compare-link:hover { background: #ddd6fe; text-decoration: underline; }
.group-id-plain { font-family: ui-monospace, monospace; font-size: 0.72rem; color: #6b7280; }

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
.label-testing  { background: #f1f5f9; color: #475569; border-color: #cbd5e1; }
/* Category accent (docs/EVENT_TAXONOMY.md) — a left border tints each chip
   by causal role while the background keeps the severity tint. The group
   tags + filter facet chips share the same four accents. */
.label-chip.cat-action    { border-left: 3px solid #8b5cf6; }
.label-chip.cat-injected  { border-left: 3px solid #ef4444; }
.label-chip.cat-condition { border-left: 3px solid #f59e0b; }
.label-chip.cat-reaction  { border-left: 3px solid #3b82f6; }
.cat-tag-action    { color: #7c3aed; }
.cat-tag-injected  { color: #dc2626; }
.cat-tag-condition { color: #d97706; }
.cat-tag-reaction  { color: #2563eb; }
.class-chip.cat-chip-action    { border-left: 3px solid #8b5cf6; }
.class-chip.cat-chip-injected  { border-left: 3px solid #ef4444; }
.class-chip.cat-chip-condition { border-left: 3px solid #f59e0b; }
.class-chip.cat-chip-reaction  { border-left: 3px solid #3b82f6; }
/* #653: "+N more" / "show less" toggle for the capped label list. */
.label-more {
  display: inline-block;
  padding: 1px 6px;
  margin: 0 3px 2px 0;
  border-radius: 10px;
  font: 600 11px system-ui;
  line-height: 1.4;
  white-space: nowrap;
  background: #eef2ff;
  color: #4338ca;
  border: 1px solid #c7d2fe;
  cursor: pointer;
}
.label-more:hover { background: #e0e7ff; }
/* #656: tiny uppercase divider tag preceding the injected / player chip
   groups, so cause vs effect read as distinct clusters. */
.label-group-tag {
  display: inline-block;
  margin: 0 4px 2px 0;
  font: 700 9px system-ui;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: #94a3b8;
  vertical-align: middle;
}
/* #658: compact pill for the testing= run metadata (test · platform), full
   set on hover — keeps the structured KV facts out of the playback chips. */
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
.lf-subhead {
  margin: 6px 2px 2px;
  font-size: 10px;
  font-weight: 700;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: #64748b;
}
.lf-subhead-hint {
  font-weight: 500;
  text-transform: none;
  letter-spacing: 0;
  color: #94a3b8;
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
  display: flex;
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
.lf-label-check { font-weight: 700; flex: none; }
/* Tighten the ⊘ glyph — many fonts render it overly wide. */
.lf-label-row.state-exclude .lf-label-check { letter-spacing: -1px; }
.lf-label-name {
  /* flex item that shrinks (min-width:0 + ellipsis) so the count sits right
     after the name rather than being pushed to the far edge. */
  flex: 0 1 auto;
  min-width: 0;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  font: 500 12px ui-monospace, Menlo, monospace;
}
.lf-label-count {
  flex: none;
  font: 600 11px ui-monospace, Menlo, monospace;
  opacity: 0.7;
  white-space: nowrap;
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
