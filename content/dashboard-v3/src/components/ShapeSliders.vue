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

const props = defineProps<{ playerId: string }>();
const { player, setRate, setDelay, setLoss, isShapeWriting } = usePlayer(
  toRef(props, 'playerId'),
);

// Deployment baseline rate cap (issue #480). When the slider is at 0
// ("no operator override") the kernel still enforces this baseline,
// so the throughput display labels the 0 case with "(baseline N)" so
// operators don't think they're seeing unlimited.
const { baselineMbps } = useBaselineRate();

// Per-field "local intent" during drag. null means "no drag — read from model".
const localRate = ref<number | null>(null);
const localDelay = ref<number | null>(null);
const localLoss = ref<number | null>(null);

// When a pattern is running the throughput slider is the *runtime*
// state of the pattern, not a user-controlled rate — disable it to
// match the legacy `range-row-disabled` behaviour, otherwise dragging
// it would race with the pattern engine on every step boundary.
const patternRunning = computed(() => {
  const p = player.value?.shape?.pattern;
  return !!(p && Array.isArray(p.steps) && p.steps.length);
});

// What the slider displays. Local wins during drag; model wins
// otherwise. When a pattern is running we display the kernel-
// enforced runtime rate (`pattern_rate_runtime_mbps`) instead of the
// static `rate_mbps` so the slider tracks the pattern as it steps,
// matching the legacy behaviour the operator expects. Slider is
// disabled in that mode (see `patternRunning`) so the moving handle
// is read-only.
const dispRate = computed(() => {
  if (localRate.value !== null) return localRate.value;
  const sh = player.value?.shape;
  if (patternRunning.value) {
    const rt = sh?.pattern_rate_runtime_mbps;
    if (rt != null && Number.isFinite(rt)) return rt;
  }
  return sh?.rate_mbps ?? 0;
});
const dispDelay = computed(() => localDelay.value ?? player.value?.shape?.delay_ms ?? 0);
const dispLoss = computed(() => localLoss.value ?? player.value?.shape?.loss_pct ?? 0);

// Debounced commit. One PATCH per drag (200ms after the last input event).
const DEBOUNCE_MS = 200;
let rateTimer: number | undefined;
let delayTimer: number | undefined;
let lossTimer: number | undefined;

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
</script>

<template>
  <div v-if="player" class="shape-sliders" :class="{ writing: isShapeWriting }">
    <div class="row">
      <label for="ss-delay">Delay</label>
      <input
        id="ss-delay"
        type="range"
        min="0"
        max="250"
        step="5"
        :value="dispDelay"
        @input="onDelayInput"
      />
      <span class="val">{{ dispDelay }} ms</span>
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

    <div class="row" :class="{ disabled: patternRunning }">
      <label for="ss-rate">
        Throughput
        <span v-if="patternRunning" class="hint">(pattern active)</span>
      </label>
      <input
        id="ss-rate"
        type="range"
        min="0"
        max="50"
        step="0.1"
        :value="dispRate"
        :disabled="patternRunning"
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
