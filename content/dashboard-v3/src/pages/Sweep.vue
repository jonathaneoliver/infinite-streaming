<script setup lang="ts">
/**
 * Sweep.vue — live monitor + control plane for the automated fault-injection
 * sweep (#772).
 *
 * ClickHouse is the master queue (CH-master migration): the `harness sweep` CLI
 * reads/writes /api/v2/sweep/experiments (ReplacingMergeTree by exp_id) directly,
 * so this page is always live — no publish step. It polls that endpoint and shows
 * the queue as status columns (pending → running → done / found) with each
 * experiment's recipe, WHY (reason it ran), VERDICT, lineage (parent → isolation
 * fan), and a session-viewer link. The Scope panel writes /api/v2/sweep/scope to
 * gate which dimension values the sweep is allowed to claim. View filters scope
 * by class / search.
 */
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue';
import ShellLayout from '@/components/ShellLayout.vue';
import LabelFrequencyTable from '@/components/LabelFrequencyTable.vue';
import SweepJobDetail, { type Experiment } from '@/components/SweepJobDetail.vue';
import { ensureVisNetwork } from '@/composables/useChartJs';

// Column order = the lifecycle flow.
const STATUSES = ['backlog', 'running', 'found', 'done', 'review', 'feedback'] as const;
const STATUS_LABEL: Record<string, string> = {
  backlog: 'Pending', running: 'Running', found: 'Found', done: 'Done',
  review: 'Review', feedback: 'Feedback',
};

const experiments = ref<Experiment[]>([]);
const loading = ref(false);
const error = ref('');
const lastUpdated = ref('');
const classFilter = ref<'all' | 'config' | 'fault'>('all');
const search = ref('');
let timer: number | undefined;

async function load() {
  loading.value = true;
  error.value = '';
  try {
    const resp = await fetch('/analytics/api/v2/sweep/experiments?limit=2000');
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = (await resp.json()) as { items: Experiment[] | null };
    experiments.value = data.items ?? [];
    lastUpdated.value = new Date().toLocaleTimeString();
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
  } finally {
    loading.value = false;
  }
}

onMounted(() => {
  load();
  loadScope();
  timer = window.setInterval(load, 5000); // the loop is slow (~100s/play); 5s is plenty
});
onUnmounted(() => {
  if (timer) window.clearInterval(timer);
});

const filtered = computed(() =>
  experiments.value.filter((e) => {
    if (classFilter.value !== 'all' && (e.class || 'config') !== classFilter.value) return false;
    const q = search.value.trim().toLowerCase();
    if (!q) return true;
    return (
      e.exp_id.toLowerCase().includes(q) ||
      (e.recipe || '').toLowerCase().includes(q) ||
      (e.why || '').toLowerCase().includes(q) ||
      (e.mode || '').toLowerCase().includes(q)
    );
  }),
);

const byStatus = computed(() => {
  const m: Record<string, Experiment[]> = {};
  for (const s of STATUSES) m[s] = [];
  for (const e of filtered.value) (m[e.status] ??= []).push(e);
  for (const s of Object.keys(m)) {
    m[s].sort((a, b) => (b.score || 0) - (a.score || 0) || a.exp_id.localeCompare(b.exp_id));
  }
  return m;
});

const totals = computed(() => {
  const t = { config: 0, fault: 0 };
  for (const e of filtered.value) {
    if ((e.class || 'config') === 'fault') t.fault++;
    else t.config++;
  }
  return t;
});

function verdictClass(v: string): string {
  switch (v) {
    case 'aberration': return 'v-aberration';
    case 'notable': return 'v-notable';
    case 'clean': return 'v-clean';
    case 'inconclusive': return 'v-inconclusive';
    default: return 'v-none';
  }
}

function viewerURL(e: Experiment): string | null {
  if (!e.player_id) return null;
  let u = `/dashboard/session-viewer.html?player_id=${encodeURIComponent(e.player_id)}`;
  if (e.play_id) u += `&play_id=${encodeURIComponent(e.play_id)}`;
  return u;
}

// ── Scope control plane (#772 CH-master): toggle which dimension values the
// sweep is allowed to claim. The forwarder gates the /claim candidate query on
// sweep_scope, so disabling a value keeps its experiments pending (never run)
// without deleting them. Only the server-controllable dimensions are here —
// app/device are observational (filter only), so they aren't gateable.
const SCOPE_DIMS = ['platform', 'protocol', 'class', 'mode'] as const;
const scope = ref<Record<string, Record<string, boolean>>>({});

