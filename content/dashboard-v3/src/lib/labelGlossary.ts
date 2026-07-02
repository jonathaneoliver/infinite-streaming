/**
 * labelGlossary — single source of truth for what each severity-tagged QoE /
 * lifecycle label means and how it's flagged (#772). Keyed by the *event* (the
 * part after `severity=`, with any leading `*` synthesized-marker stripped), so
 * `error=*qoe_vsf` and `warning=*qoe_vsf` resolve to the same entry.
 *
 * Defaults in the "how" text are the compiled-in Conviva thresholds
 * (qoe_thresholds.go); they move if a FORWARDER_QOE_THRESHOLDS_PATH overlay is
 * loaded. Use `labelTooltip(label)` for a hover string anywhere a label chip is
 * rendered.
 */

interface Entry {
  what: string;
  how?: string;
}

const GLOSSARY: Record<string, Entry> = {
  // ── terminal / player failures ───────────────────────────────────────────
  qoe_vsf: { what: 'Video Start Failure — playback never produced a first frame', how: 'startup ended in error before any frame' },
  qoe_msf: { what: 'Mid-Stream Failure — playback started then died and did not recover', how: 'fatal error after first frame' },
  qoe_ebvs: { what: 'Exit Before Video Start — abandoned during startup', how: 'session ended while still buffering, > ebvs_threshold_ms (10s)' },
  player_error: { what: 'The player reported a hard error', how: 'client-emitted error event' },
  restart_auto_recovery: { what: 'The player auto-restarted to recover from a wedge', how: 'a new attempt_id appeared without user action' },
  restart_user_retry: { what: 'The user manually retried playback', how: 'restart attributed to a user action' },

  // ── startup ──────────────────────────────────────────────────────────────
  qoe_vst_concerning: { what: 'Video Start Time slow', how: 'time-to-first-frame > vst_concerning_ms (5s)' },
  qoe_vst_breach: { what: 'Video Start Time very slow', how: 'time-to-first-frame > vst_breach_ms (10s)' },
  qoe_buffering_long_startup: { what: 'Long buffering during startup', how: 'pre-first-frame buffering over the long threshold' },
  qoe_buffering_severe_startup: { what: 'Severe buffering during startup', how: 'pre-first-frame buffering over the severe threshold' },
  qoe_stall_long_startup: { what: 'Long stall during startup' },
  qoe_stall_severe_startup: { what: 'Severe stall during startup' },

  // ── continuity / rebuffering ─────────────────────────────────────────────
  qoe_cirr_concerning: { what: 'Rebuffer ratio elevated (Connection-Induced Rebuffer Ratio)', how: 'rebuffer_time / playing_time > cirr_concerning (0.002)' },
  qoe_cirr_breach: { what: 'Rebuffer ratio high', how: 'rebuffer_time / playing_time > cirr_breach (0.004)' },
  qoe_cirt_concerning: { what: 'Long individual rebuffers (Connection-Induced Rebuffer Time)', how: 'mean rebuffer-event duration > cirt_concerning_ms (1s)' },
  qoe_cirt_breach: { what: 'Very long individual rebuffers', how: 'mean rebuffer-event duration > cirt_breach_ms (2s)' },
  qoe_stall_long_midplay: { what: 'Long stall during mid-play' },
  qoe_stall_severe_midplay: { what: 'Severe stall during mid-play', how: '≥ stall_burst_threshold (3) stalls in stall_burst_window_s (60s)' },
  qoe_buffering_severe_scrub: { what: 'Severe buffering after a seek/scrub' },
  stall_frozen: { what: 'The playhead froze — no progress while not paused', how: 'position stopped advancing (wall-clock confirmed)' },
  timejump: { what: 'The playhead jumped discontinuously', how: 'position moved more than wall-clock elapsed' },

  // ── tiers (two axes) ─────────────────────────────────────────────────────
  // qoe_tier_* = WHOLE-PLAY grade (worst QoE severity seen anywhere in the
  // play). qoe_exit_* = state at CLOSE (conditions still active on the final
  // row + terminal outcome) — a churn/abandonment proxy. They diverge when a
  // play hit a critical mid-play but recovered: tier_unacceptable, exit_premium.
  qoe_tier_unacceptable: { what: 'Whole-play QoE: unacceptable', how: 'worst severity seen anywhere in the play was critical/error' },
  qoe_tier_acceptable: { what: 'Whole-play QoE: acceptable (below premium)', how: 'worst severity seen anywhere in the play was a warning' },
  qoe_tier_premium: { what: 'Whole-play QoE: premium', how: 'no warning/critical/error seen during the play' },
  qoe_exit_unacceptable: { what: 'State at close: unacceptable', how: 'a critical/error condition was still active when the play ended' },
  qoe_exit_acceptable: { what: 'State at close: acceptable', how: 'a warning condition was still active when the play ended' },
  qoe_exit_premium: { what: 'State at close: premium', how: 'nothing degraded was active when the play ended' },

  // ── ABR / quality ────────────────────────────────────────────────────────
  qoe_downshift_storm: { what: 'ABR thrashing down', how: '> downshift_storm_threshold (3) downshifts in downshift_storm_window_s (30s)' },
  qoe_downshift_overshoot: { what: 'ABR over-corrected downward', how: 'settled ≥ downshift_overshoot_rungs (2) below the rung the cap supports' },
  qoe_min_variant_stuck: { what: 'Pinned at the lowest rung', how: 'dwelled at the floor variant > min_variant_stuck_s (30s)' },
  qoe_abr_conservative: { what: 'ABR under-using the link', how: 'selected variant < bitrate_underutilized_ratio (50%) of available throughput' },
  qoe_ladder_gap: { what: 'No ladder rung fits the available throughput', how: 'next rung needs more than abr_headroom_margin (85%) of throughput' },
  qoe_throughput_divergence: { what: 'Client vs server throughput disagree', how: 'network_bitrate diverges > throughput_divergence_factor (15%) from server throughput' },
  qoe_fps_dip: { what: 'Displayed frame rate dipped', how: 'displayed fps < fps_dip_ratio (80%) of nominal' },
  shift_up: { what: 'ABR shifted up a rung (informational)' },
  shift_down: { what: 'ABR shifted down a rung (informational)' },

  // ── network / transport ──────────────────────────────────────────────────
  qoe_rate_cap_breach: { what: 'Measured bitrate exceeded the applied cap', how: 'network bitrate > cap × rate_cap_breach_factor (1.10) — often an AVPlayer burst over-read' },
  qoe_transfer_stall: { what: 'A segment transfer stalled mid-flight', how: 'no bytes received for transfer_stall_ms (5s)' },
  master_manifest_failure: { what: 'The master playlist request failed' },
  manifest_failure: { what: 'A media playlist request failed' },
  segment_failure: { what: 'A media segment request failed' },
  http_4xx: { what: 'An HTTP 4xx response on a request' },
  http_5xx: { what: 'An HTTP 5xx response on a request' },
  slow_segment: { what: 'A segment fetch was slow (but completed)' },
  stall_segment: { what: 'A segment fetch stalled' },
  transport_disconnect: { what: 'The connection dropped at the transport layer', how: 'client/socket disconnect mid-request' },
  transfer_active_timeout: { what: 'Server closed a transfer for exceeding the active (total) timeout' },
  transfer_idle_timeout: { what: 'Server closed a transfer for exceeding the idle (no-bytes) timeout' },

  // ── live edge ────────────────────────────────────────────────────────────
  qoe_live_offset_concerning: { what: 'Playhead drifting behind the live edge', how: '> offset_concerning_margin_s (3s) beyond the recommended live offset' },
  qoe_live_offset_breach: { what: 'Playhead well behind the live edge', how: '> offset_breach_margin_s (10s) beyond the recommended live offset' },

  // ── injected-fault markers (from fault-injection tests, not real defects) ─
  fault_timeout: { what: 'INJECTED fault: a timeout was applied (fault test, expected)' },
  fault_other: { what: 'INJECTED fault: a non-categorised fault was applied (fault test, expected)' },
  fault_incomplete: { what: 'INJECTED fault: a transfer was cut off by an injected fault (fault test)' },
  fault_rule_enabled: { what: 'A fault rule was armed on the session (test metadata)' },
  fault_rule_disabled: { what: 'A fault rule was cleared (test metadata)' },

  // ── lifecycle / info ─────────────────────────────────────────────────────
  first_frame: { what: 'First decoded frame rendered (startup succeeded)' },
  play_start: { what: 'Playback started' },
  play_end: { what: 'Playback ended' },
  loop_server: { what: 'The origin is looping VOD-as-live (test content marker)' },
  // VOMM anomaly labels (`anomaly_<cond>_<surf>`, and legacy `unexpected_<cond>`)
  // are matched by prefix in anomalyWhat() below, not enumerated here — the
  // family is 4 conditions × 2 surfaces. See analytics/tools/derive_labels.py.
};

