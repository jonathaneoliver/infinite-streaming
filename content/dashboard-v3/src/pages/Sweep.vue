<script setup lang="ts">
/**
 * Sweep.vue — live monitor for the automated fault-injection sweep (#772).
 *
 * The sweep's queue lives as local .sweep/ JSON files on the runner; the loop
 * publishes a snapshot via `harness sweep publish` → forwarder
 * /api/v2/sweep/experiments (ReplacingMergeTree by exp_id). This page polls that
 * endpoint and shows the queue as status columns — pending (backlog) → running →
 * done / found — with each experiment's recipe, the WHY (reason it ran), the
 * VERDICT (result), its lineage (parent → isolation fan), and a session-viewer
 * link when it produced a play. View filters scope by class / search.
 */
import { computed, onMounted, onUnmounted, ref } from 'vue';
import ShellLayout from '@/components/ShellLayout.vue';
import LabelFrequencyTable from '@/components/LabelFrequencyTable.vue';

interface Experiment {
  exp_id: string;
  class: string;
  status: string;
  kind: string;
  platform: string;
  protocol: string;
  mode: string;
  recipe: string;
  arm: string;
  group_id: string;
  parent: string;
  depth: number;
  why: string;
  why_text: string;
  verdict: string;
  player_id: string;
  play_id: string;
  score: number;
  created_at: string;
  updated_at: string;
}

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
        <button class="refresh" @click="load">↻</button>
      </div>

      <p v-if="error" class="err">Couldn't load sweep queue: {{ error }}</p>
      <p v-else-if="!experiments.length && !loading" class="empty">
        No experiments published yet. Run <code>harness sweep publish</code> from the runner.
      </p>

      <LabelFrequencyTable class="lft-block" />

      <div class="board">
        <section v-for="s in STATUSES" :key="s" class="col">
          <h2>
            {{ STATUS_LABEL[s] }}
            <span class="count">{{ byStatus[s].length }}</span>
          </h2>
          <div class="cards">
            <article v-for="(e, i) in byStatus[s]" :key="e.exp_id" class="card" :class="'cls-' + (e.class || 'config')">
              <div class="row1">
                <span class="kind" :class="'k-' + e.kind">{{ e.kind }}</span>
                <span v-if="s === 'backlog' && i === 0" class="next" title="highest scheduler score — claimed next">next →</span>
                <span v-if="e.verdict" class="verdict" :class="verdictClass(e.verdict)">{{ e.verdict }}</span>
                <span class="score" :title="'scheduler score ' + e.score">{{ Math.round(e.score) }}</span>
              </div>
              <div class="recipe">{{ e.recipe }}</div>
              <div class="axes">{{ e.platform }} · {{ e.protocol }} · {{ e.mode }}</div>
              <div v-if="e.why" class="why" :title="e.why_text">💡 {{ e.why }}</div>
              <div v-if="e.parent" class="parent">↳ from {{ e.parent }}<span v-if="e.depth"> · d{{ e.depth }}</span></div>
              <div class="foot">
                <code class="id">{{ e.exp_id }}</code>
                <a
                  v-if="viewerURL(e)"
                  :href="viewerURL(e)!"
                  class="viewer"
                  :class="{ live: s === 'running' }"
                  target="_blank"
                  rel="noopener"
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
.lft-block { margin: 0 0 1rem; }
.board { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: .8rem; align-items: start; }
.col { background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: .5rem; }
.col h2 { font-size: .8rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-secondary, #5f6368); margin: .2rem .2rem .6rem; display: flex; justify-content: space-between; }
.count { background: var(--primary-blue-light, #e8f0fe); color: var(--primary-blue, #1a73e8); border-radius: 10px; padding: 0 .5rem; font-size: .75rem; }
.cards { display: flex; flex-direction: column; gap: .5rem; }
.card { background: var(--background, #fff); border: 1px solid var(--border-light, #e8eaed); border-left: 3px solid var(--primary-blue, #1a73e8); border-radius: 6px; padding: .5rem .6rem; font-size: .82rem; }
.card.cls-fault { border-left-color: var(--warning, #f9ab00); }
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
