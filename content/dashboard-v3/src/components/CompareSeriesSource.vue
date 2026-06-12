<script setup lang="ts">
/**
 * CompareSeriesSource.vue — renderless per-sibling time-series
 * subscriber for the compare-charts overlay (issue #579).
 *
 * Vue composables can't be called in a dynamic loop, so to subscribe a
 * variable number of grouped siblings we render one of these per sibling
 * (v-for keyed by playerId). Each instance calls useSessionTimeSeries
 * for its own player and registers the resulting `events` Stream with
 * the parent via the register/unregister emits. When the sibling leaves
 * the group the v-for drops this component, onScopeDispose inside
 * useSessionTimeSeries closes the SSE, and we emit `unregister` so the
 * parent stops overlaying its (now-stale) datasets.
 *
 * Scope is deliberately minimal — only the `events` stream with the
 * charts_minimal bundle (the same projection BandwidthChart/BufferChart
 * accessors read). No network/control/avmetrics: the overlay only plots
 * rate + buffer lines, not the sibling's lanes or per-segment markers.
 *
 * Live use (issue #579): playId is null so the sibling follows its own
 * latest play and rotates with it (two devices under test at once); the
 * default 10-minute backfill + live tail aligns with the live edge the
 * primary chart tracks.
 *
 * Archive use (issue #736): pass a locked `playId` AND the active play's
 * `fromMs`/`toMs` window. The playId pins the sibling to that historical
 * play; the window is REQUIRED because useSessionTimeSeries otherwise
 * backfills only the last 10 minutes — useless for a play hours/days old.
 * Grouped fleet plays share a wall-clock window, so the active play's
 * bounds cover every sibling.
 */
import { computed, onMounted, onUnmounted } from 'vue';
import { useSessionTimeSeries, type Stream } from '@/composables/useSessionTimeSeries';

const props = defineProps<{
  playerId: string;
  playId?: string | null;
  fromMs?: number | null;
  toMs?: number | null;
}>();
const emit = defineEmits<{
  (e: 'register', playerId: string, stream: Stream<Record<string, unknown>>): void;
  (e: 'unregister', playerId: string): void;
}>();

const playerIdRef = computed(() => props.playerId);
const playIdRef = computed<string | null>(() => props.playId ?? null);
const fromMsRef = computed<number | null>(() => props.fromMs ?? null);
const toMsRef = computed<number | null>(() => props.toMs ?? null);

const ts = useSessionTimeSeries(playerIdRef, playIdRef, {
  streams: ['events'],
  bundles: ['charts_minimal'],
  fromMs: fromMsRef,
  toMs: toMsRef,
});

onMounted(() => emit('register', props.playerId, ts.events));
onUnmounted(() => emit('unregister', props.playerId));
</script>

<template>
  <!-- renderless: this component only owns an SSE subscription -->
</template>
