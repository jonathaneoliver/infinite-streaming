<script setup lang="ts">
/**
 * Characterization.vue — run-grouped view of automated test plays.
 *
 * Queries /api/v2/plays three times (once per known test name) and
 * groups the results by `run_id_<ts>` extracted from each play's
 * `label_histogram`. One row per run with three sub-cards (rampup /
 * rampdown / pyramid). Each card links into SessionViewer.vue for the
 * full sample chart.
 *
 * PASS/FAIL is derived from the play's structured `stalls` field —
 * FAIL when stalls > 0, PASS otherwise. Known false-positive shapes
 * (cold-start step-1 buffer=0, device-capped variant missing) ride
 * on the labels themselves and are a separate tuning task.
 */
import { computed, ref, onMounted } from 'vue';
import ShellLayout from '@/components/ShellLayout.vue';
import SessionViewerLink from '@/components/SessionViewerLink.vue';
import ChatPanel from '@/components/chat/ChatPanel.vue';
import type { ChatScope } from '@/types/chat';

type TestName = 'rampup' | 'rampdown' | 'pyramid' | 'abort' | 'startup';
const TEST_NAMES: TestName[] = ['rampup', 'rampdown', 'pyramid', 'abort', 'startup'];

interface PlayRow {
  play_id: string;
  player_id: string;
  started_at: string;
  last_seen_at: string | null;
  last_state: string | null;
  stalls: number | string;
  bitrate_shifts: number | string;
  resolution_changes: number | string;
  dropped_frames: number;
  error_event_count: number | string;
  first_frame_s: number | null;
  last_player_error: string | null;
  label_histogram: [string, string][];
}

interface ApiResponse {
  items: PlayRow[] | null;
}

const RANGES = [
  { id: '1d',  label: 'Last 24 hours', hours: 24 },
  { id: '3d',  label: 'Last 3 days',   hours: 72 },
  { id: '7d',  label: 'Last 7 days',   hours: 168 },
  { id: '30d', label: 'Last 30 days',  hours: 720 },
];
const PLATFORMS = ['all', 'iphone', 'ipad-sim', 'apple-tv', 'android-tv', 'web'];

const activeRangeId = ref('7d');
const platformFilter = ref('all');
const loading = ref(false);
const error = ref('');
const allPlays = ref<Array<PlayRow & { test: TestName }>>([]);

// One entry per (run_id, test_name) — server-side characterization_runs row.
// Populated by fetchCharacterizationRuns and consumed when building cards
// so the summary metrics (lossless decimal lowest_sustainable etc) appear
// alongside the play's structured signals.
interface CharRunRow {
  run_id: string;
  test_name: string;
  platform: string;
  started_at_str?: string;
  ended_at_str?: string;
  player_id: string;
  play_ids: string[];
  passed: number;
  summary_json: string;
}
interface CharRunSummary {
  total_stalls?: number;
  total_stall_seconds?: number;
  profile_shifts?: number;
  dropped_frames?: number;
  sample_count?: number;
  mean_bitrate_mbps?: number;
  min_bitrate_mbps?: number;
  max_bitrate_mbps?: number;
  lowest_sustainable_cap_mbps?: number;
  highest_stalling_cap_mbps?: number;
  bottom_variant_floor_mbps?: number;
  variant_sample_counts?: number[];
}
const charRuns = ref<Map<string, CharRunRow & { summary?: CharRunSummary }>>(new Map());

function charRunKey(runID: string, testName: string): string {
  return runID + '|' + testName;
}

function asNumber(v: number | string | null | undefined): number {
  if (v == null) return 0;
  const n = typeof v === 'number' ? v : Number(v);
  return Number.isFinite(n) ? n : 0;
}

function findLabel(p: PlayRow, prefix: string): string | null {
  for (const [label] of p.label_histogram || []) {
    if (label.startsWith(prefix)) return label.slice(prefix.length);
  }
  return null;
}

async function fetchOneTest(name: TestName, hours: number): Promise<PlayRow[]> {
  const from = new Date(Date.now() - hours * 3600 * 1000).toISOString();
  const qs = new URLSearchParams();
  qs.append('label_has', `info=test_${name}`);
  qs.append('from', from);
  qs.append('limit', '200');
  const resp = await fetch('/analytics/api/v2/plays?' + qs.toString());
  if (!resp.ok) throw new Error(`/api/v2/plays ${name}: ${resp.status}`);
  const data: ApiResponse = await resp.json();
  return data.items ?? [];
}

async function fetchCharacterizationRuns(hours: number): Promise<CharRunRow[]> {
  const from = new Date(Date.now() - hours * 3600 * 1000).toISOString();
  const qs = new URLSearchParams();
  qs.append('from', from);
  qs.append('limit', '500');
  const resp = await fetch('/analytics/api/v2/characterization-runs?' + qs.toString());
  if (!resp.ok) throw new Error(`/api/v2/characterization-runs: ${resp.status}`);
  const data = await resp.json();
  return (data.items as CharRunRow[]) ?? [];
}

async function refresh() {
  const range = RANGES.find(r => r.id === activeRangeId.value);
  if (!range) return;
  loading.value = true;
  error.value = '';
  try {
    const [results, charList] = await Promise.all([
      Promise.all(TEST_NAMES.map(async (t) => {
        const rows = await fetchOneTest(t, range.hours);
        return rows.map(r => ({ ...r, test: t }));
      })),
      fetchCharacterizationRuns(range.hours).catch((e) => {
        // The forwarder may not have the endpoint yet (older deploy).
        // Don't fail the whole page — just leave summary metrics blank.
        console.warn('characterization-runs fetch skipped:', e);
        return [] as CharRunRow[];
      }),
    ]);
    allPlays.value = results.flat();
    const m = new Map<string, CharRunRow & { summary?: CharRunSummary }>();
    for (const row of charList) {
      let summary: CharRunSummary | undefined;
      try {
        summary = row.summary_json ? JSON.parse(row.summary_json) : undefined;
      } catch (e) {
        summary = undefined;
      }
      m.set(charRunKey(row.run_id, row.test_name), { ...row, summary });
    }
    charRuns.value = m;
    // Drop any expanded-step cache from a previous refresh — the
    // underlying play_ids may have changed.
    expandedSteps.value = new Map();
  } catch (e: any) {
    error.value = String(e?.message ?? e);
    allPlays.value = [];
  } finally {
    loading.value = false;
  }
}

onMounted(refresh);

interface RunCard {
  test: TestName;
  play: PlayRow;
  passed: boolean;
  stalls: number;
  shifts: number;
  duration: string;
  // From the server-side characterization_runs row. Optional — falls
  // back to play-row-only data when no row exists (older runs).
  summary?: CharRunSummary;
  // Whether we have a characterization_runs row for this card → drives
  // the Steps ▾ button visibility.
  hasReport: boolean;
}

