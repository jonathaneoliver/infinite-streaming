package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WaitForTopAndBuffer polls the bound player every `period` until both:
//   - VideoResolution matches one of `topResolutions` (string match), and
//   - BufferDepthS >= minBufferS
//
// Returns when both conditions are met or the deadline expires. Used by
// the abort characterization test to ensure each cycle starts from a
// clean state (player on top variant, healthy buffer) before injecting
// a fault.
//
// `topResolutions` is typically the list of top-tier variants we'll
// accept; passing the single highest is the common case.
func WaitForTopAndBuffer(ctx context.Context, s *Session, topResolutions []string, minBufferS float64, deadline time.Duration, period time.Duration) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("wait for top: no player bound")
	}
	if period <= 0 {
		period = 500 * time.Millisecond
	}
	until := time.Now().Add(deadline)
	for {
		rec, err := s.PlayerState(ctx)
		if err == nil && rec.PlayerMetrics != nil {
			pm := rec.PlayerMetrics
			if matchesAnyResolution(pm.VideoResolution, topResolutions) && pm.BufferDepthS >= minBufferS {
				return nil
			}
		}
		if time.Now().After(until) {
			got := ""
			gotBuf := 0.0
			if rec != nil && rec.PlayerMetrics != nil {
				got = rec.PlayerMetrics.VideoResolution
				gotBuf = rec.PlayerMetrics.BufferDepthS
			}
			return fmt.Errorf("wait for top: %v / buf>=%.1fs not reached within %s (last: res=%q buf=%.1fs)",
				topResolutions, minBufferS, deadline, got, gotBuf)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(period):
		}
	}
}

func matchesAnyResolution(have string, want []string) bool {
	if have == "" {
		return false
	}
	for _, w := range want {
		if have == w {
			return true
		}
	}
	return false
}

// NetworkRow is the subset of /analytics/api/v2/network_requests row
// fields the abort cycle observer needs. Other columns exist on the
// CH row (status, content_type, ttfb_ms, etc.); add them here as the
// observer needs them.
type NetworkRow struct {
	Ts            time.Time `json:"-"`
	TsRaw         string    `json:"ts"`
	URL           string    `json:"url"`
	UpstreamURL   string    `json:"upstream_url"`
	RequestKind   string    `json:"request_kind"`
	Status        int       `json:"status"`
	BytesOut      int64     `json:"bytes_out"`
	TotalMs       float64   `json:"total_ms"`
	TransferMs    float64   `json:"transfer_ms"`
	FaultType     string    `json:"fault_type"`
	FaultAction   string    `json:"fault_action"`
	FaultCategory string    `json:"fault_category"`
	RangeStart    int64     `json:"range_start"`
	RangeEnd      int64     `json:"range_end"`
	HasRange      bool      `json:"has_range_header"`
}

