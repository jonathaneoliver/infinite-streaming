<script setup lang="ts">
/**
 * NetworkShapingPattern.vue — kernel-side rate-stepping engine
 * editor. Sits on `shape.pattern` (a typed nested struct of
 * `{ template, steps, margin_pct, default_step_seconds }`).
 *
 * Five template choices:
 *   - sliders     → pattern is null (rate slider drives the kernel)
 *   - square_wave → alternates high / low across variants
 *   - ramp_up     → ascending rates
 *   - ramp_down   → descending rates
 *   - pyramid     → up then down
 *
 * Picking a non-sliders template GENERATES a step list from the
 * manifest's variants. The user can then edit each row (rate /
 * duration / enabled) before the next change commits.
 *
 * Every control mutation goes through `usePlayer.setPattern(...)`
 * via the same optimistic + revision-cursor pipeline. Slider drags
 * on individual step rates are debounced.
 */
import { computed, ref, toRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import { useManifestVariants } from '@/composables/useManifestVariants';
import type { Pattern } from '@/repo/v2-repo';

const TEMPLATES = ['sliders', 'square_wave', 'ramp_up', 'ramp_down', 'pyramid'] as const;
type Template = (typeof TEMPLATES)[number];

const TEMPLATE_LABELS: Record<Template, string> = {
  sliders:     '🎚 Sliders',
  square_wave: '▁▔ Square wave',
  ramp_up:     '↗ Ramp up',
  ramp_down:   '↘ Ramp down',
  pyramid:     '⛰ Pyramid',
};

function marginLabel(m: number): string {
  return m === 0 ? 'Exact' : `+${m}%`;
}

// 5% is the default: covers TCP/IP + TLS 1.3 + HTTP/2 framing overhead on a
// LAN with no losses (~5% measured), so pattern step rates set at variant
// AVERAGE-BANDWIDTH × 1.05 should keep the player playing without artificial
// stalls. Bigger margins exist for real-WiFi (7-8%), bursty-loss (10%), or
// "way over headroom" stress / pre-buffer tests (25-50%). "Exact" (0%) is
// the deliberate-stall footgun.
const MARGIN_CHOICES = [0, 5, 10, 25, 50] as const;
type Margin = (typeof MARGIN_CHOICES)[number];
const DEFAULT_MARGIN_PCT: Margin = 5;

const STEP_SECONDS_CHOICES = [6, 12, 18, 24] as const;
type StepSeconds = (typeof STEP_SECONDS_CHOICES)[number];

// #551 — fill density. The limit ladder carries BOTH a peak (BANDWIDTH)
// and an average (AVERAGE-BANDWIDTH) rung per variant, each ×(1+margin),
// then inserts geometric fills so no two consecutive caps differ by more
// than this ratio. Mirrors go-proxy/pkg/ladder + the harness CLI's
// --max-step. Higher = coarser + shorter pattern (a pyramid over a dense
// ladder can run ~13 min/cycle).
const MAX_STEP_CHOICES = [1.1, 1.15, 1.2, 1.5, 2.0] as const;
const DEFAULT_MAX_STEP = 1.15;
const maxStep = ref<number>(DEFAULT_MAX_STEP);

interface LadderVariant {
  avgBps: number;
  peakBps: number;
}

const round3 = (v: number): number => Math.round(v * 1000) / 1000;

/** Dual-rung (avg+peak) + geometrically-filled limit ladder, descending
 *  by Mbps. Mirrors go-proxy/pkg/ladder.StandardLadder — keep in sync
 *  with that package's golden vectors (ladder_test.go). */
function standardLadder(variants: LadderVariant[], bumpPct: number, step: number): number[] {
  const f = 1 + bumpPct / 100;
  const anchors: number[] = [];
  for (const v of variants) {
    if (v.peakBps > 0) anchors.push(round3((v.peakBps * f) / 1e6));
    if (v.avgBps > 0) anchors.push(round3((v.avgBps * f) / 1e6));
  }
  anchors.sort((a, b) => b - a); // descending
  if (step <= 1 || anchors.length < 2) return anchors;
  const out: number[] = [];
  for (let i = 0; i < anchors.length; i++) {
    out.push(anchors[i]);
    if (i + 1 >= anchors.length) continue;
    const hi = anchors[i];
    const lo = anchors[i + 1];
    if (lo <= 0 || hi <= lo) continue;
    const n = Math.ceil(Math.log(hi / lo) / Math.log(step)) - 1;
    for (let k = 1; k <= n; k++) {
      out.push(round3(hi * Math.pow(lo / hi, k / (n + 1))));
    }
  }
  return out;
}

const props = defineProps<{ playerId: string }>();
const { player, setPattern } = usePlayer(toRef(props, 'playerId'));

const pattern = computed<Pattern | null>(() => player.value?.shape?.pattern ?? null);

/** What template is currently active? Reads from draft when in edit
 *  mode, otherwise from the server pattern. `sliders` when no pattern. */
const activeTemplate = computed<Template>(() => {
  const src = editMode.value ? draftPattern.value : pattern.value;
  const t = src?.template;
  if (!src) return 'sliders';
  if (t && (TEMPLATES as readonly string[]).includes(t)) return t as Template;
  return 'sliders';
});

const margin = computed<Margin>(() => {
  const src = editMode.value ? draftPattern.value : pattern.value;
  const m = src?.margin_pct;
  return (MARGIN_CHOICES as readonly number[]).includes(m as number) ? (m as Margin) : DEFAULT_MARGIN_PCT;
});

const defaultStepSeconds = computed<StepSeconds>(() => {
  const src = editMode.value ? draftPattern.value : pattern.value;
  const d = src?.default_step_seconds;
  return (STEP_SECONDS_CHOICES as readonly number[]).includes(d as number) ? (d as StepSeconds) : 6;
});

/** Manifest variants sorted ascending by bandwidth. */
const { variants: rawVariants } = useManifestVariants(toRef(props, 'playerId'));
const sortedVariants = computed(() => {
  return [...rawVariants.value].sort((a, b) => (a.bandwidth ?? 0) - (b.bandwidth ?? 0));
});

/** Preset choices for a single step row, including a synthetic "Custom"
 *  for any rate that doesn't match a variant rung exactly. Sources:
 *    - 0 Mbps (stall test)
 *    - each manifest variant @ current margin (using AVERAGE-BANDWIDTH
 *      when the playlist provided it, else falling back to BANDWIDTH)
 *    - +10% over top variant (deliberate over-headroom)
 */
const stepPresets = computed<{ value: number; label: string }[]>(() => {
  const items: { value: number; label: string }[] = [];
  items.push({ value: 0, label: '0 Mbps (stall)' });
  const f = 1 + margin.value / 100;
  // #551 — offer BOTH rungs per variant: the average (AVERAGE-BANDWIDTH,
  // long-term sustainable) and the peak (BANDWIDTH, per-segment peak,
  // typically 30-40% higher). Carrying both lets an operator park a cap
  // inside a variant's avg→peak band to probe which scalar the player
  // keys on.
  for (const v of sortedVariants.value) {
    const avg = ((v as any).average_bandwidth as number) || 0;
    const peak = (v.bandwidth as number) || 0;
    if (avg > 0) {
      const mbps = round3((avg * f) / 1e6);
      if (mbps > 0) items.push({ value: mbps, label: `${v.resolution} avg · ${mbps.toFixed(2)} Mbps` });
    }
    if (peak > 0) {
      const mbps = round3((peak * f) / 1e6);
      if (mbps > 0) items.push({ value: mbps, label: `${v.resolution} peak · ${mbps.toFixed(2)} Mbps` });
    }
  }
  const top = items[items.length - 1];
  if (top && top.value > 0) {
    const headroom = Math.round(top.value * 1.1 * 100) / 100;
    items.push({ value: headroom, label: `+10% over top · ${headroom.toFixed(2)} Mbps` });
  }
  return items;
});

/** Match a step's current rate to a preset, or return null (Custom). */
function presetForRate(rate: number): string | null {
  const match = stepPresets.value.find((p) => Math.abs(p.value - rate) < 0.005);
  return match ? String(match.value) : null;
}
function onPresetChange(idx: number, e: Event) {
  const v = (e.target as HTMLSelectElement).value;
  if (v === '') return; // Custom — keep the existing number-input rate
  setStepField(idx, 'rate_mbps', Number(v));
}

/** Adapt the manifest variants to the ladder's neutral shape. */
function ladderVariants(): LadderVariant[] {
  return sortedVariants.value.map((v) => ({
    avgBps: ((v as any).average_bandwidth as number) || 0,
    peakBps: (v.bandwidth as number) || 0,
  }));
}

/** Generate step rates from template + margin + variants. Uses the shared
 *  dual-rung + geometrically-filled limit ladder (#551), then orders it
 *  per template — mirrors ladder.BuildPattern in go-proxy/pkg/ladder. */
function buildSteps(t: Template, marginPct: number, stepSecs: number): Pattern['steps'] {
  const desc = standardLadder(ladderVariants(), marginPct, maxStep.value); // descending
  if (!desc.length) return [];
  const asc = desc.slice().reverse(); // ascending

  let seq: number[] = [];
  if (t === 'square_wave') {
    seq = [asc[0], asc[asc.length - 1]]; // lowest + highest
  } else if (t === 'ramp_up') {
    seq = asc.slice();
  } else if (t === 'ramp_down') {
    seq = asc.slice().reverse();
  } else if (t === 'pyramid') {
    seq = asc.concat(asc.slice(0, -1).reverse()); // up then down, no apex dupe
  } else {
    seq = [];
  }

  return seq.map((rate_mbps) => ({
    duration_seconds: stepSecs,
    rate_mbps,
    enabled: true,
  }));
}

/* ─── Mutation helpers ──────────────────────────────────────────── */
//
// Two-state Apply / Edit flow (matches legacy):
//   - When a pattern is APPLIED on the server, the panel is in
//     "read-only" mode showing the runtime state, with an
//     "Edit Pattern" button.
//   - "Edit Pattern" puts the panel in DRAFT mode. Template / margin /
//     step duration / step rows edit `draftPattern`, not the server.
//     "Apply Pattern" commits the draft; "Cancel" discards it.
//   - When no pattern is applied and user picks a template, panel
//     enters draft mode automatically (template choice IS the start of
//     a draft).

const draftPattern = ref<Pattern | null>(null);
const editMode = ref(false);

// Bail out of edit mode when the server-side pattern disappears beneath
// us. Happens when the operator drives the rate / delay / loss sliders
// (ShapeSliders.vue → setRate), runs `harness shape --rate N` from a
// terminal, or hits "Clear shape" from a different surface. In all of
// those flows the server clears `shape.pattern` to null; without this
// watcher the pattern editor sticks open showing the stale draft and
// the template selector stays on the disarmed template instead of
// snapping back to "Sliders." Triggers only on the transition from
// non-null → null so re-arming a pattern from elsewhere doesn't kick
// the operator out of an active edit.
watch(pattern, (now, prev) => {
  if (prev != null && now == null && editMode.value) {
    editMode.value = false;
    draftPattern.value = null;
  }
});

// What the UI binds to — draft when in edit mode, server otherwise.
const editing = computed<Pattern | null>(() => editMode.value ? draftPattern.value : pattern.value);

// Recompute the same UX-facing fields from `editing` so the existing
// controls work in both modes without further changes.
function commit(next: Pattern | null) {
  setPattern(next as any);
}

function startEdit() {
  draftPattern.value = pattern.value
    ? JSON.parse(JSON.stringify(pattern.value)) as Pattern
    : null;
  editMode.value = true;
}

function cancelEdit() {
  draftPattern.value = null;
  editMode.value = false;
}

function applyDraft() {
  commit(draftPattern.value);
  editMode.value = false;
  draftPattern.value = null;
}

function ensureDraft(): Pattern {
  if (!draftPattern.value) {
    // Seed an empty pattern using current margin / step seconds defaults.
    // 5% margin is the default — covers protocol framing overhead so the
    // bottom rung doesn't artificially stall the player.
    draftPattern.value = {
      template: 'ramp_up' as any,
      margin_pct: DEFAULT_MARGIN_PCT as any,
      default_step_seconds: 6,
      steps: [],
    } as Pattern;
  }
  return draftPattern.value;
}

function onTemplateChange(t: Template) {
  if (t === 'sliders') {
    // Switching to sliders disarms the pattern immediately whether in
    // edit mode or not — matches legacy behaviour.
    commit(null);
    editMode.value = false;
    draftPattern.value = null;
    return;
  }
  if (!editMode.value) startEdit();
  const draft = ensureDraft();
  const m = (draft.margin_pct ?? 0) as number;
  const d = (draft.default_step_seconds ?? 6) as number;
  draftPattern.value = {
    template: t as any,
    margin_pct: m as any,
    default_step_seconds: d as any,
    steps: buildSteps(t, m, d),
  } as Pattern;
}

function onMarginChange(m: Margin) {
  if (!editMode.value) startEdit();
  const draft = ensureDraft();
  const t = (draft.template as Template) ?? 'ramp_up';
  if (t === 'sliders' as Template) return;
  const d = (draft.default_step_seconds ?? 6) as number;
  draftPattern.value = {
    template: t as any,
    margin_pct: m as any,
    default_step_seconds: d as any,
    steps: buildSteps(t, m, d),
  } as Pattern;
}

function onStepSecondsChange(d: StepSeconds) {
  if (!editMode.value) startEdit();
  const draft = ensureDraft();
  const t = (draft.template as Template) ?? 'ramp_up';
  if (t === 'sliders' as Template) return;
  const m = (draft.margin_pct ?? 0) as number;
  draftPattern.value = {
    template: t as any,
    margin_pct: m as any,
    default_step_seconds: d as any,
    steps: buildSteps(t, m, d),
  } as Pattern;
}

// #551 — fill density. Not part of the server Pattern payload (it only
// shapes how many client-side steps buildSteps emits), so we keep it in a
// local ref and regenerate the draft steps when it changes.
function onMaxStepChange(s: number) {
  maxStep.value = s;
  if (!editMode.value) startEdit();
  const draft = ensureDraft();
  const t = (draft.template as Template) ?? 'ramp_up';
  if (t === 'sliders' as Template) return;
  const m = (draft.margin_pct ?? 0) as number;
  const d = (draft.default_step_seconds ?? 6) as number;
  draftPattern.value = {
    template: t as any,
    margin_pct: m as any,
    default_step_seconds: d as any,
    steps: buildSteps(t, m, d),
  } as Pattern;
}

/* ─── Per-step row editing ─────────────────────────────────────── */
//
// Per-row edits write to `draftPattern.steps` in edit mode, or commit
// inline in non-edit mode (legacy behaviour preserved when the user
// tweaks a row of an already-applied pattern).

const displaySteps = computed<Pattern['steps']>(
  () => (editing.value?.steps as Pattern['steps']) ?? [],
);

function setStepField(idx: number, field: 'rate_mbps' | 'duration_seconds' | 'enabled', value: any) {
  if (!editMode.value && pattern.value) startEdit();
  const draft = ensureDraft();
  const next = (draft.steps ?? []).slice();
  next[idx] = { ...next[idx], [field]: value };
  draftPattern.value = { ...draft, steps: next } as Pattern;
}

/* ─── Step add / remove ─────────────────────────────────────────── */

function addStep() {
  if (!editMode.value) startEdit();
  const draft = ensureDraft();
  const cur = (draft.steps ?? []).slice();
  cur.push({
    duration_seconds: defaultStepSeconds.value,
    rate_mbps: cur[cur.length - 1]?.rate_mbps ?? 5,
    enabled: true,
  });
  draftPattern.value = { ...draft, steps: cur } as Pattern;
}
function removeStep(idx: number) {
  if (!editMode.value) startEdit();
  const draft = ensureDraft();
  const cur = (draft.steps ?? []).slice();
  cur.splice(idx, 1);
  draftPattern.value = { ...draft, steps: cur } as Pattern;
}

const runtimeMbps = computed(() => player.value?.shape?.pattern_rate_runtime_mbps ?? null);
const runtimeStep = computed(() => player.value?.shape?.pattern_step_runtime ?? null);
</script>

<template>
  <div v-if="player" class="shape-pattern">
    <div class="row template-row">
      <span class="lbl">Template</span>
      <div class="radio-group">
        <label v-for="t in TEMPLATES" :key="t">
          <input
            type="radio"
            :name="`tpl-${playerId}`"
            :value="t"
            :checked="activeTemplate === t"
            @change="onTemplateChange(t)"
          />
          {{ TEMPLATE_LABELS[t] }}
        </label>
      </div>
    </div>

    <template v-if="activeTemplate !== 'sliders'">
      <!-- When a pattern is APPLIED and we're not editing, show a
           compact running-summary + Edit Pattern button. Apply Pattern
           explicitly collapses back to this state. -->
      <div v-if="!editMode && pattern" class="applied-summary">
        <span class="applied-chip">▶ Pattern running</span>
        <span class="applied-meta">
          {{ TEMPLATE_LABELS[activeTemplate] }}
          · {{ marginLabel(margin) }}
          · {{ defaultStepSeconds }}s default step
          · {{ displaySteps.length }} step{{ displaySteps.length === 1 ? '' : 's' }}
        </span>
        <span v-if="runtimeMbps !== null" class="applied-runtime">
          step <strong>{{ runtimeStep ?? '?' }}</strong> at
          <strong>{{ runtimeMbps.toFixed(2) }} Mbps</strong>
        </span>
        <div class="applied-actions">
          <button class="edit" type="button" @click="startEdit">Edit Pattern</button>
          <button class="clear" type="button" @click="commit(null)" title="Stop the pattern and return to slider mode">
            Clear
          </button>
        </div>
      </div>

      <!-- Full editor — visible while drafting (editMode) or when no
           pattern is yet applied (first run from sliders → template).
           Hidden when a pattern is applied and the user has clicked
           Apply Pattern (collapses the definition panel back). -->
      <template v-if="editMode || !pattern">
      <div class="row">
        <span class="lbl">Margin</span>
        <div class="radio-group">
          <label v-for="m in MARGIN_CHOICES" :key="m">
            <input
              type="radio"
              :name="`mgn-${playerId}`"
              :value="m"
              :checked="margin === m"
              @change="onMarginChange(m as Margin)"
            />
            {{ marginLabel(m) }}
          </label>
        </div>
      </div>

      <div class="row">
        <span class="lbl">Step duration</span>
        <div class="radio-group">
          <label v-for="d in STEP_SECONDS_CHOICES" :key="d">
            <input
              type="radio"
              :name="`stps-${playerId}`"
              :value="d"
              :checked="defaultStepSeconds === d"
              @change="onStepSecondsChange(d as StepSeconds)"
            />
            {{ d }}s
          </label>
        </div>
      </div>

      <!-- #551 — fill density: both an avg + a peak rung per variant, then
           geometric fills to this ratio. Lower = finer + longer pattern. -->
      <div class="row">
        <span class="lbl">Fill density</span>
        <div class="radio-group">
          <label v-for="s in MAX_STEP_CHOICES" :key="s">
            <input
              type="radio"
              :name="`fill-${playerId}`"
              :value="s"
              :checked="maxStep === s"
              @change="onMaxStepChange(s)"
            />
            {{ s.toFixed(2) }}×
          </label>
          <span class="fill-count" v-if="displaySteps.length">
            → {{ displaySteps.length }} rung{{ displaySteps.length === 1 ? '' : 's' }}
          </span>
        </div>
      </div>

      <div class="runtime" v-if="runtimeMbps !== null">
        Running step <strong>{{ runtimeStep ?? '?' }}</strong> at
        <strong>{{ runtimeMbps.toFixed(2) }} Mbps</strong>
      </div>

      <div class="steps">
        <div class="step-header">
          <span class="col-idx">#</span>
          <span class="col-preset">Preset</span>
          <span class="col-rate">Rate (Mbps)</span>
          <span class="col-dur">Duration (s)</span>
          <span class="col-en">On</span>
          <span class="col-rm"></span>
        </div>
        <div v-if="!displaySteps.length" class="empty">
          No steps. Pick a template or add one.
        </div>
        <div
          v-for="(s, i) in displaySteps"
          :key="i"
          class="step-row"
          :class="{ active: runtimeStep === i + 1 }"
        >
          <span class="col-idx">{{ i + 1 }}</span>
          <select
            class="col-preset"
            :value="presetForRate(s.rate_mbps ?? 0) ?? ''"
            @change="onPresetChange(i, $event)"
          >
            <option value="">Custom…</option>
            <option v-for="p in stepPresets" :key="p.value" :value="p.value">
              {{ p.label }}
            </option>
          </select>
          <input
            class="col-rate"
            type="number"
            step="0.1"
            min="0"
            :value="s.rate_mbps"
            @input="setStepField(i, 'rate_mbps', +(($event.target as HTMLInputElement).value))"
          />
          <input
            class="col-dur"
            type="number"
            step="1"
            min="1"
            :value="s.duration_seconds"
            @input="setStepField(i, 'duration_seconds', +(($event.target as HTMLInputElement).value))"
          />
          <input
            class="col-en"
            type="checkbox"
            :checked="s.enabled !== false"
            @change="setStepField(i, 'enabled', ($event.target as HTMLInputElement).checked)"
          />
          <button class="col-rm rm" type="button" @click="removeStep(i)">×</button>
        </div>
        <div class="step-actions">
          <button class="add" type="button" @click="addStep">+ Add step</button>
          <button class="clear" type="button" @click="commit(null)" title="Stop the pattern and return to slider mode">
            Clear
          </button>
          <div class="apply-flow">
            <template v-if="editMode">
              <button class="apply" type="button" @click="applyDraft">Apply Pattern</button>
              <button class="cancel" type="button" @click="cancelEdit">Cancel</button>
            </template>
            <button v-else-if="pattern" class="edit" type="button" @click="startEdit">
              Edit Pattern
            </button>
          </div>
        </div>
      </div>

      <div v-if="editMode" class="edit-banner">
        ✏️ Editing draft pattern — server is still running the previously
        applied version. Press <strong>Apply Pattern</strong> to commit.
      </div>
      </template>
    </template>
  </div>
</template>

<style scoped>
.shape-pattern {
  display: grid;
  gap: 14px;
}

.row {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  font-size: 13px;
  color: #374151;
}

.lbl {
  min-width: 120px;
  font-weight: 500;
}

.radio-group {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
}

.radio-group label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
}