// Per-step detail rows from the report blob. Mirrors runner.Step but
// only the fields we actually render.
interface StepRow {
  rate_mbps: number;
  hold_s?: number;
  variant?: { resolution: string; margin_pct?: number };
  exit_reason?: string;
  hold_actual_s?: number;
  min_buffer_s?: number;
  max_buffer_s?: number;
  // Buffer envelope endpoints — first / last sample's buffer in the
  // step's [started_at, ended_at] window. With min/max above, the
  // four-tuple paints the full per-step buffer story without
  // opening the session viewer.
  buffer_at_start_s?: number;
  buffer_at_end_s?: number;
  stalls_delta?: number;
  shifts_delta?: number;
  // Per-step bandwidth aggregates. Mean is the average across samples;
  // max is the peak. Both video (what the player picked) and network
  // (what the proxy delivered) are reported separately.
  mean_bitrate_mbps?: number;
  max_bitrate_mbps?: number;
  mean_network_bitrate_mbps?: number;
  max_network_bitrate_mbps?: number;
  // Optional explicit window — populated by the Go runner; stepWindow
  // derives cumulatively from report.started_at + prior steps' hold
  // when absent.
  started_at?: string;
}

interface AbortCycleRow {
  cycle_idx?: number;
  fault_shape?: string;
  pre_variant?: string;
  pre_buffer_s?: number;
  pre_bw_est_mbps?: number;
  armed_at?: string;
  abort_detected?: boolean;
  abort_kind?: string;
  abort_at_s?: number;
  abort_url?: string;
  retry_found?: boolean;
  retry_had_range?: boolean;
  retry_range_start?: number;
  player_stalled?: boolean;
  downshifted_to?: string;
  downshift_after_s?: number;
  recovery_s?: number;
  post_bw_est_mbps?: number;
}

interface StartupCycleRow {
  cycle_idx?: number;
  boundary_type?: string;
  content_clip_id?: string;
  cap_mbps?: number;
  started_at?: string;
  player_id?: string;
  // The new play started by this cycle's boundary. Populated by the
  // Go runner from the first sample whose play_id != pre_play_id;
  // see StartupCycleResult.PlayID. Required for SessionViewerLink.
  play_id?: string;
  first_master_at_s?: number;
  first_variant_at_s?: number;
  first_segment_at_s?: number;
  first_variant_picked?: string;
  time_to_first_frame_s?: number;
  reached_5s_buffer_at_s?: number;
  reached_15s_buffer_at_s?: number;
  variant_at_5s?: string;
  variant_at_15s?: string;
  variant_at_30s?: string;
  upshifts_in_30s?: number;
  downshifts_in_30s?: number;
  stalls_in_30s?: number;
  dropped_frames_in_30s?: number;
  settled_variant?: string;
  network_bitrate_at_start_mbps?: number;
  network_bitrate_at_30s_mbps?: number;
}

interface ReportBlob {
  mode?: string;
  platform?: string;
  device?: { label?: string; udid?: string };
  player_id?: string;
  play_ids?: string[];
  started_at?: string;
  ended_at?: string;
  steps?: StepRow[];
  variants?: Array<{ resolution: string; avg_bps?: number; peak_bps?: number; source?: string }>;
  summary?: CharRunSummary;
  abort_cycles?: AbortCycleRow[];
  startup_cycles?: StartupCycleRow[];
}

// Per-card expand state + cache. Map key = (run_id, test_name).
const expandedSteps = ref<Map<string, { open: boolean; report?: ReportBlob; loading?: boolean; error?: string }>>(new Map());

// Chat scope: narrows to a single (run_id, test_name) when exactly
// one Details card is expanded. If zero or 2+ cards are open the
// chat falls back to characterization-fleet scope so the bot reasons
// about the whole page.
const chatScope = computed<ChatScope>(() => {
  const open: { run_id: string; test_name: string }[] = [];
  for (const [key, entry] of expandedSteps.value.entries()) {
    if (entry.open) {
      const [run_id, test_name] = key.split('|');
      open.push({ run_id, test_name });
    }
  }
  if (open.length === 1) {
    return { kind: 'characterization', run_id: open[0].run_id, test_name: open[0].test_name };
  }
  return { kind: 'characterization' };
});

async function toggleSteps(runID: string, testName: TestName) {
  const key = charRunKey(runID, testName);
  const m = new Map(expandedSteps.value);
  const cur = m.get(key);
  if (cur?.open) {
    m.set(key, { ...cur, open: false });
    expandedSteps.value = m;
    return;
  }
  // Already-loaded — just reopen.
  if (cur?.report) {
    m.set(key, { ...cur, open: true });
    expandedSteps.value = m;
    return;
  }
  // First time → fetch.
  m.set(key, { open: true, loading: true });
  expandedSteps.value = m;
  try {
    const resp = await fetch(`/analytics/api/v2/characterization-runs/${encodeURIComponent(runID)}/${encodeURIComponent(testName)}`);
    if (!resp.ok) throw new Error(`detail fetch ${resp.status}`);
    const row = await resp.json();
    let report: ReportBlob | undefined;
    if (row?.report_json) {
      try { report = JSON.parse(row.report_json); } catch { report = undefined; }
    }
    const m2 = new Map(expandedSteps.value);
    m2.set(key, { open: true, report, loading: false });
    expandedSteps.value = m2;
  } catch (e: any) {
    const m2 = new Map(expandedSteps.value);
    m2.set(key, { open: true, loading: false, error: String(e?.message ?? e) });
    expandedSteps.value = m2;
  }
}

function fmtMbps(v: number | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return v.toFixed(3) + ' Mbps';
}
function fmtPct(v: number | undefined): string {
  if (v == null || !Number.isFinite(v)) return '';
  return (v >= 0 ? '+' : '') + v + '%';
}
function fmtSeconds(v: number | undefined): string {
  if (v == null || !Number.isFinite(v)) return '—';
  if (v < 60) return v.toFixed(0) + 's';
  return Math.floor(v / 60) + 'm' + Math.floor(v % 60) + 's';
}

interface RunGroup {
  run_id: string;
  platform: string;
  earliest: string;
  latest: string;
  cards: Partial<Record<TestName, RunCard>>;
}

function isoDurationShort(fromISO: string, toISO: string | null): string {
  if (!toISO) return '—';
  const ms = new Date(toISO).getTime() - new Date(fromISO).getTime();
  if (!Number.isFinite(ms) || ms < 0) return '—';
  const total = Math.round(ms / 1000);
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h) return `${h}h${m}m`;
  if (m) return `${m}m${s}s`;
  return `${s}s`;
}

