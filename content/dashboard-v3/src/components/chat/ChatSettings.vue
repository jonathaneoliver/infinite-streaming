<script setup lang="ts">
/**
 * ChatSettings — modal panel where the user picks a profile + model
 * and pastes their API key. Persists via useChatSettings (singleton
 * localStorage).
 *
 * No "save" button — every change writes through immediately. The
 * Close button just dismisses the modal.
 */
import { computed, onMounted, ref, watch } from 'vue';
import { useChatSettings } from '@/composables/useChatSettings';
import { useLLMProfiles } from '@/composables/useLLMProfiles';

const emit = defineEmits<{ close: [] }>();

const { settings, update } = useChatSettings();
const { data: catalog, isLoading, error } = useLLMProfiles();

const showKey = ref(false);

const currentProfile = computed(() => {
  if (!catalog.value) return null;
  return catalog.value.templates.find(t => t.name === settings.value.profile) ?? null;
});

const availableModels = computed(() => currentProfile.value?.models ?? []);

// First-load default: if no profile is set and the catalog has entries,
// pick the first profile + first model.
onMounted(() => {
  if (!settings.value.profile && catalog.value?.templates.length) {
    const t = catalog.value.templates[0];
    update({ profile: t.name, model: t.models[0]?.id ?? '' });
  }
});

// When the profile changes, default the model to the first one in the
// new template if the current one isn't a member.
watch(() => settings.value.profile, () => {
  if (!currentProfile.value) return;
  const inList = currentProfile.value.models.some(m => m.id === settings.value.model);
  if (!inList) {
    update({ model: currentProfile.value.models[0]?.id ?? '' });
  }
});

function onProfileChange(e: Event) {
  update({ profile: (e.target as HTMLSelectElement).value });
}
function onModelChange(e: Event) {
  update({ model: (e.target as HTMLSelectElement).value });
}
function onKeyInput(e: Event) {
  update({ apiKey: (e.target as HTMLInputElement).value });
}
</script>

<template>
  <div class="chat-settings-backdrop" @click.self="$emit('close')">
    <div class="chat-settings-panel" role="dialog" aria-modal="true" aria-labelledby="chat-settings-title">
      <header>
        <h2 id="chat-settings-title">Chat settings</h2>
        <button class="close-btn" @click="$emit('close')" aria-label="Close">✕</button>
      </header>

      <div v-if="isLoading" class="row note">Loading profiles…</div>
      <div v-else-if="error" class="row note error">
        Couldn't load profile catalog. The chat backend may be disabled.
      </div>
      <template v-else-if="catalog">
        <label class="row">
          <span class="label">Provider</span>
          <select :value="settings.profile" @change="onProfileChange">
            <option v-for="t in catalog.templates" :key="t.name" :value="t.name">
              {{ t.label }}
            </option>
          </select>
        </label>

        <label class="row">
          <span class="label">Model</span>
          <select :value="settings.model" @change="onModelChange">
            <option v-for="m in availableModels" :key="m.id" :value="m.id">
              {{ m.label }}
            </option>
          </select>
        </label>

        <label v-if="currentProfile?.requires_api_key" class="row">
          <span class="label">API key</span>
          <div class="key-input">
            <input
              :type="showKey ? 'text' : 'password'"
              :value="settings.apiKey"
              @input="onKeyInput"
              placeholder="paste your key"
              autocomplete="off"
              spellcheck="false"
            />
            <button type="button" class="key-toggle" @click="showKey = !showKey">
              {{ showKey ? 'hide' : 'show' }}
            </button>
          </div>
        </label>
        <p v-if="currentProfile?.api_key_help" class="row help">
          {{ currentProfile.api_key_help }}
        </p>
        <p class="row help muted">
          Your key is stored in this browser only. The forwarder forwards
          it on each request and never persists it.
        </p>
      </template>

      <footer>
        <button class="primary" @click="$emit('close')">Done</button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.chat-settings-backdrop {
  position: fixed;
  inset: 0;
  background: rgba(0, 0, 0, 0.35);
  display: grid;
  place-items: center;
  z-index: 1000;
}
.chat-settings-panel {
  background: #fff;
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-lg);
  width: min(440px, 90vw);
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
.chat-settings-panel > header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 20px;
  border-bottom: 1px solid var(--border-light);
}
.chat-settings-panel > header h2 {
  font-size: 16px;
  font-weight: 600;
  margin: 0;
}
.close-btn {
  background: none;
  border: none;
  font-size: 18px;
  cursor: pointer;
  color: var(--text-secondary);
  padding: 4px 8px;
  border-radius: var(--radius-sm);
}
.close-btn:hover { background: var(--surface); }
.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 20px;
}
.row.help {
  display: block;
  font-size: 12px;
  color: var(--text-secondary);
  padding: 0 20px 12px;
  line-height: 1.4;
}
.row.help.muted {
  font-style: italic;
  opacity: 0.85;
}
.row.note {
  padding: 24px 20px;
  text-align: center;
  color: var(--text-secondary);
}
.row.note.error {
  color: var(--error);
}
.label {
  flex: 0 0 80px;
  font-size: 13px;
  color: var(--text-secondary);
  font-weight: 500;
}
select, input[type="text"], input[type="password"] {
  flex: 1;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font: inherit;
  background: #fff;
}
.key-input {
  flex: 1;
  display: flex;
  gap: 4px;
}
.key-input input { flex: 1; }
.key-toggle {
  border: 1px solid var(--border);
  background: var(--surface);
  border-radius: var(--radius-sm);
  padding: 0 10px;
  font-size: 11px;
  color: var(--text-secondary);
  cursor: pointer;
}
.key-toggle:hover { background: var(--surface-hover); }
footer {
  display: flex;
  justify-content: flex-end;
  padding: 12px 20px;
  border-top: 1px solid var(--border-light);
  background: var(--surface);
}
.primary {
  background: var(--primary-blue);
  color: #fff;
  border: none;
  border-radius: var(--radius-sm);
  padding: 8px 16px;
  font: 500 13px 'Google Sans', system-ui, sans-serif;
  cursor: pointer;
}
.primary:hover { background: var(--primary-blue-hover); }
</style>
