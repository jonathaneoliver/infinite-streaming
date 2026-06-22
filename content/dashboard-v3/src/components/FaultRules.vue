<script setup lang="ts">
/**
 * FaultRules.vue — Fault Injection panel. 5 tabs matching legacy:
 *
 *   All · Segment · Manifest · Master · Transport
 *
 * Notes vs legacy:
 *   - "Content" lives in its own peer Content Manipulation panel.
 *   - Legacy fault-type list included `403`, `connection_refused`,
 *     `dns_failure`, `rate_limiting` — those are not in the v2 OpenAPI
 *     enum (FaultRule.type), so they are intentionally omitted. The
 *     ones that ARE in the v2 enum get pretty labels below.
 *   - `corrupted` is segment-only on the server, so we only surface it
 *     on the Segment tab (legacy showed it everywhere by accident).
 *   - HTTP "Mode" default is `requests` (legacy default). Transport
 *     default is still `failures_per_seconds` because v2 does not yet
 *     model `failures_per_packets` — that needs a spec extension.
 *
 * Each surface persists into the v2 `fault_rules` array via stable
 * rule ids (`v1-segment`, `v1-manifest`, `v1-master_manifest`,
 * `v1-all`). Transport edits `shape.transport_fault`.
 */
import { computed, ref, toRef, watch } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import { useManifestVariants } from '@/composables/useManifestVariants';
import type { FaultRule } from '@/repo/v2-repo';

type Surface = 'segment' | 'manifest' | 'master_manifest' | 'all' | 'transport';

const SURFACES: { key: Surface; label: string; supportsScope: boolean }[] = [
  { key: 'all',             label: 'All',      supportsScope: true  },
  { key: 'segment',         label: 'Segment',  supportsScope: true  },
  { key: 'manifest',        label: 'Manifest', supportsScope: true  },
  { key: 'master_manifest', label: 'Master',   supportsScope: false },
  { key: 'transport',       label: 'Transport',supportsScope: false },
];

interface FaultTypeChoice { value: string; label: string; segmentOnly?: boolean }

// Common (cross-surface) types first; surface-specific (`corrupted`)
// lives at the end. Order is the display order — radios on every tab.
const HTTP_FAULT_TYPES: FaultTypeChoice[] = [
  { value: 'none',                          label: 'None' },
  { value: '403',                           label: 'HTTP 403' },
  { value: '404',                           label: 'HTTP 404' },
  { value: '500',                           label: 'HTTP 500' },
  { value: '503',                           label: 'HTTP 503' },
  { value: 'timeout',                       label: 'Timeout' },
  { value: 'connection_refused',            label: 'Conn Refused' },
  { value: 'dns_failure',                   label: 'DNS Failure' },
  { value: 'rate_limiting',                 label: 'Rate Limiting' },
  { value: 'request_connect_hang',          label: 'Conn Hang' },
  { value: 'request_connect_reset',         label: 'Conn Reset' },
  { value: 'request_connect_delayed',       label: 'Conn Delayed' },
  { value: 'request_first_byte_hang',       label: '1st-Byte Hang' },
  { value: 'request_first_byte_reset',      label: '1st-Byte Reset' },
  { value: 'request_first_byte_delayed',    label: '1st-Byte Delayed' },
  { value: 'request_body_hang',             label: 'Body Hang' },
  { value: 'request_body_reset',            label: 'Body Reset' },
  { value: 'request_body_delayed',          label: 'Body Delayed' },
  // Segment-only — must be last so non-segment tabs filter it out
  // cleanly with the `segmentOnly` flag.
  { value: 'corrupted',                     label: 'Corrupted', segmentOnly: true },
];

const TRANSPORT_FAULT_TYPES: FaultTypeChoice[] = [
  { value: 'none',   label: 'None' },
  { value: 'drop',   label: 'Drop (Blackhole)' },
  { value: 'reject', label: 'Reject (RST)' },
];

const FAULT_MODES: { value: string; label: string }[] = [
  { value: 'requests',             label: 'Requests' },
  { value: 'seconds',              label: 'Seconds' },
  { value: 'failures_per_seconds', label: 'Per N seconds' },
];

// Transport mode includes the per-packet sampler in addition to the
// time-window options. Only surfaced on the Transport tab.
const TRANSPORT_FAULT_MODES: { value: string; label: string }[] = [
  { value: 'failures_per_seconds', label: 'Per N seconds' },
  { value: 'failures_per_packets', label: 'Per N packets' },
  { value: 'seconds',              label: 'Seconds' },
  { value: 'requests',             label: 'Packets' },
];

