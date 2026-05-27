<script setup lang="ts">
/**
 * DebugLog.vue — append-only console under the player. Mirrors the
 * legacy `.debug-log` block. Public API is a single `push(message)`
 * function exposed via defineExpose so the parent (VideoPlayerFrame)
 * can call it on engine events, SSE drops, fault hits, etc.
 *
 * Line cap (200) prevents the DOM from growing unbounded during long
 * soak runs.
 */
import { ref, nextTick, useTemplateRef } from 'vue';

const LIMIT = 200;
const lines = ref<{ t: string; msg: string; level: 'info' | 'warn' | 'error' }[]>([]);
const logEl = useTemplateRef<HTMLDivElement>('logEl');

function ts() {
  const d = new Date();
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}:${String(d.getSeconds()).padStart(2, '0')}.${String(d.getMilliseconds()).padStart(3, '0')}`;
}

async function push(msg: string, level: 'info' | 'warn' | 'error' = 'info') {
  lines.value.push({ t: ts(), msg, level });
  if (lines.value.length > LIMIT) lines.value.splice(0, lines.value.length - LIMIT);
  await nextTick();
  if (logEl.value) logEl.value.scrollTop = logEl.value.scrollHeight;
}

function clear() {
  lines.value = [];
}

defineExpose({ push, clear });
</script>

<template>
  <div class="debug-log" ref="logEl">
    <div
      v-for="(l, i) in lines"
      :key="i"
      class="line"
      :class="l.level"
    >
      <span class="t">{{ l.t }}</span>
      <span class="msg">{{ l.msg }}</span>
    </div>
    <div v-if="!lines.length" class="placeholder">Debug events will appear here.</div>
  </div>
</template>

<style scoped>
.debug-log {
  background: #0f172a;
  color: #cbd5e1;
  border-radius: 6px;
  padding: 8px 12px;
  font-family: ui-monospace, 'SF Mono', Menlo, monospace;
  font-size: 11px;
  max-height: 200px;
  overflow-y: auto;
  margin-top: 12px;
  line-height: 1.5;
}
.line {
  display: flex;
  gap: 8px;
}
.line .t { color: #64748b; flex-shrink: 0; }
.line .msg { color: inherit; }
.line.warn .msg { color: #fcd34d; }
.line.error .msg { color: #fca5a5; }
.placeholder {
  color: #64748b;
  font-style: italic;
}
</style>
