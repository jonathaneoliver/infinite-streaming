<script setup lang="ts">
/**
 * LabelFrequencyTable — population-aware triage baseline (#772).
 *
 * Shows how often each severity-tagged label fires across recent sessions
 * (% of plays), so triage can tell ambient labels (fire on most streams →
 * likely threshold noise, or high-impact-if-real) from rare ones (a specific
 * stream is broken). Two derived scores resolve the "common cuts both ways"
 * tension:
 *   - Anomaly = severity × rarity (1 − freq)  → what the SWEEP should chase
 *   - Impact  = severity × frequency          → what PRODUCT should fix first
 * Sort toggles between them. Reads GET /analytics/api/v2/label_frequency.
 */
import { computed, onMounted, ref, watch } from 'vue';
import { labelTooltip, hasGlossary } from '@/lib/labelGlossary';

interface Row { label: string; severity: string; sessions: number | string; pct: number | string; }

const SEV_WEIGHT: Record<string, number> = { error: 4, critical: 3, warning: 2, info: 1, testing: 0 };

const items = ref<Row[]>([]);
const divMap = ref<Record<string, { skew: number; top: string }>>({});
const total = ref(0);
const loading = ref(false);
const error = ref('');
const days = ref(7);
const excludeFaulted = ref(true);

type SortKey = 'label' | 'severity' | 'pct' | 'impact' | 'anomaly' | 'skew';
const sortKey = ref<SortKey>('impact');
const sortDir = ref<'asc' | 'desc'>('desc');
function setSort(k: SortKey) {
  if (sortKey.value === k) sortDir.value = sortDir.value === 'desc' ? 'asc' : 'desc';
  else { sortKey.value = k; sortDir.value = k === 'label' ? 'asc' : 'desc'; }
}
function arrow(k: SortKey): string {
  return sortKey.value === k ? (sortDir.value === 'desc' ? ' ▼' : ' ▲') : '';
}

async function load() {
  loading.value = true;
  error.value = '';
  try {
    const u = `/analytics/api/v2/label_frequency?days=${days.value}&exclude_faulted=${excludeFaulted.value ? 1 : 0}`;
    const resp = await fetch(u);
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = (await resp.json()) as { total: number; items: Row[] | null };
    total.value = data.total ?? 0;
    items.value = data.items ?? [];
    // Divergence / skew per label (max lift across dimensions) — parallel fetch.
    const du = `/analytics/api/v2/label_divergence?days=${days.value}&exclude_faulted=${excludeFaulted.value ? 1 : 0}`;
    const dresp = await fetch(du);
    const m: Record<string, { skew: number; top: string }> = {};
    if (dresp.ok) {
      const dd = (await dresp.json()) as { items: Array<{ label: string; skew: number; top: string }> | null };
      for (const d of dd.items ?? []) m[d.label] = { skew: d.skew, top: d.top };
    }
    divMap.value = m;
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e);
  } finally {
    loading.value = false;
  }
}

onMounted(load);
watch([days, excludeFaulted], load);

// ── label↔dimension association drill-down (#783) ──
interface AssocItem { dim: string; value: string; n: number; with_l: number; pct: number; lift: number; significant: boolean; }
const expanded = ref<string | null>(null);
const assocItems = ref<AssocItem[]>([]);
const assocBaseline = ref(0);
const assocLoading = ref(false);
const assocWithin = ref<string | null>(null); // "dim:value" held fixed (stratify)

async function fetchAssoc(label: string, within: string | null) {
  assocItems.value = [];
  assocLoading.value = true;
  try {
    let u = `/analytics/api/v2/label_associate?label=${encodeURIComponent(label)}&days=${days.value}&exclude_faulted=${excludeFaulted.value ? 1 : 0}`;
    if (within) u += `&within=${encodeURIComponent(within)}`;
    const resp = await fetch(u);
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = (await resp.json()) as { baseline_pct: number; items: AssocItem[] | null };
    assocBaseline.value = data.baseline_pct ?? 0;
    assocItems.value = (data.items ?? []).filter((i) => i.n >= 5).sort((a, b) => b.lift - a.lift);
  } catch {
    assocItems.value = [];
  } finally {
    assocLoading.value = false;
  }
}

