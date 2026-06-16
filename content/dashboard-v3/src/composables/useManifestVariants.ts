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
import type { Stream } from './useSessionTimeSeries';
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
    const flat = parseManifestVariants((p as any)?.raw_session?.manifest_variants);
    if (flat.length) return flat as ManifestVariant[];
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

/** Coerce a raw `manifest_variants` value into `VariantLite[]`. The live
 *  PlayerRecord carries it as an array; the CH long-tail column serialises it
 *  as a JSON string. Returns `[]` for anything unusable. */
export function parseManifestVariants(value: unknown): VariantLite[] {
  let v = value;
  if (typeof v === 'string') {
    try { v = JSON.parse(v); } catch { return []; }
  }
  return Array.isArray(v) ? (v as VariantLite[]) : [];
}

/**
 * Most-recent non-empty `manifest_variants` value from a charts_minimal
 * events stream, scanning newest row first. The per-row `manifest_variants`
 * column is part of the charts_minimal projection both the live/active
 * stream and each compare-mode sibling stream carry, so reading it here
 * lets a chart build a per-session variant ladder from whichever stream it
 * holds — the active session's own (single-session / self) or a sibling's
 * (compare-mode overlay, issue #812).
 *
 * Reads `stream.version.value` so a caller invoking this inside a `computed`
 * re-derives when the stream ingests new rows. Returns `[]` for a null
 * stream or one with no usable manifest yet (e.g. pre-manifest heartbeats).
 */
export function latestManifestVariants(
  stream: Stream<Record<string, unknown>> | null | undefined,
): VariantLite[] {
  if (!stream) return [];
  void stream.version.value;
  const rows = stream.inRange(0, Number.MAX_SAFE_INTEGER);
  for (let i = rows.length - 1; i >= 0; i--) {
    const parsed = parseManifestVariants((rows[i] as Record<string, unknown>).manifest_variants);
    if (parsed.length) return parsed;
  }
  return [];
}

/**
 * Map a player's DECODED frame size (`player_metrics.video_resolution`, e.g.
 * "640x360") to the published peak BANDWIDTH (Mbps) of the ladder rung it
 * corresponds to — the "Displayed Variant" line on the bandwidth chart.
 *
 * Matched by nearest frame HEIGHT, not exact "WxH" and not by bitrate: the
 * decoded size legitimately differs from the manifest RESOLUTION attribute
 * (coded vs display, mod-16 padding, PAR / clean aperture, packager quirks),
 * and `video_bitrate` is indicatedBitrate — the FETCHED rung, which leads the
 * displayed one by the buffer during switches. This project's ladders have one
 * rung per height. Returns null when there's no usable ladder or resolution.
 */
export function displayedVariantPeakMbps(
  variants: ReadonlyArray<VariantLite> | null | undefined,
  videoResolution: string | null | undefined,
): number | null {
  const heightOf = (res?: string | null): number | null => {
    if (!res) return null;
    const m = /(\d+)\s*[x×]\s*(\d+)/i.exec(res);
    return m ? Number(m[2]) : null;
  };
  const h = heightOf(videoResolution);
  if (h == null || !variants || variants.length === 0) return null;
  let best: number | null = null;
  let bestDelta = Infinity;
  for (const v of variants) {
    const rh = heightOf(v?.resolution);
    const peak = Number(v?.bandwidth ?? 0) / 1_000_000;
    if (rh == null || !Number.isFinite(peak) || peak <= 0) continue;
    const delta = Math.abs(rh - h);
    if (delta < bestDelta) { bestDelta = delta; best = peak; }
  }
  return best;
}
