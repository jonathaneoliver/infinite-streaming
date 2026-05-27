// labels.go — write-time row labeling (issues #473, #474).
//
// At ingest, every snapshot / network row gets a small set of label
// strings stamped onto its `labels` column. Label format:
//
//   <severity>=<event>      direct from one column / last_event value
//   <severity>=*<event>     synthesized — needs cross-row state or
//                           multi-column logic. The `*` prefix is the
//                           "introduced by the labeler" signal.
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
	stallStartTs   string
	stallStartTime time.Time
	bufferingStartTs   string
	bufferingStartTime time.Time
	// Context detection: ts of the first observed first-frame /
	// playback-start signal and the most recent timejump. Used to
	// classify a stall/buffering as startup / scrub / midplay.
	firstFrameTime   time.Time
	lastTimejumpTime time.Time
	// LRU touch for GC.
	seen time.Time
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

	switch r.LastEvent {

	// info — normal lifecycle worth surfacing
	case "rate_shift_up":
		return []string{SevInfo + "=shift_up"}
	case "rate_shift_down":
		return []string{SevInfo + "=shift_down"}
	case "video_first_frame":
		labels := []string{SevInfo + "=first_frame"}
		if l := videoStartupLabel(r.VideoFirstFrameTimeS); l != "" {
			labels = append(labels, l)
		}
		return labels
	case "video_start_time":
		return []string{SevInfo + "=playback_start"}

	// warning — degraded but functioning
	case "timejump":
		return []string{SevWarning + "=timejump"}
	case "segment_stall":
		return []string{SevWarning + "=stall_segment"}

	// critical — user-visible impact
	case "frozen":
		return []string{SevCritical + "=stall_frozen"}
	case "user_marked":
		return []string{SevCritical + "=user_marked_911"}

	// restart splits on reason — operator-initiated reload is
	// informational; everything else is an involuntary recovery
	// the operator should see.
	case "restart":
		if r.TriggerType == "reload" {
			return []string{SevInfo + "=restart_reload"}
		}
		return []string{SevCritical + "=restart_auto_recovery"}

	// error — system-detected failure
	case "error":
		return []string{SevError + "=player_error"}

	// Pair-derivation: stall_start opens a window; nothing on the
	// row itself yet (the duration label lands on stall_end).
	case "stall_start":
		ps.stallStartTs = r.Ts
		ps.stallStartTime = now
		return nil
	case "stall_end":
		dur := float64(r.LastStallTimeS)
		if dur <= 0 && !ps.stallStartTime.IsZero() {
			dur = now.Sub(ps.stallStartTime).Seconds()
		}
		ps.stallStartTs = ""
		ps.stallStartTime = time.Time{}
		if dur <= 0 {
			return nil
		}
		return []string{stallLabel(dur, stallContext(ps, now))}

	case "buffering_start":
		ps.bufferingStartTs = r.Ts
		ps.bufferingStartTime = now
		return nil
	case "buffering_end":
		dur := float64(r.LastBufferingTimeS)
		if dur <= 0 && !ps.bufferingStartTime.IsZero() {
			dur = now.Sub(ps.bufferingStartTime).Seconds()
		}
		ps.bufferingStartTs = ""
		ps.bufferingStartTime = time.Time{}
		if dur <= 0 {
			return nil
		}
		return []string{bufferingLabel(dur, bufferingContext(ps, now))}
	}
	return nil
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

// videoStartupLabel classifies player-reported time-to-first-frame.
// Sits alongside the existing `info=first_frame` direct emission so a
// slow cold-start surfaces both signals on the same row.
//
//   ttff > 4s  → error=*video_startup_severe
//   ttff > 2s  → warning=*video_startup_long
//   else       → "" (fast startup; only the info=first_frame chip lands)
func videoStartupLabel(ttffS float32) string {
	switch {
	case ttffS > 4.0:
		return SevError + "=" + synthMark + "video_startup_severe"
	case ttffS > 2.0:
		return SevWarning + "=" + synthMark + "video_startup_long"
	}
	return ""
}

func stallLabel(durS float64, ctx string) string {
	b := durationBucket(durS)
	switch ctx {
	case "startup":
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "stall_short_startup"
		case bucketLong:
			return SevWarning + "=" + synthMark + "stall_long_startup"
		default:
			return SevCritical + "=" + synthMark + "stall_severe_startup"
		}
	default: // midplay
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "stall_short_midplay"
		case bucketLong:
			return SevWarning + "=" + synthMark + "stall_long_midplay"
		default:
			return SevCritical + "=" + synthMark + "stall_severe_midplay"
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
			return SevInfo + "=" + synthMark + "buffering_short_scrub"
		case bucketLong:
			return SevInfo + "=" + synthMark + "buffering_long_scrub"
		default:
			return SevWarning + "=" + synthMark + "buffering_severe_scrub"
		}
	default: // startup
		switch b {
		case bucketShort:
			return SevInfo + "=" + synthMark + "buffering_short_startup"
		case bucketLong:
			return SevWarning + "=" + synthMark + "buffering_long_startup"
		default:
			return SevCritical + "=" + synthMark + "buffering_severe_startup"
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
// pair as one `<sev>=<key>_<value>` label entry. Uses SevInfo because
// KV labels are operator metadata, not failure signals — they should
// inherit the existing `info=` chip color in the dashboard.
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
		out = append(out, SevInfo+"="+k+"_"+v)
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
