// labels.go — write-time row labeling (issues #473, #474).
//
// At ingest, every snapshot / network row gets a small set of label
// strings stamped onto its `labels` column. Label format:
//
//	<severity>=<event>      direct from one column / last_event value
//	<severity>=*<event>     synthesized — needs cross-row state or
//	                        multi-column logic. The `*` prefix is the
//	                        "introduced by the labeler" signal.
//
// Severities (worst → least): error, critical, warning, info.
//
// Labels drive:
//   - dashboard row tint (worst-severity wins)
//   - chip rendering (color = severity prefix)
//   - SessionDisplay severity filter
//   - sessions picker per-play badge counts
//   - the auto-classification tier bump in classification.go
//
// State (issue #474 Milestone A): stall + buffering pair derivation,
// per-URL retry detection. Was the eventclass package; merged here so
// the forwarder's surface stays small and labels carry the synthesized
// signals directly on the source row.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Severity prefixes — worst-first when ranking. Source for the
// dashboard's row-tint precedence and any "show me >= X" filter.
const (
	SevError    = "error"
	SevCritical = "critical"
	SevWarning  = "warning"
	SevInfo     = "info"
	// SevTesting tags operator/test-harness KV metadata (run_id, test,
	// platform, …) so it groups separately from genuine playback signal.
	// Intentionally NOT ranked by worstSeverity — testing labels must not
	// tint rows or bump the auto-classification tier. See kvLabelsFromInfo.
	SevTesting = "testing"
)

// Synthesized labels carry a `*` prefix on the event portion so the
// dashboard and operators can tell which signals came from a single
// column vs which required cross-row state.
const synthMark = "*"

// labelState holds the cross-row state the synthesized labels need.
// One process-global instance; mutex protects all maps. Bounded by the
// 5-minute GC sweeps below.
type labelState struct {
	mu       sync.Mutex
	plays    map[string]*playLabelState // key: player_id|play_id
	urls     map[string]urlEntry        // key: url; for request_retry
	lastGCAt time.Time
	// #553 — QoE label thresholds. Installed once at startup via
	// SetThresholds; nil falls back to compiled-in defaults so tests
	// and the no-config boot path stay safe.
	cfg *QoEThresholds
}

// fallbackThresholds is the compiled-in default used when no config has
// been installed (tests, or before SetThresholds runs at startup).
var fallbackThresholds = qoeDefaults()

// thresholds returns the active QoE thresholds. Caller MUST hold s.mu
// (the event labeler already does; the network labeler reads under a
// brief lock).
func (s *labelState) thresholds() *QoEThresholds {
	if s.cfg != nil {
		return s.cfg
	}
	return fallbackThresholds
}

// SetThresholds installs the loaded QoE config. Call once at startup
// before ingest begins.
func (s *labelState) SetThresholds(cfg *QoEThresholds) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// urlEntry records the most-recent fetch of one URL so the retry
// labeler can tell a real retry (prev request failed; client tried
// again) from normal repeat polling (e.g. LL-HLS manifest refresh,
// which fetches the same URL every few seconds by design).
type urlEntry struct {
	ts     time.Time
	failed bool
}

