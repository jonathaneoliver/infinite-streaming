<script setup lang="ts">
/**
 * PlayerActionsBar.vue — toolbar above the embedded video player.
 * Mirrors the legacy `.player-actions` row in testing-session.html:
 *   Retry Fetch · Restart Playback · Reload Page · Engine select ·
 *   Allow 4K · Auto-Recovery · PiP · Rotate play_id radios.
 *
 * The toggles only persist locally (localStorage) and emit events the
 * parent VideoPlayerFrame consumes — the v3 dashboard doesn't yet
 * implement every soak-style rotation, but the controls are in place
 * so the muscle memory carries over from the legacy page.
 */
import { ref, watch } from 'vue';

type Engine = 'auto' | 'hlsjs' | 'shaka' | 'videojs' | 'native';

const props = defineProps<{
  engine: Engine;
  prefer4k: boolean;
  autoRecovery: boolean;
  pip: boolean;
  rotationSeconds: number;
}>();

const emit = defineEmits<{
  (e: 'update:engine', v: Engine): void;
  (e: 'update:prefer4k', v: boolean): void;
  (e: 'update:autoRecovery', v: boolean): void;
  (e: 'update:pip', v: boolean): void;
  (e: 'update:rotationSeconds', v: number): void;
  (e: 'retry'): void;
  (e: 'restart'): void;
  (e: 'reload'): void;
}>();

const rotations = [
  { v: 0, label: 'Off' },
  { v: 300, label: '5m' },
  { v: 1800, label: '30m' },
  { v: 3600, label: '1h' },
  { v: 21600, label: '6h' },
];

// Mirror local state so we can drive change events without losing the
// two-way binding contract.
const engineLocal = ref<Engine>(props.engine);
watch(() => props.engine, (v) => { engineLocal.value = v; });

function onEngineChange(e: Event) {
  const v = (e.target as HTMLSelectElement).value as Engine;
  engineLocal.value = v;
  emit('update:engine', v);
}
</script>

<template>
  <div class="player-actions">
    <button class="btn" type="button" @click="emit('retry')">Retry Fetch</button>
    <button class="btn" type="button" @click="emit('restart')">Restart Playback</button>
    <button class="btn" type="button" @click="emit('reload')">Reload Page</button>

    <label class="field">
      Player
      <select :value="engineLocal" @change="onEngineChange">
        <option value="auto">Auto</option>
        <option value="hlsjs">HLS.js</option>
        <option value="shaka">Shaka</option>
        <option value="videojs">Video.js</option>
        <option value="native">Native</option>
      </select>
    </label>

    <label class="field check">
      <input
        type="checkbox"
        :checked="prefer4k"
        @change="emit('update:prefer4k', ($event.target as HTMLInputElement).checked)"
      />
      Allow 4K
    </label>

    <label class="field check">
      <input
        type="checkbox"
        :checked="autoRecovery"
        @change="emit('update:autoRecovery', ($event.target as HTMLInputElement).checked)"
      />
      Auto-Recovery
    </label>

    <label class="field check" title="Pop video into a floating Picture-in-Picture window">
      <input
        type="checkbox"
        :checked="pip"
        @change="emit('update:pip', ($event.target as HTMLInputElement).checked)"
      />
      PiP
    </label>

    <span class="rotation-group" title="Auto-rotate play_id for long soak runs.">
      Rotate play_id
      <label v-for="r in rotations" :key="r.v" class="rot">
        <input
          type="radio"
          name="playIdRotation"
          :value="r.v"
          :checked="rotationSeconds === r.v"
          @change="emit('update:rotationSeconds', r.v)"
        />
        {{ r.label }}
      </label>
    </span>
  </div>
</template>

<style scoped>
.player-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  align-items: center;
  gap: 10px;
  font-size: 12px;
  color: #374151;
}
.btn {
  background: #f1f3f4;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 6px 12px;
  font-size: 12px;
  font-weight: 500;
  color: #202124;
  cursor: pointer;
}
.btn:hover { background: #e8eaed; }

.field {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.field.check { user-select: none; cursor: pointer; }
.field select {
  background: #fff;
  border: 1px solid #dadce0;
  border-radius: 6px;
  padding: 5px 8px;
  font-size: 12px;
  color: #202124;
}

.rotation-group {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.rot {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  font-size: 11px;
  cursor: pointer;
}
.rot input { margin: 0; }
</style>