function toggleAssoc(label: string) {
  if (expanded.value === label) { expanded.value = null; return; }
  expanded.value = label;
  assocWithin.value = null;
  fetchAssoc(label, null);
}
function drillWithin(dim: string, value: string) {
  if (!expanded.value) return;
  assocWithin.value = `${dim}:${value}`;
  fetchAssoc(expanded.value, assocWithin.value);
}
function clearWithin() {
  if (!expanded.value) return;
  assocWithin.value = null;
  fetchAssoc(expanded.value, null);
}

function pctOf(r: Row): number { return typeof r.pct === 'string' ? parseFloat(r.pct) : r.pct; }
function sevWeight(r: Row): number { return SEV_WEIGHT[r.severity] ?? 0; }
function anomaly(r: Row): number { return sevWeight(r) * (1 - pctOf(r) / 100); }
function impact(r: Row): number { return sevWeight(r) * (pctOf(r) / 100); }

const rows = computed(() => {
  const scored = items.value
    // Triage cares about problems: drop testing= (operator metadata) and info= (informational, not defects).
    .filter((r) => r.severity !== 'testing' && r.severity !== 'info')
    .map((r) => ({
      ...r, _pct: pctOf(r), _anomaly: anomaly(r), _impact: impact(r), _sev: sevWeight(r),
      _skew: divMap.value[r.label]?.skew ?? 1, _top: divMap.value[r.label]?.top ?? '',
    }));
  const dir = sortDir.value === 'asc' ? 1 : -1;
  const k = sortKey.value;
  return scored.sort((a, b) => {
    if (k === 'label') return a.label.localeCompare(b.label) * dir;
    const pick = (x: typeof a) => k === 'severity' ? x._sev : k === 'pct' ? x._pct : k === 'anomaly' ? x._anomaly : k === 'skew' ? x._skew : x._impact;
    return (pick(a) - pick(b)) * dir;
  });
});
</script>