type playLabelState struct {
	// Pair-derivation: open stall/buffering windows.
	stallStartTs       string
	stallStartTime     time.Time
	bufferingStartTs   string
	bufferingStartTime time.Time
	// Context detection: ts of the first observed first-frame /
	// playback-start signal and the most recent timejump. Used to
	// classify a stall/buffering as startup / scrub / midplay.
	firstFrameTime   time.Time
	lastTimejumpTime time.Time
	// #553 windowed/dwell state for threshold-based qoe_* labels.
	// stallBurstTimes / downshiftTimes accumulate event timestamps;
	// the qoe labeler trims them to the configured window and counts.
	// minVariantSince marks when the play first pinned the lowest
	// rung (zero once it climbs off). peakSamples is the recent
	// server-throughput trail for qoe_throughput_divergence.
	stallBurstTimes []time.Time
	downshiftTimes  []time.Time
	minVariantSince time.Time
	peakSamples     []tsVal
	// #553 — terminal aggregate labels (vsf/msf/ebvs + tier) fire once
	// per play, on the first terminal row. playback_status is sticky
	// client pass-through, so without this guard a failed session that
	// keeps heart-beating re-stamps the label on every subsequent row
	// ("endless qoe_msf").
	terminalEmitted bool
	// #603 — set true once any non-terminal row is seen for this play
	// (play_start / restart / state_change / …). A play terminal arriving
	// while this is still false is the PREVIOUS play's play_end mis-bucketed
	// onto this play_id (no play_start boundary yet) — its accumulators are
	// the prior play's, so the QoE labeler ignores it.
	everOpened bool
	// #595 — edge-triggering for level/sticky qoe_* labels. qoeActive is
	// the set of qoe labels that were true on the previous evaluated row.
	// A label is emitted only on its rising edge (off→on): its first
	// determination, or a new instance after it cleared. While a condition
	// stays continuously true it is suppressed (no per-heartbeat repeats).
	// The terminal row force-emits the still-true set so the summary +
	// qoe_tier_* reflect end state. nil until first use.
	qoeActive map[string]struct{}
	// #657 — clear-cooldown re-arm. qoeLastOn records the last row-time each
	// qoe label was TRUE. A rising edge re-fires only if the label has been
	// off for ≥ RefireCooldownS since that time, so a condition that flaps
	// above/below its threshold within the cooldown counts as one episode
	// (one chip) instead of re-chipping on every re-crossing. nil until use.
	qoeLastOn map[string]time.Time
	// LRU touch for GC.
	seen time.Time
}

// tsVal is a timestamped scalar — recent server-throughput samples for
// the windowed-peak computation behind qoe_throughput_divergence.
type tsVal struct {
	t time.Time
	v float64
}

// trimBefore drops entries older than cutoff, in place. Used to bound
// the per-play windowed slices on every evaluation so they can't grow
// unbounded between the 5-minute GC sweeps.
func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// recentPeak appends (now, v), drops samples older than window, and
// returns the trimmed trail plus the max value within it. A zero or
// negative v is still recorded as a sample timestamp but never lifts
// the peak.
func recentPeak(samples []tsVal, now time.Time, v float64, window time.Duration) ([]tsVal, float64) {
	cutoff := now.Add(-window)
	kept := samples[:0]
	for _, s := range samples {
		if !s.t.Before(cutoff) {
			kept = append(kept, s)
		}
	}
	kept = append(kept, tsVal{t: now, v: v})
	var peak float64
	for _, s := range kept {
		if s.v > peak {
			peak = s.v
		}
	}
	return kept, peak
}

var defaultLabelState = newLabelState()

func newLabelState() *labelState {
	return &labelState{
		plays: make(map[string]*playLabelState),
		urls:  make(map[string]urlEntry),
	}
}

// gc reaps entries unseen for >5 minutes. Cheap; amortised across
// every ingested row. Caller holds the mutex.
func (s *labelState) gc(now time.Time) {
	if now.Sub(s.lastGCAt) < 5*time.Minute {
		return
	}
	cutoff := now.Add(-5 * time.Minute)
	for k, v := range s.plays {
		if v.seen.Before(cutoff) {
			delete(s.plays, k)
		}
	}
	for k, e := range s.urls {
		if e.ts.Before(cutoff) {
			delete(s.urls, k)
		}
	}
	s.lastGCAt = now
}

func playKey(playerID, playID string) string {
	return playerID + "|" + playID
}

// computeEventLabels stamps a player-event row's labels at ingest.
// Reads the post-merge row state — values are exactly what the
// session_events INSERT body carries.
//
// Empty slice (or `nil`) is the "no notable signals" case — the
// dashboard renders a healthy-looking row with no chips and no tint.
func computeEventLabels(r *row) []string {
	if r == nil {
		return nil
	}
	return computeEventLabelsWithState(defaultLabelState, r)
}

