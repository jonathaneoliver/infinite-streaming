/**
 * useDiscoveredModels — fetch GET {base_url}/v1/models from the
 * browser to surface the LIVE list of models a provider exposes.
 * For Ollama this is the canonical source (the operator can pull
 * arbitrary models locally); for hosted providers it's a nice extra
 * that supplements the catalog defaults.
 *
 * Browser-side, not server-side: the Ollama instance lives on the
 * operator's local machine, which the forwarder (in a container)
 * can't reach but the browser can. Same auth model — the api_key
 * (if any) goes in the Authorization header directly from the
 * browser to the provider.
 *
 * Returns `null` while loading, `[]` when discovery failed (UI
 * falls back to catalog defaults), or the discovered model id list.
 */

import { ref, watch } from 'vue';

interface OAIModel {
  id: string;
  object?: string;
  owned_by?: string;
}

interface OAIModelsResponse {
  data?: OAIModel[];
}

interface DiscoveryState {
  loading: boolean;
  models: string[] | null; // null = haven't tried; [] = tried and failed/empty
  error: string;
}

export function useDiscoveredModels(
  baseURL: () => string,
  apiKey: () => string,
) {
  const state = ref<DiscoveryState>({ loading: false, models: null, error: '' });

  async function discover() {
    const url = baseURL().trim();
    if (!url) {
      state.value = { loading: false, models: null, error: '' };
      return;
    }
    state.value = { loading: true, models: null, error: '' };
    const headers: Record<string, string> = { Accept: 'application/json' };
    const key = apiKey().trim();
    if (key) headers['Authorization'] = `Bearer ${key}`;
    const endpoint = url.replace(/\/+$/, '') + '/models';
    try {
      const resp = await fetch(endpoint, { headers });
      if (!resp.ok) {
        state.value = {
          loading: false,
          models: [],
          error: `HTTP ${resp.status}`,
        };
        return;
      }
      const body = (await resp.json()) as OAIModelsResponse;
      const ids = (body.data ?? []).map(m => m.id).filter(Boolean);
      state.value = {
        loading: false,
        models: ids,
        error: ids.length === 0 ? 'provider returned no models' : '',
      };
    } catch (err: any) {
      state.value = {
        loading: false,
        models: [],
        error: err?.message ?? String(err),
      };
    }
  }

  // Auto-discover when the base_url changes (e.g. user picks a new
  // profile). Debounced via a microtask to coalesce multiple writes
  // in the same tick.
  watch([baseURL], () => {
    queueMicrotask(discover);
  }, { immediate: false });

  return { state, discover };
}
