<script setup lang="ts">
/**
 * BitrateChartPanelToolbar.vue — panel-level toolbar above the
 * four metrics charts. Matches the legacy `.chart-axis-row` at the
 * top of the Bitrate Chart panel:
 *
 *   Bitrate Y Max [Auto/5/10/20/30/40/50/100 Mbps]
 *   ● Live (toggle)   ⤢ Expand   Alt-zoom hint
 *
 * All controls drive the shared useChartCoordination(playerId) state
 * so a single click affects every chart in the panel in lockstep.
 */
import { computed, toRef } from 'vue';
import { useChartCoordination } from '@/composables/useChartCoordination';

const props = defineProps<{ playerId: string }>();
const coord = useChartCoordination(toRef(props, 'playerId'));

type YMaxMode = 'auto' | '5' | '10' | '20' | '30' | '40' | '50' | '100';
const modes: YMaxMode[] = ['auto', '5', '10', '20', '30', '40', '50', '100'];

const currentMode = computed<YMaxMode>(() => {
  const v = coord.state.bandwidthYMax;
  if (v == null) return 'auto';
  const s = String(v) as YMaxMode;
  return modes.includes(s) ? s : 'auto';
});

function setMode(m: YMaxMode) {
  coord.setBandwidthYMax(m === 'auto' ? undefined : Number(m));
}

// Live toggle is "checked" when we're currently following live —
// i.e. no sticky viewport. All four charts in the panel share this
// coord state so the toolbar's toggle and each chart's toggle stay
// in lockstep regardless of which one the operator clicked.
const liveChecked = computed(() => coord.state.viewport === null);

/** Always togglePause — both directions preserve liveSpanMs.
 *  See MetricsLineChart.onLiveToggleClick for rationale. */
function onLiveToggleClick() {
  coord.togglePause();
}
</script>

<template>
  <div class="toolbar">
    <div class="ymax">
      <span class="ymax-label">Bitrate Y Max</span>
      <label v-for="m in modes" :key="m" class="pill" :class="{ active: currentMode === m }">
        <input
          type="radio"
          name="panel-bw-ymax"
          :value="m"
          :checked="currentMode === m"
          @change="setMode(m)"
        />
        <span>{{ m === 'auto' ? 'Auto' : `${m} Mbps` }}</span>
      </label>
    </div>

    <div class="actions">
      <button
        type="button"
        class="btn live-toggle"
        :class="{ checked: liveChecked }"
        @click="onLiveToggleClick"
        :title="liveChecked ? 'Pause at current live edge' : 'Resume following live (drops zoom and pan)'"
      >
        {{ liveChecked ? '●' : '○' }} Live
      </button>
      <button
        class="btn"
        type="button"
        :class="{ active: coord.state.expanded }"
        @click="coord.toggleExpanded()"
        title="Toggle expanded chart height"
      >
        ⤢
      </button>
      <span class="hint" title="Hold Alt (Option on Mac) while scrolling or dragging to zoom; right-click-drag to pan">
        Alt/⌥+scroll/drag · right-drag pan
      </span>
    </div>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 6px 0 10px;
  border-bottom: 1px solid #f1f3f4;
  margin-bottom: 12px;
}

.ymax {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 4px;
}
.ymax-label {
  font-size: 11px;
  font-weight: 600;
  color: #5f6368;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  margin-right: 4px;
}
.pill {
  display: inline-flex;
  align-items: center;
  font-size: 11px;
  padding: 2px 8px;
  border: 1px solid #dadce0;
  border-radius: 999px;
  background: #f9fafb;
  cursor: pointer;
  user-select: none;
  color: #374151;
}
.pill input { display: none; }
.pill:hover { background: #e5e7eb; }
.pill.active {
  background: #1a73e8;
  border-color: #1a73e8;
  color: white;
}

.actions {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.btn {
  background: #f3f4f6;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  padding: 4px 10px;
  font-size: 11px;
  font-weight: 500;
  color: #374151;
  cursor: pointer;
}
.btn:hover { background: #e5e7eb; }
.btn.active {
  background: #e0e7ff;
  border-color: #818cf8;
  color: #312e81;
}
/* Live toggle: filled green when checked, muted/outlined when
 * unchecked. Same scheme as MetricsLineChart / EventsTimeline so
 * all the toggles in the panel match visually. */
.btn.live-toggle.checked {
  background: #10b981;
  border-color: #059669;
  color: white;
  font-weight: 600;
}
.btn.live-toggle.checked:hover { background: #059669; }
.btn.live-toggle:not(.checked) {
  background: #f3f4f6;
  border-color: #d1d5db;
  color: #6b7280;
}
.btn.live-toggle:not(.checked):hover { background: #e5e7eb; color: #374151; }
.hint {
  font-size: 10px;
  color: #9aa0a6;
  margin-left: auto;
}
</style>
