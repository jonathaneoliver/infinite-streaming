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

const LIVE_OFFSET_CHOICES = [0, 2, 4, 6, 12, 18, 24, 30, 36, 42] as const;
type LiveOffset = (typeof LIVE_OFFSET_CHOICES)[number];

const VARIANT_ORDER_CHOICES = [
  { value: 'default', label: 'Default' },
  { value: 'ascending', label: 'Ascending' },
  { value: 'descending', label: 'Descending' },
  { value: 'first_4mbps', label: '4 Mbps first' },
] as const;
type VariantOrder = (typeof VARIANT_ORDER_CHOICES)[number]['value'];

const variantOrder = computed<VariantOrder>(() => content.value?.variant_order ?? 'default');

function onBoolChange(field: 'strip_codecs' | 'strip_average_bandwidth' | 'strip_resolution' | 'overstate_bandwidth', e: Event) {
  const v = (e.target as HTMLInputElement).checked;
  setContent({ [field]: v } as Partial<Content>);
}

function onLiveOffsetChange(value: LiveOffset) {
  setContent({ live_offset: value } as Partial<Content>);
}

function onVariantOrderChange(value: VariantOrder) {
  setContent({ variant_order: value } as Partial<Content>);
}

/** Empty allowed_variants list means "all allowed" — `allChecked = true`
 *  in that case (matches legacy renderContentVariantOptions). Otherwise
 *  every entry in `allowed_variants` is the explicit allow-list. */
const allowed = computed(() => new Set(content.value?.allowed_variants ?? []));
const isAllAllowed = computed(() => allowed.value.size === 0);

/** A variant is whitelisted if allowed_variants contains its served URI
 *  (`playlist_6s_360p.m3u8`) OR its resolution: full "640x360", bare height
 *  "360", or "360p". Mirrors the proxy's variantAllowed (go-proxy main.go) so a
 *  resolution-form keep-set — e.g. set by the characterization harness or the
 *  "Keep every other" → resolution path — is reflected here, not shown as
 *  "none selected". */
function variantMatches(v: { url?: string; resolution?: string }): boolean {
  if (v.url && allowed.value.has(v.url)) return true;
  const res = v.resolution;
  if (res) {
    if (allowed.value.has(res)) return true;
    const h = /x(\d+)/.exec(res)?.[1];
    if (h && (allowed.value.has(h) || allowed.value.has(`${h}p`))) return true;
  }
  return false;
}

function isVariantChecked(v: { url?: string; resolution?: string }): boolean {
  if (isAllAllowed.value) return true;
  return variantMatches(v);
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
  // Seed from the variants currently shown as checked (matched by url OR
  // resolution, via isVariantChecked) as URLs — so a resolution-form whitelist
  // (e.g. set by the harness) migrates cleanly to URL form on the first manual
  // toggle instead of producing a mixed url/resolution set.
  const baseline = new Set(variants.value.filter((v) => isVariantChecked(v)).map((v) => v.url));
  if (checked) baseline.add(url);
  else baseline.delete(url);
  // If every variant is checked again, collapse to the "all allowed"
  // representation (empty list).
  const allUrls = variants.value.map((v) => v.url);
  const isAll = allUrls.length > 0 && allUrls.every((u) => baseline.has(u));
  const next = isAll ? [] : Array.from(baseline);
  setContent({ allowed_variants: next } as Partial<Content>);
}

/** Adjacent BANDWIDTH ratios (ascending). A dense geometric ladder sits at
 *  ~1.41× (√2); the legacy 2× ladder is ~2.0×. Used to gate "Keep every other"
 *  so it only offers on this ladder or an equivalent (#762) — halving a 2×
 *  ladder would create ~4× gaps. */
const isGeometricLadder = computed(() => {
  const asc = variants.value
    .map((v) => v.bandwidth ?? 0)
    .filter((b) => b > 0)
    .sort((a, b) => a - b);
  if (asc.length < 5) return false;
  for (let i = 1; i < asc.length; i++) {
    if (asc[i] / asc[i - 1] > 1.7) return false; // a ~2× gap ⇒ not geometric
  }
  return true;
});

