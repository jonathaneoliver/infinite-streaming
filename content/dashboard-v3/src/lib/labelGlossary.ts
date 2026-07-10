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

// Threshold values in `how` text are the compiled-in defaults from
// qoe_thresholds.go (config-driven, movable via FORWARDER_QOE_THRESHOLDS_PATH).
// Duration bands `<1s / 1–3s / ≥3s` are hardcoded in the forwarder's
// durationBucket() and are NOT config-driven.
const GLOSSARY: Record<string, Entry> = {
  // ── terminal / player failures & recovery ────────────────────────────────
  qoe_vsf: { what: 'Video Start Failure — playback never produced a first frame', how: 'startup ended in error before any frame' },
  qoe_msf: { what: 'Mid-Stream Failure — playback started then died and did not recover', how: 'fatal error after first frame' },
  qoe_ebvs: { what: 'Exit Before Video Start — abandoned during startup', how: 'session ended while still buffering, > ebvs_threshold_ms (10s)' },
  player_error: { what: 'The player reported a hard error', how: 'client-emitted error event' },
  player_stuck: { what: 'The player auto-paused mid-stall', how: 'rate→0 with buffer drained — a hard stall that did not reach .failed' },
  wedge_detected: { what: 'Confirmed hard playback wedge', how: 'AVPlayer -12880 + sustained no-progress' },
  user_marked_911: { what: 'Operator flagged this moment (forensic 911 mark)', how: 'manual user_marked event' },
  restart_reload: { what: 'Operator-initiated player reload', how: 'restart attributed to a manual reload' },
  restart_user_retry: { what: 'The user manually retried playback', how: 'restart attributed to a user action' },
  restart_auto_recovery: { what: 'The player auto-restarted to recover from a wedge', how: 'a new attempt_id appeared without user action' },
  restart_auto_recovery_failure: { what: 'Auto-recovery restart that failed to recover', how: 'involuntary restart, recovery unsuccessful' },
  restart_auto_recovery_stuck: { what: 'Auto-recovery restart that stayed stuck', how: 'involuntary restart, still no progress after' },
  restart_auto_recovery_live_resync: { what: 'Auto-recovery via jump-to-live seek', how: 'involuntary restart resolved by seeking to the live edge' },

  // ── startup ──────────────────────────────────────────────────────────────
  qoe_vst_concerning: { what: 'Video Start Time slow', how: 'time-to-first-frame > vst_concerning_ms (5s)' },
  qoe_vst_breach: { what: 'Video Start Time very slow', how: 'time-to-first-frame > vst_breach_ms (10s)' },
  qoe_buffering_short_startup: { what: 'Brief buffering during startup', how: 'pre-first-frame buffering <1s' },
  qoe_buffering_long_startup: { what: 'Long buffering during startup', how: 'pre-first-frame buffering 1–3s' },
  qoe_buffering_severe_startup: { what: 'Severe buffering during startup', how: 'pre-first-frame buffering ≥3s' },
  qoe_stall_short_startup: { what: 'Brief stall during startup', how: 'startup stall <1s (within 10s of first frame)' },
  qoe_stall_long_startup: { what: 'Long stall during startup', how: 'startup stall 1–3s' },
  qoe_stall_severe_startup: { what: 'Severe stall during startup', how: 'startup stall ≥3s' },

  // ── continuity / rebuffering (mid-play + scrub) ──────────────────────────
  qoe_cirr_concerning: { what: 'Rebuffer ratio elevated (Connection-Induced Rebuffer Ratio)', how: 'rebuffer_time / playing_time > cirr_concerning (0.002)' },
  qoe_cirr_breach: { what: 'Rebuffer ratio high', how: 'rebuffer_time / playing_time > cirr_breach (0.004)' },
  qoe_cirt_concerning: { what: 'Long individual rebuffers (Connection-Induced Rebuffer Time)', how: 'mean rebuffer-event duration > cirt_concerning_ms (1s)' },
  qoe_cirt_breach: { what: 'Very long individual rebuffers', how: 'mean rebuffer-event duration > cirt_breach_ms (2s)' },
  qoe_stall_burst: { what: 'Stall thrashing', how: '> stall_burst_threshold (3) stalls in stall_burst_window_s (60s)' },
  qoe_stall_short_midplay: { what: 'Brief mid-play stall', how: 'a single mid-play stall <1s' },
  qoe_stall_long_midplay: { what: 'Long stall during mid-play', how: 'a single mid-play stall 1–3s' },
  qoe_stall_severe_midplay: { what: 'Severe stall during mid-play', how: 'a single mid-play stall ≥3s' },
  qoe_buffering_short_scrub: { what: 'Brief buffering after a seek/scrub', how: 'post-seek buffering <1s' },
  qoe_buffering_long_scrub: { what: 'Buffering after a seek/scrub', how: 'post-seek buffering 1–3s' },
  qoe_buffering_severe_scrub: { what: 'Severe buffering after a seek/scrub', how: 'post-seek buffering ≥3s' },
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
  qoe_cmcd_mtp_drift: { what: 'CMCD client throughput drifts from server', how: 'CMCD measured throughput diverges > cmcd_mtp_drift_ratio (50%) from the server transfer rate' },
  qoe_fps_dip: { what: 'Displayed frame rate dipped', how: 'displayed fps < fps_dip_ratio (80%) of nominal' },
  shift_up: { what: 'ABR shifted up a rung (informational)' },
  shift_down: { what: 'ABR shifted down a rung (informational)' },

  // ── network / transport (per-request) ────────────────────────────────────
  qoe_rate_cap_breach: { what: 'Measured bitrate exceeded the applied cap', how: 'network bitrate > cap × rate_cap_breach_factor (1.25) — often an AVPlayer burst over-read' },
  qoe_transfer_stall: { what: 'A segment transfer stalled mid-flight', how: 'no bytes received for transfer_stall_ms (5s)' },
  qoe_ttfb_breach: { what: 'Time-to-first-byte slow', how: 'responseStart − requestEnd > ttfb_breach_ms (500ms) — stream-level TTFB, generous on HTTP/2 keep-alive' },
  master_manifest_failure: { what: 'The master playlist request failed' },
  manifest_failure: { what: 'A media playlist request failed' },
  segment_failure: { what: 'A media segment request failed' },
  http_4xx: { what: 'An HTTP 4xx response on a request' },
  http_5xx: { what: 'An HTTP 5xx response on a request' },
  slow_request: { what: 'A request was slow (but completed)', how: 'client wait > 2s (hardcoded)' },
  slow_segment: { what: 'A segment fetch was slow (but completed)', how: 'segment transfer > 6s (hardcoded)' },
  stall_segment: { what: 'A segment fetch stalled' },
  transport_socket: { what: 'Transport fault: socket-level drop/reject', how: 'nftables DROP/REJECT on the connection' },
  transport_disconnect: { what: 'The connection dropped at the transport layer', how: 'client/socket disconnect mid-request' },
  transport_failure: { what: 'Transport fault: unspecified transfer-timeout variant', how: 'transfer_timeout with neither active nor idle sub-flavour' },
  transfer_active_timeout: { what: 'Server closed a transfer for exceeding the active (total) timeout' },
  transfer_idle_timeout: { what: 'Server closed a transfer for exceeding the idle (no-bytes) timeout' },
  request_retry: { what: 'Retry of a just-failed request', how: 'same URL re-fetched 1ms–4s after the previous fetch failed' },

  // ── live edge ────────────────────────────────────────────────────────────
  qoe_live_offset_concerning: { what: 'Playhead drifting behind the live edge', how: '> offset_concerning_margin_s (3s) beyond the recommended live offset' },
  qoe_live_offset_breach: { what: 'Playhead well behind the live edge', how: '> offset_breach_margin_s (10s) beyond the recommended live offset' },
  qoe_live_offset_tight: { what: 'Playhead closer to live than the manifest target', how: '≥ offset_tight_margin_s (3s) CLOSER to live than recommended — less buffer than the stream asked for' },
  qoe_holdback_deviation: { what: 'Configured live offset deviates from the manifest recommendation', how: '|configured − recommended| > holdback_deviation_s (2s)' },

  // ── injected-fault markers (from fault-injection tests, not real defects) ─
  fault_timeout: { what: 'INJECTED fault: a timeout was applied (fault test, expected)' },
  fault_other: { what: 'INJECTED fault: a non-categorised fault was applied (fault test, expected)' },
  fault_incomplete: { what: 'INJECTED fault: a transfer was cut off by an injected fault (fault test)' },

  // ── control-plane (operator / harness actions) ───────────────────────────
  fault_on: { what: 'A runtime fault was toggled ON', how: 'proxy fault activated — will degrade playback by design' },
  fault_off: { what: 'A runtime fault was cleared', how: 'proxy fault deactivated (resolved bookend)' },
  fault_rule_enabled: { what: 'A fault rule was armed on the session (test metadata)' },
  fault_rule_disabled: { what: 'A fault rule was cleared (test metadata)' },
  fault_rule_config_change: { what: 'A fault rule was reconfigured (test metadata)' },
  pattern_enabled: { what: 'A traffic-shaping pattern was armed', how: 'proxy applyShapePattern (mode in the info payload)' },
  pattern_disabled: { what: 'A traffic-shaping pattern was cleared' },
  pattern_step: { what: 'A traffic-shaping pattern advanced a step', how: 'next rate/step in the active pattern' },
  pattern_config_change: { what: 'A traffic-shaping pattern was reconfigured' },
  shaper_changed: { what: 'The network rate shaper changed' },
  shaper_config_change: { what: 'The rate shaper was reconfigured' },
  timeouts_changed: { what: 'Transfer-timeout config changed on the session' },
  label_changed: { what: 'Session KV labels were updated', how: 'operator/harness _v2_labels bridged into labels[]' },
  content_changed: { what: 'The session content was swapped' },
  control_change: { what: 'A session control setting changed (generic)' },

  // ── lifecycle / info ─────────────────────────────────────────────────────
  first_frame: { what: 'First decoded frame rendered (startup succeeded)' },
  play_start: { what: 'Playback started' },
  play_end: { what: 'Playback ended' },
  session_start: { what: 'Session opened' },
  session_end: { what: 'Session ended (clean)' },
  server_start: { what: 'Proxy / server (re)started', how: 'boot marker; info carries restored/skipped/baseline' },
  live_resync: { what: 'Jump-to-live seek nudge', how: 'a recovery action (METHOD 3), not a failure' },
  loop_server: { what: 'The origin is looping VOD-as-live (test content marker)' },
  // VOMM anomaly labels (`anomaly_<cond>_<surf>`, legacy `unexpected_<cond>`) and
  // per-mode pattern labels (`pattern_enabled_<mode>`, `pattern_step_<mode>`) are
  // matched by prefix below (anomalyWhat / patternModeWhat), not enumerated here.
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

/**
 * Per-mode traffic-shaping control labels: `pattern_enabled_<mode>` /
 * `pattern_step_<mode>` (e.g. pattern_enabled_rampUp). The base `pattern_enabled`
 * / `pattern_step` live in GLOSSARY; this covers the mode-suffixed variants the
 * Sessions filter emits.
 */
const PATTERN_RE = /^pattern_(enabled|step)_(.+)$/;
function patternModeWhat(ev: string): string | undefined {
  const m = PATTERN_RE.exec(ev);
  if (!m) return undefined;
  const [, kind, mode] = m;
  return kind === 'enabled'
    ? `Traffic-shaping pattern armed (mode: ${mode})`
    : `Traffic-shaping pattern advanced a step (mode: ${mode})`;
}

/**
 * Derived whole-tuple network-failure signature (#892):
 * `net_failure:<kind>/<cause>/<outcome>` — one collapsed chip summarising the
 * orthogonal facets of a single failed request (a network_requests row is one
 * HTTP request, so the tuple is unambiguous). The facet labels stay in
 * labels[] for filtering; this is the human-facing roll-up.
 */
const NET_FAILURE_PREFIX = 'net_failure:';
const NET_FAILURE_CAUSE: Record<string, string> = {
  client_disconnect: 'the client dropped it mid-transfer',
  socket: 'the socket was cut',
  active_timeout: 'an active-transfer timeout fired',
  idle_timeout: 'an idle-transfer timeout fired',
  transport: 'a transport timeout fired',
};
const NET_FAILURE_OUTCOME: Record<string, string> = {
  incomplete: 'left incomplete',
  timeout: 'timed out',
  other: 'failed',
  http_4xx: 'returned 4xx',
  http_5xx: 'returned 5xx',
};

/** humanizeNetFailure turns `net_failure:segment/client_disconnect/incomplete`
 *  into `segment · client_disconnect · incomplete` for the collapsed chip. */
export function humanizeNetFailure(ev: string): string {
  return ev.slice(NET_FAILURE_PREFIX.length).split('/').join(' · ');
}

function netFailureWhat(ev: string): string | undefined {
  if (!ev.startsWith(NET_FAILURE_PREFIX)) return undefined;
  const parts = ev.slice(NET_FAILURE_PREFIX.length).split('/');
  const kind = parts[0] ?? '';
  // outcome is always last; the optional cause sits between kind and outcome.
  const outcome = parts.length > 1 ? parts[parts.length - 1] : '';
  const cause = parts.length > 2 ? parts[1] : '';
  const kindTxt = kind === 'master_manifest' ? 'master-playlist'
    : kind === 'manifest' ? 'playlist' : kind;
  const outTxt = NET_FAILURE_OUTCOME[outcome] ?? outcome;
  const causeTxt = cause ? ` — ${NET_FAILURE_CAUSE[cause] ?? cause}` : '';
  return `One request: a ${kindTxt} ${outTxt}${causeTxt}. `
    + `Whole-tuple summary of the kind/cause/outcome facets on this row (each still filterable).`;
}

/** The facet events a net_failure signature rolls up. When the signature is
 *  present on a row these are hidden from the inline chips (still in labels[]
 *  for filtering) and surfaced via the signature chip's tooltip. */
const NET_FAILURE_FACETS = new Set<string>([
  'segment_failure', 'manifest_failure', 'master_manifest_failure',
  'transport_socket', 'transport_disconnect',
  'transfer_active_timeout', 'transfer_idle_timeout', 'transport_failure',
  'fault_incomplete', 'fault_timeout', 'fault_other',
  'http_4xx', 'http_5xx',
]);

/** collapseNetFailureLabels drops the constituent facet labels from a row's
 *  chip list IFF a net_failure signature is present, so the line reads as one
 *  failure. No-op otherwise. The input labels[] is untouched (query surface). */
export function collapseNetFailureLabels(labels: string[]): string[] {
  const hasSig = labels.some((l) => eventOf(l).startsWith(NET_FAILURE_PREFIX));
  if (!hasSig) return labels;
  return labels.filter((l) => !NET_FAILURE_FACETS.has(eventOf(l)));
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
  const dyn = anomalyWhat(ev) ?? patternModeWhat(ev) ?? netFailureWhat(ev);
  if (dyn) return dyn;
  const e = GLOSSARY[ev];
  if (!e) return '';
  return e.how ? `${e.what} · ${e.how}` : e.what;
}

/** hasGlossary reports whether a label has a definition (to style it as hoverable). */
export function hasGlossary(label: string): boolean {
  const ev = eventOf(label);
  return anomalyWhat(ev) !== undefined || patternModeWhat(ev) !== undefined
    || netFailureWhat(ev) !== undefined || GLOSSARY[ev] !== undefined;
}
