<script setup lang="ts">
/**
 * Study Report (#880, Gap 1 — the UI home for consumer 1).
 *
 * The Sessions table "rolled up": instead of one row per play, it categorizes
 * the plays of a group/study by the fields that VARY (the variant/IV fields,
 * e.g. manifest_variant), pins the fields shared by every play (the invariant
 * fields) as context, and aggregates each category DISTRIBUTIONALLY —
 * percentiles (p10/p50/p90/p99) for continuous metrics like TTFF, a rate for
 * stalls, and per-label incidence (% of the category's sessions carrying it).
 *
 * The invariant/variant auto-split + QoE-scoped verdict mirror the harness
 * `char report` logic (ported to TS). Data comes from /analytics/api/v2/plays
 * filtered server-side by ?group= (falls back to the whole window on an older
 * forwarder, which the group prefix below then narrows client-side).
 */
import { ref, computed, onMounted } from 'vue';
import { listPlays, type PlaySummary } from '@/repo/v2-repo';
import ShellLayout from '@/components/ShellLayout.vue';

const WINDOW_DAYS = 30;
const qs = new URLSearchParams(window.location.search);
const groupInput = ref(qs.get('group') ?? '');
const activeGroup = ref('');
const items = ref<PlaySummary[]>([]); // plays for the selected study's report
const dirItems = ref<PlaySummary[]>([]); // recent plays for the study directory
const loading = ref(false);
const error = ref('');
const mode = ref<'agg' | 'plays'>('agg');

function fromISO(): string {
  return new Date(Date.now() - WINDOW_DAYS * 86400000).toISOString();
}

// ---- study directory (shown when no ?group=): one row per study, where a
// "study" is the group_id prefix before the run stamp (e.g. seg-trio-valley). ----
function studyKey(p: PlaySummary): string {
  return String(p.group_id ?? '').split('/')[0];
}
interface Study { key: string; runs: Set<string>; plays: number; variants: Set<string>; platforms: Set<string>; last: string }
const studies = computed<Study[]>(() => {
  const map = new Map<string, Study>();
  for (const p of dirItems.value) {
    const k = studyKey(p);
    if (!k) continue;
    let s = map.get(k);
    if (!s) { s = { key: k, runs: new Set(), plays: 0, variants: new Set(), platforms: new Set(), last: '' }; map.set(k, s); }
    s.plays++;
    s.runs.add(String(p.group_id ?? ''));
    const v = facetVal(p, 'manifest_variant'); if (v) s.variants.add(v);
    const pf = facetVal(p, 'platform'); if (pf) s.platforms.add(pf);
    const t = String(p.last_seen_at ?? p.started_at ?? '');
    if (t > s.last) s.last = t;
  }
  return [...map.values()].sort((a, b) => b.last.localeCompare(a.last));
});

async function loadDirectory() {
  loading.value = true; error.value = '';
  try {
    dirItems.value = await listPlays({ from: fromISO(), limit: 5000 });
    if (dirItems.value.length === 0) error.value = `no studies in the last ${WINDOW_DAYS} days`;
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
    dirItems.value = [];
  } finally { loading.value = false; }
}

async function loadReport(g: string) {
  loading.value = true; error.value = '';
  try {
    const rows = await listPlays({ group: g, from: fromISO(), limit: 5000 });
    // Client-side narrow (an older forwarder ignores ?group= and returns the window).
    items.value = rows.filter((p) => String(p.group_id ?? '').startsWith(g));
    if (items.value.length === 0) error.value = `no plays for group "${g}"`;
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
    items.value = [];
  } finally { loading.value = false; }
}

function selectStudy(key: string) {
  groupInput.value = key;
  activeGroup.value = key;
  const u = new URL(window.location.href);
  u.searchParams.set('group', key);
  window.history.pushState({}, '', u);
  loadReport(key);
}
function backToDirectory() {
  activeGroup.value = '';
  groupInput.value = '';
  items.value = [];
  const u = new URL(window.location.href);
  u.searchParams.delete('group');
  window.history.pushState({}, '', u);
  if (!dirItems.value.length) loadDirectory();
}
// Load button / Enter: a value → open that study's report; empty → directory.
function load() {
  const g = groupInput.value.trim();
  if (g) selectStudy(g);
  else backToDirectory();
}

