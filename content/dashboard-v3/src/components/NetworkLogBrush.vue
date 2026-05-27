<script setup lang="ts">
/**
 * NetworkLogBrush.vue — overview rail + draggable brush window above
 * the network log table. Matches the legacy `.network-log-waterfall-
 * overview` widget:
 *
 *   - One tick per request, coloured by status (2xx/3xx/4xx/5xx/faulted)
 *   - A brush rectangle spanning the visible time window
 *   - Left + right edge handles to resize the window
 *   - Click + drag on the rail to redraw the window from scratch
 *   - Double-click to clear the brush (return to "show everything")
 *
 * Emits `update:brush` whenever the brush changes — parent applies the
 * filter to the table rows.
 */
import { computed, ref, watch } from 'vue';

export interface BrushTick {
  ts: number;
  status: number;
  faulted: boolean;
}

const props = defineProps<{
  ticks: BrushTick[];
  brushStartMs: number | null;
  brushEndMs: number | null;
}>();
const emit = defineEmits<{
  (e: 'update:brush', v: { startMs: number; endMs: number } | null): void;
}>();

const railEl = ref<HTMLDivElement | null>(null);

const range = computed(() => {
  if (!props.ticks.length) return null;
  let minT = Infinity;
  let maxT = -Infinity;
  for (const t of props.ticks) {
    if (t.ts < minT) minT = t.ts;
    if (t.ts > maxT) maxT = t.ts;
  }
  if (!Number.isFinite(minT) || !Number.isFinite(maxT)) return null;
  // Pad the right edge slightly so the latest request isn't pinned
  // to the rail's right pixel.
  const pad = Math.max(1_000, (maxT - minT) * 0.02);
  return { start: minT, end: maxT + pad, span: Math.max(1, (maxT - minT) + pad) };
});

function tickClass(t: BrushTick): string {
  if (t.faulted) return 'tk fault';
  if (t.status >= 500) return 'tk s5';
  if (t.status >= 400) return 'tk s4';
  if (t.status >= 300) return 'tk s3';
  if (t.status >= 200) return 'tk s2';
  return 'tk';
}

function tickLeft(t: BrushTick): string {
  const r = range.value;
  if (!r) return '0%';
  return `${((t.ts - r.start) / r.span) * 100}%`;
}

const brushStyle = computed(() => {
  const r = range.value;
  if (!r || props.brushStartMs == null || props.brushEndMs == null) return null;
  const left = ((props.brushStartMs - r.start) / r.span) * 100;
  const width = ((props.brushEndMs - props.brushStartMs) / r.span) * 100;
  return {
    left: `${Math.max(0, left).toFixed(3)}%`,
    width: `${Math.max(0.2, width).toFixed(3)}%`,
  };
});

interface DragState {
  mode: 'create' | 'move' | 'left' | 'right';
  startClientX: number;
  rect: DOMRect;
  origStart: number;
  origEnd: number;
}
let drag: DragState | null = null;

function railTimeFromClientX(clientX: number, rect: DOMRect): number {
  const r = range.value;
  if (!r) return 0;
  const w = Math.max(1, rect.width);
  const pct = Math.min(1, Math.max(0, (clientX - rect.left) / w));
  return r.start + pct * r.span;
}

function clampPair(a: number, b: number): { start: number; end: number } {
  const r = range.value!;
  let start = Math.max(r.start, Math.min(a, b));
  let end = Math.min(r.end, Math.max(a, b));
  // Minimum brush width: 500ms.
  if (end - start < 500) end = Math.min(r.end, start + 500);
  return { start, end };
}

function onRailMouseDown(e: MouseEvent) {
  if (!railEl.value || !range.value) return;
  if (e.button !== 0) return;
  const rect = railEl.value.getBoundingClientRect();
  const t = railTimeFromClientX(e.clientX, rect);
  drag = {
    mode: 'create',
    startClientX: e.clientX,
    rect,
    origStart: t,
    origEnd: t,
  };
  e.preventDefault();
  window.addEventListener('mousemove', onWindowMouseMove);
  window.addEventListener('mouseup', onWindowMouseUp);
}

function onHandleDown(side: 'left' | 'right', e: MouseEvent) {
  if (!railEl.value || !range.value) return;
  if (e.button !== 0) return;
  e.stopPropagation();
  e.preventDefault();
  const rect = railEl.value.getBoundingClientRect();
  drag = {
    mode: side,
    startClientX: e.clientX,
    rect,
    origStart: props.brushStartMs ?? range.value.start,
    origEnd: props.brushEndMs ?? range.value.end,
  };
  window.addEventListener('mousemove', onWindowMouseMove);
  window.addEventListener('mouseup', onWindowMouseUp);
}

function onBrushMouseDown(e: MouseEvent) {
  if (!railEl.value || !range.value) return;
  if (e.button !== 0) return;
  if ((e.target as HTMLElement).classList.contains('handle')) return;
  e.stopPropagation();
  e.preventDefault();
  const rect = railEl.value.getBoundingClientRect();
  drag = {
    mode: 'move',
    startClientX: e.clientX,
    rect,
    origStart: props.brushStartMs ?? range.value.start,
    origEnd: props.brushEndMs ?? range.value.end,
  };
  window.addEventListener('mousemove', onWindowMouseMove);
  window.addEventListener('mouseup', onWindowMouseUp);
}

