<script setup lang="ts">
/**
 * BitrateChartPanelToolbar.vue — panel-level toolbar above the
 * four metrics charts. Matches the legacy `.chart-axis-row` at the
 * top of the Bitrate Chart panel:
 *
 *   Bitrate Y Max [Auto/5/10/20/30/40/50/100 Mbps]
 *   Reset Zoom   ⏸ Pause / ▶ Live   ⤢ Expand   Alt-zoom hint
 *
 * All controls drive the shared useChartCoordination(playerId) state
 * so a single click affects every chart in the panel in lockstep.
 */
import { computed } from 'vue';
import { useChartCoordination } from '@/composables/useChartCoordination';

const props = defineProps<{ playerId: string }>();
const coord = useChartCoordination(props.playerId);

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

const pauseLabel = computed(() => (coord.state.paused ? '▶ Live' : '⏸ Pause'));
const zoomActive = computed(() => coord.state.viewport !== null);
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
        class="btn"
        type="button"
        :class="{ active: zoomActive }"
        @click="coord.resetZoom()"
        title="Snap back to live edge"
      >
        Reset Zoom
      </button>
      <button
        class="btn"
        type="button"
        :class="{ live: coord.state.paused }"
        @click="coord.togglePause()"
      >
        {{ pauseLabel }}
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
        Alt/⌥+scroll/drag · right-drag pan · click pause
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
.btn.live {
  background: #10b981;
  border-color: #059669;
  color: white;
}
.btn.live:hover { background: #059669; }
.hint {
  font-size: 10px;
  color: #9aa0a6;
  margin-left: auto;
}
</style>