async function loadScope() {
  try {
    const resp = await fetch('/analytics/api/v2/sweep/scope');
    if (!resp.ok) return;
    const data = (await resp.json()) as { items: { dimension: string; value: string; enabled: number }[] | null };
    const m: Record<string, Record<string, boolean>> = {};
    for (const row of data.items ?? []) (m[row.dimension] ??= {})[row.value] = row.enabled !== 0;
    scope.value = m;
  } catch { /* scope is best-effort UI */ }
}

// Distinct values for a dimension = those seen in the queue ∪ any explicitly
// toggled (so a disabled value with no current experiments still shows).
function scopeValues(dim: string): string[] {
  const set = new Set<string>();
  for (const e of experiments.value) {
    const v = (e[dim as keyof Experiment] as string) || (dim === 'class' ? 'config' : '');
    if (v) set.add(v);
  }
  for (const v of Object.keys(scope.value[dim] ?? {})) set.add(v);
  return [...set].sort();
}

const isEnabled = (dim: string, val: string): boolean => scope.value[dim]?.[val] !== false;

async function toggleScope(dim: string, val: string) {
  const next = !isEnabled(dim, val);
  (scope.value[dim] ??= {})[val] = next; // optimistic
  try {
    const resp = await fetch('/analytics/api/v2/sweep/scope', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ dimension: dim, value: val, enabled: next }),
    });
    if (!resp.ok) throw new Error(String(resp.status));
  } catch {
    (scope.value[dim] ??= {})[val] = !next; // revert on failure
  }
  loadScope();
}

const disabledCount = computed(() =>
  Object.values(scope.value).reduce(
    (n, vs) => n + Object.values(vs).filter((en) => en === false).length, 0),
);

// ── Per-job detail (click a card to expand all its fields) ───────────────────
const expanded = ref<Set<string>>(new Set());
function toggleExpand(id: string) {
  const s = new Set(expanded.value);
  s.has(id) ? s.delete(id) : s.add(id);
  expanded.value = s;
}

// ── Lineage graph view (vis-network) ────────────────────────────────────────
// The sweep is a DAG: parent (seed → isolation fan → bisect) + group (A/B pairs).
// Toggle Board | Graph; nodes = experiments colored by verdict/status, edges =
// lineage, click a node → its detail.
const viewMode = ref<'board' | 'graph' | 'history'>('board');
const graphEl = ref<HTMLElement | null>(null);
const selectedExpId = ref('');
const selectedExp = computed(() => experiments.value.find((e) => e.exp_id === selectedExpId.value) || null);
let network: any = null;

function nodeColor(e: Experiment): { background: string; border: string } {
  switch (e.verdict) {
    case 'aberration': return { background: '#fce8e6', border: '#d93025' };
    case 'notable': return { background: '#fef7e0', border: '#f9ab00' };
    case 'clean': return { background: '#e6f4ea', border: '#1e8e3e' };
    case 'inconclusive': return { background: '#f1f3f4', border: '#9aa0a6' };
  }
  if (e.status === 'running') return { background: '#e6f4ea', border: '#1e8e3e' };
  return { background: '#e8f0fe', border: '#1a73e8' }; // pending / other
}

async function buildGraph() {
  if (!graphEl.value) return;
  const vis = await ensureVisNetwork();
  const items = filtered.value;
  const ids = new Set(items.map((e) => e.exp_id));
  const nodes = items.map((e) => ({
    id: e.exp_id,
    label: `${e.kind}\n${e.recipe || e.mode}`,
    color: nodeColor(e),
    borderWidth: e.exp_id === selectedExpId.value ? 3 : 1,
  }));
  const edges: any[] = [];
  for (const e of items) {
    if (e.parent && ids.has(e.parent)) edges.push({ from: e.parent, to: e.exp_id, arrows: 'to', color: { color: '#bdc1c6' } });
  }
  // dashed links among A/B group members (they also share a parent)
  const groups: Record<string, string[]> = {};
  for (const e of items) if (e.group_id) (groups[e.group_id] ??= []).push(e.exp_id);
  for (const g of Object.values(groups)) for (let i = 1; i < g.length; i++) edges.push({ from: g[0], to: g[i], dashes: true, color: { color: '#dadce0' } });

  const data = { nodes: new vis.DataSet(nodes), edges: new vis.DataSet(edges) };
  const options = {
    layout: { hierarchical: { enabled: true, direction: 'UD', sortMethod: 'directed', levelSeparation: 95, nodeSpacing: 150 } },
    physics: false,
    nodes: { shape: 'box', font: { size: 11, face: 'monospace', color: '#202124' }, margin: 7, widthConstraint: { maximum: 170 } },
    edges: { smooth: { enabled: true, type: 'cubicBezier' } },
    interaction: { hover: true, zoomView: true, dragView: true },
  };
  if (network) network.destroy();
  network = new vis.Network(graphEl.value, data, options);
  network.once('afterDrawing', () => network && network.fit({ animation: false }));
  network.on('click', (params: any) => {
    selectedExpId.value = params.nodes?.length ? params.nodes[0] : '';
  });
}