const props = defineProps<{ playerId: string }>();
const {
  player, upsertFaultRule, setTransportFault,
} = usePlayer(toRef(props, 'playerId'));

const activeTab = ref<Surface>('all');

function findRule(surface: Exclude<Surface, 'transport'>): FaultRule | null {
  const rules = player.value?.fault_rules ?? [];
  const want = 'v1-' + surface;
  return (
    rules.find((r) => r.id === want)
    ?? rules.find((r) => {
      const kinds = r.filter?.request_kind ?? null;
      if (surface === 'all') return !r.filter && r.id !== 'v1-segment' && r.id !== 'v1-manifest' && r.id !== 'v1-master_manifest';
      return Array.isArray(kinds) && kinds.length === 1 && kinds[0] === surface;
    })
    ?? null
  );
}

const allRule = computed(() => findRule('all'));
const segmentRule = computed(() => findRule('segment'));
const manifestRule = computed(() => findRule('manifest'));
const masterRule = computed(() => findRule('master_manifest'));
const transportFault = computed(() => player.value?.shape?.transport_fault);
const faultCounters = computed(() => player.value?.fault_counters);

function ruleFor(s: Surface) {
  if (s === 'transport') return null;
  if (s === 'all') return allRule.value;
  if (s === 'segment') return segmentRule.value;
  if (s === 'manifest') return manifestRule.value;
  return masterRule.value;
}

function patchSurface(surface: Exclude<Surface, 'transport'>, partial: Partial<FaultRule>) {
  const current = findRule(surface);
  const base: FaultRule = current
    ? ({ ...current, ...partial } as FaultRule)
    : ({
        id: 'v1-' + surface,
        type: (partial.type as FaultRule['type']) ?? 'none',
        // Default mode = "every N seconds". Operators reach for the
        // time-based cadence first ("inject a fault every 5 s") far
        // more often than the request-counter mode, so make that the
        // happy-path default everywhere. Matches transport's existing
        // default below.
        mode: (partial.mode as FaultRule['mode']) ?? 'failures_per_seconds',
        consecutive: partial.consecutive ?? 0,
        frequency: partial.frequency ?? 0,
        filter: surface === 'all' ? undefined : { request_kind: [surface as any] },
        ...partial,
      } as FaultRule);
  upsertFaultRule(base);
}

function patchTransport(partial: { type?: string; mode?: string; frequency?: number; consecutive?: number }) {
  const cur = transportFault.value;
  const next = {
    type: (partial.type ?? cur?.type ?? 'drop') as 'drop' | 'reject',
    mode: (partial.mode ?? cur?.mode ?? 'failures_per_seconds') as
      | 'failures_per_seconds' | 'failures_per_packets' | 'seconds' | 'requests',
    consecutive: partial.consecutive ?? cur?.consecutive ?? 0,
    frequency: partial.frequency ?? cur?.frequency ?? 0,
  };
  setTransportFault(next);
}

const DEBOUNCE_MS = 200;
const localFreq = ref<Record<Surface, number | null>>({ all: null, segment: null, manifest: null, master_manifest: null, transport: null });
const localCons = ref<Record<Surface, number | null>>({ all: null, segment: null, manifest: null, master_manifest: null, transport: null });
const freqTimers: Record<Surface, number | undefined> = {} as any;
const consTimers: Record<Surface, number | undefined> = {} as any;

function commitFreq(surface: Surface, v: number) {
  if (surface === 'transport') patchTransport({ frequency: v });
  else patchSurface(surface, { frequency: v });
}
function commitCons(surface: Surface, v: number) {
  if (surface === 'transport') patchTransport({ consecutive: v });
  else patchSurface(surface, { consecutive: v });
}
function onFreqInput(surface: Surface, e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localFreq.value[surface] = v;
  if (freqTimers[surface]) clearTimeout(freqTimers[surface]);
  freqTimers[surface] = window.setTimeout(() => commitFreq(surface, v), DEBOUNCE_MS);
}
function onConsInput(surface: Surface, e: Event) {
  const v = +(e.target as HTMLInputElement).value;
  localCons.value[surface] = v;
  if (consTimers[surface]) clearTimeout(consTimers[surface]);
  consTimers[surface] = window.setTimeout(() => commitCons(surface, v), DEBOUNCE_MS);
}

