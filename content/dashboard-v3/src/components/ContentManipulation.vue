<script setup lang="ts">
/**
 * ContentManipulation.vue — master-playlist mutation controls.
 * Three checkboxes (strip-codecs / strip-average-bandwidth /
 * overstate-bandwidth) + a live-offset radio group + an
 * allowed-variants checkbox list driven by the manifest's variants.
 */
import { computed, toRef } from 'vue';
import { usePlayer } from '@/composables/usePlayer';
import { useManifestVariants } from '@/composables/useManifestVariants';
import type { ContentManipulation as Content } from '@/repo/v2-repo';

const props = defineProps<{ playerId: string }>();
const { player, setContent } = usePlayer(toRef(props, 'playerId'));

const content = computed(() => player.value?.content);
const { variants: rawVariants } = useManifestVariants(toRef(props, 'playerId'));
// Highest-bitrate-first, matching legacy `sortedPlaylists`.
const variants = computed(() => {
  return rawVariants.value.slice().sort((a, b) => (b.bandwidth ?? 0) - (a.bandwidth ?? 0));
});

const LIVE_OFFSET_CHOICES = [0, 6, 18, 24] as const;
type LiveOffset = (typeof LIVE_OFFSET_CHOICES)[number];

function onBoolChange(field: 'strip_codecs' | 'strip_average_bandwidth' | 'strip_resolution' | 'overstate_bandwidth', e: Event) {
  const v = (e.target as HTMLInputElement).checked;
  setContent({ [field]: v } as Partial<Content>);
}

function onLiveOffsetChange(value: LiveOffset) {
  setContent({ live_offset: value } as Partial<Content>);
}

/** Empty allowed_variants list means "all allowed" — `allChecked = true`
 *  in that case (matches legacy renderContentVariantOptions). Otherwise
 *  every entry in `allowed_variants` is the explicit allow-list. */
const allowed = computed(() => new Set(content.value?.allowed_variants ?? []));
const isAllAllowed = computed(() => allowed.value.size === 0);

function isVariantChecked(url: string): boolean {
  if (isAllAllowed.value) return true;
  return allowed.value.has(url);
}

function onAllToggle(on: boolean) {
  if (on) {
    // All checked → clear the explicit list (empty = all allowed).
    setContent({ allowed_variants: [] } as Partial<Content>);
  } else {
    // All unchecked → mark an empty allow-list explicitly. Server-side
    // semantics: an empty array still means "all allowed", so we set
    // it to the impossible "(none)" sentinel string the legacy used.
    // Actually the cleanest v2 way is to list nothing — but that's
    // identical to all-allowed. Instead pin to a single sentinel
    // so the user can see they unticked everything; one more click
    // re-enables. Matches legacy "uncheck everything = no streams".
    setContent({ allowed_variants: ['__none__'] } as Partial<Content>);
  }
}

function onVariantToggle(url: string, checked: boolean) {
  // Starting state: if we're in "all allowed" mode, seed from every
  // variant so toggling one off narrows from there.
  const baseline = isAllAllowed.value
    ? new Set(variants.value.map((v) => v.url))
    : new Set(allowed.value);
  if (checked) baseline.add(url);
  else baseline.delete(url);
  // If every variant is checked again, collapse to the "all allowed"
  // representation (empty list).
  const allUrls = variants.value.map((v) => v.url);
  const isAll = allUrls.length > 0 && allUrls.every((u) => baseline.has(u));
  const next = isAll ? [] : Array.from(baseline);
  setContent({ allowed_variants: next } as Partial<Content>);
}

function heightOf(res?: string | null): string {
  if (!res) return '?';
  const m = res.match(/x(\d+)/i);
  return m ? `${m[1]}p` : res;
}
function variantLabel(v: { url: string; resolution?: string; bandwidth?: number }): string {
  const h = heightOf(v.resolution);
  const kbps = v.bandwidth ? `${Math.round(v.bandwidth / 1000)} kbps` : '';
  return [h, kbps].filter(Boolean).join(' / ');
}
</script>

