<script setup lang="ts">
/**
 * TransferTimeouts.vue — server-side per-request transfer timeouts.
 * Two sliders (active/idle seconds) + three checkboxes (applies to
 * segments / manifests / master). Same model-driven pattern as
 * ShapeSliders.
 */
import { computed, ref, toRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import type { TransferTimeouts } from '@/repo/v2-repo';

const props = defineProps<{ playerId: string }>();
const { player, setTransferTimeouts } = usePlayer(toRef(props, 'playerId'));

const tt = computed(() => player.value?.transfer_timeouts);
const faultCounters = computed(() => player.value?.fault_counters);

// Sliders need debouncing so a single drag doesn't fire 30 PATCHes.
// Checkboxes don't need debouncing — clicks are discrete.
const DEBOUNCE_MS = 200;

const localActive = ref<number | null>(null);
const localIdle = ref<number | null>(null);
const dispActive = computed(() => localActive.value ?? tt.value?.active_timeout_seconds ?? 0);
const dispIdle = computed(() => localIdle.value ?? tt.value?.idle_timeout_seconds ?? 0);

let activeTimer: number | undefined;
let idleTimer: number | undefined;

function onActiveInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localActive.value = v;
  if (activeTimer) clearTimeout(activeTimer);
  activeTimer = window.setTimeout(
    () => setTransferTimeouts({ active_timeout_seconds: v } as Partial<TransferTimeouts>),
    DEBOUNCE_MS,
  );
}
function onIdleInput(e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localIdle.value = v;
  if (idleTimer) clearTimeout(idleTimer);
  idleTimer = window.setTimeout(
    () => setTransferTimeouts({ idle_timeout_seconds: v } as Partial<TransferTimeouts>),
    DEBOUNCE_MS,
  );
}

watch(
  () => tt.value?.active_timeout_seconds,
  (v) => {
    if (localActive.value !== null && v === localActive.value) localActive.value = null;
  },
);
watch(
  () => tt.value?.idle_timeout_seconds,
  (v) => {
    if (localIdle.value !== null && v === localIdle.value) localIdle.value = null;
  },
);

function onAppliesChange(field: 'applies_segments' | 'applies_manifests' | 'applies_master', e: Event) {
  const v = (e.target as HTMLInputElement).checked;
  setTransferTimeouts({ [field]: v } as Partial<TransferTimeouts>);
}
</script>

<template>
  <div v-if="player" class="transfer-timeouts">
    <div class="row">
      <label for="tt-active">Active</label>
      <input
        id="tt-active"
        type="range"
        min="0"
        max="30"
        step="1"
        :value="dispActive"
        @input="onActiveInput"
      />
      <span class="val">{{ dispActive }} s</span>
    </div>

    <div class="row">
      <label for="tt-idle">Idle</label>
      <input
        id="tt-idle"
        type="range"
        min="0"
        max="30"
        step="1"
        :value="dispIdle"
        @input="onIdleInput"
      />
      <span class="val">{{ dispIdle }} s</span>
    </div>

    <div class="applies">
      <span class="label">Apply To</span>
      <label>
        <input
          type="checkbox"
          :checked="tt?.applies_segments ?? false"
          @change="onAppliesChange('applies_segments', $event)"
        />
        Segments
      </label>
      <label>
        <input
          type="checkbox"
          :checked="tt?.applies_manifests ?? false"
          @change="onAppliesChange('applies_manifests', $event)"
        />
        Media manifests
      </label>
      <label>
        <input
          type="checkbox"
          :checked="tt?.applies_master ?? false"
          @change="onAppliesChange('applies_master', $event)"
        />
        Master manifest
      </label>
    </div>

    <div class="counters">
      <span class="label">Fault Counters</span>
      <div class="tiles">
        <div class="tile">
          <span class="lbl">Active</span>
          <span class="vl">{{ faultCounters?.transfer_active_timeout ?? 0 }}</span>
        </div>
        <div class="tile">
          <span class="lbl">Idle</span>
          <span class="vl">{{ faultCounters?.transfer_idle_timeout ?? 0 }}</span>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.transfer-timeouts {
  display: grid;
  gap: 12px;
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

.applies {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  align-items: center;
  padding-top: 4px;
  font-size: 13px;
  color: #374151;
}

.applies .label {
  font-weight: 500;
}

.applies label {
  display: flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
}

.applies input[type='checkbox'] {
  accent-color: #2563eb;
}

.counters {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-top: 4px;
  font-size: 13px;
}
.counters .label {
  font-weight: 500;
  color: #374151;
}
.tiles { display: flex; gap: 8px; }
.tile {
  background: #f8f9fa;
  border: 1px solid #e8eaed;
  border-radius: 6px;
  padding: 6px 12px;
  min-width: 80px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.tile .lbl {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: #5f6368;
}
.tile .vl {
  font-size: 14px;
  font-weight: 600;
  color: #202124;
  font-variant-numeric: tabular-nums;
}
</style>