<template>
  <section class="lft">
    <header class="lft-head">
      <h2>Label frequency <span class="sub">across {{ total }} sessions / {{ days }}d</span></h2>
      <div class="ctl">
        <label class="chk"><input type="checkbox" v-model="excludeFaulted" /> exclude faulted</label>
        <select v-model.number="days" class="sel">
          <option :value="1">24h</option><option :value="7">7d</option><option :value="30">30d</option>
        </select>
      </div>
    </header>
    <p v-if="error" class="err">label frequency unavailable: {{ error }}</p>
    <table v-else class="tbl">
      <thead><tr>
        <th class="srt" @click="setSort('label')">label{{ arrow('label') }}</th>
        <th class="srt" @click="setSort('severity')">sev{{ arrow('severity') }}</th>
        <th class="srt num" @click="setSort('pct')" title="% of sessions with this label">% sessions{{ arrow('pct') }}</th>
        <th class="srt num" @click="setSort('impact')" title="severity × frequency — fix-first for product">impact{{ arrow('impact') }}</th>
        <th class="srt num" @click="setSort('anomaly')" title="severity × rarity — chase-first for the sweep">anomaly{{ arrow('anomaly') }}</th>
        <th class="srt num" @click="setSort('skew')" title="max lift across dimensions — how dimension-driven this label is (click the label for the breakdown)">skew{{ arrow('skew') }}</th>
      </tr></thead>
      <tbody>
        <template v-for="r in rows" :key="r.label">
          <tr :class="{ open: expanded === r.label }">
            <td class="lab assoc-toggle" :title="labelTooltip(r.label) || r.label" @click="toggleAssoc(r.label)">
              <span class="caret">{{ expanded === r.label ? '▾' : '▸' }}</span>
              <span :class="{ defd: hasGlossary(r.label) }">{{ r.label }}</span>
            </td>
            <td><span class="sev" :class="'sv-' + r.severity">{{ r.severity }}</span></td>
            <td class="num"><span class="bar" :style="{ width: r._pct + '%' }"></span>{{ r._pct }}%</td>
            <td class="num">{{ r._impact.toFixed(1) }}</td>
            <td class="num">{{ r._anomaly.toFixed(1) }}</td>
            <td class="num skew" :class="{ hot: r._skew >= 3 }" :title="r._top ? 'driven by ' + r._top : ''">{{ r._skew >= 1.05 ? r._skew.toFixed(1) + '×' : '—' }}</td>
          </tr>
          <tr v-if="expanded === r.label" class="assoc">
            <td colspan="6">
              <div v-if="assocLoading" class="assoc-note">loading associations…</div>
              <div v-else-if="!assocItems.length" class="assoc-note">no dimension associations (not enough sessions)</div>
              <template v-else>
                <div class="assoc-head">
                  Which platforms / content does this label hit most? · normally {{ assocBaseline }}% of sessions · ratio = how much more (or less) likely than that — 1× is average
                  <span v-if="assocWithin" class="within-chip">held fixed: {{ assocWithin.replace(':', '=') }} <button @click="clearWithin" title="show all again">✕</button></span>
                </div>
                <table class="assoc-tbl">
                  <thead><tr><th>dimension = value <span class="hint-h">(click to hold fixed →)</span></th><th class="num">sessions</th><th class="num">% with label</th><th class="num">lift</th></tr></thead>
                  <tbody>
                    <tr v-for="a in assocItems" :key="a.dim + a.value" :class="{ sig: a.significant }">
                      <td class="adv" @click="drillWithin(a.dim, a.value)" title="hold this value fixed and compare the other dimensions only within it — a fair, like-for-like comparison">
                        <span class="adim">{{ a.dim }}</span> = {{ a.value }}
                      </td>
                      <td class="num">{{ a.n }}</td>
                      <td class="num">{{ a.pct }}%</td>
                      <td class="num lift" :class="{ up: a.lift >= 1.5, down: a.lift <= 0.67 }">{{ a.lift }}×<span v-if="a.significant" class="star" title="significant (enough sessions + effect)"> ✦</span></td>
                    </tr>
                  </tbody>
                </table>
                <p class="assoc-caveat" v-if="!assocWithin">⚠ A high ratio can mislead — each platform / content was tested differently, so it may reflect <em>what we ran there</em>, not the dimension itself. Click a row to hold that value fixed and compare the rest on equal footing.</p>
              </template>
            </td>
          </tr>
        </template>
      </tbody>
    </table>
    <p class="note">
      Ambient labels (high %) are likely threshold noise — relax via
      <code>FORWARDER_QOE_THRESHOLDS_PATH</code> — or high-impact if real.
      Rare + severe (high anomaly) = a specific stream is broken.
    </p>
  </section>
</template>

