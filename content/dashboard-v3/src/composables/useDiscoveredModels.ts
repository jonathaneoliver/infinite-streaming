/**
 * useDiscoveredModels — surface the live list of models a provider
 * exposes via OAI-compat `GET /v1/models`.
 *
 * Routing:
 *   - localhost-shaped base_url → fetch from the browser directly.
 *     The forwarder (in a container on a remote box) can't reach
 *     the operator's `localhost:11434` (Ollama) or `localhost:4000`
 *     (LiteLLM), so the browser is the only path that works. CORS
 *     gates apply — Ollama needs OLLAMA_ORIGINS configured.
 *   - everything else → proxy through the forwarder via
 *     POST /api/v2/chat/discover-models. Hosted providers (Anthropic,
 *     OpenAI, HF) don't send CORS headers because their APIs aren't
 *     browser-facing; the forwarder has no such restriction.
 *
 * Returns `null` while loading, `[]` when discovery failed (UI falls
 * back to catalog defaults), or the discovered model id list.
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

interface ServerDiscoveryResponse {
  models?: string[];
  source?: string;
  error?: string;
}

interface DiscoveryState {
  loading: boolean;
  models: string[] | null; // null = haven't tried; [] = tried and failed/empty
  error: string;
  source: 'browser' | 'server' | '';
}

const LOOPBACK_RE = /:\/\/(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])/i;
const FORWARDER_DISCOVERY_URL = '/analytics/api/v2/chat/discover-models';

export function useDiscoveredModels(
  baseURL: () => string,
  apiKey: () => string,
) {
  const state = ref<DiscoveryState>({ loading: false, models: null, error: '', source: '' });

  async function discoverBrowser(url: string, key: string): Promise<{ ids: string[]; error: string }> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (key) headers['Authorization'] = `Bearer ${key}`;
    const endpoint = url.replace(/\/+$/, '') + '/models';
    try {
      const resp = await fetch(endpoint, { headers });
      if (!resp.ok) return { ids: [], error: `HTTP ${resp.status}` };
      const body = (await resp.json()) as OAIModelsResponse;
      const ids = (body.data ?? []).map(m => m.id).filter(Boolean);
      return { ids, error: ids.length === 0 ? 'provider returned no models' : '' };
    } catch (err: any) {
      return { ids: [], error: err?.message ?? String(err) };
    }
  }

  async function discoverServer(url: string, key: string): Promise<{ ids: string[]; error: string }> {
    try {
      const resp = await fetch(FORWARDER_DISCOVERY_URL, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
        body: JSON.stringify({ base_url: url, api_key: key }),
      });
      if (!resp.ok) return { ids: [], error: `forwarder HTTP ${resp.status}` };
      const body = (await resp.json()) as ServerDiscoveryResponse;
      return { ids: body.models ?? [], error: body.error ?? '' };
    } catch (err: any) {
      return { ids: [], error: err?.message ?? String(err) };
    }
  }

  async function discover() {
    const url = baseURL().trim();
    if (!url) {
      state.value = { loading: false, models: null, error: '', source: '' };
      return;
    }
    state.value = { loading: true, models: null, error: '', source: '' };
    const key = apiKey().trim();
    const useBrowser = LOOPBACK_RE.test(url);
    const { ids, error } = useBrowser
      ? await discoverBrowser(url, key)
      : await discoverServer(url, key);
    state.value = {
      loading: false,
      models: ids,
      error: error,
      source: useBrowser ? 'browser' : 'server',
    };
  }

  // Auto-discover when the base_url changes (e.g. user picks a new
  // profile). queueMicrotask coalesces multiple writes in the same tick.
  watch([baseURL], () => {
    queueMicrotask(discover);
  }, { immediate: false });

  return { state, discover };
}