function getFreq(surface: Surface): number {
  if (localFreq.value[surface] !== null) return localFreq.value[surface]!;
  if (surface === 'transport') return transportFault.value?.frequency ?? 0;
  return ruleFor(surface)?.frequency ?? 0;
}
function getCons(surface: Surface): number {
  if (localCons.value[surface] !== null) return localCons.value[surface]!;
  if (surface === 'transport') return transportFault.value?.consecutive ?? 0;
  return ruleFor(surface)?.consecutive ?? 0;
}
function getType(surface: Surface): string {
  if (surface === 'transport') {
    const t = transportFault.value?.type;
    return t === 'drop' || t === 'reject' ? t : 'none';
  }
  return ruleFor(surface)?.type ?? 'none';
}
function getMode(surface: Surface): string {
  if (surface === 'transport') return transportFault.value?.mode ?? 'failures_per_seconds';
  // Same "every N seconds" default as patchSurface, so the dropdown
  // matches the value we'd write if the operator nudges any other
  // control on this surface.
  return ruleFor(surface)?.mode ?? 'failures_per_seconds';
}

function onTypeChange(surface: Surface, e: Event) {
  const v = (e.target as HTMLInputElement).value;
  if (surface === 'transport') {
    patchTransport({ type: v });
  } else {
    patchSurface(surface, { type: v as FaultRule['type'] });
  }
}
function onModeChange(surface: Surface, e: Event) {
  const v = (e.target as HTMLSelectElement).value;
  if (surface === 'transport') patchTransport({ mode: v });
  else patchSurface(surface, { mode: v as FaultRule['mode'] });
}

watch(
  () => SURFACES.map((s) => [s.key, getFreq(s.key)] as const).map(([_, v]) => v),
  () => {
    for (const { key } of SURFACES) {
      const local = localFreq.value[key];
      if (local === null) continue;
      const serverV = key === 'transport' ? transportFault.value?.frequency : ruleFor(key)?.frequency;
      if (serverV === local) localFreq.value[key] = null;
    }
  },
);
watch(
  () => SURFACES.map((s) => [s.key, getCons(s.key)] as const).map(([_, v]) => v),
  () => {
    for (const { key } of SURFACES) {
      const local = localCons.value[key];
      if (local === null) continue;
      const serverV = key === 'transport' ? transportFault.value?.consecutive : ruleFor(key)?.consecutive;
      if (serverV === local) localCons.value[key] = null;
    }
  },
);

function typeChoicesFor(surface: Surface): FaultTypeChoice[] {
  if (surface === 'transport') return TRANSPORT_FAULT_TYPES;
  if (surface === 'segment') return HTTP_FAULT_TYPES;
  return HTTP_FAULT_TYPES.filter((c) => !c.segmentOnly);
}

// ---- Scope checkbox group (All / Audio / per-variant) ----
//
// Layout (mirrors legacy "Scope" row on All / Segment / Manifest tabs):
//   [✓] All     [✓] Audio     [✓] 360p · 998k     [✓] 540p · 1840k …
//
// Audio is treated as ONE of the synthetic-plus-real variants —
// equivalent to a `audio` resolution in the legacy set-of-URLs model.
// All the boxes are in / out together:
//
//   - "All" is the select-all toggle. Checking it ticks Audio + every
//     resolution. Unchecking it clears them all (rule matches nothing).
//   - Toggling any individual box (Audio or a resolution) auto-syncs
//     "All": it's checked iff every individual box is checked.
//   - Storage: video resolutions → `filter.variant.resolutions`; audio
//     → presence of audio classifier(s) in `filter.request_kind`.
//
// When the rule has no `filter.variant`, every resolution is implicitly
// in scope (matches the rule's request_kind). When `filter.request_kind`
// is absent or already contains the audio classifier for this tab,
// audio is in scope too.

// Full ladder — fault injection targets any available variant, even ones thinned
// out of the player's current manifest by allowed_variants (not just the allowed
// subset). Bandwidth chart uses the thinned `variants`; this needs them all.
const { variantsAll: rawManifestVariants } = useManifestVariants(toRef(props, 'playerId'));
// Legacy sorts descending by bandwidth (highest rung first).
const manifestVariants = computed(() => {
  return rawManifestVariants.value.slice().sort((a, b) => (b.bandwidth ?? 0) - (a.bandwidth ?? 0));
});

// Non-audio request kinds we enumerate on the "All" tab when audio
// needs to be excluded explicitly (otherwise the All-tab rule has no
// kind constraint and would match everything, including audio).
const ALL_NON_AUDIO_KINDS = ['master_manifest', 'manifest', 'segment', 'partial', 'init'] as const;

