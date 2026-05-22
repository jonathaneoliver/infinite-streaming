<script setup lang="ts">
/**
 * SessionViewerLink — single-source link to /dashboard/v3/session-viewer.html
 * for one time-bounded window. Used by every characterization cycle/step
 * row on the Characterization page so the operator can jump from a
 * per-cycle result straight to the replay scoped to that cycle's window.
 *
 * Contract — the session-viewer page reads these URL params:
 *   - player_id (required)
 *   - play_id   (optional but recommended — scopes SSE filter)
 *   - start_time (ISO; absolute UTC; the viewer brushes to this)
 *   - end_time   (ISO or "live"; absent = follow-live)
 *
 * Inputs:
 *   - playerId         the player UUID
 *   - playId           the play UUID for the row (omit for runs that don't
 *                      have a per-row play_id — viewer still works but
 *                      shows the whole player's stream)
 *   - startMs          absolute epoch ms; this component handles the
 *                      pre-roll padding (subtracts `preRollMs`)
 *   - endMs            absolute epoch ms; padded by `postRollMs`
 *   - preRollMs        default 10_000 — extra context BEFORE the cycle
 *                      so the operator sees the player state going in
 *   - postRollMs       default 10_000 — extra context AFTER the cycle
 *                      so the operator sees recovery
 *   - label            link text; default "↗ Viewer"
 *   - title            tooltip; default explains what opens
 */
import { computed } from 'vue';
import { sessionViewerURL } from '@/composables/urlTimeFormat';

interface Props {
  playerId: string;
  playId?: string;
  startMs: number;
  endMs: number;
  preRollMs?: number;
  postRollMs?: number;
  label?: string;
  title?: string;
}
const props = withDefaults(defineProps<Props>(), {
  preRollMs: 10_000,
  postRollMs: 10_000,
  label: '↗ Viewer',
  title: 'Open this cycle in the session viewer',
});

const href = computed(() =>
  sessionViewerURL({
    playerId: props.playerId,
    playId: props.playId,
    fromMs: props.startMs - props.preRollMs,
    toMs: props.endMs + props.postRollMs,
  }),
);

const disabled = computed(() => !props.playerId || !Number.isFinite(props.startMs));
</script>

<template>
  <a
    class="session-viewer-link"
    :class="{ disabled }"
    :href="disabled ? undefined : href"
    target="_blank"
    rel="noopener"
    :title="title"
  >{{ label }}</a>
</template>

<style scoped>
.session-viewer-link {
  display: inline-block;
  font-size: 11px;
  font-weight: 600;
  color: #2563eb;
  text-decoration: none;
  padding: 1px 6px;
  border-radius: 4px;
  border: 1px solid #dbeafe;
  background: #eff6ff;
  white-space: nowrap;
}
.session-viewer-link:hover { background: #dbeafe; }
.session-viewer-link.disabled {
  color: #9ca3af;
  border-color: #e5e7eb;
  background: #f3f4f6;
  pointer-events: none;
  cursor: not-allowed;
}
</style>
