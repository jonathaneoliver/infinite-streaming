<script setup lang="ts">
/**
 * CitationCard — one citation the chat backend emitted via the
 * cite() tool. Rendered as a clickable button that deep-links into
 * the relevant dashboard surface.
 *
 * Kinds (each maps to a URL):
 *   play     → session-viewer.html#play_id=X&at=Y
 *   range    → session-viewer.html#play_id=X&from=A&to=B
 *   finding  → opens a modal preview (TODO: route to a findings page)
 *   standard → opens a modal preview (TODO: standards page)
 *   skill    → opens a modal preview (TODO: skills page)
 *   run      → characterization.html#run_id=X&cycle=N
 */
import { computed } from 'vue';
import type { Citation } from '@/types/chat';

const props = defineProps<{ citation: Citation }>();
const emit = defineEmits<{ preview: [citation: Citation] }>();

const ICONS: Record<string, string> = {
  play: '▶',
  range: '⤇',
  finding: '🔎',
  standard: '📘',
  skill: '🛠',
  run: '🧪',
};

const KIND_COLORS: Record<string, string> = {
  play: '#1a73e8',
  range: '#0d9488',
  finding: '#a855f7',
  standard: '#0f766e',
  skill: '#b45309',
  run: '#dc2626',
};

const icon = computed(() => ICONS[props.citation.kind] ?? '•');
const color = computed(() => KIND_COLORS[props.citation.kind] ?? '#5f6368');

const href = computed(() => {
  const c = props.citation;
  switch (c.kind) {
    case 'play':
      return c.play_id
        ? `/dashboard/v3/session-viewer.html?play_id=${encodeURIComponent(c.play_id)}${c.at ? `&at=${encodeURIComponent(c.at)}` : ''}`
        : null;
    case 'range':
      return c.play_id
        ? `/dashboard/v3/session-viewer.html?play_id=${encodeURIComponent(c.play_id)}${c.from ? `&from=${encodeURIComponent(c.from)}` : ''}${c.to ? `&to=${encodeURIComponent(c.to)}` : ''}`
        : null;
    case 'run':
      return c.run_id
        ? `/dashboard/v3/characterization.html?run_id=${encodeURIComponent(c.run_id)}${c.cycle ? `&cycle=${c.cycle}` : ''}`
        : null;
    case 'finding':
    case 'standard':
    case 'skill':
      // No dedicated page yet — preview opens via the emit.
      return null;
  }
  return null;
});

const tooltip = computed(() => {
  const c = props.citation;
  const parts = [`${c.kind}: ${c.label}`, `[${c.span_id}]`];
  if (c.play_id) parts.push(`play=${c.play_id.slice(0, 8)}…`);
  if (c.at) parts.push(`at=${c.at}`);
  if (c.slug) parts.push(`slug=${c.slug}`);
  if (c.name) parts.push(`name=${c.name}`);
  return parts.join(' · ');
});

function onClick(e: MouseEvent) {
  if (href.value) return; // <a> handles navigation
  e.preventDefault();
  emit('preview', props.citation);
}
</script>

<template>
  <component
    :is="href ? 'a' : 'button'"
    :href="href || undefined"
    :target="href ? '_blank' : undefined"
    class="cite-card"
    :style="{ borderColor: color, color: color }"
    :title="tooltip"
    @click="onClick"
  >
    <span class="cite-icon">{{ icon }}</span>
    <span class="cite-label">{{ citation.label }}</span>
    <span class="cite-span">[{{ citation.span_id }}]</span>
  </component>
</template>

<style scoped>
.cite-card {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 2px 8px;
  margin: 0 2px;
  border: 1px solid;
  border-radius: var(--radius-full);
  background: #fff;
  font: 600 12px/1.4 'Google Sans', system-ui, sans-serif;
  cursor: pointer;
  text-decoration: none;
  transition: background var(--transition), transform var(--transition);
  vertical-align: baseline;
}
.cite-card:hover {
  background: var(--surface);
  transform: translateY(-1px);
}
.cite-icon {
  font-size: 11px;
  opacity: 0.85;
}
.cite-label {
  max-width: 220px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.cite-span {
  opacity: 0.55;
  font-size: 10px;
  font-weight: 500;
}
</style>
