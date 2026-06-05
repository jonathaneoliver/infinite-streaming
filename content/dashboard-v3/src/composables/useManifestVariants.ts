/**
 * useManifestVariants(playerIdRef) — return the active play's manifest
 * variants for the given player, smoothing over a v2-projection gap.
 *
 * The v2 server-side projection (`current_play.manifest.variants`)
 * isn't fully populated yet — it returns 0 entries even when the
 * player has fetched the master playlist. The flat raw_session has
 * had `manifest_variants` populated since v1, so we fall back to that
 * shape and adapt it to the typed v2 ManifestVariant.
 *
 * Once translate.go starts producing `current_play.manifest.variants`,
 * the typed path takes precedence automatically (no callers change).
 */
import { computed, type Ref } from 'vue';
import { usePlayer } from './usePlayer';
import type { components } from '@/types/v2';

type ManifestVariant = components['schemas']['ManifestVariant'];

export function useManifestVariants(playerId: Ref<string> | string) {
  const idRef = typeof playerId === 'string'
    ? ({ value: playerId } as Ref<string>)
    : playerId;
  const { player } = usePlayer(idRef);

  const variants = computed<ManifestVariant[]>(() => {
    const p = player.value;
    const typed = p?.current_play?.manifest?.variants;
    if (Array.isArray(typed) && typed.length) return typed;
    let flat = (p as any)?.raw_session?.manifest_variants;
    if (typeof flat === 'string') {
      // raw_session may serialize the variant list as a JSON string
      // when coming through the CH long-tail column. Parse if so.
      try { flat = JSON.parse(flat); } catch { flat = undefined; }
    }
    if (Array.isArray(flat) && flat.length) return flat as ManifestVariant[];
    return [];
  });

  return { variants };
}

/** Minimal manifest-variant shape the snap helper needs. */
export interface VariantLite {
  bandwidth?: number;
  resolution?: string;
}

/**
 * Snap a player-reported bitrate (Mbps) to the manifest rung whose published
 * peak BANDWIDTH is closest, returning that rung's resolution + peak Mbps.
 *
 * The player's `video_bitrate` is AVPlayer's `indicatedBitrate` — a jittery
 * EWMA estimate that wobbles around the true rung (e.g. 29.6/29.9 for a
 * 29.86 Mbps 4K variant). Anywhere a reported bitrate must identify a
 * DISCRETE variant (the bandwidth chart's Fetching line, the timeline
 * VARIANT lane) we snap it here so one rung doesn't fragment into phantom
 * near-duplicates and jitter doesn't fire spurious up/down shifts. The raw
 * value is left intact at the source — this is a display-time derivation.
 *
 * Returns null when there are no usable variants (e.g. pre-manifest
 * heartbeats), so callers fall back to the raw value or skip. Issue #619.
 */
export function nearestVariantByBitrate(
  variants: ReadonlyArray<VariantLite> | null | undefined,
  reportedMbps: number,
): { resolution: string; peakMbps: number } | null {
  if (!variants || variants.length === 0 || !Number.isFinite(reportedMbps)) return null;
  let best: { resolution: string; peakMbps: number } | null = null;
  let bestDelta = Infinity;
  for (const v of variants) {
    const bw = Number(v?.bandwidth ?? 0);
    if (!Number.isFinite(bw) || bw <= 0) continue;
    const peakMbps = bw / 1_000_000;
    const delta = Math.abs(peakMbps - reportedMbps);
    if (delta < bestDelta) {
      bestDelta = delta;
      best = { resolution: String(v?.resolution ?? '').trim(), peakMbps };
    }
  }
  return best;
}