func computeEventLabelsWithState(s *labelState, r *row) []string {
	now := parseChTs(r.Ts)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gc(time.Now())

	// Touch / lazy-init the per-play state. Even when this row
	// itself doesn't emit a label we may need the cached startup ts
	// later, so we always update first-frame / timejump witnesses.
	key := playKey(r.PlayerID, r.PlayID)
	ps := s.plays[key]
	if ps == nil {
		ps = &playLabelState{}
		s.plays[key] = ps
	}
	ps.seen = time.Now()

	// First-frame and start-time signals — recorded once per play.
	// Any row whose VideoFirstFrameTimeS / VideoStartTimeS first
	// becomes non-zero counts; the LastEvent string is a faster
	// path for the explicit POSTs.
	if ps.firstFrameTime.IsZero() {
		switch r.LastEvent {
		case "video_first_frame", "video_start_time":
			ps.firstFrameTime = now
		}
		if ps.firstFrameTime.IsZero() && (r.VideoFirstFrameTimeS > 0 || r.VideoStartTimeS > 0) {
			ps.firstFrameTime = now
		}
	}
	if r.LastEvent == "timejump" {
		ps.lastTimejumpTime = now
	}

	// #550 Phase 3: the LastEvent switch is additive (was early-return).
	// Each arm appends to `out`; the QoE labeler then layers its
	// threshold-based labels on top before we return. Keeping the arms
	// fall-through is what makes the orthogonal qoe_* labels reachable.
	var out []string

	switch r.LastEvent {

	// info — normal lifecycle worth surfacing
	case "rate_shift_up":
		out = []string{SevInfo + "=shift_up"}
	case "rate_shift_down":
		out = []string{SevInfo + "=shift_down"}
		ps.downshiftTimes = append(ps.downshiftTimes, now) // #553 qoe_downshift_storm window
	case "video_first_frame":
		out = []string{SevInfo + "=first_frame"}
	// #622 — `video_start_time` no longer derives a label. Its old
	// `playback_start` relabel marked the VST/first-render moment,
	// which `first_frame` already covers, and the name read like a
	// play-level boundary — easily confused with the real play-open
	// `play_start` (#603/#621). The video_start_time METRIC (startup
	// latency) is untouched; only the derived label is gone.
	// Forward-only: historical rows keep their playback_start labels.
	case "play_start":
		// #603 — explicit play-open boundary (symmetric to play_end).
		// Distinct from `restart`, which is mid-session recovery only.
		out = []string{SevInfo + "=play_start"}

	// warning — degraded but functioning
	case "timejump":
		out = []string{SevWarning + "=timejump"}
	case "segment_stall":
		out = []string{SevWarning + "=stall_segment"}
	// #703a/#778 — the jump-to-live seek nudge (METHOD 3). A recovery action,
	// not itself a failure; keeps the item (no restart). Warning so it surfaces.
	case "live_resync":
		out = []string{SevWarning + "=live_resync"}

	// critical / error — user-visible impact
	case "frozen":
		out = []string{SevCritical + "=stall_frozen"}
	case "user_marked":
		out = []string{SevCritical + "=user_marked_911"}
	// #703a — AVPlayer auto-paused mid-stall (rate→0, buffer drained): a hard
	// stall that needs intervention and does NOT reach .failed. Error severity.
	case "player_stuck":
		out = []string{SevError + "=player_stuck"}
	// #703 — confirmed hard wedge (-12880 + sustained no-progress). Observability
	// only post-#703a (a real wedge surfaces as .failed and recovers there), but
	// a confirmed wedge is still a play-ending fault signature → error.
	case "wedge_detected":
		out = []string{SevError + "=wedge_detected"}

	// restart splits on reason — operator-initiated reload is
	// informational; everything else is an involuntary recovery
	// the operator should see.
	case "restart":
		// #603 — `restart` is a mid-play recovery (play boundaries are
		// play_start/play_end). Split on the client's restart_reason:
		// auto_recovery (player hit a fatal error and recovered itself) is
		// critical; a manual user_retry is a warning (user perceived a problem
		// and re-attempted); a legacy reload restart is info. Falls back to
		// TriggerType=="reload" for older rows that predate restart_reason.
		switch {
		case r.RestartReason == "user_retry":
			out = []string{SevWarning + "=restart_user_retry"}
		case r.RestartReason == "reload" || r.TriggerType == "reload":
			out = []string{SevInfo + "=restart_reload"}
		// #703a — distinguish the auto-recovery methods so the dashboard/harness
		// can tell which path drove the restart (failure vs stuck vs live-resync),
		// instead of collapsing all three into one `restart_auto_recovery` chip.
		case r.RestartReason == "auto_recovery_failure":
			out = []string{SevCritical + "=restart_auto_recovery_failure"}
		case r.RestartReason == "auto_recovery_stuck":
			out = []string{SevCritical + "=restart_auto_recovery_stuck"}
		case r.RestartReason == "auto_recovery_live_resync":
			out = []string{SevCritical + "=restart_auto_recovery_live_resync"}
		default: // legacy auto_recovery / unknown reason
			out = []string{SevCritical + "=restart_auto_recovery"}
		}

	// error — system-detected failure
	case "error":
		out = []string{SevError + "=player_error"}

	// Pair-derivation: stall_start opens a window; nothing on the
	// row itself yet (the duration label lands on stall_end).
	case "stall_start":
		ps.stallStartTs = r.Ts
		ps.stallStartTime = now
		ps.stallBurstTimes = append(ps.stallBurstTimes, now) // #553 qoe_stall_burst window
	case "stall_end":
		// stall_duration_ms is the canonical sticky per-event duration.
		// last_stall_time_s fallback removed; if neither value is set
		// the in-process pair cache below derives from end-start.
		dur := float64(r.StallDurationMs) / 1000.0
		if dur <= 0 && !ps.stallStartTime.IsZero() {
			dur = now.Sub(ps.stallStartTime).Seconds()
		}
		ps.stallStartTs = ""
		ps.stallStartTime = time.Time{}
		if dur > 0 {
			out = []string{stallLabel(dur, stallContext(ps, now))}
		}

	case "buffering_start":
		ps.bufferingStartTs = r.Ts
		ps.bufferingStartTime = now
	case "buffering_end":
		// Same precedence as stall_end: buffering_duration_ms canonical,
		// in-process pair cache fills in for clients that don't send it.
		dur := float64(r.BufferingDurationMs) / 1000.0
		if dur <= 0 && !ps.bufferingStartTime.IsZero() {
			dur = now.Sub(ps.bufferingStartTime).Seconds()
		}
		ps.bufferingStartTs = ""
		ps.bufferingStartTime = time.Time{}
		if dur > 0 {
			out = []string{bufferingLabel(dur, bufferingContext(ps, now))}
		}
	}

	// #550 Phase 3 — threshold-based QoE auto-labels. Orthogonal to the
	// LastEvent switch above; layered on whatever the event arm emitted.
	// s.thresholds() is read while we still hold s.mu (locked at the top).
	out = append(out, computeQoEEventLabels(s.thresholds(), ps, r, now)...)

	return out
}