.radio-group input {
  accent-color: #2563eb;
}

.runtime {
  font-size: 12px;
  color: #065f46;
  background: #d1fae5;
  padding: 6px 10px;
  border-radius: 4px;
  align-self: start;
}

.applied-summary {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  background: #ecfdf5;
  border: 1px solid #bbf7d0;
  border-radius: 8px;
  padding: 10px 14px;
  font-size: 13px;
  color: #065f46;
}
.applied-chip {
  font-weight: 700;
}
.applied-meta {
  color: #047857;
}
.applied-runtime {
  background: #d1fae5;
  padding: 2px 8px;
  border-radius: 999px;
  font-size: 12px;
}
.applied-actions {
  margin-left: auto;
  display: flex;
  gap: 6px;
}

.steps {
  border: 1px solid #e5e7eb;
  border-radius: 6px;
  padding: 8px;
}

.step-header,
.step-row {
  display: grid;
  grid-template-columns: 24px minmax(140px, 1.4fr) 1fr 1fr 40px 32px;
  gap: 8px;
  align-items: center;
  padding: 4px 0;
}

.step-row select.col-preset,
.step-header .col-preset {
  font-size: 12px;
  padding: 3px 6px;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  background: #fff;
  min-width: 0;
}

.step-header {
  font-size: 11px;
  color: #6b7280;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  border-bottom: 1px solid #e5e7eb;
  padding-bottom: 6px;
  margin-bottom: 4px;
}

