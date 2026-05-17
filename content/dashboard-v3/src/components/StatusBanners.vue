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

const shapingBanner = computed<string | null>(() => {
  // Only flag when the server actively reported the shaper is off.
  // A fetch failure most often means the v2 proxy isn't deployed —
  // not the same problem; don't spam a banner.
  if (!nftablesInfo.value) return null;
  if (nftablesInfo.value.status === 'enabled') return null;
  const platform = nftablesInfo.value.platform || 'unknown';
  const reason = nftablesInfo.value.reason || 'Traffic shaping is unavailable.';
  return `Network shaping disabled (${platform}): ${reason}`;
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