// bucket categorises a duration in seconds.
type bucket int

const (
	bucketShort  bucket = iota // <1s
	bucketLong                 // 1-3s
	bucketSevere               // >=3s
)

func durationBucket(s float64) bucket {
	switch {
	case s < 1.0:
		return bucketShort
	case s < 3.0:
		return bucketLong
	default:
		return bucketSevere
	}
}

// stallContext: "startup" if within 10s of first-frame, else "midplay".
// Stalls don't have a scrub variant — a scrub-triggered re-buffer is
// surfaced as buffering_*scrub, not as stall.
func stallContext(ps *playLabelState, now time.Time) string {
	if !ps.firstFrameTime.IsZero() && now.Sub(ps.firstFrameTime) <= 10*time.Second {
		return "startup"
	}
	return "midplay"
}

// bufferingContext: "startup" within 10s of first-frame, "scrub"
// within 3s of last timejump, else "midplay" (which collapses to
// stall — semantically the same user experience).
func bufferingContext(ps *playLabelState, now time.Time) string {
	if !ps.firstFrameTime.IsZero() && now.Sub(ps.firstFrameTime) <= 10*time.Second {
		return "startup"
	}
	if !ps.lastTimejumpTime.IsZero() && now.Sub(ps.lastTimejumpTime) <= 3*time.Second {
		return "scrub"
	}
	return "midplay"
}