.step-row.active {
  background: #ecfdf5;
  border-radius: 4px;
}

.step-row input[type='number'] {
  width: 100%;
  padding: 3px 6px;
  font-size: 13px;
  font-family: ui-monospace, monospace;
  border: 1px solid #d1d5db;
  border-radius: 4px;
}

.col-idx {
  font-family: ui-monospace, monospace;
  font-size: 12px;
  color: #6b7280;
  text-align: center;
}

.col-en input {
  accent-color: #2563eb;
}

.rm {
  background: transparent;
  color: #b91c1c;
  border: 0;
  font-size: 18px;
  line-height: 1;
  cursor: pointer;
  padding: 0;
}
.rm:hover {
  color: #7f1d1d;
}

.empty {
  font-size: 13px;
  color: #9ca3af;
  padding: 12px;
  text-align: center;
}

.step-actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}
.add {
  background: #f1f3f4;
  border: 1px solid #dadce0;
  border-radius: 4px;
  padding: 4px 12px;
  font-size: 12px;
  color: #202124;
  cursor: pointer;
}
.add:hover { background: #e8eaed; }
.clear {
  background: #fee2e2;
  color: #991b1b;
  border: 1px solid #fca5a5;
  border-radius: 4px;
  padding: 4px 12px;
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
}
.clear:hover { background: #fecaca; }

.apply-flow {
  display: flex;
  gap: 6px;
  margin-left: auto;
}
.apply {
  background: #1a73e8;
  color: white;
  border: 1px solid #1a73e8;
  border-radius: 4px;
  padding: 4px 14px;
  font-size: 12px;
  font-weight: 600;
  cursor: pointer;
}
.apply:hover { background: #1765cc; }
.cancel,
.edit {
  background: #f1f3f4;
  border: 1px solid #dadce0;
  border-radius: 4px;
  padding: 4px 12px;
  font-size: 12px;
  cursor: pointer;
}
.cancel:hover, .edit:hover { background: #e8eaed; }

.edit-banner {
  font-size: 12px;
  background: #f0f9ff;
  border: 1px solid #bae6fd;
  color: #075985;
  padding: 8px 10px;
  border-radius: 6px;
  margin-top: 12px;
}

.add {
  margin-top: 8px;
  width: 100%;
  background: #eff6ff;
  border: 1px dashed #93c5fd;
  color: #2563eb;
  padding: 6px;
  border-radius: 4px;
  font-size: 12px;
  cursor: pointer;
}
.add:hover {
  background: #dbeafe;
}
</style>
