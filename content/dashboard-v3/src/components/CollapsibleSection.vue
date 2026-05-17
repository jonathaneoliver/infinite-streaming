<script setup lang="ts">
/**
 * CollapsibleSection.vue — fold/unfold panel that matches the legacy
 * dashboard's collapsible look (▶/▼ icon, panel title, optional badge
 * slot for things like "12 requests" on the network log). Uses native
 * <details>/<summary> under the hood so keyboard + screen-reader
 * behaviour is correct without extra wiring.
 *
 * Persistence — matches the legacy `testing_session_collapse_*`
 * localStorage scheme. Pass `persist-key="<id>"` and the open/closed
 * state survives reloads. The same `persist-key` is the section
 * identifier accepted by `?open_folds=A,B,C` URLs.
 */
import { onMounted, ref, watch } from 'vue';

const props = defineProps<{
  title: string;
  open?: boolean;
  /** Stable id used for localStorage + ?open_folds= deep-linking.
   *  Omit to opt out of persistence (state stays per-tab). */
  persistKey?: string;
}>();

const STORAGE_PREFIX = 'testing_session_collapse_';
const OPEN_FOLDS_PARAM = 'open_folds';

function readStored(key: string): boolean | null {
  try {
    const v = localStorage.getItem(STORAGE_PREFIX + key);
    if (v == null) return null;
    // Legacy stored 'true' when the section was OPEN, 'false' when closed.
    return v === 'true';
  } catch {
    return null;
  }
}

function writeStored(key: string, isOpen: boolean) {
  try {
    localStorage.setItem(STORAGE_PREFIX + key, isOpen ? 'true' : 'false');
  } catch {
    /* private mode / quota — ignore */
  }
}

/** Pre-expand sections named in the URL's `?open_folds=a,b,c`.
 *  Idempotent — called from every CollapsibleSection on first mount;
 *  parses the URL once per call. */
function urlForcesOpen(key: string): boolean {
  try {
    const raw = new URLSearchParams(window.location.search).get(OPEN_FOLDS_PARAM);
    if (!raw) return false;
    const names = raw.split(',').map((s) => s.trim().toLowerCase()).filter(Boolean);
    return names.includes(key.toLowerCase());
  } catch {
    return false;
  }
}

// Resolve the effective initial open state.
//   1. ?open_folds= wins (deep link override)
//   2. localStorage if set
//   3. default from props.open
function resolveInitial(): boolean {
  if (props.persistKey && urlForcesOpen(props.persistKey)) {
    // Persist the deep-link choice so the user's next reload stays open.
    writeStored(props.persistKey, true);
    return true;
  }
  if (props.persistKey) {
    const stored = readStored(props.persistKey);
    if (stored != null) return stored;
  }
  return !!props.open;
}

const isOpen = ref<boolean>(resolveInitial());

// `<details>` fires a native `toggle` event with `.open` set. Mirror it
// into our ref and persist.
function onToggle(e: Event) {
  const det = e.currentTarget as HTMLDetailsElement;
  isOpen.value = det.open;
  if (props.persistKey) {
    writeStored(props.persistKey, det.open);
    // eslint-disable-next-line no-console
    console.debug('[CollapsibleSection] persisted', props.persistKey, '=', det.open);
  }
}

// If `persistKey` arrives async (rare), re-resolve once it's set.
onMounted(() => {
  if (props.persistKey) {
    isOpen.value = resolveInitial();
  }
});

// Re-resolve if persist-key prop ever changes (rare but defensive).
watch(
  () => props.persistKey,
  () => { isOpen.value = resolveInitial(); },
);
</script>

<template>
  <details class="panel" :open="isOpen" @toggle="onToggle">
    <summary class="head">
      <span class="icon">▶</span>
      <span class="title">{{ title }}</span>
      <span class="badge"><slot name="badge" /></span>
    </summary>
    <div class="body">
      <slot />
    </div>
  </details>
</template>

<style scoped>
.panel {
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 8px;
  margin-bottom: 12px;
  overflow: hidden;
}
.head {
  list-style: none;
  cursor: pointer;
  user-select: none;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 12px 16px;
  font-size: 13px;
  font-weight: 600;
  color: #1f2937;
  background: #f9fafb;
  border-bottom: 1px solid transparent;
}
.head::-webkit-details-marker { display: none; }
.panel[open] > .head {
  border-bottom-color: #e5e7eb;
}
.head:hover { background: #f3f4f6; }

.icon {
  display: inline-block;
  width: 14px;
  font-size: 10px;
  color: #6b7280;
  transition: transform 0.15s ease;
}
.panel[open] > .head .icon {
  transform: rotate(90deg);
}

.title {
  flex: 1;
  text-transform: uppercase;
  letter-spacing: 0.4px;
  font-size: 12px;
}

.badge {
  font-size: 11px;
  font-weight: 500;
  color: #6b7280;
  background: #f3f4f6;
  padding: 2px 8px;
  border-radius: 10px;
}
.badge:empty { display: none; }

.body {
  padding: 16px;
}
</style>