const grouped = computed<RunGroup[]>(() => {
  const byRun = new Map<string, RunGroup>();
  for (const p of allPlays.value) {
    const runID = findLabel(p, 'info=run_id_') ?? '(no-run-id)';
    const platform = findLabel(p, 'info=platform_') ?? 'unknown';
    if (platformFilter.value !== 'all' && platform !== platformFilter.value) continue;

    let g = byRun.get(`${runID}|${platform}`);
    if (!g) {
      g = { run_id: runID, platform, earliest: p.started_at, latest: p.last_seen_at ?? p.started_at, cards: {} };
      byRun.set(`${runID}|${platform}`, g);
    }
    if (p.started_at < g.earliest) g.earliest = p.started_at;
    const end = p.last_seen_at ?? p.started_at;
    if (end > g.latest) g.latest = end;
    const charRow = charRuns.value.get(charRunKey(runID, p.test));
    g.cards[p.test] = {
      test: p.test,
      play: p,
      passed: asNumber(p.stalls) === 0,
      stalls: asNumber(p.stalls),
      shifts: asNumber(p.bitrate_shifts),
      duration: isoDurationShort(p.started_at, p.last_seen_at),
      summary: charRow?.summary,
      hasReport: !!charRow,
    };
  }
  return Array.from(byRun.values())
    .sort((a, b) => (a.earliest < b.earliest ? 1 : -1)); // newest first
});

function shortRunID(runID: string): string {
  // run_id is a UTC timestamp the test framework stamps at start, e.g.
  // "20260521T155558Z". Convert to the browser's local timezone for
  // display — UTC stays the canonical wire value (per project memory:
  // local for display, UTC for storage).
  const m = runID.match(/^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/);
  if (!m) return runID;
  const iso = `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`;
  const d = new Date(iso);
  if (isNaN(d.getTime())) return runID;
  // YYYY-MM-DD HH:MM in the user's local zone, with a small tz hint
  // so it's unambiguous on a screen that may also show wire UTC.
  const y = d.getFullYear();
  const mo = String(d.getMonth() + 1).padStart(2, '0');
  const da = String(d.getDate()).padStart(2, '0');
  const h = String(d.getHours()).padStart(2, '0');
  const mi = String(d.getMinutes()).padStart(2, '0');
  return `${y}-${mo}-${da} ${h}:${mi}`;
}

function playerShort(p: string): string {
  return p ? p.slice(0, 8) : '';
}

function playViewerHref(playID: string, playerID: string): string {
  const qs = new URLSearchParams({ play_id: playID, player_id: playerID });
  return `/dashboard/v3/session-viewer.html?${qs}`;
}

function startedAtLocal(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleString();
}

// Cycle-window helpers — convert a cycle's per-test timing surface
// into a [startMs, endMs] pair the SessionViewerLink can render. The
// link itself adds ±10s pre/post-roll, so these return the cycle's
// canonical bounds (no padding).
//
// Observation windows are constants here mirroring the Go side
// (startupObserveWindow / abortObserveWindow). Keep in sync — when a
// test's window changes, update both.
const STARTUP_OBSERVE_WINDOW_MS = 30_000;
const ABORT_OBSERVE_WINDOW_MS = 60_000;

function startupCycleWindow(c: StartupCycleRow): { startMs: number; endMs: number } {
  const startMs = c.started_at ? Date.parse(c.started_at) : NaN;
  return { startMs, endMs: startMs + STARTUP_OBSERVE_WINDOW_MS };
}
function abortCycleWindow(c: AbortCycleRow): { startMs: number; endMs: number } {
  const startMs = c.armed_at ? Date.parse(c.armed_at) : NaN;
  return { startMs, endMs: startMs + ABORT_OBSERVE_WINDOW_MS };
}

// stepWindow derives the [startMs, endMs] for a single step in a
// non-cycle-style test (rampup / rampdown / pyramid). Steps don't
// carry explicit timestamps in older runner output, so fall back to
// cumulative hold-time from report.started_at:
//   step[0].start  = report.started_at
//   step[i].start  = report.started_at + sum(steps[0..i-1].hold_actual_s)
//   step[i].end    = step[i].start + step[i].hold_actual_s
// Inaccurate by whatever overhead the runner has between steps
// (variant probe, settle wait, etc.) — close enough for a viewer
// pre-roll. The runner can ship a per-step started_at later and
// this helper transparently prefers it.
function stepWindow(report: ReportBlob, stepIdx: number): { startMs: number; endMs: number } {
  const steps = report.steps ?? [];
  const s = steps[stepIdx];
  if (!s) return { startMs: NaN, endMs: NaN };
  let startMs: number;
  if (s.started_at) {
    startMs = Date.parse(s.started_at);
  } else {
    const reportStart = report.started_at ? Date.parse(report.started_at) : NaN;
    if (!Number.isFinite(reportStart)) return { startMs: NaN, endMs: NaN };
    let offsetMs = 0;
    for (let i = 0; i < stepIdx; i++) {
      const prev = steps[i];
      offsetMs += ((prev?.hold_actual_s ?? prev?.hold_s ?? 0)) * 1000;
    }
    startMs = reportStart + offsetMs;
  }
  const holdMs = ((s.hold_actual_s ?? s.hold_s ?? 0)) * 1000;
  return { startMs, endMs: startMs + holdMs };
}
</script>

