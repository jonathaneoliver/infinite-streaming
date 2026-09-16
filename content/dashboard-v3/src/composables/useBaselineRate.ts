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

// ShapingCapabilities mirrors the proxy's boot-time probe result surfaced on
// /api/v2/info.shaping (#910): which network-shaping controls the kernel can
// actually apply on this host, plus the resolved mode. The dashboard reads
// this to disable/annotate controls instead of showing a phantom cap.
export interface ShapingCapabilities {
  rate: boolean;
  delay: boolean;
  loss: boolean;
  transport_fault: boolean;
  mode: string; // "kernel" | "http-only"
  forced: boolean;
  reason: string;
}

interface ProxyInfo {
  default_rate_mbps?: number;
  shaping?: ShapingCapabilities;
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

/**
 * useShapingCapabilities — exposes the proxy's shaping-capability probe
 * (#910) off the same cached /api/v2/info query as useBaselineRate (no extra
 * fetch). `degraded` is the single flag the UI keys off to show the
 * degraded-mode banner and grey out unavailable controls.
 *
 * Defaults are permissive (all controls available, mode "kernel") until the
 * info request resolves, so the UI doesn't flash "disabled" on load.
 */
export function useShapingCapabilities() {
  const query = useQuery<ProxyInfo>({
    queryKey: ['proxy', 'info'],
    queryFn: fetchProxyInfo,
    staleTime: 5 * 60_000,
    retry: 1,
  });
  const shaping = computed<ShapingCapabilities>(() => {
    const s = query.data.value?.shaping;
    // Permissive fallback pre-resolve / on older proxies without the field.
    return (
      s ?? {
        rate: true,
        delay: true,
        loss: true,
        transport_fault: true,
        mode: 'kernel',
        forced: false,
        reason: '',
      }
    );
  });
  const degraded = computed(() => {
    const s = shaping.value;
    return !s.rate || !s.delay || !s.loss || !s.transport_fault;
  });
  return { shaping, degraded, isLoading: query.isLoading, isError: query.isError };
}
