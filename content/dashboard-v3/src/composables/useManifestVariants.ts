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
    const typed = player.value?.current_play?.manifest?.variants;
    if (Array.isArray(typed) && typed.length) return typed;
    const flat = ((player.value as any)?.raw_session?.manifest_variants) as
      | { url: string; bandwidth: number; resolution: string }[]
      | undefined;
    if (Array.isArray(flat)) return flat as ManifestVariant[];
    return [];
  });

  return { variants };
}
