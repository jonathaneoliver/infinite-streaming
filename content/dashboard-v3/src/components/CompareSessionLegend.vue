<script setup lang="ts">
/**
 * CompareSessionLegend.vue — the S1/S2/… chip row for compare mode
 * (issue #579). One chip per grouped session; the chip's swatch shows
 * that session's line style (solid = active session, dashed = siblings,
 * matching the charts). Hovering a chip pops every line for that session
 * across ALL charts and dims the rest; clicking toggles that session's
 * lines on/off everywhere. Drives the shared CompareView the charts read.
 */
import type { CompareView } from '@/composables/useCompareContext';

interface SessionChip {
  tag: string;
  label: string;
  dash: number[];
  isSelf: boolean;
}

const props = defineProps<{
  sessions: SessionChip[];
  view: CompareView;
}>();

function isHidden(tag: string): boolean {
  return props.view.hidden.value.has(tag);
}
function toggle(tag: string) {
  const next = new Set(props.view.hidden.value);
  if (next.has(tag)) next.delete(tag); else next.add(tag);
  props.view.hidden.value = next;
}
function enter(tag: string) { props.view.hovered.value = tag; }
function leave() { props.view.hovered.value = null; }
function dashArray(dash: number[]): string {
  return dash.length ? dash.join(' ') : '';
}
</script>

<template>
  <div class="session-legend" @mouseleave="leave">
    <span class="sl-label">Sessions</span>
    <button
      v-for="s in sessions"
      :key="s.tag"
      type="button"
      class="sl-chip"
      :class="{ off: isHidden(s.tag), self: s.isSelf }"
      @mouseenter="enter(s.tag)"
      @focus="enter(s.tag)"
      @blur="leave"
      @click="toggle(s.tag)"
      :title="isHidden(s.tag)
        ? `Show all ${s.tag} lines`
        : `Hide all ${s.tag} lines · hover to highlight just this session`"
    >
      <svg class="sl-swatch" width="24" height="8" aria-hidden="true">
        <line
          x1="1" y1="4" x2="23" y2="4"
          stroke="currentColor" stroke-width="2"
          :stroke-dasharray="dashArray(s.dash)"
        />
      </svg>
      <span class="sl-tag">{{ s.tag }}</span>
      <span class="sl-name">{{ s.label }}</span>
    </button>
  </div>
</template>

<style scoped>
.session-legend {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  margin: 0 0 8px;
  padding: 6px 8px;
  background: #f8fafc;
  border: 1px solid #e5e7eb;
  border-radius: 6px;
}
.sl-label {
  font-size: 11px;
  font-weight: 600;
  color: #6b7280;
  text-transform: uppercase;
  letter-spacing: 0.3px;
  margin-right: 2px;
}
.sl-chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 3px 9px;
  font-size: 11px;
  background: #fff;
  border: 1px solid #d1d5db;
  border-radius: 999px;
  color: #1f2937;
  cursor: pointer;
  line-height: 1.4;
}
.sl-chip:hover { border-color: #9ca3af; background: #f9fafb; }
.sl-chip.self { border-color: #c7d2fe; }
.sl-chip.off {
  opacity: 0.5;
  background: #f3f4f6;
  text-decoration: line-through;
}
.sl-swatch { color: #475569; flex: none; }
.sl-tag { font-weight: 700; font-variant-numeric: tabular-nums; }
.sl-name { color: #6b7280; max-width: 160px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