function onWindowMouseMove(e: MouseEvent) {
  if (!drag || !range.value) return;
  const t = railTimeFromClientX(e.clientX, drag.rect);
  if (drag.mode === 'create') {
    drag.origEnd = t;
    const { start, end } = clampPair(drag.origStart, t);
    emit('update:brush', { startMs: start, endMs: end });
  } else if (drag.mode === 'left') {
    const newStart = Math.min(t, drag.origEnd - 500);
    const { start, end } = clampPair(newStart, drag.origEnd);
    emit('update:brush', { startMs: start, endMs: end });
  } else if (drag.mode === 'right') {
    const newEnd = Math.max(t, drag.origStart + 500);
    const { start, end } = clampPair(drag.origStart, newEnd);
    emit('update:brush', { startMs: start, endMs: end });
  } else if (drag.mode === 'move') {
    const span = drag.origEnd - drag.origStart;
    const dx = (e.clientX - drag.startClientX) / Math.max(1, drag.rect.width) * range.value.span;
    let s = drag.origStart + dx;
    let en = drag.origEnd + dx;
    if (s < range.value.start) { s = range.value.start; en = s + span; }
    if (en > range.value.end) { en = range.value.end; s = en - span; }
    emit('update:brush', { startMs: s, endMs: en });
  }
}

function onWindowMouseUp() {
  drag = null;
  window.removeEventListener('mousemove', onWindowMouseMove);
  window.removeEventListener('mouseup', onWindowMouseUp);
}

function clearBrush() {
  emit('update:brush', null);
}

// When the rail's data range changes (new requests appear), if a brush
// was pinned to the right edge, slide it along. Otherwise leave alone.
watch(range, (next, prev) => {
  if (!next || !prev) return;
  if (props.brushStartMs == null || props.brushEndMs == null) return;
  const atRightEdge = Math.abs(props.brushEndMs - prev.end) < 2_000;
  if (atRightEdge) {
    const shift = next.end - prev.end;
    emit('update:brush', { startMs: props.brushStartMs + shift, endMs: next.end });
  }
});
</script>

<template>
  <div class="brush-wrap" v-if="range">
    <div
      class="rail"
      ref="railEl"
      @mousedown="onRailMouseDown"
      @dblclick="clearBrush"
      :title="brushStyle ? 'Drag to redraw · double-click to clear' : 'Drag across the rail to filter by time'"
    >
      <div
        v-for="(t, i) in ticks"
        :key="i"
        :class="tickClass(t)"
        :style="{ left: tickLeft(t) }"
      />

      <div
        v-if="brushStyle"
        class="brush"
        :style="brushStyle"
        @mousedown="onBrushMouseDown"
      >
        <div class="handle left" @mousedown="onHandleDown('left', $event)" />
        <div class="handle right" @mousedown="onHandleDown('right', $event)" />
      </div>
    </div>
    <div v-if="brushStyle" class="hint">
      brush: {{ new Date(props.brushStartMs!).toLocaleTimeString() }} →
      {{ new Date(props.brushEndMs!).toLocaleTimeString() }}
      <button class="clr" type="button" @click="clearBrush">Clear</button>
    </div>
  </div>
</template>

<style scoped>
.brush-wrap {
  display: grid;
  gap: 4px;
  user-select: none;
}
.rail {
  position: relative;
  height: 28px;
  background: #f3f4f6;
  border: 1px solid #e5e7eb;
  border-radius: 4px;
  cursor: crosshair;
  overflow: hidden;
}
.tk {
  position: absolute;
  top: 8px;
  bottom: 8px;
  width: 2px;
  border-radius: 1px;
  background: #9ca3af;
}
.tk.s2 { background: #10b981; }
.tk.s3 { background: #3b82f6; }
.tk.s4 { background: #f59e0b; }
.tk.s5 { background: #ef4444; }
/* Fault ticks pulled up to the full rail height + thicker stroke +
 * darker tone so they stand out as event markers. Matches the legacy
 * cyan/red overlay placed on the bandwidth chart's network event
 * row — same conceptual marker, applied to the brush rail here. */
.tk.fault {
  background: #991b1b;
  top: 0;
  bottom: 0;
  width: 3px;
  box-shadow: 0 0 0 1px rgba(153, 27, 27, 0.35);
}

.brush {
  position: absolute;
  top: 0;
  bottom: 0;
  background: rgba(59, 130, 246, 0.18);
  border-left: 1px solid #3b82f6;
  border-right: 1px solid #3b82f6;
  cursor: grab;
}
.brush:active { cursor: grabbing; }
.handle {
  position: absolute;
  top: 0;
  bottom: 0;
  width: 6px;
  background: rgba(59, 130, 246, 0.85);
  cursor: ew-resize;
}
.handle.left { left: -3px; }
.handle.right { right: -3px; }

.hint {
  font-size: 11px;
  color: #6b7280;
  display: flex;
  align-items: center;
  gap: 8px;
}
.clr {
  background: transparent;
  border: 1px solid #dadce0;
  border-radius: 4px;
  padding: 2px 8px;
  font-size: 11px;
  cursor: pointer;
  color: #374151;
}
.clr:hover { background: #f1f3f4; }
</style>
