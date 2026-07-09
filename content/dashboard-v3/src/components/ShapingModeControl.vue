<script setup lang="ts">
/**
 * ShapingModeControl.vue — #910 per-session shaping-mode tristate.
 *
 * Sits directly under the "Session Controls" heading because it governs MORE
 * than the Network Shaping fold: Faults-only also disables the Transport
 * fault tab (needs nftables) and the Pattern engine (kernel rate-stepping),
 * while leaving the portable HTTP-fault surface (errors / hung / corrupted /
 * content / timeouts) fully live.
 *
 * Visibility (see useSessionShaping): hidden on a kernel-capable host in normal
 * use — kernel is just used. Shown in developer mode (the A/B instrument) or
 * when the host genuinely can't kernel-shape (fallback picker, Kernel disabled).
 */
import { toRef } from 'vue';
import { useSessionShaping } from '@/composables/useSessionShaping';

const props = defineProps<{ playerId: string }>();
const {
  showControl,
  kernelSelectable,
  developerMode,
  activeId,
  degraded,
  effMode,
  settingMode,
  modeError,
  setShapingMode,
  SHAPING_MODES,
} = useSessionShaping(toRef(props, 'playerId'));
</script>

<template>
  <div v-if="showControl" class="shaping-mode-block">
    <span class="shaping-mode-title">
      Session shaping
      <span v-if="degraded" class="shaping-mode-flag">degraded: {{ effMode }}</span>
    </span>
    <div class="shaping-mode-seg" role="group" aria-label="Session shaping mode">
      <button
        v-for="m in SHAPING_MODES"
        :key="m.id"
        type="button"
        class="shaping-mode-btn"
        :class="{ active: activeId === m.id }"
        :disabled="settingMode || (m.id === '' && !kernelSelectable)"
        :title="m.id === '' && !kernelSelectable ? 'This host has no NET_ADMIN — kernel shaping unavailable' : ''"
        @click="setShapingMode(m.id)"
      >
        {{ m.label }}
      </button>
    </div>
    <span class="shaping-mode-hint">
      <template v-if="!kernelSelectable">
        This host can't do kernel shaping (no NET_ADMIN) — pick how this session
        degrades. Rate/delay/loss and transport faults won't take effect; HTTP
        faults still do.
      </template>
      <template v-else-if="developerMode">
        Developer A/B: force THIS session into Faults-only to compare against the
        kernel path. Kernel shaping (rate/delay/loss) and transport faults are
        gated in Faults-only; HTTP faults still fire.
      </template>
    </span>
    <span v-if="modeError" class="shaping-mode-error">⚠️ {{ modeError }}</span>
  </div>
</template>

<style scoped>
/* #910 per-session shaping-mode tristate — sits under the Session Controls
   heading, above the fault/shaping folds it governs. */
.shaping-mode-block {
  display: grid;
  gap: 6px;
  margin: 0 0 14px;
  padding: 10px 12px;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  background: #fafafa;
}
.shaping-mode-title {
  font-size: 13px;
  font-weight: 600;
  color: #374151;
  display: flex;
  align-items: center;
  gap: 8px;
}
.shaping-mode-flag {
  padding: 1px 6px;
  border-radius: 4px;
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.02em;
  color: #991b1b;
  background: #fee2e2;
  border: 1px solid #fca5a5;
}
.shaping-mode-seg {
  display: inline-flex;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  overflow: hidden;
  width: fit-content;
}
.shaping-mode-btn {
  padding: 5px 14px;
  font-size: 12px;
  font-weight: 500;
  color: #374151;
  background: #fff;
  border: none;
  border-right: 1px solid #d1d5db;
  cursor: pointer;
}
.shaping-mode-btn:last-child {
  border-right: none;
}
.shaping-mode-btn:hover:not(:disabled) {
  background: #f9fafb;
}
.shaping-mode-btn.active {
  background: #2563eb;
  color: #fff;
}
.shaping-mode-btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
.shaping-mode-hint {
  font-size: 11px;
  color: #6b7280;
}
.shaping-mode-hint:empty {
  display: none;
}
.shaping-mode-error {
  font-size: 11px;
  color: #991b1b;
}
</style>