watch([viewMode, experiments], () => {
  if (viewMode.value === 'graph') nextTick(buildGraph);
});
onUnmounted(() => { if (network) network.destroy(); });

// ── Run history (sweep_runs — append-only, every run survives the queue) ─────
interface SweepRun {
  play_id: string; exp_id: string; class: string; kind: string;
  platform: string; protocol: string; mode: string; recipe: string;
  verdict: string; why: string; why_text: string; note: string;
  player_id: string; run_at: string;
}
const history = ref<SweepRun[]>([]);
const historyLoaded = ref(false);
async function loadHistory() {
  try {
    const resp = await fetch('/analytics/api/v2/sweep/runs?limit=500');
    if (!resp.ok) return;
    const data = (await resp.json()) as { items: SweepRun[] | null };
    history.value = data.items ?? [];
    historyLoaded.value = true;
  } catch { /* best-effort */ }
}
function runViewer(r: SweepRun): string | null {
  if (!r.player_id) return null;
  let u = `/dashboard/session-viewer.html?player_id=${encodeURIComponent(r.player_id)}`;
  if (r.play_id) u += `&play_id=${encodeURIComponent(r.play_id)}`;
  return u;
}
function runDate(s: string): string {
  if (!s) return '—';
  const d = new Date(s.replace(' ', 'T') + 'Z');
  return isNaN(d.getTime()) ? s : d.toLocaleString();
}
watch(viewMode, () => { if (viewMode.value === 'history') loadHistory(); });
</script>

