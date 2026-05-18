package eventclass

import (
	"regexp"
	"strconv"
	"strings"
)

// Fault + slowness classifiers — port the legacy SQL's faulted /
// slow_request / slow_segment UNION branches over network_requests.

func init() {
	RegisterNetwork("network_fault", networkFaultClassifier{})
	RegisterNetwork("network_slow", networkSlowClassifier{})
}

type networkFaultClassifier struct{}

// faulted=1 splits three ways:
//   - fault_type contains "timeout" → request_timeout
//   - fault_type contains corrupt/partial/abandon OR status 2xx →
//     request_incomplete
//   - else → request_faulted
//
// Strings matched case-insensitively (legacy SQL used
// positionCaseInsensitive).
func (networkFaultClassifier) ClassifyRequest(req *NetworkRequest) []Event {
	if req == nil || req.Faulted == 0 {
		return nil
	}
	ft := strings.ToLower(req.FaultType)
	var typ string
	switch {
	case strings.Contains(ft, "timeout"):
		typ = TypeRequestTimeout
	case strings.Contains(ft, "corrupt"),
		strings.Contains(ft, "partial"),
		strings.Contains(ft, "abandon"),
		req.Status >= 200 && req.Status < 300:
		typ = TypeRequestIncomplete
	default:
		typ = TypeRequestFaulted
	}
	return []Event{{
		Ts: req.Ts, PlayerID: req.PlayerID, PlayID: req.PlayID,
		AttemptID: req.AttemptID, SessionID: req.SessionID,
		Classification: req.Classification,
		Type:           typ,
		Info:           req.FaultType + " " + req.Method + " " + req.Path,
	}}
}

type networkSlowClassifier struct{}

// segmentPathRe identifies HLS/DASH media segments by extension —
// used as the gate for the slow_segment branch (the legacy SQL's
// `match(path, '\.(m4s|ts|mp4|m4a|m4v|aac|webm|mp3)($|\?)')`).
var segmentPathRe = regexp.MustCompile(`\.(m4s|ts|mp4|m4a|m4v|aac|webm|mp3)($|\?)`)

func (networkSlowClassifier) ClassifyRequest(req *NetworkRequest) []Event {
	if req == nil || req.Faulted != 0 || req.Status >= 400 {
		return nil
	}
	base := Event{
		Ts: req.Ts, PlayerID: req.PlayerID, PlayID: req.PlayID,
		AttemptID: req.AttemptID, SessionID: req.SessionID,
		Classification: req.Classification,
	}
	var out []Event
	if req.ClientWaitMs > 2000 {
		e := base
		e.Type = TypeSlowRequest
		e.Info = formatMsInt(req.ClientWaitMs) + "ms " + req.Method + " " + req.Path
		out = append(out, e)
	}
	if req.TransferMs > 6000 && segmentPathRe.MatchString(req.Path) {
		e := base
		e.Type = TypeSlowSegment
		e.Info = formatMsInt(req.TransferMs) + "ms " + req.Method + " " + req.Path
		out = append(out, e)
	}
	return out
}

// formatMsInt rounds a float ms value to the nearest int, matching
// the legacy SQL's `round(client_wait_ms, 0)` → `toString` output
// where the trailing ".0" is dropped.
func formatMsInt(ms float32) string {
	return strconv.FormatInt(int64(ms+0.5), 10)
}
