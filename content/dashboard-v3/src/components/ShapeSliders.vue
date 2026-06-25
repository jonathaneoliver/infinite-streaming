<script setup lang="ts">
/**
 * ShapeSliders.vue — rate / delay / loss sliders.
 *
 * Pattern: local UI state during drag, debounced commit to the model.
 * Without debounce, every pixel of a drag fires a PATCH; the first one
 * advances the server's revision and every subsequent in-flight PATCH
 * 412s on its (now stale) If-Match header. The optimistic rollback
 * chain then bounces the slider back to the pre-drag value.
 *
 * With debounce + local state:
 *   - Slider :value reads from `localRate` (or model if not dragging).
 *   - @input updates `localRate` instantly (no network).
 *   - 200ms after the last input, setRate(localRate) commits via the
 *     useMutation in usePlayer. Exactly one PATCH per drag.
 *   - When the PATCH lands and updates the cache, the watcher clears
 *     `localRate` so the slider goes back to reading from the model.
 *
 * Cross-tab control updates flow into the model as before; if no local
 * drag is in flight, the slider snaps to the new model value.
 */
import { computed, ref, toRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import { useBaselineRate } from '@/composables/useBaselineRate';
import { LINK_PROFILES, LINK_PROFILES_BY_ID } from '@/lib/linkProfiles';

const props = defineProps<{ playerId: string }>();
const {
  player,
  setRate,
  setDelay,
  setLoss,
  setJitter,
  setLossCorrelation,
  setJitterCorrelation,
  applyProfile,
  isShapeWriting,
} = usePlayer(toRef(props, 'playerId'));

// Named link profiles (#826). Selecting one applies its whole impairment block
// in a single PATCH; pure-overlay recipes leave the throughput cap untouched.
// The grouped lists mirror the recipe/nlc split in linkProfiles.ts.
const recipeProfiles = computed(() => LINK_PROFILES.filter((p) => p.group === 'recipe'));
const nlcProfiles = computed(() => LINK_PROFILES.filter((p) => p.group === 'nlc'));

// Which radio is checked: the profile whose values the live shape currently
// matches. Dragging an individual slider off a profile's values de-selects the
// radio (no profile matches), which is the honest state — the shape is now
// custom. A recipe (no rate cap) ignores rate_mbps when matching.
const activeProfileId = computed(() => {
  const sh = player.value?.shape;
  if (!sh) return '';
  const eq = (a: number | undefined, b: number | undefined) => (a ?? 0) === (b ?? 0);
  const match = LINK_PROFILES.find((p) => {
    const s = p.shape;
    const axesMatch =
      eq(sh.delay_ms, s.delay_ms) &&
      eq(sh.loss_pct, s.loss_pct) &&
      eq(sh.jitter_ms, s.jitter_ms) &&
      eq(sh.loss_correlation_pct, s.loss_correlation_pct) &&
      eq(sh.jitter_correlation_pct, s.jitter_correlation_pct);
    // NLC presets pin a rate too; recipes are pure overlays (ignore rate).
    const rateMatch = s.rate_mbps == null || eq(sh.rate_mbps, s.rate_mbps);
    return axesMatch && rateMatch;
  });
  return match?.id ?? '';
});

function selectProfile(id: string) {
  const entry = LINK_PROFILES_BY_ID[id];
  if (!entry) return;
  const s = entry.shape;
  // Deterministic IMPAIRMENT: always set all five impairment axes (the ones a
  // profile omits → 0) so no stale jitter/loss leaks when switching profiles.
  // Throughput is the OVERLAY axis, deliberately handled differently: only the
  // NLC presets (which model a full link's bandwidth) set rate_mbps; the four
  // recipes leave the operator's throughput cap untouched so an impairment
  // recipe can be stamped on top of an existing bandwidth test.
  applyProfile({
    delay_ms: s.delay_ms ?? 0,
    loss_pct: s.loss_pct ?? 0,
    jitter_ms: s.jitter_ms ?? 0,
    loss_correlation_pct: s.loss_correlation_pct ?? 0,
    jitter_correlation_pct: s.jitter_correlation_pct ?? 0,
    ...(s.rate_mbps != null ? { rate_mbps: s.rate_mbps } : {}),
  });
}

// Deployment baseline rate cap (issue #480). When the slider is at 0
// ("no operator override") the kernel still enforces this baseline,
// so the throughput display labels the 0 case with "(baseline N)" so
// operators don't think they're seeing unlimited.
const { baselineMbps } = useBaselineRate();

// Per-field "local intent" during drag. null means "no drag — read from model".
const localRate = ref<number | null>(null);
const localDelay = ref<number | null>(null);
const localLoss = ref<number | null>(null);
const localJitter = ref<number | null>(null);
const localLossCorr = ref<number | null>(null);
const localJitterCorr = ref<number | null>(null);

// When a pattern is running the throughput slider is the *runtime*
// state of the pattern, not a user-controlled rate — disable it to
// match the legacy `range-row-disabled` behaviour, otherwise dragging
// it would race with the pattern engine on every step boundary.
const patternRunning = computed(() => {
  const p = player.value?.shape?.pattern;
  return !!(p && Array.isArray(p.steps) && p.steps.length);
});

// Single-owner group shaping: a driven SLAVE has no pattern of its own but its
// kernel cap is fanned per-tick from the group master (`group_driven_by`). Show
// the same disabled-slider + runtime-rate treatment as a local pattern, with a
// "driven by master" annotation. Guarded on `!patternRunning` so the MASTER
// (which carries the pattern) never reads as driven — the proxy stamps the
// marker on the master too, but its own pattern takes precedence here.
const groupDrivenBy = computed<string | null>(() => {
  const by = player.value?.shape?.group_driven_by;
  return !patternRunning.value && by ? by : null;
});

// What the slider displays. Local wins during drag; model wins
// otherwise. When a pattern is running — OR this is a group-driven slave — we
// display the kernel-enforced runtime rate (`pattern_rate_runtime_mbps`) instead
// of the static `rate_mbps` so the slider tracks the pattern as it steps. Slider
// is disabled in those modes (see template) so the moving handle is read-only.
const dispRate = computed(() => {
  if (localRate.value !== null) return localRate.value;
  const sh = player.value?.shape;
  if (patternRunning.value || groupDrivenBy.value) {
    const rt = sh?.pattern_rate_runtime_mbps;
    if (rt != null && Number.isFinite(rt)) return rt;
  }
  return sh?.rate_mbps ?? 0;
});
const dispDelay = computed(() => localDelay.value ?? player.value?.shape?.delay_ms ?? 0);
const dispLoss = computed(() => localLoss.value ?? player.value?.shape?.loss_pct ?? 0);
const dispJitter = computed(() => localJitter.value ?? player.value?.shape?.jitter_ms ?? 0);
const dispLossCorr = computed(
  () => localLossCorr.value ?? player.value?.shape?.loss_correlation_pct ?? 0,
);
const dispJitterCorr = computed(
  () => localJitterCorr.value ?? player.value?.shape?.jitter_correlation_pct ?? 0,
);

// Debounced commit. One PATCH per drag (200ms after the last input event).
const DEBOUNCE_MS = 200;
let rateTimer: number | undefined;
let delayTimer: number | undefined;
let lossTimer: number | undefined;
let jitterTimer: number | undefined;
let lossCorrTimer: number | undefined;
let jitterCorrTimer: number | undefined;

function onRateInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localRate.value = v;
  if (rateTimer) clearTimeout(rateTimer);
  rateTimer = window.setTimeout(() => setRate(v), DEBOUNCE_MS);
}
function onDelayInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localDelay.value = v;
  if (delayTimer) clearTimeout(delayTimer);
  delayTimer = window.setTimeout(() => setDelay(v), DEBOUNCE_MS);
}
function onLossInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localLoss.value = v;
  if (lossTimer) clearTimeout(lossTimer);
  lossTimer = window.setTimeout(() => setLoss(v), DEBOUNCE_MS);
}
function onJitterInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localJitter.value = v;
  if (jitterTimer) clearTimeout(jitterTimer);
  jitterTimer = window.setTimeout(() => setJitter(v), DEBOUNCE_MS);
}
function onLossCorrInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localLossCorr.value = v;
  if (lossCorrTimer) clearTimeout(lossCorrTimer);
  lossCorrTimer = window.setTimeout(() => setLossCorrelation(v), DEBOUNCE_MS);
}
function onJitterCorrInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localJitterCorr.value = v;
  if (jitterCorrTimer) clearTimeout(jitterCorrTimer);
  jitterCorrTimer = window.setTimeout(() => setJitterCorrelation(v), DEBOUNCE_MS);
}

