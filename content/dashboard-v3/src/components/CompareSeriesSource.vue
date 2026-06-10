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
 * playId is null so the sibling follows its own latest play and rotates
 * with it — matching the live-comparison use case (two devices under
 * test at once). The default 10-minute backfill + live tail aligns with
 * the live edge the primary chart tracks. (The archive path — locking a
 * sibling to a specific historical play_id — is a follow-up; this
 * component takes a playId prop so that wiring is a one-line change.)
 */
import { computed, onMounted, onUnmounted } from 'vue';
import { useSessionTimeSeries, type Stream } from '@/composables/useSessionTimeSeries';

const props = defineProps<{ playerId: string; playId?: string | null }>();
const emit = defineEmits<{
  (e: 'register', playerId: string, stream: Stream<Record<string, unknown>>): void;
  (e: 'unregister', playerId: string): void;
}>();

const playerIdRef = computed(() => props.playerId);
const playIdRef = computed<string | null>(() => props.playId ?? null);

const ts = useSessionTimeSeries(playerIdRef, playIdRef, {
  streams: ['events'],
  bundles: ['charts_minimal'],
});

onMounted(() => emit('register', props.playerId, ts.events));
onUnmounted(() => emit('unregister', props.playerId));
</script>

<template>
  <!-- renderless: this component only owns an SSE subscription -->
</template>
