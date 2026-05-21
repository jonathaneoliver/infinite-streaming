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

type TestName = 'rampup' | 'rampdown' | 'pyramid';
const TEST_NAMES: TestName[] = ['rampup', 'rampdown', 'pyramid'];

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
  started_at: string;
  ended_at: string;
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
  stalls_delta?: number;
  shifts_delta?: number;
  bitrate_min_mbps?: number;
  bitrate_max_mbps?: number;
}

interface ReportBlob {
  steps?: StepRow[];
  variants?: Array<{ resolution: string; avg_bps?: number; peak_bps?: number; source?: string }>;
}

// Per-card expand state + cache. Map key = (run_id, test_name).
const expandedSteps = ref<Map<string, { open: boolean; report?: ReportBlob; loading?: boolean; error?: string }>>(new Map());

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
                    @click="toggleSteps(g.run_id, t)"
                  >
                    {{ expandedSteps.get(charRunKey(g.run_id, t))?.open ? 'Steps ▲' : 'Steps ▼' }}
                  </button>
                  <a :href="playViewerHref(g.cards[t]!.play.play_id, g.cards[t]!.play.player_id)" class="btn btn-secondary btn-sm">Open</a>
                </div>
                <div v-if="expandedSteps.get(charRunKey(g.run_id, t))?.open" class="steps-panel">
                  <div v-if="expandedSteps.get(charRunKey(g.run_id, t))?.loading" class="steps-loading">loading…</div>
                  <div v-else-if="expandedSteps.get(charRunKey(g.run_id, t))?.error" class="steps-error">
                    error: {{ expandedSteps.get(charRunKey(g.run_id, t))?.error }}
                  </div>
                  <div v-else-if="expandedSteps.get(charRunKey(g.run_id, t))?.report?.steps?.length" class="steps-tablewrap">
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
                          <td class="mono">{{ s.variant?.resolution ?? '—' }} <span v-if="s.variant?.margin_pct != null" class="muted">{{ fmtPct(s.variant.margin_pct) }}</span></td>
                          <td>{{ s.exit_reason ?? '—' }}</td>
                          <td>{{ fmtSeconds(s.hold_actual_s ?? s.hold_s) }}</td>
                          <td>{{ s.min_buffer_s?.toFixed(1) ?? '—' }} / {{ s.max_buffer_s?.toFixed(1) ?? '—' }}</td>
                          <td>{{ s.stalls_delta ?? 0 }}</td>
                          <td>{{ s.shifts_delta ?? 0 }}</td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                  <div v-else class="steps-loading">(no steps recorded)</div>
                </div>
              </div>
              <div v-else class="run-card-body empty-card">
                (no play landed for this test in this run)
              </div>
            </div>
          </div>
        </div>
      </div>
    </main>
  </ShellLayout>
</template>

<style scoped>
.page-header { padding: 16px 0; }
.page-title { font-size: 24px; font-weight: 600; color: #e6edf3; }
.page-subtitle { color: #8b949e; margin-top: 6px; font-size: 14px; }

.panel { background: #0d1117; border: 1px solid #21262d; border-radius: 8px; margin-bottom: 16px; }
.panel-header { padding: 12px 16px; border-bottom: 1px solid #21262d; }
.panel-title { font-weight: 600; color: #e6edf3; display: flex; gap: 12px; align-items: center; }
.status-message { font-size: 12px; color: #8b949e; font-weight: normal; }

.picker-wrap { padding: 12px 16px; border-bottom: 1px solid #21262d; }
.range-row { display: flex; gap: 16px; align-items: center; flex-wrap: wrap; }
.ctrl-label { display: flex; gap: 6px; align-items: center; font-size: 13px; color: #c9d1d9; }
.ctrl-input { background: #0d1117; border: 1px solid #30363d; color: #e6edf3; padding: 4px 8px; border-radius: 4px; }
.btn { border: 1px solid #30363d; background: #21262d; color: #e6edf3; padding: 4px 10px; border-radius: 4px; cursor: pointer; font-size: 13px; }
.btn:hover { background: #30363d; }
.btn-sm { padding: 2px 8px; font-size: 12px; }
.btn-secondary { background: #161b22; }
.error { color: #f85149; }

.empty { padding: 24px 16px; color: #8b949e; text-align: center; }
.empty code { background: #161b22; padding: 2px 6px; border-radius: 3px; }

.run-row { padding: 16px; border-bottom: 1px solid #21262d; }
.run-row:last-child { border-bottom: none; }
.run-row-header { display: flex; gap: 16px; align-items: baseline; margin-bottom: 12px; font-size: 13px; }
.run-id { font-weight: 600; color: #e6edf3; font-family: ui-monospace, SFMono-Regular, monospace; }
.run-platform { color: #58a6ff; background: #0d2440; padding: 2px 8px; border-radius: 10px; font-size: 12px; }
.run-duration { color: #8b949e; }
.run-started { color: #6e7681; font-size: 12px; margin-left: auto; }

.run-cards { display: grid; grid-template-columns: repeat(3, 1fr); gap: 12px; }
.run-card { background: #161b22; border: 1px solid #30363d; border-radius: 6px; padding: 12px; }
.run-card.pass { border-left: 3px solid #3fb950; }
.run-card.fail { border-left: 3px solid #f85149; }
.run-card.missing { border-left: 3px solid #30363d; opacity: 0.6; }
.run-card-title { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
.test-name { font-weight: 600; color: #e6edf3; text-transform: capitalize; }
.status-chip { font-size: 11px; font-weight: 700; padding: 2px 8px; border-radius: 10px; letter-spacing: 0.5px; }
.chip-pass { background: #0d2410; color: #3fb950; border: 1px solid #2ea043; }
.chip-fail { background: #240d10; color: #f85149; border: 1px solid #da3633; }
.chip-missing { background: #21262d; color: #8b949e; }
.run-card-body { font-size: 12px; }
.metric { display: flex; justify-content: space-between; padding: 2px 0; color: #c9d1d9; }
.metric .label { color: #8b949e; }
.metric .value.mono { font-family: ui-monospace, SFMono-Regular, monospace; }
.card-actions { margin-top: 8px; display: flex; gap: 6px; }
.empty-card { color: #6e7681; font-style: italic; padding: 12px 0; text-align: center; }

.metric-headline .value { color: #e6edf3; font-weight: 600; }

.steps-panel { margin-top: 10px; padding-top: 10px; border-top: 1px dashed #30363d; }
.steps-loading { color: #8b949e; font-style: italic; font-size: 12px; }
.steps-error { color: #f85149; font-size: 12px; }
.steps-tablewrap { overflow-x: auto; max-height: 360px; overflow-y: auto; }
.steps-table { width: 100%; border-collapse: collapse; font-size: 11px; }
.steps-table th, .steps-table td { padding: 3px 6px; text-align: left; border-bottom: 1px solid #21262d; }
.steps-table th { color: #8b949e; font-weight: 600; position: sticky; top: 0; background: #161b22; }
.steps-table td.mono { font-family: ui-monospace, SFMono-Regular, monospace; }
.steps-table .muted { color: #8b949e; }
</style>