// Startup latency is now classified solely by qoe_vst_* (qoe_labels.go),
// keyed on VideoStartTimeMs — the industry-standard Video Start Time
// (playback actually starting), config-driven thresholds. The legacy
// video_startup_* labels (keyed on VideoFirstFrameTimeS, hardcoded 2s/4s)
// were retired in #568; they duplicated qoe_vst_* and disagreed on
// severity. VideoFirstFrameTimeMs is retained — qoe_vsf/qoe_msf still use
// it as the "did a frame ever render?" discriminator.

func stallLabel(durS float64, ctx string) string {
	b := durationBucket(durS)
	switch ctx {
	case "startup":
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "qoe_stall_short_startup"
		case bucketLong:
			return SevWarning + "=" + synthMark + "qoe_stall_long_startup"
		default:
			return SevCritical + "=" + synthMark + "qoe_stall_severe_startup"
		}
	default: // midplay
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "qoe_stall_short_midplay"
		case bucketLong:
			return SevWarning + "=" + synthMark + "qoe_stall_long_midplay"
		default:
			return SevCritical + "=" + synthMark + "qoe_stall_severe_midplay"
		}
	}
}

func bufferingLabel(durS float64, ctx string) string {
	b := durationBucket(durS)
	switch ctx {
	case "midplay":
		// Collapse to stall — same UX, same vocabulary.
		return stallLabel(durS, "midplay")
	case "scrub":
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "qoe_buffering_short_scrub"
		case bucketLong:
			return SevInfo + "=" + synthMark + "qoe_buffering_long_scrub"
		default:
			return SevWarning + "=" + synthMark + "qoe_buffering_severe_scrub"
		}
	default: // startup
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "qoe_buffering_short_startup"
		case bucketLong:
			return SevWarning + "=" + synthMark + "qoe_buffering_long_startup"
		default:
			return SevCritical + "=" + synthMark + "qoe_buffering_severe_startup"
		}
	}
}

// computeNetworkLabels stamps a network row's labels at ingest.
// Multiple labels can apply to one row — e.g. a 404 manifest fetch
// carries both `warning=http_4xx` AND `warning=*manifest_failure`.
func computeNetworkLabels(r *netRow) []string {
	if r == nil {
		return nil
	}
	return computeNetworkLabelsWithState(defaultLabelState, r)
}