// Once the PATCH lands and the model's value matches our local intent
// (or differs because of a competing edit), release back to model.
watch(
  () => player.value?.shape?.rate_mbps,
  (v) => {
    if (localRate.value !== null && v === localRate.value) localRate.value = null;
  },
);
watch(
  () => player.value?.shape?.delay_ms,
  (v) => {
    if (localDelay.value !== null && v === localDelay.value) localDelay.value = null;
  },
);
watch(
  () => player.value?.shape?.loss_pct,
  (v) => {
    if (localLoss.value !== null && v === localLoss.value) localLoss.value = null;
  },
);
watch(
  () => player.value?.shape?.jitter_ms,
  (v) => {
    if (localJitter.value !== null && v === localJitter.value) localJitter.value = null;
  },
);
watch(
  () => player.value?.shape?.loss_correlation_pct,
  (v) => {
    if (localLossCorr.value !== null && v === localLossCorr.value) localLossCorr.value = null;
  },
);
watch(
  () => player.value?.shape?.jitter_correlation_pct,
  (v) => {
    if (localJitterCorr.value !== null && v === localJitterCorr.value) localJitterCorr.value = null;
  },
);
</script>

<template>
  <div v-if="player" class="shape-sliders" :class="{ writing: isShapeWriting }">
    <!-- Named link profiles (#826): one-click apply of a realistic
         latency/loss/jitter recipe. The checked radio reflects the live
         shape — dragging a slider off a profile de-selects it. -->
    <div class="profile-block">
      <span class="profile-title">Link profile</span>
      <div class="profile-group">
        <span class="profile-group-label">Recipes</span>
        <div class="profile-radios">
          <label v-for="p in recipeProfiles" :key="p.id" class="profile-radio">
            <input
              type="radio"
              name="ss-link-profile"
              :value="p.id"
              :checked="activeProfileId === p.id"
              @change="selectProfile(p.id)"
            />
            <span>{{ p.label }}</span>
          </label>
        </div>
      </div>
      <div class="profile-group">
        <span class="profile-group-label">Apple Network Link Conditioner</span>
        <div class="profile-radios">
          <label v-for="p in nlcProfiles" :key="p.id" class="profile-radio">
            <input
              type="radio"
              name="ss-link-profile"
              :value="p.id"
              :checked="activeProfileId === p.id"
              @change="selectProfile(p.id)"
            />
            <span>{{ p.label }}</span>
          </label>
        </div>
      </div>
    </div>

    <div class="row">
      <label for="ss-delay">Delay</label>
      <input
        id="ss-delay"
        type="range"
        min="0"
        max="500"
        step="5"
        :value="dispDelay"
        @input="onDelayInput"
      />
      <span class="val">{{ dispDelay }} ms</span>
    </div>

    <div class="row">
      <label for="ss-jitter">Jitter</label>
      <input
        id="ss-jitter"
        type="range"
        min="0"
        max="100"
        step="1"
        :value="dispJitter"
        @input="onJitterInput"
      />
      <span class="val">{{ dispJitter }} ms</span>
    </div>

    <div class="row">
      <label for="ss-jitter-corr">
        Jitter corr.
        <span class="hint">delay distribution</span>
      </label>
      <input
        id="ss-jitter-corr"
        type="range"
        min="0"
        max="100"
        step="5"
        :value="dispJitterCorr"
        @input="onJitterCorrInput"
      />
      <span class="val">{{ dispJitterCorr }} %</span>
    </div>

    <div class="row">
      <label for="ss-loss">Packet Loss</label>
      <input
        id="ss-loss"
        type="range"
        min="0"
        max="10"
        step="0.5"
        :value="dispLoss"
        @input="onLossInput"
      />
      <span class="val">{{ dispLoss.toFixed(1) }} %</span>
    </div>

    <div class="row">
      <label for="ss-loss-corr">
        Loss corr.
        <span class="hint">burstiness</span>
      </label>
      <input
        id="ss-loss-corr"
        type="range"
        min="0"
        max="100"
        step="5"
        :value="dispLossCorr"
        @input="onLossCorrInput"
      />
      <span class="val">{{ dispLossCorr }} %</span>
    </div>

    <div class="row" :class="{ disabled: patternRunning || !!groupDrivenBy }">
      <label for="ss-rate">
        Throughput
        <span v-if="patternRunning" class="hint">(pattern active)</span>
        <span v-else-if="groupDrivenBy" class="hint">⛓ driven by group master {{ groupDrivenBy }}</span>
      </label>
      <input
        id="ss-rate"
        type="range"
        min="0"
        max="100"
        step="0.1"
        :value="dispRate"
        :disabled="patternRunning || !!groupDrivenBy"
        @input="onRateInput"
      />
      <span class="val">
        {{ dispRate.toFixed(1) }} Mbps
        <!-- When the slider is at 0 ("no operator override") and the
             deployment has a baseline (#480), make it explicit that the
             kernel will still cap at the baseline. Operator otherwise
             reads "0 Mbps" and wonders why throughput isn't unlimited. -->
        <span v-if="dispRate === 0 && baselineMbps > 0" class="baseline-hint">
          (baseline {{ baselineMbps }})
        </span>
      </span>
    </div>
  </div>
</template>

<style scoped>
.shape-sliders {
  display: grid;
  gap: 12px;
}

.shape-sliders.writing {
  opacity: 0.96;
}

.row {
  display: grid;
  grid-template-columns: 120px 1fr 110px;
  gap: 12px;
  align-items: center;
}

.row label {
  font-size: 13px;
  font-weight: 500;
  color: #374151;
}

.row input[type='range'] {
  width: 100%;
  accent-color: #2563eb;
}

.val {
  font-family: ui-monospace, monospace;
  font-size: 13px;
  color: #111827;
  text-align: right;
}

/* #826 profile radios. */
.profile-block {
  display: grid;
  gap: 8px;
  padding-bottom: 4px;
  border-bottom: 1px solid #f3f4f6;
}

.profile-title {
  font-size: 13px;
  font-weight: 600;
  color: #374151;
}

.profile-group {
  display: grid;
  gap: 4px;
}

.profile-group-label {
  font-size: 11px;
  font-weight: 500;
  color: #9ca3af;
  text-transform: uppercase;
  letter-spacing: 0.03em;
}

.profile-radios {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
}

.profile-radio {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 13px;
  color: #374151;
  cursor: pointer;
}

.profile-radio input[type='radio'] {
  accent-color: #2563eb;
  cursor: pointer;
}

.row.disabled label,
.row.disabled .val {
  color: #9ca3af;
}
.row.disabled input[type='range'] {
  opacity: 0.45;
}

.hint {
  margin-left: 6px;
  font-size: 11px;
  color: #f59e0b;
  font-weight: normal;
  font-style: italic;
}

/* Issue #480 — surfaces the deployment baseline when the operator is
 * NOT overriding. Same amber as the .hint above so it reads as "this
 * is meta-state, not the value you set" without making the slider
 * row visually noisy. */
.baseline-hint {
  margin-left: 4px;
  font-size: 11px;
  color: #92400e;
  font-weight: normal;
}
</style>
