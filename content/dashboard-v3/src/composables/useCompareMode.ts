/**
 * useCompareMode(playerId) — shared "Compare Charts" toggle, keyed by
 * the active player UUID. Mirrors the useChartCoordination(playerId)
 * module-level Map pattern so the toggle rendered in GroupBanner and the
 * overlay consumed by SessionDisplay/BandwidthChart read the same
 * reactive flag without prop-drilling (issue #579).
 *
 * Compare mode overlays each grouped sibling session's rate/buffer
 * series onto the bandwidth + buffer charts — restoring the legacy
 * testing.html compare-mode the v3 cutover dropped. It only does
 * anything when the active player is in a group with ≥2 members; the
 * flag is just remembered per player so flipping it on a degenerate
 * (ungrouped) session is harmless and survives a later regrouping.
 *
 * State is intentionally NOT persisted to localStorage — compare mode is
 * a transient investigation gesture, and defaulting it off on reload
 * matches how the legacy checkbox behaved (unchecked on page load).
 */
import { isRef, reactive, ref, type Ref } from 'vue';

interface CompareModeState {
  enabled: boolean;
}

const states = new Map<string, CompareModeState>();

function ensureState(pid: string): CompareModeState {
  let s = states.get(pid);
  if (!s) {
    s = reactive<CompareModeState>({ enabled: false });
    states.set(pid, s);
  }
  return s;
}

export function useCompareMode(playerIdInput: string | Ref<string>) {
  const playerIdRef: Ref<string> = isRef(playerIdInput)
    ? playerIdInput
    : ref(playerIdInput);

  function cur(): CompareModeState {
    return ensureState(playerIdRef.value);
  }

  return {
    /** Reactive state object — read `state.enabled` inside a computed
     *  or template to track the toggle. */
    get state(): CompareModeState {
      return cur();
    },
    toggle() {
      const s = cur();
      s.enabled = !s.enabled;
    },
    setEnabled(v: boolean) {
      cur().enabled = v;
    },
  };
}