// Base request_kind set for a surface tab (before audio is layered on).
function baseRequestKinds(surface: Exclude<Surface, 'transport'>): string[] {
  if (surface === 'all') return [];
  return [surface];
}

function audioKindsFor(surface: Exclude<Surface, 'transport'>): string[] {
  if (surface === 'segment') return ['audio_segment'];
  if (surface === 'manifest') return ['audio_manifest'];
  return ['audio_segment', 'audio_manifest'];
}

function selectedResolutions(surface: Exclude<Surface, 'transport'>): string[] {
  const r = ruleFor(surface);
  const res = r?.filter?.variant?.resolutions;
  return Array.isArray(res) ? res : [];
}

/** True iff audio requests are in scope for this rule. An absent
 *  request_kind constraint means everything matches → audio is in. */
function isAudioScoped(surface: Exclude<Surface, 'transport'>): boolean {
  const r = ruleFor(surface);
  const kinds = r?.filter?.request_kind;
  if (!Array.isArray(kinds) || kinds.length === 0) return true;
  const audioKinds = audioKindsFor(surface);
  return audioKinds.some((k) => kinds.includes(k as any));
}

function isResolutionInScope(surface: Exclude<Surface, 'transport'>, res: string): boolean {
  const r = ruleFor(surface);
  const hasFilter = r?.filter?.variant != null;
  if (!hasFilter) return true;
  return selectedResolutions(surface).includes(res);
}

/** "All" is checked iff every individual sub-box is checked — i.e.
 *  no variant narrowing AND audio is in scope. */
function isAllChecked(surface: Exclude<Surface, 'transport'>): boolean {
  const r = ruleFor(surface);
  return r?.filter?.variant == null && isAudioScoped(surface);
}

interface BuildFilterArgs {
  /** Explicit resolutions list. `undefined` = no variant narrowing.
   *  `[]` = "match no video variants". */
  resolutions?: string[];
  /** Whether audio is in scope. */
  audio: boolean;
  surface: Exclude<Surface, 'transport'>;
}

/** Construct a FaultFilter from the user-facing scope state. Returns
 *  undefined when no constraints apply. */
function buildFilter(args: BuildFilterArgs): any {
  const { resolutions, audio, surface } = args;
  let kinds: string[] = [];
  if (surface === 'all') {
    if (!audio) {
      // Need an explicit non-audio kind list so the rule excludes audio.
      kinds = ALL_NON_AUDIO_KINDS.slice();
    }
    // If audio AND no variant narrowing — no request_kind needed (matches
    // every kind). If audio AND variant narrowing — request_kind stays
    // empty (variant filter narrows video; audio matches via kind defaults).
  } else {
    kinds = baseRequestKinds(surface).slice();
    if (audio) {
      for (const k of audioKindsFor(surface)) {
        if (!kinds.includes(k)) kinds.push(k);
      }
    }
  }
  const filter: any = {};
  if (kinds.length) filter.request_kind = kinds;
  if (Array.isArray(resolutions)) filter.variant = { resolutions };
  return Object.keys(filter).length ? filter : undefined;
}

function toggleAll(surface: Exclude<Surface, 'transport'>, on: boolean) {
  // All ON → clear variant filter + audio in scope. All OFF → empty
  // resolutions list + audio out of scope (rule matches nothing).
  const filter = on
    ? buildFilter({ surface, resolutions: undefined, audio: true })
    : buildFilter({ surface, resolutions: [], audio: false });
  patchSurface(surface, { filter: filter as any });
}

function toggleAudio(surface: Exclude<Surface, 'transport'>, on: boolean) {
  // Preserve the current variant-narrowing state.
  const r = ruleFor(surface);
  const resolutions = r?.filter?.variant?.resolutions as string[] | undefined;
  const filter = buildFilter({ surface, resolutions, audio: on });
  patchSurface(surface, { filter: filter as any });
}

function toggleScope(surface: Exclude<Surface, 'transport'>, res: string, on: boolean) {
  const r = ruleFor(surface);
  const hasFilter = r?.filter?.variant != null;
  // When the rule was in the all-checked state, start from every
  // variant; when it was already narrowed, start from the explicit
  // list. Either way, apply the toggle and decide if it collapses
  // back to the "no variant filter" state.
  const baseline = hasFilter
    ? selectedResolutions(surface)
    : manifestVariants.value.map((v) => v.resolution).filter(Boolean);
  const set = new Set(baseline);
  if (on) set.add(res);
  else set.delete(res);
  const allRes = manifestVariants.value.map((v) => v.resolution).filter(Boolean);
  const isAll = allRes.length > 0 && allRes.every((r2) => set.has(r2));
  const resolutions = isAll ? undefined : Array.from(set);
  const filter = buildFilter({ surface, resolutions, audio: isAudioScoped(surface) });
  patchSurface(surface, { filter: filter as any });
}