/** Thin to the "skip every other" 2× subset: on the bandwidth-sorted ladder
 *  keep indices 0,2,4,… plus the last, so floor AND ceiling are retained. On
 *  this geometric ladder that yields exactly the original 2× rungs. Sets the
 *  existing allowed_variants whitelist — the proxy already filters the master
 *  to it, so no backend change (#762). */
function keepEveryOther() {
  const asc = variants.value
    .slice()
    .sort((a, b) => (a.bandwidth ?? 0) - (b.bandwidth ?? 0));
  const n = asc.length;
  const keep = asc.filter((_, i) => i % 2 === 0 || i === n - 1).map((v) => v.url);
  setContent({ allowed_variants: keep } as Partial<Content>);
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
      HLS-only. Rewrites the manifest's <code>EXT-X-START:TIME-OFFSET</code>
      (join point) and <code>EXT-X-SERVER-CONTROL:HOLD-BACK</code> (target
      offset) to N seconds, on both the master <em>and</em> the variant
      playlists. Players honour the variant <code>HOLD-BACK</code> for their
      join offset (iOS / Android / web). Note the HLS spec requires
      <code>HOLD-BACK ≥ 3× the max segment duration</code> — below that AVPlayer
      rejects the playlist (<code>-12646</code>). It does NOT strip segments.
    </p>

    <div class="order-row" data-testid="content-variant-order">
      <span class="label">Variant order</span>
      <div class="segmented" role="radiogroup" aria-label="Master playlist variant order">
        <button
          v-for="opt in VARIANT_ORDER_CHOICES"
          :key="opt.value"
          type="button"
          role="radio"
          class="seg"
          :class="{ active: variantOrder === opt.value }"
          :aria-checked="variantOrder === opt.value"
          :data-testid="`content-variant-order-${opt.value}`"
          @click="onVariantOrderChange(opt.value)"
        >
          {{ opt.label }}
        </button>
      </div>
    </div>
    <p class="note">
      Re-sorts the master playlist's video variants by BANDWIDTH to probe how
      AVPlayer picks its initial variant (#682). <strong>4 Mbps first</strong>
      promotes the variant nearest 4 Mbps to first-listed. Audio/subtitle
      renditions are untouched. Takes effect on the next master fetch.
    </p>

    <div class="variants">
      <div class="variants-head">
        <span class="label">Allowed variants <span class="muted">(All checked = allow every variant)</span></span>
        <button
          type="button"
          class="thin-btn"
          data-testid="content-keep-every-other"
          :disabled="!isGeometricLadder"
          :title="isGeometricLadder
            ? 'Drop the ~1.41× fill rungs → keep the 2× subset (floor + ceiling retained)'
            : 'Only on a dense ~1.41× geometric ladder — this content isn’t one'"
          @click="keepEveryOther"
        >
          Keep every other (2×)
        </button>
      </div>
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
            :checked="isVariantChecked(v)"
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

.offset-row,
.order-row {
  display: flex;
  flex-wrap: wrap;
  gap: 16px;
  align-items: center;
  font-size: 13px;
  color: #374151;
}

.segmented {
  display: inline-flex;
  border: 1px solid #d1d5db;
  border-radius: 6px;
  overflow: hidden;
}
.seg {
  border: none;
  border-left: 1px solid #d1d5db;
  background: #fff;
  color: #374151;
  font-size: 13px;
  padding: 6px 12px;
  cursor: pointer;
}
.seg:first-child {
  border-left: none;
}
.seg.active {
  background: #2563eb;
  color: #fff;
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

.variants-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.thin-btn {
  border: 1px solid #d1d5db;
  background: #fff;
  color: #374151;
  font-size: 12px;
  padding: 5px 10px;
  border-radius: 6px;
  cursor: pointer;
}
.thin-btn:hover:not(:disabled) { background: #f3f4f6; }
.thin-btn:disabled { opacity: 0.5; cursor: not-allowed; }

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
