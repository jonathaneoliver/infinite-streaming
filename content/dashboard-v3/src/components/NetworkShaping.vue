<script setup lang="ts">
/**
 * NetworkShaping.vue — the body of the "Network Shaping" fold.
 *
 * In a degraded (Faults-only) session there is NO kernel shaping, so the whole
 * fold collapses to a single "not available" message — no sliders, no profiles,
 * no pattern editor (#910). In kernel mode it renders the normal controls:
 * the rate/delay/loss sliders + link profiles, then the Pattern engine.
 *
 * Wrapping both pages' identical `ShapeSliders + Pattern` block here keeps the
 * degraded/kernel decision in ONE place (Testing.vue + TestingSession.vue both
 * mount this).
 */
import { toRef } from 'vue';
import ShapeSliders from '@/components/ShapeSliders.vue';
import NetworkShapingPattern from '@/components/NetworkShapingPattern.vue';
import { useSessionShaping } from '@/composables/useSessionShaping';

const props = defineProps<{ playerId: string }>();
const { degraded, effMode } = useSessionShaping(toRef(props, 'playerId'));
</script>

<template>
  <div v-if="degraded" class="ns-unavailable">
    Network shaping is unavailable in <strong>{{ effMode }}</strong> mode — this
    session has no kernel shaping (rate / delay / loss / pattern). This feature
    requires NET_ADMIN access.
  </div>
  <template v-else>
    <ShapeSliders :player-id="playerId" />
    <h3 class="subhead">Pattern</h3>
    <NetworkShapingPattern :player-id="playerId" />
  </template>
</template>

<style scoped>
.ns-unavailable {
  font-size: 13px;
  background: #fef2f2;
  border: 1px solid #fecaca;
  color: #991b1b;
  padding: 12px 14px;
  border-radius: 8px;
  line-height: 1.5;
}
.subhead {
  margin: 20px 0 12px 0;
  font-size: 12px;
  font-weight: 600;
  color: #6b7280;
  text-transform: uppercase;
  letter-spacing: 0.5px;
}
</style>