// Cross-tab override warning: when All is non-none, HTTP-surface tabs
// are functionally overridden by the All rule (since first-match-wins
// + All has no filter and is typically evaluated first).
const allActive = computed(() => allRule.value && allRule.value.type !== 'none');
function showOverrideWarning(surface: Surface): boolean {
  if (surface === 'all' || surface === 'transport') return false;
  return !!allActive.value;
}

// Slider ranges differ per surface.
function freqMax(surface: Surface): number {
  if (surface === 'transport') return 60;
  return 30;
}
function consMax(surface: Surface): number {
  return 10;
}
function freqLabel(surface: Surface): string {
  if (surface === 'transport' && getMode(surface) === 'failures_per_seconds') return 'Frequency (secs)';
  if (surface !== 'transport' && getMode(surface) === 'failures_per_seconds') return 'Frequency (secs)';
  return 'Frequency';
}
function consLabel(surface: Surface): string {
  if (surface === 'transport') return 'Consecutive (secs)';
  return 'Consecutive';
}
</script>

<template>
  <div v-if="player" class="fault-rules">
    <div class="tabs">
      <button
        v-for="s in SURFACES"
        :key="s.key"
        :class="{ active: activeTab === s.key }"
        @click="activeTab = s.key"
      >
        {{ s.label }}
        <span
          v-if="getType(s.key) !== 'none'"
          class="dot"
          :title="`Active: ${getType(s.key)}`"
        />
      </button>
    </div>

    <div class="panel">
      <p v-if="activeTab === 'all'" class="note">
        Faults configured here apply to <strong>every</strong> request. They
        override per-surface rules on the same player when both would match.
      </p>
      <p v-else-if="showOverrideWarning(activeTab)" class="warning">
        ⚠️ The <strong>All</strong> tab has an active fault — it will fire
        before this surface-specific rule on most requests.
      </p>

      <div class="row">
        <label>Failure Type</label>
        <div class="radio-group">
          <label v-for="t in typeChoicesFor(activeTab)" :key="t.value">
            <input
              type="radio"
              :name="`fault-type-${activeTab}-${playerId}`"
              :value="t.value"
              :checked="getType(activeTab) === t.value"
              @change="onTypeChange(activeTab, $event)"
            />
            {{ t.label }}
          </label>
        </div>
      </div>

      <!-- Scope row: legacy "All / Audio / per-variant" group on All /
           Segment / Manifest tabs (Master has no scope). Persists via
           filter.variant.resolutions + filter.request_kind = audio_*. -->
      <div
        v-if="activeTab !== 'transport' && activeTab !== 'master_manifest'"
        class="row scope"
      >
        <label>Scope</label>
        <div class="scope-checks">
          <label class="scope-check all">
            <input
              type="checkbox"
              :checked="isAllChecked(activeTab as any)"
              @change="toggleAll(activeTab as any, ($event.target as HTMLInputElement).checked)"
            />
            <span>All</span>
          </label>
          <label class="scope-check audio">
            <input
              type="checkbox"
              :checked="isAudioScoped(activeTab as any)"
              @change="toggleAudio(activeTab as any, ($event.target as HTMLInputElement).checked)"
            />
            <span>Audio</span>
          </label>
          <label v-for="v in manifestVariants" :key="v.url" class="scope-check">
            <input
              type="checkbox"
              :checked="isResolutionInScope(activeTab as any, v.resolution)"
              @change="toggleScope(activeTab as any, v.resolution, ($event.target as HTMLInputElement).checked)"
            />
            <span>{{ v.resolution }} · {{ Math.round(v.bandwidth / 1000) }} kbps</span>
          </label>
          <span v-if="!manifestVariants.length" class="muted">
            Play content once to populate variant list.
          </span>
        </div>
      </div>

      <div class="row">
        <label :for="`mode-${activeTab}`">Mode</label>
        <select
          :id="`mode-${activeTab}`"
          :value="getMode(activeTab)"
          @change="onModeChange(activeTab, $event)"
        >
          <option
            v-for="m in (activeTab === 'transport' ? TRANSPORT_FAULT_MODES : FAULT_MODES)"
            :key="m.value"
            :value="m.value"
          >
            {{ m.label }}
          </option>
        </select>
      </div>

      <!-- Legacy order: Consecutive THEN Frequency. -->
      <div class="row">
        <label :for="`cons-${activeTab}`">{{ consLabel(activeTab) }}</label>
        <input
          :id="`cons-${activeTab}`"
          type="range"
          min="0"
          :max="consMax(activeTab)"
          step="1"
          :value="getCons(activeTab)"
          @input="onConsInput(activeTab, $event)"
        />
        <span class="val">{{ getCons(activeTab) }}</span>
      </div>

      <div class="row">
        <label :for="`freq-${activeTab}`">{{ freqLabel(activeTab) }}</label>
        <input
          :id="`freq-${activeTab}`"
          type="range"
          min="0"
          :max="freqMax(activeTab)"
          step="1"
          :value="getFreq(activeTab)"
          @input="onFreqInput(activeTab, $event)"
        />
        <span class="val">{{ getFreq(activeTab) }}</span>
      </div>

      <!-- Transport-only readouts: live State + Fault Counters tile. -->
      <div v-if="activeTab === 'transport'" class="row tiles">
        <label>State</label>
        <div class="tile-row">
          <div class="tile">
            <span class="lbl">State</span>
            <span class="vl" :class="{ active: getType('transport') !== 'none' }">
              {{ getType('transport') === 'none' ? 'Idle' : 'Active' }}
            </span>
          </div>
          <div class="tile">
            <span class="lbl">Drop pkts</span>
            <span class="vl">{{ faultCounters?.transport_drop_pkts ?? 0 }}</span>
          </div>
          <div class="tile">
            <span class="lbl">Reject pkts</span>
            <span class="vl">{{ faultCounters?.transport_reject_pkts ?? 0 }}</span>
          </div>
        </div>
      </div>

    </div>
  </div>