// FetchNetworkRows returns recent network_requests rows for the bound
// play, filtered client-side to the window [from, to]. Used by
// ObserveAbortCycle to spot the abort row + retry row.
//
// Filters by play_id (bound on the session). Caller passes the play_id
// explicitly because tests may want to bind a fresh play mid-run.
func FetchNetworkRows(ctx context.Context, playerID, playID string, from, to time.Time, limit int) ([]NetworkRow, error) {
	if limit <= 0 {
		limit = 200
	}
	path := fmt.Sprintf("/analytics/api/v2/network_requests?player_id=%s&play_id=%s&limit=%d",
		playerID, playID, limit)
	body, err := runHarness(ctx, "raw", "GET", path)
	if err != nil {
		return nil, fmt.Errorf("fetch network rows: %w", err)
	}
	var wrap struct {
		Items []NetworkRow `json:"items"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("decode network rows: %w", err)
	}
	out := make([]NetworkRow, 0, len(wrap.Items))
	for _, r := range wrap.Items {
		ts, err := parseNetworkTs(r.TsRaw)
		if err != nil {
			continue
		}
		if ts.Before(from) || ts.After(to) {
			continue
		}
		r.Ts = ts
		out = append(out, r)
	}
	return out, nil
}

func parseNetworkTs(raw string) (time.Time, error) {
	// CH returns either "2026-05-21T18:19:50.702Z" (RFC3339) or
	// "2026-05-21 18:19:50.702" (ClickHouse string). Try both.
	if ts, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return ts, nil
	}
	if ts, err := time.Parse("2006-01-02 15:04:05.000", raw); err == nil {
		return ts, nil
	}
	if ts, err := time.Parse("2006-01-02 15:04:05", raw); err == nil {
		return ts, nil
	}
	return time.Time{}, fmt.Errorf("unparseable ts %q", raw)
}

// ObserveAbortCycle scans network rows + in-process samples for the
// observation window and builds an AbortCycleResult. Caller passes:
//   - faultShape: the named fault we armed (used as a tag)
//   - pre: snapshot taken just before the fault was armed
//   - armedAt: the moment ArmFault returned
//   - window: how long to look forward (typically 30s)
//   - samples: in-process samples already collected by the sampler
//
// Detection logic:
//   - AbortKind: first row in the window with non-empty fault_type
//     (the proxy stamps the type when its own fault firing closes the
//     transfer) OR fault_action=transfer_abandoned (client-initiated).
//   - RetryFound: subsequent GET on the same URL after the abort row.
//   - RetryHadRange: that retry's has_range_header / range_start.
//   - DownshiftedTo: first sample after armedAt whose VideoResolution
//     differs from pre.PreVariant.
//   - PlayerStalled: any post-armedAt sample with PositionS not
//     advancing for > 5s.
func ObserveAbortCycle(faultShape string, pre AbortCycleResult, armedAt time.Time, rows []NetworkRow, samples []Sample) AbortCycleResult {
	out := pre
	out.FaultShape = faultShape
	out.ArmedAt = armedAt

	// Find the first faulted row in the window — that's the abort.
	for i := range rows {
		r := &rows[i]
		if r.Ts.Before(armedAt) {
			continue
		}
		if r.FaultType != "" || r.FaultAction == "transfer_abandoned" {
			out.AbortDetected = true
			out.AbortKind = pickFaultKind(r)
			out.AbortAtS = r.Ts.Sub(armedAt).Seconds()
			out.AbortURL = r.URL
			// Look for a retry on the same URL among later rows.
			for j := i + 1; j < len(rows); j++ {
				next := &rows[j]
				if next.URL != r.URL {
					continue
				}
				out.RetryFound = true
				out.RetryHadRange = next.HasRange || next.RangeStart > 0
				out.RetryRangeStart = next.RangeStart
				break
			}
			break
		}
	}

	// Walk post-armedAt samples — find downshift, stall, recovery.
	var lastPosTs time.Time
	var lastPos float64
	stallStart := time.Time{}
	for _, s := range samples {
		if s.Ts.Before(armedAt) {
			continue
		}
		// Downshift detection — first resolution change.
		if out.DownshiftedTo == "" && s.VideoResolution != "" && s.VideoResolution != pre.PreVariant {
			out.DownshiftedTo = s.VideoResolution
			out.DownshiftAfterS = s.Ts.Sub(armedAt).Seconds()
		}
		// Stall detection — position frozen.
		if !lastPosTs.IsZero() && s.PositionS == lastPos {
			if stallStart.IsZero() {
				stallStart = lastPosTs
			} else if s.Ts.Sub(stallStart) > 5*time.Second {
				out.PlayerStalled = true
			}
		} else {
			stallStart = time.Time{}
		}
		lastPos = s.PositionS
		lastPosTs = s.Ts
		out.PostBwEstMbps = s.NetworkBitrateMbps
	}

	return out
}

func pickFaultKind(r *NetworkRow) string {
	if r.FaultType != "" {
		return r.FaultType
	}
	if r.FaultAction != "" {
		return r.FaultAction
	}
	if r.FaultCategory != "" {
		return r.FaultCategory
	}
	return "unknown"
}

// SamplesAfter returns the subset of samples with Ts >= cutoff.
// Convenience for callers that want to scope observation windows.
func SamplesAfter(samples []Sample, cutoff time.Time) []Sample {
	out := make([]Sample, 0, len(samples))
	for _, s := range samples {
		if !s.Ts.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

// JoinShapes formats fault-shape names for log lines.
func JoinShapes(shapes []string) string { return strings.Join(shapes, ", ") }
