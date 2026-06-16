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
  /** When true, mount the slot eagerly (display: none when collapsed
   *  instead of removed from the DOM). Required for chart panels in
   *  the v3 session-viewer so the chart's `watch(player.value)` runs
   *  during the bulk snapshot replay even if the user has the
   *  section folded — without this, opening the section after replay
   *  shows the chart with only the latest sample. */
  eager?: boolean;
  /** External "default-collapse" signal (issue #579 compare mode). When
   *  this transitions to true the section folds; when it returns to
   *  false the section restores whatever open state it had before. The
   *  operator can still toggle freely while it's active — this only sets
   *  the default on each transition — and the auto-fold is NOT persisted,
   *  so it never overwrites the operator's saved preference. */
  forceCollapsed?: boolean;
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
/** Click on the header — flip open state and persist. Owns the state
 *  in Vue's reactivity rather than delegating to native `<details>`
 *  toggle: the native event has races (Vue patches `:open` on every
 *  re-render, the listener attaches AFTER the initial setAttribute,
 *  etc) that meant the persistence write didn't always run. With a
 *  plain div + click handler the write is guaranteed for every user
 *  toggle. */
function toggle() {
  isOpen.value = !isOpen.value;
  if (props.persistKey) writeStored(props.persistKey, isOpen.value);
}

// If `persistKey` arrives async (rare), re-resolve once it's set. But NOT
// when forceCollapsed is active — the immediate forceCollapsed watch (below)
// already folded us during setup, and re-resolving here to the persisted/open
// default would silently re-open a section that compare mode wants folded
// (#736: Player State defaults `:open="true"`, so it kept popping back open).
onMounted(() => {
  if (props.persistKey && !props.forceCollapsed) {
    isOpen.value = resolveInitial();
  }
});

// Re-resolve if persist-key prop ever changes (rare but defensive).
watch(
  () => props.persistKey,
  () => { isOpen.value = resolveInitial(); },
);

// External default-collapse (issue #579). On false→true, remember the
// current open state and fold; on true→false, restore it. The operator
// can still toggle in between (toggle() writes isOpen + persists); the
// auto-fold itself is deliberately NOT persisted.
let savedOpenBeforeForce: boolean | null = null;
watch(
  () => props.forceCollapsed,
  (v, old) => {
    // `immediate` so a section that mounts with forceCollapsed ALREADY true
    // still folds — the archive compare view (#736) defaults compare on, so
    // there's no false→true transition to catch otherwise. On the immediate
    // tick `old` is undefined; treat that like `false` so true folds and
    // false is a no-op.
    if (v && !old) {
      savedOpenBeforeForce = isOpen.value;
      isOpen.value = false;
    } else if (!v && old) {
      isOpen.value = savedOpenBeforeForce ?? resolveInitial();
      savedOpenBeforeForce = null;
    }
  },
  { immediate: true },
);
</script>

<template>
  <div class="panel" :class="{ open: isOpen }">
    <div class="head" role="button" tabindex="0"
         @click="toggle"
         @keydown.enter.prevent="toggle"
         @keydown.space.prevent="toggle"
         :aria-expanded="isOpen">
      <span class="icon">▶</span>
      <span class="title">{{ title }}</span>
      <span class="badge"><slot name="badge" /></span>
    </div>
    <div v-if="eager || isOpen" v-show="eager ? isOpen : true" class="body">
      <slot />
    </div>
  </div>
</template>

<style scoped>
.panel {
  background: #fff;
  border: 1px solid #e5e7eb;
  border-radius: 4px;
  margin-bottom: 2px;
  overflow: hidden;
}
.head {
  list-style: none;
  cursor: pointer;
  user-select: none;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 1px 8px;
  font-size: 11px;
  font-weight: 600;
  color: #1f2937;
  background: #f9fafb;
  border-bottom: 1px solid transparent;
  line-height: 1.15;
  min-height: 18px;
}
.panel.open > .head {
  border-bottom-color: #e5e7eb;
}
.head:hover { background: #f3f4f6; }
.head:focus-visible { outline: 2px solid #3b82f6; outline-offset: -2px; }

.icon {
  display: inline-block;
  width: 10px;
  font-size: 9px;
  color: #6b7280;
  transition: transform 0.15s ease;
}
.panel.open > .head .icon {
  transform: rotate(90deg);
}

.title {
  flex: 1;
  text-transform: uppercase;
  letter-spacing: 0.3px;
  font-size: 11px;
}

.badge {
  font-size: 10px;
  font-weight: 500;
  color: #6b7280;
  background: #f3f4f6;
  padding: 1px 6px;
  border-radius: 8px;
}
.badge:empty { display: none; }

.body {
  padding: 10px 12px;
}
</style>