<template>
  <div v-if="player" class="content-manip">
    <div class="bool-grid">
      <label class="bool-item">
        <input
          type="checkbox"
          :checked="content?.strip_codecs ?? false"
          @change="onBoolChange('strip_codecs', $event)"
        />
        <div class="bool-text">
          <div class="bool-title">Strip CODEC Information</div>
          <div class="bool-desc">Remove CODECS attributes from master playlist</div>
        </div>
      </label>
      <label class="bool-item">
        <input
          type="checkbox"
          :checked="content?.strip_average_bandwidth ?? false"
          @change="onBoolChange('strip_average_bandwidth', $event)"
        />
        <div class="bool-text">
          <div class="bool-title">Strip AVERAGE-BANDWIDTH</div>
          <div class="bool-desc">Remove AVERAGE-BANDWIDTH attribute from master playlist</div>
        </div>
      </label>
      <label class="bool-item">
        <input
          type="checkbox"
          :checked="content?.strip_resolution ?? false"
          @change="onBoolChange('strip_resolution', $event)"
        />
        <div class="bool-text">
          <div class="bool-title">Strip RESOLUTION</div>
          <div class="bool-desc">Remove RESOLUTION attribute from EXT-X-STREAM-INF. AVPlayer keeps playing but variant.video.size becomes empty (issue #486).</div>
        </div>
      </label>
      <label class="bool-item">
        <input
          type="checkbox"
          :checked="content?.overstate_bandwidth ?? false"
          @change="onBoolChange('overstate_bandwidth', $event)"
        />
        <div class="bool-text">
          <div class="bool-title">Overstate Bandwidth (+10%)</div>
          <div class="bool-desc">Increase BANDWIDTH and AVERAGE-BANDWIDTH attributes by 10%</div>
        </div>
      </label>
    </div>

    <div class="offset-row">
      <span class="label">Live offset</span>
      <label v-for="opt in LIVE_OFFSET_CHOICES" :key="opt">
        <input
          type="radio"
          :name="`content-live-offset-${playerId}`"
          :value="opt"
          :checked="(content?.live_offset ?? 0) === opt"
          @change="onLiveOffsetChange(opt)"
        />
        {{ opt === 0 ? 'None' : opt + 's' }}
      </label>
    </div>
    <p class="note">
      HLS-only. Forces the player to fall back further from the live edge by
      stripping the most-recent N seconds of segments from the manifest.
      Pairs well with `delay_ms` to surface live-offset-driven stalls.
    </p>

    <div class="variants">
      <span class="label">Allowed variants <span class="muted">(All checked = allow every variant)</span></span>
      <div v-if="!variants.length" class="muted">Play content once to populate variant list.</div>
      <div v-else class="variant-list">
        <label class="all">
          <input
            type="checkbox"
            :checked="isAllAllowed"
            @change="onAllToggle(($event.target as HTMLInputElement).checked)"
          />
          All
        </label>
        <label v-for="v in variants" :key="v.url">
          <input
            type="checkbox"
            :checked="isVariantChecked(v.url)"
            @change="onVariantToggle(v.url, ($event.target as HTMLInputElement).checked)"
          />
          {{ variantLabel(v) }}
        </label>
      </div>
    </div>
  </div>
</template>

<style scoped>
.content-manip {
  display: grid;
  gap: 14px;
}

.bool-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: 12px;
}
.bool-item {
  display: grid;
  grid-template-columns: 20px 1fr;
  gap: 10px;
  align-items: start;
  background: #f8f9fa;
  border: 1px solid #e8eaed;
  border-radius: 6px;
  padding: 10px 12px;
  cursor: pointer;
}
.bool-item input { margin-top: 2px; }
.bool-title {
  font-size: 13px;
  font-weight: 600;
  color: #202124;
}
.bool-desc {
  font-size: 11px;
  color: #5f6368;
  margin-top: 2px;
}

.note {
  font-size: 11px;
  background: #f0f9ff;
  border: 1px solid #bae6fd;
  color: #075985;
  padding: 6px 10px;
  border-radius: 6px;
  margin: 0;
  line-height: 1.4;
}

.offset-row {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  align-items: center;
  font-size: 13px;
  color: #374151;
}

.label {
  font-weight: 500;
}

.muted {
  color: #9ca3af;
  font-weight: 400;
}

.offset-row label,
.variant-list label {
  display: flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
}

.offset-row input,
.variant-list input {
  accent-color: #2563eb;
}

.variants {
  display: grid;
  gap: 6px;
}

.variant-list {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(180px, 1fr));
  gap: 6px;
  font-size: 13px;
  color: #374151;
}
.variant-list label.all {
  font-weight: 700;
}
</style>