<template>
  <ShellLayout active-page="characterization">
    <template #header>
      <div class="page-title-bar">Automated Testing</div>
    </template>

    <main class="ism-content-wide">
      <div class="page-header">
        <div class="page-title">Automated Testing</div>
        <div class="page-subtitle">
          Characterization runs grouped by run_id. Each run produces up to three plays (rampup / rampdown / pyramid).
          Click a card to open the play in the Session Viewer.
        </div>
        <div class="page-callout">
          <strong>How to run:</strong> Characterization tests run from the developer host, not this UI — this page only displays results that have already been archived. Trigger a run with <code>go test</code> on the host:
          <pre class="page-callout-code">go test -C tests/characterization ./modes/... -v \
  -run <span class="muted">TestStartupIPadSim</span> \
  -timeout 30m -count=1 -launch-mode=appium</pre>
          Available <code>-run</code> values: <code>TestStartupIPadSim</code>, <code>TestAbortIPadSim</code>, <code>TestRampupIPadSim</code> (and <code>IPhone</code>, <code>AppleTV</code>, <code>AndroidTV</code>, <code>Web</code> variants per test). The run uploads its report via <code>harness post characterization</code> and lands here within a few seconds.
        </div>
      </div>

      <div class="panel">
        <div class="panel-header">
          <div class="panel-title">
            Runs
            <span v-if="loading" class="status-message">loading…</span>
            <span v-else class="status-message">{{ grouped.length }} run<span v-if="grouped.length !== 1">s</span></span>
          </div>
        </div>

        <div class="picker-wrap">
          <div class="range-row">
            <label class="ctrl-label">
              <span>Time range:</span>
              <select v-model="activeRangeId" @change="refresh" class="ctrl-input">
                <option v-for="r in RANGES" :key="r.id" :value="r.id">{{ r.label }}</option>
              </select>
            </label>
            <label class="ctrl-label">
              <span>Platform:</span>
              <select v-model="platformFilter" class="ctrl-input">
                <option v-for="p in PLATFORMS" :key="p" :value="p">{{ p }}</option>
              </select>
            </label>
            <button type="button" class="btn btn-secondary" @click="refresh">Refresh</button>
            <span v-if="error" class="error">{{ error }}</span>
          </div>
        </div>

        <div v-if="!loading && grouped.length === 0" class="empty">
          No characterization runs in this window. Kick one off with <code>make characterize-iphone</code> (or another platform).
        </div>

        <div v-for="g in grouped" :key="g.run_id + '|' + g.platform" class="run-row">
          <div class="run-row-header">
            <span class="run-id">{{ shortRunID(g.run_id) }}</span>
            <span class="run-platform">{{ g.platform }}</span>
            <span class="run-duration">⏳ {{ isoDurationShort(g.earliest, g.latest) }}</span>
            <span class="run-started">started {{ startedAtLocal(g.earliest) }}</span>
          </div>

          <div class="run-cards">
            <div
              v-for="t in TEST_NAMES"
              :key="t"
              class="run-card"
              :class="g.cards[t] ? (g.cards[t]!.passed ? 'pass' : 'fail') : 'missing'"
            >
              <div class="run-card-title">
                <span class="test-name">{{ t }}</span>
                <span v-if="g.cards[t]" class="status-chip" :class="g.cards[t]!.passed ? 'chip-pass' : 'chip-fail'">
                  {{ g.cards[t]!.passed ? 'PASS' : 'FAIL' }}
                </span>
                <span v-else class="status-chip chip-missing">—</span>
              </div>
              <div v-if="g.cards[t]" class="run-card-body">
                <template v-if="g.cards[t]!.summary">
                  <div v-if="g.cards[t]!.summary!.lowest_sustainable_cap_mbps != null && g.cards[t]!.summary!.lowest_sustainable_cap_mbps > 0" class="metric metric-headline">
                    <span class="label">lowest sustainable</span>
                    <span class="value">{{ fmtMbps(g.cards[t]!.summary!.lowest_sustainable_cap_mbps) }}</span>
                  </div>
                  <div v-if="g.cards[t]!.summary!.bottom_variant_floor_mbps != null && g.cards[t]!.summary!.bottom_variant_floor_mbps > 0" class="metric">
                    <span class="label">bottom floor</span>
                    <span class="value">{{ fmtMbps(g.cards[t]!.summary!.bottom_variant_floor_mbps) }}</span>
                  </div>
                </template>
                <div class="metric"><span class="label">stalls</span> <span class="value">{{ g.cards[t]!.stalls }}</span></div>
                <div class="metric"><span class="label">shifts</span> <span class="value">{{ g.cards[t]!.shifts }}</span></div>
                <div class="metric"><span class="label">duration</span> <span class="value">{{ g.cards[t]!.duration }}</span></div>
                <div class="metric"><span class="label">play_id</span> <span class="value mono">{{ playerShort(g.cards[t]!.play.play_id) }}</span></div>
                <div class="card-actions">
                  <button
                    v-if="g.cards[t]!.hasReport"
                    type="button"
                    class="btn btn-secondary btn-sm"
                    :class="{ 'btn-active': expandedSteps.get(charRunKey(g.run_id, t))?.open }"
                    @click="toggleSteps(g.run_id, t)"
                  >
                    {{ expandedSteps.get(charRunKey(g.run_id, t))?.open ? 'Details ▲' : 'Details ▼' }}
                  </button>
                  <a :href="playViewerHref(g.cards[t]!.play.play_id, g.cards[t]!.play.player_id)" class="btn btn-secondary btn-sm" title="Open the live samples replay (Chart.js timeline)">Replay</a>
                </div>
                <!-- Details panel renders below the cards row at full
                     width — see .run-row-details below this card grid.
                     The inline template that was here is dead code
                     gated by v-if="false" so the markup stays as a
                     reference until the row-level renderer is verified
                     in production. -->
                <template v-if="false">
                  <div v-if="expandedSteps.get(charRunKey(g.run_id, t))?.open" class="steps-panel">
                  <div v-if="expandedSteps.get(charRunKey(g.run_id, t))?.loading" class="steps-loading">loading…</div>
                  <div v-else-if="expandedSteps.get(charRunKey(g.run_id, t))?.error" class="steps-error">
                    error: {{ expandedSteps.get(charRunKey(g.run_id, t))?.error }}
                  </div>
                  <template v-else-if="expandedSteps.get(charRunKey(g.run_id, t))?.report">
                    <!-- Summary block -->
                    <div class="details-section">
                      <div class="details-section-title">Summary</div>
                      <table class="summary-table">
                        <tbody>
                          <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.lowest_sustainable_cap_mbps"><td class="label">lowest sustainable cap</td><td class="value mono">{{ fmtMbps(expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.lowest_sustainable_cap_mbps) }}</td></tr>
                          <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.bottom_variant_floor_mbps"><td class="label">bottom variant floor</td><td class="value mono">{{ fmtMbps(expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.bottom_variant_floor_mbps) }}</td></tr>
                          <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.highest_stalling_cap_mbps"><td class="label">highest stalling cap</td><td class="value mono">{{ fmtMbps(expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.highest_stalling_cap_mbps) }}</td></tr>
                          <tr><td class="label">stalls</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.total_stalls ?? 0 }} ({{ (expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.total_stall_seconds ?? 0).toFixed(1) }}s)</td></tr>
                          <tr><td class="label">profile shifts</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.profile_shifts ?? 0 }}</td></tr>
                          <tr><td class="label">dropped frames</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.dropped_frames ?? 0 }}</td></tr>
                          <tr><td class="label">samples</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.sample_count ?? 0 }}</td></tr>
                          <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.min_bitrate_mbps != null">
                            <td class="label">bitrate min / mean / max</td>
                            <td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.min_bitrate_mbps?.toFixed(2) }} / {{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.mean_bitrate_mbps?.toFixed(2) }} / {{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.max_bitrate_mbps?.toFixed(2) }} Mbps</td>
                          </tr>
                        </tbody>
                      </table>
                    </div>

                    <!-- Variants block -->
                    <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.variants?.length" class="details-section">
                      <div class="details-section-title">Variants ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.variants!.length }})</div>
                      <table class="steps-table">
                        <thead>
                          <tr><th>resolution</th><th>avg Mbps</th><th>peak Mbps</th><th>samples</th></tr>
                        </thead>
                        <tbody>
                          <tr v-for="(v, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.variants!" :key="i">
                            <td class="mono">{{ v.resolution }}</td>
                            <td class="mono">{{ v.avg_bps != null ? (v.avg_bps! / 1_000_000).toFixed(3) : '—' }}</td>
                            <td class="mono">{{ v.peak_bps != null ? (v.peak_bps! / 1_000_000).toFixed(3) : '—' }}</td>
                            <td class="mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.variant_sample_counts?.[i] ?? 0 }}</td>
                          </tr>
                        </tbody>
                      </table>
                    </div>

                    <!-- Abort cycles block — populated only by the abort test -->
                    <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.abort_cycles?.length" class="details-section">
                      <div class="details-section-title">Abort Cycles ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.abort_cycles!.length }})</div>
                      <div class="steps-tablewrap">
                        <table class="steps-table">
                          <thead>
                            <tr>
                              <th>#</th>
                              <th>fault shape</th>
                              <th>pre variant</th>
                              <th>abort</th>
                              <th>kind</th>
                              <th>retry</th>
                              <th>range</th>
                              <th>downshift</th>
                              <th>stalled</th>
                              <th>recovery</th>
                            </tr>
                          </thead>
                          <tbody>
                            <tr v-for="(c, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.abort_cycles!" :key="i">
                              <td>{{ c.cycle_idx ?? i + 1 }}</td>
                              <td class="mono">{{ c.fault_shape ?? '—' }}</td>
                              <td class="mono">{{ c.pre_variant ?? '—' }}</td>
                              <td>
                                <span v-if="c.abort_detected" class="status-chip chip-pass">YES</span>
                                <span v-else class="status-chip chip-fail">NO</span>
                                <span v-if="c.abort_at_s != null && c.abort_detected" class="muted"> @ {{ c.abort_at_s!.toFixed(1) }}s</span>
                              </td>
                              <td class="mono">{{ c.abort_kind || '—' }}</td>
                              <td>{{ c.retry_found ? 'yes' : 'no' }}</td>
                              <td>{{ c.retry_had_range ? 'yes' : (c.retry_found ? 'no' : '—') }}</td>
                              <td class="mono">{{ c.downshifted_to || '—' }}<span v-if="c.downshifted_to && c.downshift_after_s != null" class="muted"> ({{ c.downshift_after_s!.toFixed(1) }}s)</span></td>
                              <td>
                                <span v-if="c.player_stalled" class="status-chip chip-fail">YES</span>
                                <span v-else>no</span>
                              </td>
                              <td>{{ c.recovery_s != null ? c.recovery_s!.toFixed(1) + 's' : '—' }}</td>
                            </tr>
                          </tbody>
                        </table>
                      </div>
                    </div>

                    <!-- Startup cycles block — populated only by the startup test -->
                    <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.startup_cycles?.length" class="details-section">
                      <div class="details-section-title">Startup Cycles ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.startup_cycles!.length }})</div>
                      <div class="steps-tablewrap">
                        <table class="steps-table">
                          <thead>
                            <tr>
                              <th>#</th>
                              <th>boundary</th>
                              <th>clip</th>
                              <th>cap Mbps</th>
                              <th>first var</th>
                              <th>ttff</th>
                              <th>5s buf at</th>
                              <th>settled</th>
                              <th>shifts ↑/↓</th>
                              <th>stalls</th>
                              <th>net bw start/end</th>
                            </tr>
                          </thead>
                          <tbody>
                            <tr v-for="(c, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.startup_cycles!" :key="i">
                              <td>{{ c.cycle_idx ?? i + 1 }}</td>
                              <td class="mono">{{ c.boundary_type ?? '—' }}</td>
                              <td class="mono">{{ c.content_clip_id ? c.content_clip_id!.slice(0, 24) : '—' }}</td>
                              <td class="mono">{{ c.cap_mbps != null ? c.cap_mbps!.toFixed(2) : '—' }}</td>
                              <td class="mono">{{ c.first_variant_picked || '—' }}</td>
                              <td>{{ c.time_to_first_frame_s != null ? c.time_to_first_frame_s!.toFixed(2) + 's' : '—' }}</td>
                              <td>{{ c.reached_5s_buffer_at_s ? c.reached_5s_buffer_at_s!.toFixed(1) + 's' : 'never' }}</td>
                              <td class="mono">{{ c.settled_variant || '—' }}</td>
                              <td>{{ (c.upshifts_in_30s ?? 0) }}/{{ (c.downshifts_in_30s ?? 0) }}</td>
                              <td>
                                <span v-if="(c.stalls_in_30s ?? 0) > 0" class="status-chip chip-fail">{{ c.stalls_in_30s }}</span>
                                <span v-else>0</span>
                              </td>
                              <td class="mono">{{ (c.network_bitrate_at_start_mbps ?? 0).toFixed(1) }}/{{ (c.network_bitrate_at_30s_mbps ?? 0).toFixed(1) }}</td>
                            </tr>
                          </tbody>
                        </table>
                      </div>
                    </div>

                    <!-- Steps block -->
                    <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.steps?.length" class="details-section">
                      <div class="details-section-title">Steps ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.steps!.length }})</div>
                      <div class="steps-tablewrap">
                        <table class="steps-table">
                          <thead>
                            <tr>
                              <th>#</th>
                              <th>cap</th>
                              <th>variant</th>
                              <th>exit</th>
                              <th>held</th>
                              <th>min/max buf</th>
                              <th>stalls</th>
                              <th>shifts</th>
                            </tr>
                          </thead>
                          <tbody>
                            <tr v-for="(s, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.steps!" :key="i">
                              <td>{{ i + 1 }}</td>
                              <td class="mono">{{ s.rate_mbps?.toFixed(3) }}</td>
                              <td class="mono">{{ s.variant?.resolution ?? '—' }} <span v-if="s.variant?.margin_pct != null" class="muted">{{ fmtPct(s.variant!.margin_pct) }}</span></td>
                              <td>{{ s.exit_reason ?? '—' }}</td>
                              <td>{{ fmtSeconds(s.hold_actual_s ?? s.hold_s) }}</td>
                              <td>{{ s.min_buffer_s?.toFixed(1) ?? '—' }} / {{ s.max_buffer_s?.toFixed(1) ?? '—' }}</td>
                              <td>{{ s.stalls_delta ?? 0 }}</td>
                              <td>{{ s.shifts_delta ?? 0 }}</td>
                            </tr>
                          </tbody>
                        </table>
                      </div>
                    </div>
                  </template>
                  <div v-else class="steps-loading">(no report data)</div>
                </div>
                </template>
              </div>
              <div v-else class="run-card-body empty-card">
                (no play landed for this test in this run)
              </div>
            </div>
          </div>

          <!-- Row-level Details panel — renders below the run-cards
               grid at FULL PAGE WIDTH when any card in this run is
               expanded. The card-level Details button still controls
               state via expandedSteps; the panel itself lives here. -->
          <template v-for="t in TEST_NAMES" :key="`detail-${t}`">
            <div v-if="expandedSteps.get(charRunKey(g.run_id, t))?.open && g.cards[t]" class="run-row-details">
              <div class="run-row-details-header">
                <span class="details-test-label">Details — {{ t }}</span>
                <span v-if="g.cards[t]" class="details-test-status" :class="g.cards[t]!.passed ? 'chip-pass' : 'chip-fail'">
                  {{ g.cards[t]!.passed ? 'PASS' : 'FAIL' }}
                </span>
                <button type="button" class="btn btn-secondary btn-sm" @click="toggleSteps(g.run_id, t)">Close ▲</button>
              </div>
              <div v-if="expandedSteps.get(charRunKey(g.run_id, t))?.loading" class="steps-loading">loading…</div>
              <div v-else-if="expandedSteps.get(charRunKey(g.run_id, t))?.error" class="steps-error">
                error: {{ expandedSteps.get(charRunKey(g.run_id, t))?.error }}
              </div>
              <template v-else-if="expandedSteps.get(charRunKey(g.run_id, t))?.report">
                <div class="details-grid">
                  <!-- Summary block -->
                  <div class="details-section">
                    <div class="details-section-title">Summary</div>
                    <table class="summary-table">
                      <tbody>
                        <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.lowest_sustainable_cap_mbps"><td class="label">lowest sustainable cap</td><td class="value mono">{{ fmtMbps(expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.lowest_sustainable_cap_mbps) }}</td></tr>
                        <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.bottom_variant_floor_mbps"><td class="label">bottom variant floor</td><td class="value mono">{{ fmtMbps(expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.bottom_variant_floor_mbps) }}</td></tr>
                        <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.highest_stalling_cap_mbps"><td class="label">highest stalling cap</td><td class="value mono">{{ fmtMbps(expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.highest_stalling_cap_mbps) }}</td></tr>
                        <tr><td class="label">stalls</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.total_stalls ?? 0 }} ({{ (expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.total_stall_seconds ?? 0).toFixed(1) }}s)</td></tr>
                        <tr><td class="label">profile shifts</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.profile_shifts ?? 0 }}</td></tr>
                        <tr><td class="label">dropped frames</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.dropped_frames ?? 0 }}</td></tr>
                        <tr><td class="label">samples</td><td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.sample_count ?? 0 }}</td></tr>
                        <tr v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.min_bitrate_mbps != null">
                          <td class="label">bitrate min / mean / max</td>
                          <td class="value mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.min_bitrate_mbps?.toFixed(2) }} / {{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.mean_bitrate_mbps?.toFixed(2) }} / {{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary!.max_bitrate_mbps?.toFixed(2) }} Mbps</td>
                        </tr>
                      </tbody>
                    </table>
                  </div>

                  <!-- Variants block -->
                  <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.variants?.length" class="details-section">
                    <div class="details-section-title">Variants ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.variants!.length }})</div>
                    <table class="steps-table">
                      <thead>
                        <tr><th>resolution</th><th>avg Mbps</th><th>peak Mbps</th><th>samples</th></tr>
                      </thead>
                      <tbody>
                        <tr v-for="(v, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.variants!" :key="i">
                          <td class="mono">{{ v.resolution }}</td>
                          <td class="mono">{{ v.avg_bps != null ? (v.avg_bps! / 1_000_000).toFixed(3) : '—' }}</td>
                          <td class="mono">{{ v.peak_bps != null ? (v.peak_bps! / 1_000_000).toFixed(3) : '—' }}</td>
                          <td class="mono">{{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.summary?.variant_sample_counts?.[i] ?? 0 }}</td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </div>

                <!-- Abort cycles block — full-width row -->
                <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.abort_cycles?.length" class="details-section">
                  <div class="details-section-title">Abort Cycles ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.abort_cycles!.length }})</div>
                  <div class="steps-tablewrap">
                    <table class="steps-table">
                      <thead>
                        <tr>
                          <th>#</th><th>fault shape</th><th>pre variant</th><th>abort</th><th>kind</th><th>retry</th><th>range</th><th>downshift</th><th>stalled</th><th>recovery</th><th>view</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="(c, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.abort_cycles!" :key="i">
                          <td>{{ c.cycle_idx ?? i + 1 }}</td>
                          <td class="mono">{{ c.fault_shape ?? '—' }}</td>
                          <td class="mono">{{ c.pre_variant ?? '—' }}</td>
                          <td>
                            <span v-if="c.abort_detected" class="status-chip chip-pass">YES</span>
                            <span v-else class="status-chip chip-fail">NO</span>
                            <span v-if="c.abort_at_s != null && c.abort_detected" class="muted"> @ {{ c.abort_at_s!.toFixed(1) }}s</span>
                          </td>
                          <td class="mono">{{ c.abort_kind || '—' }}</td>
                          <td>{{ c.retry_found ? 'yes' : 'no' }}</td>
                          <td>{{ c.retry_had_range ? 'yes' : (c.retry_found ? 'no' : '—') }}</td>
                          <td class="mono">{{ c.downshifted_to || '—' }}<span v-if="c.downshifted_to && c.downshift_after_s != null" class="muted"> ({{ c.downshift_after_s!.toFixed(1) }}s)</span></td>
                          <td>
                            <span v-if="c.player_stalled" class="status-chip chip-fail">YES</span>
                            <span v-else>no</span>
                          </td>
                          <td>{{ c.recovery_s != null ? c.recovery_s!.toFixed(1) + 's' : '—' }}</td>
                          <td>
                            <SessionViewerLink
                              :player-id="expandedSteps.get(charRunKey(g.run_id, t))!.report!.player_id ?? ''"
                              :play-id="expandedSteps.get(charRunKey(g.run_id, t))!.report!.play_ids?.[0]"
                              :start-ms="abortCycleWindow(c).startMs"
                              :end-ms="abortCycleWindow(c).endMs"
                            />
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </div>

                <!-- Startup cycles block — full-width row -->
                <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.startup_cycles?.length" class="details-section">
                  <div class="details-section-title">Startup Cycles ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.startup_cycles!.length }})</div>
                  <div class="steps-tablewrap">
                    <table class="steps-table">
                      <thead>
                        <tr>
                          <th>#</th><th>boundary</th><th>clip</th><th>cap Mbps</th><th>first var</th><th>ttff</th><th>5s buf at</th><th>settled</th><th>shifts ↑/↓</th><th>stalls</th><th>net bw start/end</th><th>view</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="(c, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.startup_cycles!" :key="i">
                          <td>{{ c.cycle_idx ?? i + 1 }}</td>
                          <td class="mono">{{ c.boundary_type ?? '—' }}</td>
                          <td class="mono">{{ c.content_clip_id ? c.content_clip_id!.slice(0, 24) : '—' }}</td>
                          <td class="mono">{{ c.cap_mbps != null ? c.cap_mbps!.toFixed(2) : '—' }}</td>
                          <td class="mono">{{ c.first_variant_picked || '—' }}</td>
                          <td>{{ c.time_to_first_frame_s != null ? c.time_to_first_frame_s!.toFixed(2) + 's' : '—' }}</td>
                          <td>{{ c.reached_5s_buffer_at_s ? c.reached_5s_buffer_at_s!.toFixed(1) + 's' : 'never' }}</td>
                          <td class="mono">{{ c.settled_variant || '—' }}</td>
                          <td>{{ (c.upshifts_in_30s ?? 0) }}/{{ (c.downshifts_in_30s ?? 0) }}</td>
                          <td>
                            <span v-if="(c.stalls_in_30s ?? 0) > 0" class="status-chip chip-fail">{{ c.stalls_in_30s }}</span>
                            <span v-else>0</span>
                          </td>
                          <td class="mono">{{ (c.network_bitrate_at_start_mbps ?? 0).toFixed(1) }}/{{ (c.network_bitrate_at_30s_mbps ?? 0).toFixed(1) }}</td>
                          <td>
                            <SessionViewerLink
                              :player-id="c.player_id ?? expandedSteps.get(charRunKey(g.run_id, t))!.report!.player_id ?? ''"
                              :play-id="c.play_id"
                              :start-ms="startupCycleWindow(c).startMs"
                              :end-ms="startupCycleWindow(c).endMs"
                            />
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </div>

                <!-- Steps block — full-width row -->
                <div v-if="expandedSteps.get(charRunKey(g.run_id, t))!.report!.steps?.length" class="details-section">
                  <div class="details-section-title">Steps ({{ expandedSteps.get(charRunKey(g.run_id, t))!.report!.steps!.length }})</div>
                  <div class="steps-tablewrap">
                    <table class="steps-table">
                      <thead>
                        <tr>
                          <th>#</th>
                          <th>cap</th>
                          <th>variant</th>
                          <th>exit</th>
                          <th>held</th>
                          <th title="start / min / max / end buffer (s)">buffer s/m/M/e</th>
                          <th title="video bitrate: avg / peak (Mbps)">video bw</th>
                          <th title="network throughput: avg / peak (Mbps)">net bw</th>
                          <th>stalls</th>
                          <th>shifts</th>
                          <th>view</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="(s, i) in expandedSteps.get(charRunKey(g.run_id, t))!.report!.steps!" :key="i">
                          <td>{{ i + 1 }}</td>
                          <td class="mono">{{ s.rate_mbps?.toFixed(3) }}</td>
                          <td class="mono">{{ s.variant?.resolution ?? '—' }} <span v-if="s.variant?.margin_pct != null" class="muted">{{ fmtPct(s.variant!.margin_pct) }}</span></td>
                          <td>{{ s.exit_reason ?? '—' }}</td>
                          <td>{{ fmtSeconds(s.hold_actual_s ?? s.hold_s) }}</td>
                          <td class="mono">
                            {{ s.buffer_at_start_s != null ? s.buffer_at_start_s!.toFixed(1) : '—' }} /
                            {{ s.min_buffer_s != null ? s.min_buffer_s!.toFixed(1) : '—' }} /
                            {{ s.max_buffer_s != null ? s.max_buffer_s!.toFixed(1) : '—' }} /
                            {{ s.buffer_at_end_s != null ? s.buffer_at_end_s!.toFixed(1) : '—' }}
                          </td>
                          <td class="mono">
                            {{ s.mean_bitrate_mbps != null ? s.mean_bitrate_mbps!.toFixed(2) : '—' }}<span class="muted"> / </span>{{ s.max_bitrate_mbps != null ? s.max_bitrate_mbps!.toFixed(2) : '—' }}
                          </td>
                          <td class="mono">
                            {{ s.mean_network_bitrate_mbps != null ? s.mean_network_bitrate_mbps!.toFixed(2) : '—' }}<span class="muted"> / </span>{{ s.max_network_bitrate_mbps != null ? s.max_network_bitrate_mbps!.toFixed(2) : '—' }}
                          </td>
                          <td>{{ s.stalls_delta ?? 0 }}</td>
                          <td>{{ s.shifts_delta ?? 0 }}</td>
                          <td>
                            <SessionViewerLink
                              :player-id="expandedSteps.get(charRunKey(g.run_id, t))!.report!.player_id ?? ''"
                              :play-id="expandedSteps.get(charRunKey(g.run_id, t))!.report!.play_ids?.[0]"
                              :start-ms="stepWindow(expandedSteps.get(charRunKey(g.run_id, t))!.report!, i).startMs"
                              :end-ms="stepWindow(expandedSteps.get(charRunKey(g.run_id, t))!.report!, i).endMs"
                            />
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </div>
              </template>
              <div v-else class="steps-loading">(no report data)</div>
            </div>
          </template>
        </div>
      </div>
    </main>
    <Teleport to="body">
      <div class="chat-dock">
        <ChatPanel
          :scope="chatScope"
          scope-key="characterization:fleet"
          variant="panel"
          :start-collapsed="true"
        />
      </div>
    </Teleport>
  </ShellLayout>