</template>

<style scoped>
.fault-rules { display: grid; gap: 12px; }

.tabs {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  border-bottom: 1px solid #e5e7eb;
}
.tabs button {
  background: transparent;
  border: 0;
  padding: 8px 14px;
  font-size: 13px;
  font-weight: 500;
  color: #6b7280;
  cursor: pointer;
  border-bottom: 2px solid transparent;
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.tabs button:hover { color: #111827; }
.tabs button.active {
  color: #2563eb;
  border-bottom-color: #2563eb;
}
.dot {
  display: inline-block;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: #f97316;
}

.panel { display: grid; gap: 12px; padding: 8px 0; }

.note {
  font-size: 12px;
  background: #f0f9ff;
  border: 1px solid #bae6fd;
  color: #075985;
  padding: 8px 10px;
  border-radius: 6px;
  margin: 0;
}
.warning {
  font-size: 12px;
  background: #fef3c7;
  border: 1px solid #fcd34d;
  color: #92400e;
  padding: 8px 10px;
  border-radius: 6px;
  margin: 0;
}

.row {
  display: grid;
  grid-template-columns: 120px 1fr 60px;
  gap: 12px;
  align-items: center;
}
.row > label {
  font-size: 13px;
  font-weight: 500;
  color: #374151;
}

.radio-group {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  font-size: 12px;
  color: #374151;
}
.radio-group label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
}

.row select {
  font-size: 13px;
  padding: 4px 8px;
  border: 1px solid #d1d5db;
  border-radius: 4px;
  background: #fff;
}
.row input[type='range'] {
  width: 100%;
  accent-color: #2563eb;
}
.val {
  font-family: ui-monospace, monospace;
  font-size: 13px;
  color: #111827;
  text-align: right;
}

.scope { grid-template-columns: 120px 1fr; }
.scope-checks {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 14px;
}
.scope-check {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: #374151;
  cursor: pointer;
}
.scope-check.all {
  font-weight: 700;
  padding-right: 6px;
  border-right: 1px solid #e5e7eb;
}
.scope-check.audio {
  font-style: italic;
  padding-right: 6px;
  border-right: 1px solid #e5e7eb;
}
.muted {
  font-size: 12px;
  color: #9ca3af;
  font-style: italic;
}

.tiles { grid-template-columns: 120px 1fr; }
.tile-row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.tile {
  background: #f8f9fa;
  border: 1px solid #e8eaed;
  border-radius: 6px;
  padding: 6px 12px;
  min-width: 110px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.tile .lbl {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: #5f6368;
}
.tile .vl {
  font-size: 14px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.tile .vl.active { color: #d93025; }

</style>