func computeNetworkLabelsWithState(s *labelState, r *netRow) []string {
	var out []string

	// HTTP status class. Healthy 2xx gets no label — absence is the
	// signal. Anything outside [200,300) earns its tier.
	switch {
	case r.Status >= 500:
		out = append(out, SevError+"=http_5xx")
	case r.Status >= 400:
		out = append(out, SevWarning+"=http_4xx")
	}

	// Fault classification (independent of status). If the proxy
	// flagged it `faulted=1`, the `fault_type` distinguishes
	// timeout vs incomplete vs other.
	if r.Faulted != 0 {
		ft := strings.ToLower(r.FaultType)
		switch {
		case strings.Contains(ft, "timeout"):
			out = append(out, SevError+"=fault_timeout")
		case strings.Contains(ft, "corrupt"),
			strings.Contains(ft, "partial"),
			strings.Contains(ft, "abandon"):
			out = append(out, SevWarning+"=fault_incomplete")
		default:
			if r.Status >= 200 && r.Status < 300 {
				out = append(out, SevWarning+"=fault_incomplete")
			} else {
				out = append(out, SevError+"=fault_other")
			}
		}
	}

	// Slow requests — only meaningful on clean rows.
	if r.Faulted == 0 && r.Status < 400 {
		if r.ClientWaitMs > 2000 {
			out = append(out, SevWarning+"=slow_request")
		}
		if r.TransferMs > 6000 && isSegmentPath(r.Path) {
			out = append(out, SevWarning+"=slow_segment")
		}
		// #553 — QoE network-tier breaches on clean rows. Thresholds
		// from config; read under a brief lock (this path doesn't hold
		// the labelState mutex outside the retry section below).
		s.mu.Lock()
		cfg := s.thresholds()
		s.mu.Unlock()
		if r.TTFBMs > float32(cfg.Network.TTFBBreachMs) {
			out = append(out, qoeLabel(SevWarning, "qoe_ttfb_breach"))
		}
		if r.TransferMs > float32(cfg.Network.TransferStallMs) {
			out = append(out, qoeLabel(SevWarning, "qoe_transfer_stall"))
		}
	}

	// Synthesized: per-kind failure labels.
	// Triggered when the request kind is known AND the row carries
	// an error status OR is flagged faulted. Stack with the HTTP /
	// fault labels above — a 404 manifest fetch gets both
	// `warning=http_4xx` and `warning=*manifest_failure`.
	failed := r.Status >= 400 || r.Faulted != 0
	if failed {
		switch r.RequestKind {
		case "master_manifest":
			out = append(out, SevError+"="+synthMark+"master_manifest_failure")
		case "manifest", "audio_manifest":
			out = append(out, SevWarning+"="+synthMark+"manifest_failure")
		case "segment", "audio_segment", "init", "partial":
			out = append(out, SevWarning+"="+synthMark+"segment_failure")
		}
	}

	// Transport-fault classification (orthogonal to request_kind).
	// Split socket vs client_disconnect so the labels carry the same
	// sub-flavour the Flags glyph shows (!✂ vs !↩). Issue #474 follow-up.
	if r.Faulted != 0 {
		cat := strings.ToLower(r.FaultCategory)
		switch cat {
		case "socket":
			out = append(out, SevWarning+"="+synthMark+"transport_socket")
		case "client_disconnect":
			out = append(out, SevWarning+"="+synthMark+"transport_disconnect")
		case "transfer_timeout":
			// Active vs idle variant comes from FaultType (the
			// proxy stamps "transfer_active_timeout" /
			// "transfer_idle_timeout" there).
			ft := strings.ToLower(r.FaultType)
			switch {
			case strings.Contains(ft, "active"):
				out = append(out, SevWarning+"="+synthMark+"transfer_active_timeout")
			case strings.Contains(ft, "idle"):
				out = append(out, SevWarning+"="+synthMark+"transfer_idle_timeout")
			default:
				// Unspecified — fall back to a generic transport
				// label so the operator still sees something.
				out = append(out, SevWarning+"="+synthMark+"transport_failure")
			}
		}
	}

	// Synthesized: request_retry on the second fetch of the same URL
	// within 1-4 s, but ONLY when the previous fetch failed (status
	// >= 400 OR faulted). Without that guard, normal LL-HLS manifest
	// polling at every-few-seconds cadence tagged each poll as a
	// retry of the prior one. Per-URL cache, GC'd by labelState.
	failedNow := r.Status >= 400 || r.Faulted != 0
	if r.URL != "" {
		now := parseChTs(r.Ts)
		if !now.IsZero() {
			s.mu.Lock()
			s.gc(time.Now())
			if prev, ok := s.urls[r.URL]; ok && prev.failed {
				delta := now.Sub(prev.ts)
				if delta >= time.Millisecond && delta <= 4*time.Second {
					out = append(out, SevInfo+"="+synthMark+"request_retry")
				}
			}
			s.urls[r.URL] = urlEntry{ts: now, failed: failedNow}
			s.mu.Unlock()
		}
	}

	return out
}