onMounted(() => {
  const g = groupInput.value.trim();
  if (g) { activeGroup.value = g; loadReport(g); }
  else loadDirectory();
});
window.addEventListener('popstate', () => {
  const g = (new URLSearchParams(window.location.search).get('group') ?? '').trim();
  if (g) { groupInput.value = g; activeGroup.value = g; loadReport(g); }
  else { activeGroup.value = ''; if (!dirItems.value.length) loadDirectory(); }
});

// ---- helpers ported from the harness char-report logic ----
const SCENARIO_FACETS = ['manifest_variant', 'platform', 'content_id', 'device_model', 'os_version', 'app_version', 'test'];
const LIFECYCLE = new Set(['unexpected_end', 'unexpected_fault', 'unexpected_startup', 'first_frame', 'play_start', 'session_start', 'server_start', 'loop_server']);
const SEV_RANK: Record<string, number> = { error: 4, critical: 3, warning: 2, info: 1 };
const VERDICT_RANK: Record<string, number> = { premium: 0, ok: 1, warn: 2, BAD: 3 };

function facetVal(p: PlaySummary, f: string): string {
  const sc = (p.scenario ?? {}) as Record<string, unknown>;
  const v = sc[f];
  return v == null ? '' : String(v);
}
function toNum(v: unknown): number | null {
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v === 'string' && v.trim() !== '') { const n = Number(v); return Number.isNaN(n) ? null : n; }
  return null;
}
function percentile(xs: number[], p: number): number | null {
  if (!xs.length) return null;
  const s = [...xs].sort((a, b) => a - b);
  if (s.length === 1) return s[0];
  const idx = (p / 100) * (s.length - 1);
  const lo = Math.floor(idx), hi = Math.ceil(idx);
  return lo === hi ? s[lo] : s[lo] + (s[hi] - s[lo]) * (idx - lo);
}
function labelEvents(p: PlaySummary): { sev: string; event: string }[] {
  const out: { sev: string; event: string }[] = [];
  for (const pair of p.label_histogram ?? []) {
    const lbl = Array.isArray(pair) ? String(pair[0]) : '';
    const i = lbl.indexOf('=');
    if (i < 0) continue;
    out.push({ sev: lbl.slice(0, i), event: lbl.slice(i + 1).replace(/^\*/, '') });
  }
  return out;
}
function playVerdict(p: PlaySummary): string {
  let tier = '';
  let best = 0;
  for (const { sev, event } of labelEvents(p)) {
    if (event.startsWith('qoe_tier_')) { tier = event; continue; }
    if (LIFECYCLE.has(event)) continue;
    best = Math.max(best, SEV_RANK[sev] ?? 0);
  }
  if (tier === 'qoe_tier_premium') return 'premium';
  if (tier === 'qoe_tier_acceptable') return 'ok';
  if (tier === 'qoe_tier_unacceptable') return 'BAD';
  return best >= 3 ? 'BAD' : best === 2 ? 'warn' : 'ok';
}

// ---- invariant / variant auto-split ----
const iv = computed(() => {
  const vals: Record<string, Set<string>> = {};
  for (const p of items.value)
    for (const f of SCENARIO_FACETS) {
      const v = facetVal(p, f);
      if (v) (vals[f] ??= new Set()).add(v);
    }
  const ivCols: string[] = [];
  const constants: string[] = [];
  for (const f of SCENARIO_FACETS) {
    const n = vals[f]?.size ?? 0;
    if (n === 1) constants.push(`${f}=${[...vals[f]!][0]}`);
    else if (n > 1) ivCols.push(f);
  }
  constants.sort();
  return { ivCols, constants };
});

interface Cat { key: string; iv: string[]; plays: PlaySummary[] }
const categories = computed<Cat[]>(() => {
  const cols = iv.value.ivCols;
  const map = new Map<string, Cat>();
  for (const p of items.value) {
    const ivVals = cols.map((f) => facetVal(p, f));
    const key = ivVals.join('');
    let c = map.get(key);
    if (!c) { c = { key, iv: ivVals, plays: [] }; map.set(key, c); }
    c.plays.push(p);
  }
  return [...map.values()].sort((a, b) => a.key.localeCompare(b.key));
});