<style scoped>
.lft { background: var(--surface, #f8f9fa); border: 1px solid var(--border, #dadce0); border-radius: 8px; padding: .75rem 1rem; color: var(--text-primary, #202124); }
.lft-head { display: flex; justify-content: space-between; align-items: center; gap: 1rem; flex-wrap: wrap; }
.lft-head h2 { font-size: 1rem; margin: 0; }
.sub { color: var(--text-secondary, #5f6368); font-weight: 400; font-size: .8rem; }
.ctl { display: flex; gap: .6rem; align-items: center; }
.chk { color: var(--text-secondary, #5f6368); font-size: .8rem; }
.sel { background: var(--background, #fff); color: var(--text-primary, #202124); border: 1px solid var(--border, #dadce0); border-radius: 6px; padding: .25rem .4rem; }
.err { color: var(--error, #d93025); }
.tbl { width: 100%; border-collapse: collapse; margin-top: .6rem; font-size: .82rem; background: var(--background, #fff); }
.tbl th { text-align: left; color: var(--text-secondary, #5f6368); font-weight: 600; border-bottom: 1px solid var(--border, #dadce0); padding: .3rem .5rem; }
.tbl th.srt { cursor: pointer; user-select: none; }
.tbl th.srt:hover { color: var(--primary-blue, #1a73e8); }
.tbl th.num, .tbl td.num { text-align: right; font-variant-numeric: tabular-nums; }
.tbl td { padding: .25rem .5rem; border-bottom: 1px solid var(--border-light, #e8eaed); }
.lab { font-family: ui-monospace, monospace; color: var(--text-primary, #202124); }
.lab.defd { text-decoration: underline dotted var(--text-disabled, #9aa0a6); text-underline-offset: 2px; cursor: help; }
.sev { font-size: .68rem; text-transform: uppercase; padding: 0 .4rem; border-radius: 8px; }
.sv-error, .sv-critical { background: var(--error-light, #fce8e6); color: var(--error, #d93025); }
.sv-warning { background: var(--warning-light, #fef7e0); color: #a86a00; }
.sv-info { background: var(--info-light, #e8f0fe); color: var(--info, #1a73e8); }
.sv-testing { background: var(--surface-hover, #f1f3f4); color: var(--text-disabled, #9aa0a6); }
.skew.hot { color: var(--error, #d93025); font-weight: 600; }
.num .bar { display: inline-block; height: .55em; background: var(--primary-blue, #1a73e8); border-radius: 2px; margin-right: .4rem; vertical-align: middle; opacity: .35; }
.assoc-toggle { cursor: pointer; }
.assoc-toggle .caret { color: var(--text-disabled, #9aa0a6); margin-right: .3rem; }
tr.open { background: var(--surface-hover, #f1f3f4); }
tr.assoc > td { background: var(--surface, #f8f9fa); padding: .5rem .75rem; }
.assoc-note { color: var(--text-secondary, #5f6368); font-size: .8rem; }
.assoc-head { color: var(--text-secondary, #5f6368); font-size: .76rem; margin-bottom: .35rem; }
.assoc-tbl { width: 100%; border-collapse: collapse; font-size: .8rem; }
.assoc-tbl th { text-align: left; color: var(--text-disabled, #9aa0a6); font-weight: 600; padding: .15rem .4rem; }
.assoc-tbl td { padding: .15rem .4rem; border-top: 1px solid var(--border-light, #e8eaed); }
.assoc-tbl .num { text-align: right; font-variant-numeric: tabular-nums; }
.assoc-tbl tr.sig { font-weight: 600; }
.adim { color: var(--primary-blue, #1a73e8); font-family: ui-monospace, monospace; }
.within-chip { margin-left: .5rem; background: var(--primary-blue-light, #e8f0fe); color: var(--primary-blue, #1a73e8); border-radius: 8px; padding: .05rem .4rem; font-size: .72rem; }
.within-chip button { background: none; border: none; color: var(--primary-blue, #1a73e8); cursor: pointer; font-size: .72rem; padding: 0 0 0 .2rem; }
.adv { cursor: pointer; }
.adv:hover { background: var(--primary-blue-light, #e8f0fe); }
.hint-h { color: var(--text-disabled, #9aa0a6); font-weight: 400; font-size: .7rem; }
.assoc-caveat { color: #a86a00; font-size: .72rem; margin: .4rem 0 0; }
.lift.up { color: var(--error, #d93025); }
.lift.down { color: var(--success, #1e8e3e); }
.star { color: var(--warning, #f9ab00); }
.note { color: var(--text-secondary, #5f6368); font-size: .75rem; margin: .6rem 0 0; }
.note code { background: var(--surface-hover, #f1f3f4); padding: .05rem .3rem; border-radius: 4px; }
</style>
