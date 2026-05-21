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

async function refresh() {
  const range = RANGES.find(r => r.id === activeRangeId.value);
  if (!range) return;
  loading.value = true;
  error.value = '';
  try {
    const results = await Promise.all(
      TEST_NAMES.map(async (t) => {
        const rows = await fetchOneTest(t, range.hours);
        return rows.map(r => ({ ...r, test: t }));
      })
    );
    allPlays.value = results.flat();
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
    g.cards[p.test] = {
      test: p.test,
      play: p,
      passed: asNumber(p.stalls) === 0,
      stalls: asNumber(p.stalls),
      shifts: asNumber(p.bitrate_shifts),
      duration: isoDurationShort(p.started_at, p.last_seen_at),
    };
  }
  return Array.from(byRun.values())
    .sort((a, b) => (a.earliest < b.earliest ? 1 : -1)); // newest first
});

function shortRunID(runID: string): string {
  // 20260521T155558Z → 2026-05-21 15:55
  const m = runID.match(/^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})/);
  if (!m) return runID;
  return `${m[1]}-${m[2]}-${m[3]} ${m[4]}:${m[5]}`;
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
                <div class="metric"><span class="label">stalls</span> <span class="value">{{ g.cards[t]!.stalls }}</span></div>
                <div class="metric"><span class="label">shifts</span> <span class="value">{{ g.cards[t]!.shifts }}</span></div>
                <div class="metric"><span class="label">duration</span> <span class="value">{{ g.cards[t]!.duration }}</span></div>
                <div class="metric"><span class="label">player</span> <span class="value mono">{{ playerShort(g.cards[t]!.play.player_id) }}</span></div>
                <div class="metric"><span class="label">play_id</span> <span class="value mono">{{ playerShort(g.cards[t]!.play.play_id) }}</span></div>
                <div class="card-actions">
                  <a :href="playViewerHref(g.cards[t]!.play.play_id, g.cards[t]!.play.player_id)" class="btn btn-secondary btn-sm">Open</a>
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
</style>