/**
 * VOMM per-row surprise labels: `anomaly_<cond>_<surf>` (derive_labels.py), where
 * cond ∈ {startup,fault,stall,end} anchors a play episode and surf ∈ {net,event}
 * is the surface the surprising token landed on. Also matches the legacy
 * `unexpected_<cond>` name (pre-rename rows still in ClickHouse until TTL).
 */
const ANOMALY_RE = /^(?:anomaly|unexpected)_(startup|fault|stall|end)(?:_(net|event))?$/;
const ANOMALY_COND: Record<string, string> = {
  startup: 'startup', fault: 'fault-handling', stall: 'stall', end: 'end-of-play',
};
function anomalyWhat(ev: string): string | undefined {
  const m = ANOMALY_RE.exec(ev);
  if (!m) return undefined;
  const [, cond, surf] = m;
  const where = surf === 'net' ? ' on a network-transfer row'
    : surf === 'event' ? ' on a player-event row' : '';
  return `VOMM flagged the ${ANOMALY_COND[cond] ?? cond} episode as statistically `
    + `surprising vs trained plays${where}`;
}

/** eventOf strips the `<severity>=` prefix and any leading `*` marker. */
export function eventOf(label: string): string {
  const eq = label.indexOf('=');
  const ev = eq >= 0 ? label.slice(eq + 1) : label;
  return ev.replace(/^\*/, '');
}

/** labelTooltip returns a hover string ("what · how") for a label, or '' if unknown. */
export function labelTooltip(label: string): string {
  const ev = eventOf(label);
  const anom = anomalyWhat(ev);
  if (anom) return anom;
  const e = GLOSSARY[ev];
  if (!e) return '';
  return e.how ? `${e.what} · ${e.how}` : e.what;
}

/** hasGlossary reports whether a label has a definition (to style it as hoverable). */
export function hasGlossary(label: string): boolean {
  const ev = eventOf(label);
  return anomalyWhat(ev) !== undefined || GLOSSARY[ev] !== undefined;
}