// ---- per-category aggregates ----
function vals(c: Cat, field: string): number[] {
  return c.plays.map((p) => toNum((p as Record<string, unknown>)[field])).filter((x): x is number => x != null);
}
function pctile(c: Cat, field: string, p: number): number | null {
  return percentile(vals(c, field), p);
}
function stallRatePct(c: Cat): number {
  if (!c.plays.length) return 0;
  const stalled = c.plays.filter((p) => (toNum(p.stalls) ?? 0) > 0).length;
  return (stalled / c.plays.length) * 100;
}
function catVerdict(c: Cat): string {
  let worst = c.plays.length ? 'premium' : '–';
  let wr = -1;
  for (const p of c.plays) {
    const v = playVerdict(p);
    const r = VERDICT_RANK[v] ?? 0;
    if (r > wr) { wr = r; worst = v; }
  }
  return worst;
}

// ---- label incidence (label × category, % of sessions carrying it) ----
const allLabels = computed<string[]>(() => {
  const maxInc: Record<string, number> = {};
  for (const c of categories.value)
    for (const p of c.plays)
      for (const { event } of labelEvents(p)) {
        const inc = labelIncidence(c, event);
        if (inc > (maxInc[event] ?? -1)) maxInc[event] = inc;
      }
  return Object.keys(maxInc).sort((a, b) => {
    // qoe_tier_* first (the verdict distribution), then by peak incidence desc.
    const at = a.startsWith('qoe_tier_') ? 0 : 1;
    const bt = b.startsWith('qoe_tier_') ? 0 : 1;
    if (at !== bt) return at - bt;
    return (maxInc[b] ?? 0) - (maxInc[a] ?? 0) || a.localeCompare(b);
  });
});
function labelIncidence(c: Cat, event: string): number {
  if (!c.plays.length) return 0;
  const has = c.plays.filter((p) => labelEvents(p).some((e) => e.event === event)).length;
  return (has / c.plays.length) * 100;
}

// ---- response curve (TTFF p50, with p90 tail) ----
const curve = computed(() => {
  const pts = categories.value.map((c) => ({
    label: c.iv.join(' / ') || '(all)',
    p50: pctile(c, 'first_frame_s', 50),
    p90: pctile(c, 'first_frame_s', 90),
  }));
  const max = Math.max(0, ...pts.map((p) => p.p90 ?? p.p50 ?? 0));
  return { pts, max };
});

const summary = computed(() => `${items.value.length} plays · ${categories.value.length} categories`);

function fmt(n: number | null, dp = 2): string { return n == null ? '–' : n.toFixed(dp); }
function fmtPct(n: number): string { return `${n.toFixed(0)}%`; }
function barWidth(v: number | null, max: number): string { return max > 0 && v != null ? `${(v / max) * 100}%` : '0%'; }
function healthClass(v: string): string {
  return v === 'BAD' ? 'v-bad' : v === 'warn' ? 'v-warn' : v === 'premium' ? 'v-premium' : 'v-ok';
}
function incClass(pct: number): string {
  return pct >= 66 ? 'inc-hi' : pct >= 33 ? 'inc-mid' : pct > 0 ? 'inc-lo' : 'inc-zero';
}
function fmtTime(t: string): string {
  if (!t) return '–';
  const d = new Date(t.includes('T') ? t : t.replace(' ', 'T') + 'Z');
  return isNaN(d.getTime()) ? t : d.toLocaleString();
}
</script>