</template>

<style>
/* Unscoped — Teleport-to-body element needs the parent style applied
   directly. Same pattern as Sessions.vue / SessionViewer.vue. */
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
/* Light-theme palette to match Sessions.vue. Reads var(--bg-*, …)
 * tokens where the rest of the dashboard sets them, falls back to
 * concrete colours otherwise. */
.page-header { padding: 16px 0; }
.page-title { font-size: 24px; font-weight: 600; color: var(--text-primary, #111827); }
.page-subtitle { color: var(--text-secondary, #6b7280); margin-top: 6px; font-size: 14px; }
.page-callout {
  margin-top: 12px;
  padding: 10px 14px;
  background: #f0f9ff;
  border: 1px solid #bae6fd;
  border-radius: 6px;
  color: #0c4a6e;
  font-size: 13px;
  line-height: 1.5;
}
.page-callout code {
  background: #e0f2fe;
  padding: 1px 5px;
  border-radius: 3px;
  font-size: 12px;
  font-family: ui-monospace, monospace;
}
.page-callout-code {
  margin: 6px 0;
  padding: 8px 10px;
  background: #082f49;
  color: #e0f2fe;
  border-radius: 4px;
  font-size: 12px;
  font-family: ui-monospace, monospace;
  overflow-x: auto;
  white-space: pre;
}
.page-callout-code .muted { color: #7dd3fc; }

.panel { background: var(--bg-card, #ffffff); border: 1px solid var(--border, #e5e7eb); border-radius: 8px; margin-bottom: 16px; }
.panel-header { padding: 12px 16px; border-bottom: 1px solid var(--border, #e5e7eb); }
.panel-title { font-weight: 600; color: var(--text-primary, #111827); display: flex; gap: 12px; align-items: center; }
.status-message { font-size: 12px; color: var(--text-secondary, #6b7280); font-weight: normal; }

.picker-wrap { padding: 12px 16px; border-bottom: 1px solid var(--border, #e5e7eb); }
.range-row { display: flex; gap: 16px; align-items: center; flex-wrap: wrap; }
.ctrl-label { display: flex; gap: 6px; align-items: center; font-size: 13px; color: var(--text-primary, #111827); }
.ctrl-input { background: #ffffff; border: 1px solid #d1d5db; color: #111827; padding: 4px 8px; border-radius: 4px; }
.btn { border: 1px solid #d1d5db; background: var(--bg-secondary, #f9fafb); color: #1f2937; padding: 4px 10px; border-radius: 4px; cursor: pointer; font-size: 13px; }
.btn:hover { background: #f3f4f6; }
.btn-sm { padding: 2px 8px; font-size: 12px; }
.btn-secondary { background: var(--bg-secondary, #f9fafb); }
.error { color: #b91c1c; }

.empty { padding: 24px 16px; color: #6b7280; text-align: center; }
.empty code { background: #f3f4f6; padding: 2px 6px; border-radius: 3px; color: #1f2937; }

.run-row { padding: 16px; border-bottom: 1px solid var(--border, #e5e7eb); }
.run-row:last-child { border-bottom: none; }
.run-row-header { display: flex; gap: 16px; align-items: baseline; margin-bottom: 12px; font-size: 13px; }
.run-id { font-weight: 600; color: #111827; font-family: ui-monospace, SFMono-Regular, monospace; }
.run-platform { color: #1d4ed8; background: #dbeafe; padding: 2px 8px; border-radius: 10px; font-size: 12px; }
.run-duration { color: #6b7280; }
.run-started { color: #9ca3af; font-size: 12px; margin-left: auto; }

.run-cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; }
.run-card { background: var(--bg-secondary, #f9fafb); border: 1px solid #e5e7eb; border-radius: 6px; padding: 12px; }
.run-card.pass { border-left: 3px solid #16a34a; }
.run-card.fail { border-left: 3px solid #dc2626; }
.run-card.missing { border-left: 3px solid #d1d5db; opacity: 0.7; }
.run-card-title { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
.test-name { font-weight: 600; color: #111827; text-transform: capitalize; }
.status-chip { font-size: 11px; font-weight: 700; padding: 2px 8px; border-radius: 10px; letter-spacing: 0.5px; }
.chip-pass { background: #dcfce7; color: #14532d; border: 1px solid #86efac; }
.chip-fail { background: #fee2e2; color: #7f1d1d; border: 1px solid #fca5a5; }
.chip-missing { background: #f3f4f6; color: #6b7280; }
.run-card-body { font-size: 12px; }
.metric { display: flex; justify-content: space-between; padding: 2px 0; color: #374151; }
.metric .label { color: #6b7280; }
.metric .value.mono { font-family: ui-monospace, SFMono-Regular, monospace; color: #1f2937; }
.card-actions { margin-top: 8px; display: flex; gap: 6px; }
.empty-card { color: #9ca3af; font-style: italic; padding: 12px 0; text-align: center; }

.metric-headline .value { color: #111827; font-weight: 600; }

.steps-panel { margin-top: 10px; padding-top: 10px; border-top: 1px dashed #d1d5db; }

/* Full-width row-level Details panel — replaces the in-card inline panel.
 * Sits below the cards grid, spans the full row width, gives tables room
 * to breathe. */
.run-row-details {
  margin-top: 12px;
  padding: 16px;
  background: #ffffff;
  border: 1px solid #d1d5db;
  border-left: 3px solid #1d4ed8;
  border-radius: 6px;
}
.run-row-details-header {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
  padding-bottom: 10px;
  border-bottom: 1px solid #e5e7eb;
}
.details-test-label {
  font-size: 14px;
  font-weight: 600;
  color: #111827;
  text-transform: capitalize;
}
.details-test-status {
  font-size: 11px;
  font-weight: 700;
  padding: 2px 8px;
  border-radius: 10px;
  letter-spacing: 0.5px;
}
.run-row-details-header .btn { margin-left: auto; }

/* Two-column grid for Summary + Variants when both present (compact
 * side-by-side at row width). Larger cycle/step tables stay
 * full-width below. */
.details-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 16px;
  margin-bottom: 12px;
}

.btn-active {
  background: #dbeafe;
  border-color: #1d4ed8;
  color: #1d4ed8;
}
.steps-loading { color: #6b7280; font-style: italic; font-size: 12px; }
.steps-error { color: #b91c1c; font-size: 12px; }
.steps-tablewrap { overflow-x: auto; max-height: 360px; overflow-y: auto; border: 1px solid #e5e7eb; border-radius: 4px; }
.steps-table { width: 100%; border-collapse: collapse; font-size: 11px; background: #ffffff; }
.steps-table th, .steps-table td { padding: 4px 8px; text-align: left; border-bottom: 1px solid #f3f4f6; color: #1f2937; }
.steps-table th { color: #4b5563; font-weight: 600; position: sticky; top: 0; background: #f9fafb; border-bottom: 1px solid #e5e7eb; }
.steps-table td.mono { font-family: ui-monospace, SFMono-Regular, monospace; }
.steps-table .muted { color: #9ca3af; }

.details-section { margin-top: 12px; }
.details-section-title { color: #4b5563; font-size: 11px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.5px; margin-bottom: 6px; }
.summary-table { width: 100%; border-collapse: collapse; font-size: 12px; background: #ffffff; border: 1px solid #e5e7eb; border-radius: 4px; }
.summary-table td { padding: 4px 8px; border-bottom: 1px solid #f3f4f6; }
.summary-table td.label { color: #6b7280; }
.summary-table td.value { color: #111827; text-align: right; }
.summary-table td.value.mono { font-family: ui-monospace, SFMono-Regular, monospace; }
</style>
