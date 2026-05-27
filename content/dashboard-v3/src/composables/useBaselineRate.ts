/**
 * useBaselineRate — fetches the deployment's baseline rate cap from
 * /api/v2/info once per session and caches the result.
 *
 * Returns the configured baseline in Mbps, or 0 when the deployment is
 * unlimited (prod-style). UI uses this to render the persistent
 * "Network baseline" chip on testing pages and to label the slider
 * at min as "(baseline)" when there's a real floor. Issue #480.
 *
 * The value is essentially static — only changes on a proxy restart
 * with a different env var — so a long staleTime is fine. Cached
 * under a single query key (no params); shared across every consumer
 * on the page.
 */

import { computed } from 'vue';
import { useQuery } from '@tanstack/vue-query';

interface ProxyInfo {
  default_rate_mbps?: number;
  // Other Info fields (version, content_dir, ...) exist but we don't
  // need them here; keep the type minimal so future Info additions
  // don't churn this file.
  [k: string]: unknown;
}

async function fetchProxyInfo(): Promise<ProxyInfo> {
  const res = await fetch('/api/v2/info', { headers: { accept: 'application/json' } });
  if (!res.ok) throw new Error(`/api/v2/info ${res.status}`);
  return (await res.json()) as ProxyInfo;
}

export function useBaselineRate() {
  const query = useQuery<ProxyInfo>({
    queryKey: ['proxy', 'info'],
    queryFn: fetchProxyInfo,
    // Baseline only changes on a proxy restart. 5 min staleTime is
    // generous; the cache also survives the page session via Vue
    // Query's default in-memory store.
    staleTime: 5 * 60_000,
    retry: 1,
  });
  const baselineMbps = computed(() => {
    const v = query.data.value?.default_rate_mbps;
    return typeof v === 'number' && Number.isFinite(v) && v > 0 ? v : 0;
  });
  return { baselineMbps, isLoading: query.isLoading, isError: query.isError };
}