<template>
  <ShellLayout active-page="study">
    <template #header><div class="page-title-bar">Study Report</div></template>
    <main class="ism-content-wide">
      <div class="page-header">
        <div class="page-title">Study Report</div>
        <div class="page-subtitle">
          Group/study plays categorized by the fields that varied, aggregated distributionally
          (p10/p50/p90/p99, rates, label incidence).
        </div>
      </div>

      <div class="panel">
        <div class="panel-header">
          <div class="controls">
            <input
              v-model="groupInput"
              class="group-input"
              placeholder="group_id or prefix (e.g. seg-trio-valley)"
              @keyup.enter="load"
            />
            <button class="btn" :disabled="loading" @click="load">{{ loading ? 'loading…' : 'Load' }}</button>
            <div v-if="activeGroup" class="toggle" role="tablist">
              <button :class="['tog', { on: mode === 'agg' }]" @click="mode = 'agg'">Aggregated</button>
              <button :class="['tog', { on: mode === 'plays' }]" @click="mode = 'plays'">Per-play</button>
            </div>
            <span v-if="activeGroup && !error" class="status-message">{{ summary }}</span>
          </div>
        </div>

        <p v-if="error" class="err">{{ error }}</p>

        <!-- ===== Study directory (no study selected) ===== -->
        <template v-else-if="!activeGroup">
          <div v-if="studies.length" class="table-wrap">
            <div class="dir-hint">{{ studies.length }} studies · last {{ WINDOW_DAYS }} days — click a row to open its report</div>
            <table class="tbl">
              <thead>
                <tr><th>study</th><th class="num">runs</th><th class="num">plays</th><th>variants</th><th>platforms</th><th>last run</th></tr>
              </thead>
              <tbody>
                <tr v-for="s in studies" :key="s.key" class="clickable" @click="selectStudy(s.key)">
                  <td class="ivcell">{{ s.key }}</td>
                  <td class="num">{{ s.runs.size }}</td>
                  <td class="num">{{ s.plays }}</td>
                  <td>{{ [...s.variants].join(' / ') || '–' }}</td>
                  <td>{{ [...s.platforms].join(', ') || '–' }}</td>
                  <td class="mono">{{ fmtTime(s.last) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else-if="!loading" class="hint">no studies in the last {{ WINDOW_DAYS }} days</p>
        </template>

        <!-- ===== Aggregated report (a study selected) ===== -->
        <template v-else-if="items.length">
          <a class="back" @click="backToDirectory">← all studies</a>
          <p v-if="iv.constants.length" class="held">
            <span class="held-label">held constant:</span>
            <span v-for="c in iv.constants" :key="c" class="chip">{{ c }}</span>
          </p>

          <!-- ===== Aggregated view ===== -->
          <template v-if="mode === 'agg'">
            <div class="table-wrap">
              <table class="tbl">
                <thead>
                  <tr>
                    <th v-for="col in iv.ivCols" :key="col">{{ col }}</th>
                    <th class="num">n</th>
                    <th class="num">TTFF p10</th>
                    <th class="num">p50</th>
                    <th class="num">p90</th>
                    <th class="num">p99</th>
                    <th class="num">stall&nbsp;rate</th>
                    <th class="num">shifts p50</th>
                    <th>verdict</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="c in categories" :key="c.key">
                    <td v-for="(v, i) in c.iv" :key="i" class="ivcell">{{ v || '–' }}</td>
                    <td class="num">{{ c.plays.length }}</td>
                    <td class="num">{{ fmt(pctile(c, 'first_frame_s', 10)) }}</td>
                    <td class="num strong">{{ fmt(pctile(c, 'first_frame_s', 50)) }}</td>
                    <td class="num">{{ fmt(pctile(c, 'first_frame_s', 90)) }}</td>
                    <td class="num">{{ fmt(pctile(c, 'first_frame_s', 99)) }}</td>
                    <td class="num">{{ fmtPct(stallRatePct(c)) }}</td>
                    <td class="num">{{ fmt(pctile(c, 'bitrate_shifts', 50), 0) }}</td>
                    <td><span class="verdict" :class="healthClass(catVerdict(c))">{{ catVerdict(c) }}</span></td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- response curve -->
            <div class="curve">
              <div class="curve-title">TTFF response — p50 bar, p90 marker (s)</div>
              <div v-for="p in curve.pts" :key="p.label" class="curve-row">
                <div class="curve-label">{{ p.label }}</div>
                <div class="curve-track">
                  <span class="curve-bar" :style="{ width: barWidth(p.p50, curve.max) }"></span>
                  <span v-if="p.p90 != null" class="curve-tail" :style="{ left: barWidth(p.p90, curve.max) }"></span>
                </div>
                <div class="curve-val">{{ fmt(p.p50) }}<span class="curve-tailval"> / {{ fmt(p.p90) }}</span></div>
              </div>
            </div>

            <!-- label incidence -->
            <div v-if="allLabels.length" class="table-wrap incidence">
              <div class="curve-title">Label incidence — % of a category's sessions carrying each label</div>
              <table class="tbl">
                <thead>
                  <tr>
                    <th>label</th>
                    <th v-for="c in categories" :key="c.key" class="num">{{ c.iv.join('/') || '(all)' }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="lbl in allLabels" :key="lbl">
                    <td class="labcell" :class="{ tierrow: lbl.startsWith('qoe_tier_') }">{{ lbl }}</td>
                    <td v-for="c in categories" :key="c.key" class="num">
                      <span :class="['inc', incClass(labelIncidence(c, lbl))]">{{ fmtPct(labelIncidence(c, lbl)) }}</span>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </template>

          <!-- ===== Per-play view ===== -->
          <div v-else class="table-wrap">
            <table class="tbl">
              <thead>
                <tr>
                  <th v-for="col in iv.ivCols" :key="col">{{ col }}</th>
                  <th class="num">TTFF</th>
                  <th class="num">frames</th>
                  <th class="num">dropped</th>
                  <th class="num">stalls</th>
                  <th class="num">shifts</th>
                  <th>verdict</th>
                  <th>play</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="p in items" :key="String(p.play_id)">
                  <td v-for="col in iv.ivCols" :key="col" class="ivcell">{{ facetVal(p, col) || '–' }}</td>
                  <td class="num">{{ fmt(toNum(p.first_frame_s)) }}</td>
                  <td class="num">{{ fmt(toNum(p.frames_displayed), 0) }}</td>
                  <td class="num">{{ fmt(toNum(p.frames_dropped), 0) }}</td>
                  <td class="num">{{ fmt(toNum(p.stalls), 0) }}</td>
                  <td class="num">{{ fmt(toNum(p.bitrate_shifts), 0) }}</td>
                  <td><span class="verdict" :class="healthClass(playVerdict(p))">{{ playVerdict(p) }}</span></td>
                  <td class="mono">{{ String(p.play_id).slice(0, 8) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>

        <p v-else-if="!loading" class="err">no data</p>
      </div>
    </main>
  </ShellLayout>
</template>

<style scoped>
.page-header { margin: 0 0 0.8rem; }
.page-title { font-size: 1.2rem; font-weight: 600; color: var(--text-primary, #202124); }
.page-subtitle { font-size: 0.82rem; color: var(--text-secondary, #5f6368); margin-top: 0.2rem; }
.panel { background: var(--background, #fff); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: 0.9rem 1rem; }
.panel-header { margin-bottom: 0.6rem; }
.controls { display: flex; align-items: center; gap: 0.6rem; flex-wrap: wrap; }
.group-input {
  flex: 1 1 20rem; min-width: 14rem; padding: 0.35rem 0.55rem; font-size: 0.85rem;
  border: 1px solid var(--border, #dadce0); border-radius: 6px; color: var(--text-primary, #202124);
  background: var(--bg-secondary, #f8f9fa);
}
.btn {
  padding: 0.35rem 0.8rem; font-size: 0.82rem; font-weight: 600; cursor: pointer;
  border: 1px solid var(--primary-blue, #1a73e8); border-radius: 6px;
  background: var(--primary-blue, #1a73e8); color: #fff;
}
.btn:disabled { opacity: 0.6; cursor: default; }
.toggle { display: inline-flex; border: 1px solid var(--border, #dadce0); border-radius: 6px; overflow: hidden; }
.tog { padding: 0.32rem 0.7rem; font-size: 0.8rem; cursor: pointer; border: 0; background: var(--background, #fff); color: var(--text-secondary, #5f6368); }
.tog.on { background: var(--primary-blue, #1a73e8); color: #fff; }
.status-message { font-size: 0.8rem; color: var(--text-secondary, #5f6368); }
.err { color: var(--error, #d93025); font-size: 0.85rem; }
.hint { color: var(--text-secondary, #5f6368); font-size: 0.85rem; }
.dir-hint { font-size: 0.78rem; color: var(--text-secondary, #5f6368); margin-bottom: 0.4rem; }
.clickable { cursor: pointer; }
.clickable:hover td { background: var(--surface-hover, #f1f3f4); }
.back { display: inline-block; margin: 0 0 0.6rem; font-size: 0.8rem; color: var(--primary-blue, #1a73e8); cursor: pointer; }
.back:hover { text-decoration: underline; }

.held { display: flex; align-items: center; gap: 0.4rem; flex-wrap: wrap; margin: 0.2rem 0 0.7rem; font-size: 0.78rem; }
.held-label { color: var(--text-secondary, #5f6368); }
.chip { padding: 0.1rem 0.45rem; border-radius: 10px; background: var(--bg-secondary, #f8f9fa); border: 1px solid var(--border-light, #e8eaed); color: var(--text-primary, #202124); font-variant-numeric: tabular-nums; }

.table-wrap { overflow-x: auto; margin-top: 0.4rem; }
.tbl { width: 100%; border-collapse: collapse; font-size: 0.82rem; background: var(--background, #fff); }
.tbl th { text-align: left; color: var(--text-secondary, #5f6368); font-weight: 600; border-bottom: 1px solid var(--border, #dadce0); padding: 0.35rem 0.55rem; white-space: nowrap; }
.tbl td { padding: 0.3rem 0.55rem; border-bottom: 1px solid var(--border-light, #e8eaed); }
.tbl .num { text-align: right; font-variant-numeric: tabular-nums; }
.tbl .strong { font-weight: 700; color: var(--text-primary, #202124); }
.ivcell { font-weight: 600; color: var(--text-primary, #202124); }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; color: var(--text-secondary, #5f6368); }

.verdict { padding: 0.05rem 0.4rem; border-radius: 10px; font-size: 0.75rem; font-weight: 600; }
.v-ok { background: #e6f4ea; color: var(--success, #1e8e3e); }
.v-premium { background: #e8f0fe; color: var(--primary-blue, #1a73e8); }
.v-warn { background: #fef7e0; color: #b06000; }
.v-bad { background: #fce8e6; color: var(--error, #d93025); }

.curve { margin: 1rem 0 0.4rem; }
.curve-title { font-size: 0.78rem; color: var(--text-secondary, #5f6368); margin-bottom: 0.4rem; font-weight: 600; }
.curve-row { display: flex; align-items: center; gap: 0.6rem; margin: 0.2rem 0; }
.curve-label { flex: 0 0 12rem; font-size: 0.8rem; font-weight: 600; color: var(--text-primary, #202124); text-align: right; }
.curve-track { flex: 1 1 auto; position: relative; height: 14px; background: var(--bg-secondary, #f8f9fa); border-radius: 3px; }
.curve-bar { position: absolute; left: 0; top: 0; bottom: 0; background: var(--primary-blue, #1a73e8); border-radius: 3px; }
.curve-tail { position: absolute; top: -2px; bottom: -2px; width: 2px; background: var(--error, #d93025); }
.curve-val { flex: 0 0 6rem; font-size: 0.8rem; font-variant-numeric: tabular-nums; color: var(--text-primary, #202124); }
.curve-tailval { color: var(--text-disabled, #9aa0a6); }

.incidence { margin-top: 1.1rem; }
.labcell { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.76rem; color: var(--text-primary, #202124); white-space: nowrap; }
.labcell.tierrow { font-weight: 700; }
.inc { display: inline-block; min-width: 2.4rem; padding: 0.03rem 0.35rem; border-radius: 4px; font-variant-numeric: tabular-nums; }
.inc-zero { color: var(--text-disabled, #9aa0a6); }
.inc-lo { background: #fef7e0; color: #7a5900; }
.inc-mid { background: #fde293; color: #7a5900; }
.inc-hi { background: #fce8e6; color: var(--error, #d93025); font-weight: 700; }
</style>