// computeControlLabels stamps a control_events row's labels at ingest.
// Closed-set vocabulary keyed by the `event` value (issue #474
// Milestone B). The synthMark prefix applies — every control_events
// label is "introduced" by the labeler (the table itself is a synthetic
// surface).
func computeControlLabels(r *ctrlRow) []string {
	if r == nil || r.Event == "" {
		return nil
	}
	switch r.Event {
	// Runtime fault toggles — fault_on is the warning, fault_off
	// is the resolved-info bookend.
	case "fault_on":
		return []string{SevWarning + "=" + synthMark + "fault_on"}
	case "fault_off":
		return []string{SevInfo + "=" + synthMark + "fault_off"}
	// Pattern step / shaper change / server loop — informational.
	case "pattern_step":
		// Mirror the per-pattern label scheme from pattern_enabled so a
		// Sessions filter can pull rows by template mode.
		out := []string{SevInfo + "=" + synthMark + "pattern_step"}
		if mode := patternModeFromInfo(r.Info); mode != "" {
			out = append(out, SevInfo+"="+synthMark+"pattern_step_"+mode)
		}
		return out
	case "shaper_changed":
		return []string{SevInfo + "=" + synthMark + "shaper_changed"}
	case "loop_server":
		return []string{SevInfo + "=" + synthMark + "loop_server"}
	// Operator: enabling a fault is warning-level (it'll degrade
	// playback by design); other config changes are info.
	case "fault_rule_enabled":
		return []string{SevWarning + "=" + synthMark + "fault_rule_enabled"}
	case "fault_rule_disabled":
		return []string{SevInfo + "=" + synthMark + "fault_rule_disabled"}
	case "fault_rule_config_change":
		return []string{SevWarning + "=" + synthMark + "fault_rule_config_change"}
	case "pattern_enabled":
		// Pattern_enabled gets a generic label AND a per-mode label
		// (e.g. `info=*pattern_enabled_rampUp`) so the Sessions filter
		// can distinguish ramp-up vs stairs vs custom at a glance.
		// The mode comes off the proxy-side info JSON written by
		// applyShapePattern.
		out := []string{SevInfo + "=" + synthMark + "pattern_enabled"}
		if mode := patternModeFromInfo(r.Info); mode != "" {
			out = append(out, SevInfo+"="+synthMark+"pattern_enabled_"+mode)
		}
		return out
	case "pattern_disabled":
		return []string{SevInfo + "=" + synthMark + "pattern_disabled"}
	case "pattern_config_change":
		return []string{SevInfo + "=" + synthMark + "pattern_config_change"}
	case "shaper_config_change":
		return []string{SevInfo + "=" + synthMark + "shaper_config_change"}
	case "timeouts_changed":
		return []string{SevInfo + "=" + synthMark + "timeouts_changed"}
	case "label_changed":
		// Carry every KV pair from the session's labels map onto the
		// control_event's labels[] column, so they're queryable via the
		// existing Sessions `--label-has` filter (no new filter UI
		// needed). Issue #482 follow-up: bridge user KV labels (proxy
		// `_v2_labels`) into the forwarder's classified labels surface.
		out := []string{SevInfo + "=" + synthMark + "label_changed"}
		out = append(out, kvLabelsFromInfo(r.Info)...)
		return out
	case "content_changed":
		return []string{SevInfo + "=" + synthMark + "content_changed"}
	case "server_start":
		// Proxy restart/boot marker (#671). Global (no player_id) — info
		// carries restored/skipped/baseline_mbps. info-tier so it never
		// tints a row, but is filterable via `--label-has info=*server_start`.
		return []string{SevInfo + "=" + synthMark + "server_start"}
	case "session_start":
		return []string{SevInfo + "=" + synthMark + "session_start"}
	case "session_end":
		return []string{SevInfo + "=" + synthMark + "session_end"}
	// Generic fallback when the changed field isn't yet enumerated.
	case "control_change":
		return []string{SevInfo + "=" + synthMark + "control_change"}
	}
	return nil
}

