<script setup lang="ts">
/**
 * CycleBandsRail — horizontal time-axis overlay rendering one band
 * per characterization cycle that ran during the visible window.
 *
 * Cycles are derived from `control_events` rows where event ==
 * 'label_changed' AND the row's `info` JSON contains a `cycle_id`
 * key. PATCH semantics give us implicit boundaries: each non-empty
 * cycle_id opens a band; the band ends at the next change to
 * cycle_id (any value, including empty = explicit EndCycle).
 *
 * See .claude/standards/characterization-principles.md § 9 for the
 * label schema this rail consumes.
 *
 * Self-contained — does NOT integrate with vis-timeline. The rail
 * has its own absolute layout driven by the same fromMs/toMs the
 * brush uses, so it stays aligned with the time axis above.
 *
 * Props:
 *   controlStream  the control Stream<Record<string, unknown>>
 *                  from useSessionTimeSeries
 *   fromMs / toMs  the visible time domain (matches the brush)
 *
 * Renders nothing when no cycles overlap the domain.
 */
import { computed } from 'vue';
import type { Stream } from '@/composables/useSessionTimeSeries';

interface Props {
  controlStream: Stream<Record<string, unknown>>;
  fromMs: number;
  toMs: number;
}
const props = defineProps<Props>();

interface Band {
  startMs: number;
  endMs: number;
  cycleId: string;
}

const bands = computed<Band[]>(() => {
  // Read version so we re-run on stream updates.
  void props.controlStream.version.value;
  if (!Number.isFinite(props.fromMs) || !Number.isFinite(props.toMs) || props.toMs <= props.fromMs) {
    return [];
  }
  // Pull a wider range than the visible window so bands that started
  // BEFORE fromMs still get a left edge — without this, a cycle that
  // began 10 s before the brush is invisible until the operator
  // scrolls back.
  const pad = 60 * 60_000;
  const rows = props.controlStream.inRange(props.fromMs - pad, props.toMs);
  type Change = { tsMs: number; cycleId: string };
  const changes: Change[] = [];
  for (const r of rows) {
    if (String(r.event) !== 'label_changed') continue;
    const info = r.info;
    if (typeof info !== 'string' || !info) continue;
    let parsed: Record<string, string> | null = null;
    try { parsed = JSON.parse(info) as Record<string, string>; } catch { continue; }
    if (!parsed || typeof parsed.cycle_id !== 'string') continue;
    const tsMs = typeof r.ts === 'string' ? Date.parse(r.ts as string) : Number(r.ts);
    if (!Number.isFinite(tsMs)) continue;
    changes.push({ tsMs, cycleId: parsed.cycle_id });
  }
  changes.sort((a, b) => a.tsMs - b.tsMs);

  // Walk changes — each non-empty cycle_id opens a band; the band
  // ends at the next change (any value) or at toMs (still in flight).
  const out: Band[] = [];
  for (let i = 0; i < changes.length; i++) {
    const ch = changes[i];
    if (!ch.cycleId) continue;
    const next = changes[i + 1];
    const endMs = next ? next.tsMs : props.toMs;
    // Clip to visible window — drop bands entirely outside [fromMs, toMs].
    const clipStart = Math.max(ch.tsMs, props.fromMs);
    const clipEnd = Math.min(endMs, props.toMs);
    if (clipEnd <= clipStart) continue;
    out.push({ startMs: clipStart, endMs: clipEnd, cycleId: ch.cycleId });
  }
  return out;
});

function bandStyle(b: Band): Record<string, string> {
  const span = props.toMs - props.fromMs;
  const left = ((b.startMs - props.fromMs) / span) * 100;
  const width = ((b.endMs - b.startMs) / span) * 100;
  return { left: `${left}%`, width: `${width}%` };
}

// Stable color per cycle_id — hash the string to one of N hues so
// adjacent cycles are visually distinct without coordinating colors
// across runs.
function bandColor(cycleId: string): string {
  let h = 0;
  for (let i = 0; i < cycleId.length; i++) h = (h * 31 + cycleId.charCodeAt(i)) >>> 0;
  const hue = h % 360;
  return `hsl(${hue} 70% 90%)`;
}
function bandBorder(cycleId: string): string {
  let h = 0;
  for (let i = 0; i < cycleId.length; i++) h = (h * 31 + cycleId.charCodeAt(i)) >>> 0;
  const hue = h % 360;
  return `hsl(${hue} 50% 55%)`;
}
</script>

<template>
  <div v-if="bands.length" class="cycle-bands-rail" :title="`${bands.length} cycle band(s) in view`">
    <div
      v-for="(b, i) in bands"
      :key="`${b.cycleId}-${b.startMs}-${i}`"
      class="cycle-band"
      :style="{ ...bandStyle(b), background: bandColor(b.cycleId), borderLeft: `2px solid ${bandBorder(b.cycleId)}` }"
      :title="`${b.cycleId} — ${new Date(b.startMs).toLocaleTimeString()} → ${new Date(b.endMs).toLocaleTimeString()}`"
    >
      <span class="cycle-chip">{{ b.cycleId }}</span>
    </div>
  </div>
</template>

<style scoped>
.cycle-bands-rail {
  position: relative;
  height: 22px;
  background: #f9fafb;
  border-top: 1px solid #e5e7eb;
  border-bottom: 1px solid #e5e7eb;
  overflow: hidden;
}
.cycle-band {
  position: absolute;
  top: 0;
  bottom: 0;
  min-width: 2px;
  display: flex;
  align-items: center;
  padding-left: 4px;
  overflow: hidden;
  white-space: nowrap;
}
.cycle-chip {
  font-family: ui-monospace, monospace;
  font-size: 10px;
  font-weight: 600;
  color: #1f2937;
  pointer-events: none;
}
</style>
