/**
 * useChatSettings — localStorage-backed (profile, model, api_key)
 * persistence for the AI chat panel (#497).
 *
 * BYO key model: each browser keeps its own key in localStorage.
 * The forwarder never stores it; it's sent on each chat request.
 * The composable is a singleton so every chat panel on the page
 * sees the same settings.
 */

import { computed, ref, watch } from 'vue';
import type { ChatSettings } from '@/types/chat';

const STORAGE_KEY = 'isLLMChatSettings';

const DEFAULT: ChatSettings = {
  profile: '',
  model: '',
  apiKey: '',
};

function readStored(): ChatSettings {
  if (typeof localStorage === 'undefined') return { ...DEFAULT };
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return { ...DEFAULT };
    const parsed = JSON.parse(raw);
    return {
      profile: typeof parsed.profile === 'string' ? parsed.profile : '',
      model: typeof parsed.model === 'string' ? parsed.model : '',
      apiKey: typeof parsed.apiKey === 'string' ? parsed.apiKey : '',
    };
  } catch {
    return { ...DEFAULT };
  }
}

// Singleton ref — every useChatSettings() call returns the same state.
const settings = ref<ChatSettings>(readStored());

// Persist on every mutation.
watch(settings, (v) => {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(v));
  } catch {
    // Quota exceeded or denied — ignore silently; the user can still chat
    // this session, they just won't persist after reload.
  }
}, { deep: true });

export function useChatSettings() {
  const isConfigured = computed(() =>
    settings.value.profile !== '' && settings.value.model !== ''
  );
  function update(patch: Partial<ChatSettings>) {
    settings.value = { ...settings.value, ...patch };
  }
  function clearKey() {
    settings.value = { ...settings.value, apiKey: '' };
  }
  return {
    settings,
    isConfigured,
    update,
    clearKey,
  };
}
