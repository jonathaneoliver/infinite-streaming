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
  owner: string;
  claimed_at: string;
  raw_json: string;
  score: number;
  created_at: string;
  updated_at: string;
}

// The full recipe-of-record stored in raw_json (the runner replays this).
interface RawExperiment {
  content?: string;
  duration_s?: number;
  reps?: number;
  rep_group?: string;
  fault?: Record<string, unknown>;
  shape?: Record<string, unknown>;
  content_manipulation?: Record<string, unknown>;
  transfer_timeouts?: Record<string, unknown>;
  result?: { verdict?: string; labels?: string[]; note?: string };
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

// Parse the full recipe-of-record out of raw_json (legacy rows have none).
// Cached by the raw_json string so template re-renders don't re-parse.
const rawCache = new Map<string, RawExperiment>();
function rawOf(e: Experiment): RawExperiment {
  if (!e.raw_json) return {};
  let r = rawCache.get(e.raw_json);
  if (!r) {
    try { r = JSON.parse(e.raw_json) as RawExperiment; } catch { r = {}; }
    rawCache.set(e.raw_json, r);
  }
  return r;
}

// Render a recipe sub-object (fault/shape/…) as "k=v · k=v" for compact display.
function kv(obj?: Record<string, unknown>): string {
  if (!obj) return '';
  return Object.entries(obj)
    .filter(([, v]) => v !== '' && v !== null && v !== undefined)
    .map(([k, v]) => `${k}=${v}`)
    .join(' · ');
}

function chDateToLocal(s: string): string {
  if (!s || s.startsWith('1970')) return '—';
  const d = new Date(s.replace(' ', 'T') + 'Z');
  return isNaN(d.getTime()) ? s : d.toLocaleString();
}

function isStaleClaim(claimedAt: string): boolean {
  if (!claimedAt || claimedAt.startsWith('1970')) return false;
  const d = new Date(claimedAt.replace(' ', 'T') + 'Z');
  return !isNaN(d.getTime()) && Date.now() - d.getTime() > 60 * 60 * 1000; // > 60 min
}

// Derived "what to do next" — a pure function of the row (the agenda logic),
// so the page tells you how to keep going from CH state alone.
function nextAction(e: Experiment): { label: string; tone: 'go' | 'warn' | 'muted' } {
  switch (e.status) {
    case 'backlog':
      return { label: 'Claim & run', tone: 'go' };
    case 'running':
      if (!e.play_id) return isStaleClaim(e.claimed_at)
        ? { label: 'Reap & re-claim (stale)', tone: 'warn' }
        : { label: 'Running — probe in flight', tone: 'muted' };
      if (!e.verdict) return { label: `Analyze play ${e.play_id.slice(0, 8)}`, tone: 'go' };
      return { label: 'Analyzed — awaiting move', tone: 'muted' };
    case 'found':
      return { label: 'Investigate → isolate → promote', tone: 'go' };
    case 'review':
    case 'feedback':
      return { label: 'Needs a human', tone: 'warn' };
    case 'done':
      return { label: 'Done (clean)', tone: 'muted' };
    default:
      return { label: '—', tone: 'muted' };
  }
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

      <div class="board">
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
              <div v-if="expanded.has(e.exp_id)" class="detail" @click.stop>
                <div class="na" :class="'na-' + nextAction(e).tone">
                  <span class="na-k">next</span> {{ nextAction(e).label }}
                </div>

                <h3>Recipe</h3>
                <dl>
                  <div><dt>class</dt><dd>{{ e.class || 'config' }}</dd></div>
                  <div><dt>content</dt><dd>{{ rawOf(e).content || '—' }}</dd></div>
                  <div><dt>mode</dt><dd>{{ e.mode }}<span v-if="rawOf(e).duration_s"> · {{ rawOf(e).duration_s }}s</span></dd></div>
                  <div v-if="rawOf(e).fault"><dt>fault</dt><dd>{{ kv(rawOf(e).fault) }}</dd></div>
                  <div v-if="rawOf(e).shape"><dt>shape</dt><dd>{{ kv(rawOf(e).shape) }}</dd></div>
                  <div v-if="rawOf(e).content_manipulation"><dt>content&nbsp;manip</dt><dd>{{ kv(rawOf(e).content_manipulation) }}</dd></div>
                  <div v-if="rawOf(e).transfer_timeouts"><dt>xfer&nbsp;timeout</dt><dd>{{ kv(rawOf(e).transfer_timeouts) }}</dd></div>
                </dl>

                <h3>Provenance &amp; trigger</h3>
                <dl>
                  <div><dt>kind</dt><dd>{{ e.kind }}<span v-if="e.arm"> · {{ e.arm }}</span></dd></div>
                  <div v-if="e.group_id"><dt>group</dt><dd>{{ e.group_id }}</dd></div>
                  <div v-if="e.parent"><dt>parent</dt><dd>{{ e.parent }} · depth {{ e.depth }}</dd></div>
                  <div v-if="rawOf(e).reps"><dt>reps</dt><dd>{{ rawOf(e).reps }}<span v-if="rawOf(e).rep_group"> · {{ rawOf(e).rep_group }}</span></dd></div>
                  <div><dt>why</dt><dd>{{ e.why_text || e.why || '— (no trigger recorded)' }}</dd></div>
                </dl>

                <h3>Outcome</h3>
                <dl>
                  <div><dt>verdict</dt><dd><span v-if="e.verdict" class="verdict" :class="verdictClass(e.verdict)">{{ e.verdict }}</span><span v-else>—</span></dd></div>
                  <div v-if="rawOf(e).result?.note"><dt>note</dt><dd>{{ rawOf(e).result?.note }}</dd></div>
                  <div v-if="rawOf(e).result?.labels?.length"><dt>labels</dt><dd class="labels"><code v-for="l in rawOf(e).result?.labels" :key="l">{{ l }}</code></dd></div>
                  <div v-if="e.owner"><dt>owner</dt><dd>{{ e.owner }} · claimed {{ chDateToLocal(e.claimed_at) }}</dd></div>
                  <div v-if="e.play_id"><dt>play</dt><dd>{{ e.play_id }}</dd></div>
                  <div><dt>created</dt><dd>{{ chDateToLocal(e.created_at) }} · updated {{ chDateToLocal(e.updated_at) }}</dd></div>
                </dl>
              </div>

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
.detail { margin: .5rem 0 .2rem; border-top: 1px solid var(--border-light, #e8eaed); padding-top: .5rem; }
.detail h3 { font-size: .66rem; text-transform: uppercase; letter-spacing: .04em; color: var(--text-secondary, #5f6368); margin: .55rem 0 .25rem; }
.detail h3:first-of-type { margin-top: .4rem; }
.detail dl { margin: 0; }
.detail dl > div { display: flex; gap: .5rem; padding: .08rem 0; align-items: baseline; }
.detail dt { flex: none; width: 6.5rem; color: var(--text-disabled, #9aa0a6); font-size: .72rem; }
.detail dd { margin: 0; color: var(--text-primary, #202124); font-size: .76rem; word-break: break-word; min-width: 0; }
.detail dd.labels { display: flex; flex-wrap: wrap; gap: .25rem; }
.detail dd.labels code { background: var(--surface-hover, #f1f3f4); border-radius: 3px; padding: 0 .3rem; font-size: .68rem; }
.na { border-radius: 5px; padding: .25rem .5rem; font-size: .76rem; font-weight: 600; margin-bottom: .4rem; }
.na-k { font-size: .62rem; text-transform: uppercase; letter-spacing: .04em; opacity: .7; margin-right: .35rem; }
.na-go { background: var(--success-light, #e6f4ea); color: var(--success, #1e8e3e); }
.na-warn { background: var(--warning-light, #fef7e0); color: #a86a00; }
.na-muted { background: var(--surface-hover, #f1f3f4); color: var(--text-secondary, #5f6368); }
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