<template>
  <ShellLayout activePage="sweep">
    <div class="sweep-page">
      <header class="head">
        <div>
          <h1>Fault Sweep</h1>
          <p class="sub">
            Automated stream-config / fault-recovery sweep (#772). Live queue —
            pending experiments, results, and the reasons each ran.
          </p>
        </div>
        <div class="meta">
          <span v-if="lastUpdated">updated {{ lastUpdated }}</span>
          <span v-if="loading" class="dot">●</span>
        </div>
      </header>

      <div class="controls">
        <div class="seg">
          <button :class="{ on: classFilter === 'all' }" @click="classFilter = 'all'">All</button>
          <button :class="{ on: classFilter === 'config' }" @click="classFilter = 'config'">
            config ({{ totals.config }})
          </button>
          <button :class="{ on: classFilter === 'fault' }" @click="classFilter = 'fault'">
            fault ({{ totals.fault }})
          </button>
        </div>
        <input v-model="search" class="search" placeholder="filter by id / recipe / why / mode…" />
        <div class="seg">
          <button :class="{ on: viewMode === 'board' }" @click="viewMode = 'board'">Board</button>
          <button :class="{ on: viewMode === 'graph' }" @click="viewMode = 'graph'">Graph</button>
          <button :class="{ on: viewMode === 'history' }" @click="viewMode = 'history'">History</button>
        </div>
        <button class="refresh" @click="load">↻</button>
      </div>

      <p v-if="error" class="err">Couldn't load sweep queue: {{ error }}</p>
      <p v-else-if="!experiments.length && !loading" class="empty">
        No experiments yet. Run <code>harness sweep seed</code> from the runner.
      </p>

      <section class="scope">
        <div class="scope-head">
          <strong>Scope</strong>
          <span class="scope-sub">
            what the sweep is allowed to run — click to disable a value; its experiments stay
            <em>pending</em> but are never claimed (no delete)
          </span>
          <span v-if="disabledCount" class="scope-badge">{{ disabledCount }} disabled</span>
        </div>
        <div v-for="dim in SCOPE_DIMS" :key="dim" class="scope-row">
          <span class="scope-dim">{{ dim }}</span>
          <button
            v-for="val in scopeValues(dim)"
            :key="val"
            class="chip"
            :class="{ off: !isEnabled(dim, val) }"
            :title="isEnabled(dim, val) ? 'enabled — click to disable' : 'disabled — click to enable'"
            @click="toggleScope(dim, val)"
          >{{ val }}</button>
          <span v-if="!scopeValues(dim).length" class="scope-empty">—</span>
        </div>
      </section>

      <LabelFrequencyTable class="lft-block" />

      <!-- Graph: the lineage DAG (seed → isolation fan → bisect, + A/B groups) -->
      <div v-show="viewMode === 'graph'" class="graphwrap">
        <div ref="graphEl" class="graph"></div>
        <aside class="graph-side">
          <p class="graph-hint">
            Arrows = lineage (parent → child) · dashed = A/B group · color = verdict.
            Click a node for details.
          </p>
          <SweepJobDetail v-if="selectedExp" :e="selectedExp" />
        </aside>
      </div>

      <!-- History: append-only log of every run (sweep_runs); re-runs don't overwrite -->
      <div v-show="viewMode === 'history'" class="history">
        <p class="hist-sub">
          Every run we've made — append-only ({{ history.length }} runs, newest first). The queue collapses by
          recipe; this keeps the full record (kept a year; plays kept 90 days).
        </p>
        <table v-if="history.length" class="hist-table">
          <thead>
            <tr><th>when</th><th>experiment</th><th>verdict</th><th>recipe</th><th>why</th><th>conclusion</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-for="r in history" :key="r.play_id">
              <td class="nowrap">{{ runDate(r.run_at) }}</td>
              <td><code>{{ r.exp_id }}</code><div class="hist-axes">{{ r.platform }} · {{ r.protocol }} · {{ r.mode }}</div></td>
              <td><span v-if="r.verdict" class="verdict" :class="verdictClass(r.verdict)">{{ r.verdict }}</span><span v-else>—</span></td>
              <td>{{ r.recipe }}</td>
              <td class="hist-why">{{ r.why_text || r.why || '—' }}</td>
              <td class="hist-note">{{ r.note || '—' }}</td>
              <td><a v-if="runViewer(r)" :href="runViewer(r)!" target="_blank" rel="noopener" class="viewer">↗</a></td>
            </tr>
          </tbody>
        </table>
        <p v-else-if="historyLoaded" class="empty">No runs recorded yet. They appear here after <code>harness sweep analyze</code>.</p>
      </div>

      <div v-show="viewMode === 'board'" class="board">
        <section v-for="s in STATUSES" :key="s" class="col">
          <h2>
            {{ STATUS_LABEL[s] }}
            <span class="count">{{ byStatus[s].length }}</span>
          </h2>
          <div class="cards">
            <article
              v-for="(e, i) in byStatus[s]"
              :key="e.exp_id"
              class="card"
              :class="['cls-' + (e.class || 'config'), { expanded: expanded.has(e.exp_id) }]"
              @click="toggleExpand(e.exp_id)"
            >
              <div class="row1">
                <span class="kind" :class="'k-' + e.kind">{{ e.kind }}</span>
                <span v-if="s === 'backlog' && i === 0" class="next" title="highest scheduler score — claimed next">next →</span>
                <span v-if="e.verdict" class="verdict" :class="verdictClass(e.verdict)">{{ e.verdict }}</span>
                <span class="score" :title="'scheduler score ' + e.score">{{ Math.round(e.score) }}</span>
                <span class="chev">{{ expanded.has(e.exp_id) ? '▾' : '▸' }}</span>
              </div>
              <div class="recipe">{{ e.recipe }}</div>
              <div class="axes">{{ e.platform }} · {{ e.protocol }} · {{ e.mode }}</div>
              <div v-if="e.why" class="why" :title="e.why_text">💡 {{ e.why }}</div>
              <div v-if="e.parent" class="parent">↳ from {{ e.parent }}<span v-if="e.depth"> · d{{ e.depth }}</span></div>

              <!-- expanded: all details for this job -->
              <SweepJobDetail v-if="expanded.has(e.exp_id)" :e="e" @click.stop />

              <div class="foot">
                <code class="id">{{ e.exp_id }}</code>
                <a
                  v-if="viewerURL(e)"
                  :href="viewerURL(e)!"
                  class="viewer"
                  :class="{ live: s === 'running' }"
                  target="_blank"
                  rel="noopener"
                  @click.stop
                >{{ s === 'running' ? '▶ watch live' : '↗ viewer' }}</a>
                <span v-else-if="s === 'running'" class="starting" title="session not bootstrapped yet">⏳ starting…</span>
              </div>
            </article>
          </div>
        </section>
      </div>
    </div>
  </ShellLayout>
</template>

<style scoped>
.sweep-page { padding: 1rem 1.25rem; color: var(--text-primary, #202124); }
.head { display: flex; justify-content: space-between; align-items: flex-start; gap: 1rem; }
.head h1 { margin: 0; font-size: 1.4rem; }
.sub { margin: .2rem 0 0; color: var(--text-secondary, #5f6368); font-size: .85rem; max-width: 60ch; }
.meta { color: var(--text-secondary, #5f6368); font-size: .8rem; white-space: nowrap; }
.meta .dot { color: var(--success, #1e8e3e); margin-left: .4rem; animation: pulse 1s infinite; }
@keyframes pulse { 50% { opacity: .3; } }
.controls { display: flex; gap: .6rem; align-items: center; margin: 1rem 0; flex-wrap: wrap; }
.seg { display: inline-flex; border: 1px solid var(--border, #dadce0); border-radius: 6px; overflow: hidden; }
.seg button { background: var(--background, #fff); color: var(--text-secondary, #5f6368); border: none; padding: .35rem .7rem; cursor: pointer; font-size: .82rem; }
.seg button.on { background: var(--primary-blue, #1a73e8); color: #fff; }
.search { flex: 1; min-width: 200px; background: var(--background, #fff); border: 1px solid var(--border, #dadce0); border-radius: 6px; color: var(--text-primary, #202124); padding: .4rem .6rem; }
.refresh { background: var(--background, #fff); border: 1px solid var(--border, #dadce0); border-radius: 6px; color: var(--text-secondary, #5f6368); padding: .4rem .6rem; cursor: pointer; }
.err { color: var(--error, #d93025); }
.empty { color: var(--text-secondary, #5f6368); }
.empty code { background: var(--surface-hover, #f1f3f4); padding: .1rem .35rem; border-radius: 4px; }
.scope { background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: .6rem .75rem; margin: 0 0 1rem; }
.scope-head { display: flex; align-items: baseline; gap: .6rem; margin-bottom: .5rem; flex-wrap: wrap; }
.scope-head strong { font-size: .85rem; }
.scope-sub { color: var(--text-secondary, #5f6368); font-size: .76rem; }
.scope-badge { margin-left: auto; background: var(--warning-light, #fef7e0); color: #a86a00; border-radius: 10px; padding: 0 .55rem; font-size: .72rem; font-weight: 600; }
.scope-row { display: flex; align-items: center; gap: .35rem; padding: .15rem 0; flex-wrap: wrap; }
.scope-dim { width: 5rem; flex: none; color: var(--text-secondary, #5f6368); font-size: .76rem; text-transform: uppercase; letter-spacing: .03em; }
.chip { background: var(--primary-blue-light, #e8f0fe); color: var(--primary-blue, #1a73e8); border: 1px solid transparent; border-radius: 12px; padding: .15rem .6rem; font-size: .76rem; cursor: pointer; }
.chip:hover { border-color: var(--primary-blue, #1a73e8); }
.chip.off { background: var(--background, #fff); color: var(--text-disabled, #9aa0a6); border-color: var(--border, #dadce0); text-decoration: line-through; }
.scope-empty { color: var(--text-disabled, #9aa0a6); font-size: .76rem; }
.lft-block { margin: 0 0 1rem; }
.board { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: .8rem; align-items: start; }
.col { background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: .5rem; }
.col h2 { font-size: .8rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-secondary, #5f6368); margin: .2rem .2rem .6rem; display: flex; justify-content: space-between; }
.count { background: var(--primary-blue-light, #e8f0fe); color: var(--primary-blue, #1a73e8); border-radius: 10px; padding: 0 .5rem; font-size: .75rem; }
.cards { display: flex; flex-direction: column; gap: .5rem; }
.card { background: var(--background, #fff); border: 1px solid var(--border-light, #e8eaed); border-left: 3px solid var(--primary-blue, #1a73e8); border-radius: 6px; padding: .5rem .6rem; font-size: .82rem; cursor: pointer; }
.card:hover { border-color: var(--border, #dadce0); }
.card.expanded { box-shadow: 0 1px 6px rgba(60,64,67,.15); }
.card.cls-fault { border-left-color: var(--warning, #f9ab00); }
.chev { color: var(--text-disabled, #9aa0a6); font-size: .7rem; margin-left: .3rem; }
/* per-job detail styles now live in SweepJobDetail.vue (shared by board + graph) */
.graphwrap { display: grid; grid-template-columns: 1fr minmax(260px, 340px); gap: .8rem; align-items: start; }
.graph { height: 70vh; min-height: 420px; background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; }
.graph-side { background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: .6rem .75rem; max-height: 70vh; overflow: auto; }
.graph-hint { color: var(--text-secondary, #5f6368); font-size: .76rem; margin: 0 0 .4rem; }
@media (max-width: 760px) { .graphwrap { grid-template-columns: 1fr; } }
.history { background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: .75rem 1rem; }
.hist-sub { color: var(--text-secondary, #5f6368); font-size: .8rem; margin: 0 0 .6rem; }
.hist-table { width: 100%; border-collapse: collapse; font-size: .8rem; }
.hist-table th { text-align: left; color: var(--text-secondary, #5f6368); font-size: .72rem; text-transform: uppercase; letter-spacing: .03em; border-bottom: 1px solid var(--border, #dadce0); padding: .3rem .5rem; }
.hist-table td { padding: .4rem .5rem; border-bottom: 1px solid var(--border-light, #e8eaed); vertical-align: top; }
.hist-table code { font-size: .72rem; color: var(--text-primary, #202124); }
.hist-axes { color: var(--text-disabled, #9aa0a6); font-size: .7rem; margin-top: .1rem; }
.hist-why { color: var(--text-secondary, #5f6368); max-width: 24ch; }
.hist-note { color: var(--text-primary, #202124); max-width: 32ch; }
.nowrap { white-space: nowrap; color: var(--text-secondary, #5f6368); }
.row1 { display: flex; justify-content: space-between; align-items: center; gap: .4rem; }
.kind { font-size: .68rem; text-transform: uppercase; letter-spacing: .03em; color: var(--text-secondary, #5f6368); }
.next { font-size: .64rem; font-weight: 700; color: var(--success, #1e8e3e); background: var(--success-light, #e6f4ea); border-radius: 8px; padding: 0 .4rem; }
.score { margin-left: auto; font-size: .68rem; color: var(--text-disabled, #9aa0a6); font-variant-numeric: tabular-nums; }
.k-isolation { color: #b06a00; }
.k-bisect { color: #6f42c1; }
.verdict { font-size: .7rem; font-weight: 600; padding: 0 .4rem; border-radius: 8px; }
.v-aberration { background: var(--error-light, #fce8e6); color: var(--error, #d93025); }
.v-notable { background: var(--warning-light, #fef7e0); color: #a86a00; }
.v-clean { background: var(--success-light, #e6f4ea); color: var(--success, #1e8e3e); }
.v-inconclusive { background: var(--surface-hover, #f1f3f4); color: var(--text-disabled, #9aa0a6); }
.recipe { font-weight: 600; margin: .25rem 0 .1rem; color: var(--text-primary, #202124); }
.axes { color: var(--text-secondary, #5f6368); font-size: .76rem; }
.why { margin-top: .35rem; color: var(--text-primary, #202124); font-size: .78rem; }
.parent { margin-top: .25rem; color: var(--text-disabled, #9aa0a6); font-size: .73rem; }
.foot { display: flex; justify-content: space-between; align-items: center; margin-top: .4rem; gap: .4rem; }
.id { font-size: .66rem; color: var(--text-disabled, #9aa0a6); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 60%; }
.viewer { color: var(--primary-blue, #1a73e8); text-decoration: none; font-size: .73rem; white-space: nowrap; }
.viewer:hover { text-decoration: underline; }
.viewer.live { color: var(--success, #1e8e3e); font-weight: 700; }
.starting { color: var(--text-secondary, #5f6368); font-size: .73rem; white-space: nowrap; }
</style>
