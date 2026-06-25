/**
 * linkProfiles.ts — named link-impairment profiles (#826).
 *
 * Mirror of the canonical Go registry in
 * tools/harness-cli/internal/sweep/profiles.go. Keep the two in sync: the Go
 * table drives the matrix grammar + harness CLI; this copy drives the dashboard
 * profile selector. If you add or retune a profile in one, update the other.
 *
 * Conventions (see the Go file for the full rationale):
 *   - Our four hand-tuned recipes (clean / home / mobile-good / mobile-poor) are
 *     pure-impairment OVERLAYS — no rate_mbps, so they layer on an existing
 *     bandwidth cap. They carry jitter + correlations.
 *   - The Apple NLC presets (nlc-*) set rate_mbps (downlink). NLC has no jitter
 *     term, so jitter_ms stays 0. nlc-high-latency-dns is intentionally absent
 *     (DNS-only, not expressible via netem); nlc-100-loss routes a total outage
 *     through netem 100% loss.
 *   - Delays are one-way (observed RTT ≈ delay).
 */

import type { components } from '@/types/v2';

type Shape = components['schemas']['Shape'];

/** The impairment fields a profile may set. Omitted fields are left untouched. */
export type LinkProfile = Pick<
  Shape,
  | 'rate_mbps'
  | 'delay_ms'
  | 'loss_pct'
  | 'jitter_ms'
  | 'loss_correlation_pct'
  | 'jitter_correlation_pct'
>;

export interface LinkProfileEntry {
  id: string;
  label: string;
  group: 'recipe' | 'nlc';
  shape: LinkProfile;
}

/**
 * Ordered list so the selector groups our recipes first, then the NLC presets.
 * `clean` zeroes every impairment axis (it's how you clear a profile back to a
 * clean link); the other recipes omit rate_mbps so they stay pure overlays.
 */
export const LINK_PROFILES: LinkProfileEntry[] = [
  {
    id: 'clean',
    label: 'Clean (baseline)',
    group: 'recipe',
    shape: { delay_ms: 0, loss_pct: 0, jitter_ms: 0, loss_correlation_pct: 0, jitter_correlation_pct: 0 },
  },
  {
    id: 'home',
    label: 'Home (real network)',
    group: 'recipe',
    shape: { delay_ms: 20, loss_pct: 0.2, jitter_ms: 5, loss_correlation_pct: 25, jitter_correlation_pct: 25 },
  },
  {
    id: 'mobile-good',
    label: 'Mobile — good (LTE/5G)',
    group: 'recipe',
    shape: { delay_ms: 40, loss_pct: 0.5, jitter_ms: 20, loss_correlation_pct: 25, jitter_correlation_pct: 25 },
  },
  {
    id: 'mobile-poor',
    label: 'Mobile — poor',
    group: 'recipe',
    shape: { delay_ms: 150, loss_pct: 3, jitter_ms: 80, loss_correlation_pct: 50, jitter_correlation_pct: 25 },
  },

  // 802.11ac is 1100 Mbps DL (effectively unimpaired); capped at the 100 Mbps
  // test ceiling so no profile drives throughput above the slider's range.
  { id: 'nlc-wifi-ac', label: 'NLC Wi-Fi (802.11ac)', group: 'nlc', shape: { rate_mbps: 100, delay_ms: 1 } },
  { id: 'nlc-wifi', label: 'NLC Wi-Fi', group: 'nlc', shape: { rate_mbps: 40, delay_ms: 1 } },
  { id: 'nlc-lte', label: 'NLC LTE (4G)', group: 'nlc', shape: { rate_mbps: 50, delay_ms: 65 } },
  { id: 'nlc-dsl', label: 'NLC DSL', group: 'nlc', shape: { rate_mbps: 2, delay_ms: 5 } },
  { id: 'nlc-3g', label: 'NLC 3G (HSPA)', group: 'nlc', shape: { rate_mbps: 0.78, delay_ms: 100 } },
  { id: 'nlc-edge', label: 'NLC EDGE (2G)', group: 'nlc', shape: { rate_mbps: 0.24, delay_ms: 400 } },
  {
    id: 'nlc-very-bad',
    label: 'NLC Very Bad Network',
    group: 'nlc',
    shape: { rate_mbps: 1, delay_ms: 500, loss_pct: 10, loss_correlation_pct: 25 },
  },
  { id: 'nlc-100-loss', label: 'NLC 100% loss (outage)', group: 'nlc', shape: { loss_pct: 100 } },
];

export const LINK_PROFILES_BY_ID: Record<string, LinkProfileEntry> = Object.fromEntries(
  LINK_PROFILES.map((p) => [p.id, p]),
);
