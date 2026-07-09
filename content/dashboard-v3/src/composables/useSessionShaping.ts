/**
 * useSessionShaping — the per-session view of #910's shaping-mode state,
 * shared by the ShapingModeControl (the tristate), ShapeSliders (rate/delay/
 * loss gating), FaultRules (transport-tab gating) and NetworkShapingPattern.
 *
 * One place owns the whole model so the four consumers can't drift:
 *
 *   - `forcedMode`  — the per-SESSION override ("" | "http-only"), read off the
 *     raw session (getPlayer requests ?include=raw). This is the A/B
 *     instrument: force ONE session degraded while the host + every other
 *     session stay on the kernel path.
 *   - `effMode`     — the mode that actually applies to this session: a forced
 *     degrade overrides the host's own capability ("kernel" | "http-only").
 *   - `controlAvailable(c)` / `badgeFor(c)` — per-control (rate/delay/loss/
 *     transport) "is the kernel backing this?" + the disabled-badge text.
 *
 * Visibility (the operator never sees a pointless choice):
 *   - `showControl`      — render the tristate only when it's meaningful:
 *     developer mode (the A/B instrument, on any box) OR the host genuinely
 *     can't kernel-shape (then it's the fallback picker). On a kernel-capable
 *     host in normal use it's HIDDEN — kernel is just used.
 *   - `kernelSelectable` — the Kernel option is only pickable when the host can
 *     actually do it; on a NET_ADMIN-less host it shows disabled.
 */
import { computed, ref, type Ref } from 'vue';
import { useQueryClient } from '@tanstack/vue-query';
import { usePlayer } from '@/composables/usePlayer';
import { useShapingCapabilities } from '@/composables/useBaselineRate';

// id === '' is the kernel (full-shaping) mode; the API takes 'off' for it.
// The only degraded mode is http-only: no network shaping, HTTP faults still
// fire. (A userspace-approximation mode was considered and dropped — a proxy
// above TCP can't faithfully reproduce rate/delay/loss.)
export const SHAPING_MODES: Array<{ id: string; label: string }> = [
  { id: '',          label: 'Kernel' },
  { id: 'http-only', label: 'Faults-only' },
];

export function useSessionShaping(playerId: Ref<string>) {
  const { player } = usePlayer(playerId);
  const { shaping } = useShapingCapabilities();
  const qc = useQueryClient();

  // `?developer=1` — the shared convention (SessionDetails, ShellLayout, Grid).
  const developerMode = computed(
    () => new URLSearchParams(window.location.search).get('developer') === '1',
  );

  // Host-level: can this box do full kernel shaping at all? All four controls
  // must probe available AND the mode must not be env-forced-degraded.
  const hostCanKernel = computed(() => {
    const s = shaping.value;
    return !s.forced && s.rate && s.delay && s.loss && s.transport_fault;
  });

  const forcedMode = computed<string>(
    () => ((player.value as any)?.raw_session?.shaping_forced_mode as string) || '',
  );

  // A forced degrade overrides the host mode; otherwise inherit the host's.
  const effMode = computed(() => forcedMode.value || shaping.value.mode || 'kernel');
  const degraded = computed(() => effMode.value !== 'kernel');

  // The segmented control's highlighted button ('' for kernel).
  const activeId = computed(() => (effMode.value === 'kernel' ? '' : effMode.value));

  const showControl = computed(() => developerMode.value || !hostCanKernel.value);
  const kernelSelectable = computed(() => hostCanKernel.value);

  const settingMode = ref(false);
  const modeError = ref('');

  // Drives the per-session degrade endpoint (by player_id) and refreshes the
  // player so raw_session.shaping_forced_mode + all gating recompute.
  async function setShapingMode(mode: string) {
    if (settingMode.value || mode === forcedMode.value) return;
    // Can't pick the kernel path on a host that can't do it.
    if (mode === '' && !kernelSelectable.value) return;
    settingMode.value = true;
    modeError.value = '';
    try {
      const res = await fetch('/api/nftables/shaping-mode', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ player_id: playerId.value, mode: mode || 'off' }),
      });
      if (!res.ok) {
        modeError.value =
          res.status === 404
            ? 'No active proxy session for this player — start playback first.'
            : `Couldn't set shaping mode (HTTP ${res.status}).`;
      }
    } catch (e) {
      modeError.value = `Couldn't set shaping mode: ${e instanceof Error ? e.message : String(e)}`;
    } finally {
      await qc.invalidateQueries({ queryKey: ['player', playerId.value] });
      settingMode.value = false;
    }
  }

  return {
    developerMode,
    hostCanKernel,
    forcedMode,
    effMode,
    degraded,
    activeId,
    showControl,
    kernelSelectable,
    settingMode,
    modeError,
    setShapingMode,
    SHAPING_MODES,
  };
}