// kvLabelsFromInfo parses a label_changed control_event's `info` JSON
// (the player's labels map at the moment of change) and renders each
// pair as one `<sev>=<key>_<value>` label entry. Uses SevTesting: these
// KV labels are set by the automated test harness (run_id / test /
// platform / cycle_id via LabelPlay), so they're test metadata, not
// playback signal — they group under the dashboard's "Testing" tier
// rather than drowning real events in Info. testing is unranked in
// worstSeverity, so these never tint a row or bump classification.
//
// Sanitises both key and value to [A-Za-z0-9_-] so the label stays in
// the strict `<sev>=<event>` grammar.
func kvLabelsFromInfo(info string) []string {
	if info == "" {
		return nil
	}
	var kv map[string]string
	if err := json.Unmarshal([]byte(info), &kv); err != nil {
		return nil
	}
	out := make([]string, 0, len(kv))
	for k, v := range kv {
		k = sanitizeLabelToken(k)
		v = sanitizeLabelToken(v)
		if k == "" || v == "" {
			continue
		}
		out = append(out, SevTesting+"="+k+"_"+v)
	}
	return out
}

// sanitizeLabelToken keeps only [A-Za-z0-9_:.-]. Empty after stripping → "".
//
// `:` and `.` were added (was [A-Za-z0-9_-]) so structured label values
// — most notably the characterization-test cycle_id format
// `<test>:<axis>:<cap>:<rep>` (e.g. `startup:app_cold:cap0.8:rep0`) —
// survive sanitization into labels[] intact, instead of collapsing to
// `startupapp_coldcap08rep0`. That collapse made
// `hasAny(labels, ['info=cycle_id_startup:app_cold:cap0.8:rep0'])`
// grep impossible. See .claude/standards/characterization-principles.md § 9.
//
// `,` and `=` remain forbidden (they're the label-grammar separators).
func sanitizeLabelToken(s string) string {
	if s == "" {
		return ""
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' || c == ':' || c == '.'
		if ok {
			b = append(b, c)
		}
	}
	return string(b)
}

// patternModeFromInfo extracts the `mode` field from a pattern_*
// control_event's info JSON ({"mode":"rampUp",…}). Returns "" when
// info is empty, malformed, or has no mode key. Sanitised so the
// resulting label stays in the `<sev>=<event>` shape: only
// [A-Za-z0-9_] survive.
func patternModeFromInfo(info string) string {
	if info == "" {
		return ""
	}
	var meta struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(info), &meta); err != nil {
		return ""
	}
	b := make([]byte, 0, len(meta.Mode))
	for i := 0; i < len(meta.Mode); i++ {
		c := meta.Mode[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '_':
			b = append(b, c)
		}
	}
	return string(b)
}

// isSegmentPath matches HLS / DASH media segment extensions.
func isSegmentPath(path string) bool {
	for _, ext := range []string{".m4s", ".ts", ".mp4", ".m4a", ".m4v", ".aac", ".webm", ".mp3"} {
		if i := strings.LastIndex(path, ext); i > 0 {
			tail := path[i+len(ext):]
			if tail == "" || tail[0] == '?' {
				return true
			}
		}
	}
	return false
}

// worstSeverity returns the highest-severity prefix present in labels.
// Empty string when no labels carry a severity prefix.
func worstSeverity(labels []string) string {
	hasError, hasCritical, hasWarning, hasInfo := false, false, false, false
	for _, l := range labels {
		switch {
		case strings.HasPrefix(l, SevError+"="):
			hasError = true
		case strings.HasPrefix(l, SevCritical+"="):
			hasCritical = true
		case strings.HasPrefix(l, SevWarning+"="):
			hasWarning = true
		case strings.HasPrefix(l, SevInfo+"="):
			hasInfo = true
		}
	}
	switch {
	case hasError:
		return SevError
	case hasCritical:
		return SevCritical
	case hasWarning:
		return SevWarning
	case hasInfo:
		return SevInfo
	}
	return ""
}

// parseChTs parses CH's "YYYY-MM-DD HH:MM:SS.fff" timestamp form (or
// RFC3339Nano). Returns zero time when neither shape matches.
func parseChTs(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05.000", s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
