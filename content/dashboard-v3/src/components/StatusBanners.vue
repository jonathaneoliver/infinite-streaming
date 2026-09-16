<script setup lang="ts">
/**
 * StatusBanners.vue — surface platform-level warnings above the
 * panels. Matches the legacy `#networkShapingBanner` and
 * `#streamAccessBanner` strips.
 *
 *  - Network shaping banner: lit when `/api/nftables/capabilities`
 *    reports `status !== "enabled"` (kernel module missing, running
 *    as non-root in dev, etc.). Tells the operator that loss/delay/
 *    rate sliders will be no-ops on this host.
 *
 *  - Stream access banner: lit by a prop, used by the parent when a
 *    play start was 4xx'd because the WAN deploy enforces a 2-stream
 *    cap for unauthenticated callers.
 */
import { computed, onMounted, ref } from 'vue';

const props = defineProps<{
  streamAccessMessage?: string | null;
}>();

interface NftablesInfo {
  status?: string;
  platform?: string;
  reason?: string;
  // #910 capability-probe fields.
  mode?: string; // "kernel" | "http-only"
  forced?: boolean;
  rate?: boolean;
  delay?: boolean;
  loss?: boolean;
  transport_fault?: boolean;
}

const nftablesInfo = ref<NftablesInfo | null>(null);
const fetchFailed = ref(false);

async function fetchCapabilities() {
  try {
    const r = await fetch('/api/nftables/capabilities');
    if (!r.ok) { fetchFailed.value = true; return; }
    nftablesInfo.value = await r.json();
  } catch {
    fetchFailed.value = true;
  }
}

onMounted(fetchCapabilities);

// #910: a session is degraded whenever ANY network-shaping control is
// unavailable — full http-only (all off) OR partial kernel (e.g. tc works but
// nftables transport faults don't). We must surface it either way so a
// configured cap is never mistaken for active.
const shapingBanner = computed<string | null>(() => {
  const info = nftablesInfo.value;
  // A fetch failure most often means the v2 proxy isn't deployed — not the
  // same problem; don't spam a banner.
  if (!info) return null;
  const controls: Array<[string, boolean | undefined]> = [
    ['rate', info.rate],
    ['delay', info.delay],
    ['loss', info.loss],
    ['transport faults', info.transport_fault],
  ];
  // Older proxies (pre-#910) don't send the per-control booleans; fall back
  // to the coarse status flag so those deploys still get the basic banner.
  const hasFields = controls.some(([, v]) => typeof v === 'boolean');
  const unavailable = controls.filter(([, v]) => v === false).map(([n]) => n);
  const degraded = hasFields ? unavailable.length > 0 : info.status !== 'enabled';
  if (!degraded) return null;

  const mode = info.mode || (info.status === 'enabled' ? 'kernel' : 'http-only');
  const forcedNote = info.forced ? ' — forced for testing' : '';
  if (hasFields && unavailable.length > 0) {
    return `Shaping mode: ${mode}${forcedNote}. Unavailable on this host: ${unavailable.join(', ')}. These controls are shown disabled — a configured value will NOT take effect.`;
  }
  const platform = info.platform || 'unknown';
  const reason = info.reason || 'Traffic shaping is unavailable.';
  return `Network shaping disabled (${platform})${forcedNote}: ${reason}`;
});
</script>

<template>
  <div class="banners">
    <div v-if="shapingBanner" class="banner banner-warn" role="alert">
      ⚠️ {{ shapingBanner }}
    </div>
    <div v-if="props.streamAccessMessage" class="banner banner-error" role="alert">
      🚫 {{ props.streamAccessMessage }}
    </div>
  </div>
</template>

<style scoped>
.banners {
  display: grid;
  gap: 8px;
}
.banners:empty { display: none; }

.banner {
  padding: 10px 14px;
  border-radius: 6px;
  font-size: 13px;
  line-height: 1.4;
}
.banner-warn {
  background: #fef7e0;
  border: 1px solid #fcd34d;
  color: #92400e;
}
.banner-error {
  background: #fce8e6;
  border: 1px solid #fca5a5;
  color: #991b1b;
}
</style>
