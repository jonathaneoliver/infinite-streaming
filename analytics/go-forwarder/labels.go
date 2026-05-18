// labels.go — write-time row labeling (issue #473).
//
// At ingest, every snapshot / network row gets a small set of
// `<severity>=<event>` label strings stamped onto its `labels` column.
// Replaces the bucket-A markers (~9 marker types that were pure
// re-labels of one source row). Labels carry both the SEVERITY tier
// (`error` / `critical` / `warning` / `info`) and the specific event
// in one string so a single CH column drives:
//
//   - dashboard row tint (worst-severity wins)
//   - chip rendering (color = severity prefix)
//   - filter UI (PRIORITY_META → SEVERITY_META, keyed by severity)
//   - sessions picker per-play badge counts
//   - the auto-classification tier bump in classification.go
//   - harness CLI `--severity error,critical` filter
//
// Bucket-B markers (stall durations, counter-bump edges, fault
// toggles, request_retry) keep emitting marker rows — they carry
// information that doesn't live on any single source row.
package main

import (
	"strings"
)

// Severity prefixes — worst-first when ranking. Source for the
// dashboard's row-tint precedence and any "show me >= X" filter.
const (
	SevError    = "error"
	SevCritical = "critical"
	SevWarning  = "warning"
	SevInfo     = "info"
)

// computeEventLabels stamps a player-event row's labels at ingest.
// Reads the post-merge row state — values are exactly what the
// session_events INSERT body carries. The mapping is the design
// agreed on in issue #473's discussion.
//
// Empty slice (or `nil`) is the "no notable signals" case — the
// dashboard renders a healthy-looking row with no chips and no tint.
func computeEventLabels(r *row) []string {
	if r == nil {
		return nil
	}
	switch r.LastEvent {

	// info — normal lifecycle worth surfacing
	case "rate_shift_up":
		return []string{SevInfo + "=shift_up"}
	case "rate_shift_down":
		return []string{SevInfo + "=shift_down"}
	case "video_first_frame":
		return []string{SevInfo + "=first_frame"}
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
		// `player_error` is the error text; we just label it
		// `error=player_error` — operators inspect the field
		// itself for the specific code/string.
		return []string{SevError + "=player_error"}
	}
	return nil
}

// computeNetworkLabels stamps a network row's labels at ingest.
// Same shape as computeEventLabels — multiple labels can apply to
// one row (e.g. a 5xx that's also slow gets both).
func computeNetworkLabels(r *netRow) []string {
	if r == nil {
		return nil
	}
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
			// Includes the legacy SQL's "faulted but status 2xx"
			// degenerate case — proxy flagged it but the body
			// arrived. Still worth a chip.
			if r.Status >= 200 && r.Status < 300 {
				out = append(out, SevWarning+"=fault_incomplete")
			} else {
				out = append(out, SevError+"=fault_other")
			}
		}
	}

	// Slow requests — only meaningful on clean (non-faulted,
	// non-error-status) rows. A faulted timeout is already
	// labeled `error=fault_timeout`; double-labeling with
	// `warning=slow_request` is noise.
	if r.Faulted == 0 && r.Status < 400 {
		if r.ClientWaitMs > 2000 {
			out = append(out, SevWarning+"=slow_request")
		}
		if r.TransferMs > 6000 && isSegmentPath(r.Path) {
			out = append(out, SevWarning+"=slow_segment")
		}
	}
	return out
}

// isSegmentPath matches HLS / DASH media segment extensions —
// scoped to the slow_segment label so a slow manifest fetch doesn't
// incorrectly land there. Same regex pattern the legacy SQL used.
func isSegmentPath(path string) bool {
	// Inline check rather than regex compile-once because the path
	// list is short and string-contains is cheap.
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

// worstSeverity returns the highest-severity prefix present in
// labels, ordered error > critical > warning > info. Empty string
// when no labels carry a severity prefix. Used by the auto-
// classification tier bump and (mirrored on the client) by the
// dashboard's row-tint logic.
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
