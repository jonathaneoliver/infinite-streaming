/**
 * useChatSettings — localStorage-backed settings for the AI chat
 * panel (#497). Singleton ref so every chat panel on the page sees
 * the same state.
 *
 * Storage shape (v2, 2026-05-25): keys are kept per-profile so
 * switching between Anthropic / HuggingFace / OpenAI / etc. doesn't
 * lose the previously-pasted key for the inactive provider.
 *
 *   {
 *     profile: "anthropic-claude",          // currently-active profile
 *     model:   "claude-sonnet-4-6",
 *     baseUrlOverride: "",                  // per-user upstream override
 *     keys: {                               // one slot per profile, persists
 *       "anthropic-claude": "sk-ant-...",   // independently when profile
 *       "huggingface":     "hf_...",        // is switched
 *       "openai":          "sk-proj-..."
 *     }
 *   }
 *
 * The public Ref<ChatSettings> exposed by useChatSettings() preserves
 * the v1 shape ({profile, model, apiKey, baseUrlOverride}) so callers
 * (useChat, ChatPanel, ChatSettings.vue) don't need to change.
 * Reading settings.value.apiKey returns the key for the active
 * profile; updating it writes to keys[profile] under the hood.
 *
 * BYO key model: keys live in localStorage only. The forwarder
 * receives them on each chat request and never stores them.
 */

import { computed, ref, watch } from 'vue';
import type { ChatSettings } from '@/types/chat';

const STORAGE_KEY = 'isLLMChatSettings';

interface StorageV2 {
  profile: string;
  model: string;
  baseUrlOverride: string;
  keys: Record<string, string>; // profile name → API key
}

const DEFAULT_STORAGE: StorageV2 = {
  profile: '',
  model: '',
  baseUrlOverride: '',
  keys: {},
};

/**
 * readStored — load + migrate from disk. Handles v1 (single apiKey)
 * → v2 (keys map) transparently: a v1 blob with a non-empty apiKey
 * is migrated into keys[profile]. v1 blobs with profile=''
 * (never-configured) get an empty keys map, losing nothing.
 */
function readStored(): StorageV2 {
  if (typeof localStorage === 'undefined') return { ...DEFAULT_STORAGE, keys: {} };
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT_STORAGE, keys: {} };
    const parsed = JSON.parse(raw);

    // Pick whichever shape is present. v2 has `keys` object.
    const keys: Record<string, string> = {};
    if (parsed.keys && typeof parsed.keys === 'object') {
      for (const [k, v] of Object.entries(parsed.keys)) {
        if (typeof k === 'string' && typeof v === 'string') keys[k] = v;
      }
    }
    // v1 migration: hoist single apiKey into keys[profile] (if both
    // present and profile not already in keys).
    const v1Key = typeof parsed.apiKey === 'string' ? parsed.apiKey : '';
    const profile = typeof parsed.profile === 'string' ? parsed.profile : '';
    if (v1Key && profile && !keys[profile]) {
      keys[profile] = v1Key;
    }
    return {
      profile,
      model: typeof parsed.model === 'string' ? parsed.model : '',
      baseUrlOverride: typeof parsed.baseUrlOverride === 'string' ? parsed.baseUrlOverride : '',
      keys,
    };
  } catch {
    return { ...DEFAULT_STORAGE, keys: {} };
  }
}

// Internal source of truth — singleton. Public API derives from this.
const storage = ref<StorageV2>(readStored());

// Persist on every mutation. Writes v2 shape only; v1 readers (none
// exist) would just see an empty apiKey field — acceptable since v1
// hasn't existed since this same release.
watch(storage, (v) => {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(v));
  } catch {
    // Quota exceeded — silent skip; in-memory state still works
    // for this session.
  }
}, { deep: true });

/**
 * Public ChatSettings view — derives apiKey from keys[active profile]
 * so existing consumers don't need to learn about the map.
 */
const publicSettings = computed<ChatSettings>(() => ({
  profile:         storage.value.profile,
  model:           storage.value.model,
  baseUrlOverride: storage.value.baseUrlOverride,
  apiKey:          storage.value.keys[storage.value.profile] ?? '',
}));

export function useChatSettings() {
  const isConfigured = computed(() =>
    storage.value.profile !== '' && storage.value.model !== ''
  );

  /**
   * update — partial mutation. Writes apiKey to keys[active profile]
   * if profile is set; ignores apiKey writes when profile is empty
   * (would lose the key on the next profile-set anyway).
   */
  function update(patch: Partial<ChatSettings>) {
    const next: StorageV2 = {
      profile:         patch.profile         ?? storage.value.profile,
      model:           patch.model           ?? storage.value.model,
      baseUrlOverride: patch.baseUrlOverride ?? storage.value.baseUrlOverride,
      keys:            { ...storage.value.keys },
    };
    if (patch.apiKey !== undefined && next.profile) {
      next.keys[next.profile] = patch.apiKey;
    }
    storage.value = next;
  }

  /** Clear the key for the ACTIVE profile only. Other profiles' keys remain. */
  function clearKey() {
    if (!storage.value.profile) return;
    const nextKeys = { ...storage.value.keys };
    delete nextKeys[storage.value.profile];
    storage.value = { ...storage.value, keys: nextKeys };
  }

  /** Map of profile name → "has a stored key?" — for settings UI. */
  const configuredProfiles = computed<Record<string, boolean>>(() => {
    const out: Record<string, boolean> = {};
    for (const [k, v] of Object.entries(storage.value.keys)) {
      out[k] = !!v;
    }
    return out;
  });

  return {
    settings: publicSettings,
    isConfigured,
    update,
    clearKey,
    configuredProfiles,
  };
}
