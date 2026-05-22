package runner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CycleID is the standard cycle-identity label set required at every
// characterization-cycle start. The forwarder's `label_changed`
// control_event records each PATCH so the cycle timeline lives in
// control_events for free — see
// `.claude/standards/characterization-principles.md § 9` for the
// schema this struct enforces.
//
// Test is the run-wide test name ("startup", "abort", "retry", …) —
// constant across all cycles in a single test run. Idx is the cycle
// counter (1-based). Rep is the repetition counter within the same
// (boundary, fault, cap) tuple. CapMbps is "none" when no cap applies;
// otherwise the integer Mbps value. Boundary and Fault are
// test-specific axes; set one or the other, not both. Extra carries
// any additional per-test labels that don't fit the well-known keys
// (rarely needed — prefer adding the field to the cycle result struct
// instead).
//
// ComposeID() returns the canonical `cycle_id` value used as the join
// key: <test>:<axis>:<cap>:<rep>. Examples:
//   - "startup:app_cold:cap30:rep2"
//   - "abort:server_timeout:cap5:rep0"
//   - "retry:request_first_byte_hang:none:rep1"
type CycleID struct {
	Test     string
	Idx      int
	Rep      int
	CapMbps  string // "30", "0.8", "none" — caller renders the float however they like; "" defaults to "none"
	Boundary string // startup-style axis (app_cold, channel_change, …)
	Fault    string // abort/retry-style axis (server_timeout, request_body_reset, …)
	Extra    map[string]string
}

// labelValueOK matches the strict label-value vocab: alphanumerics +
// `_:.-`. Per characterization-principles § 9 and the LabelPlay memory
// — `,` and `=` are silently rejected by the forwarder, so we don't
// even risk them. Anything else gets rewritten to `_`.
var labelValueOK = regexp.MustCompile(`[^A-Za-z0-9_:.\-]`)

// sanitizeLabelValue replaces forbidden characters with `_`. Idempotent.
func sanitizeLabelValue(v string) string {
	return labelValueOK.ReplaceAllString(v, "_")
}

// ComposeID builds the canonical cycle_id value:
// <test>:<boundary-or-fault>:<cap>:<rep>. All four segments are always
// present so the cycle_id has a stable arity — easier to parse on the
// dashboard side. Falls back to "none" for missing axis/cap.
func (c CycleID) ComposeID() string {
	axis := c.Boundary
	if axis == "" {
		axis = c.Fault
	}
	if axis == "" {
		axis = "none"
	}
	cap := c.CapMbps
	if cap == "" {
		cap = "none"
	} else if !strings.HasPrefix(cap, "cap") && cap != "none" {
		cap = "cap" + cap
	}
	return fmt.Sprintf("%s:%s:%s:rep%d",
		sanitizeLabelValue(c.Test),
		sanitizeLabelValue(axis),
		sanitizeLabelValue(cap),
		c.Rep,
	)
}

// labelSet renders the full required label map for a cycle start
// PATCH. The map is shaped so a single LabelPlay call writes every
// required key in one round-trip (one `label_changed` row per cycle).
func (c CycleID) labelSet() map[string]string {
	out := map[string]string{
		"test":      sanitizeLabelValue(c.Test),
		"cycle_id":  c.ComposeID(),
		"cycle_idx": fmt.Sprintf("%d", c.Idx),
		"rep":       fmt.Sprintf("%d", c.Rep),
	}
	if c.Boundary != "" {
		out["boundary"] = sanitizeLabelValue(c.Boundary)
	}
	if c.Fault != "" {
		out["fault"] = sanitizeLabelValue(c.Fault)
	}
	if c.CapMbps != "" {
		out["cap_mbps"] = sanitizeLabelValue(c.CapMbps)
	}
	for k, v := range c.Extra {
		if _, taken := out[k]; taken {
			// Reserved keys silently win — extra is opt-in metadata,
			// never an override of the schema.
			continue
		}
		out[sanitizeLabelValue(k)] = sanitizeLabelValue(v)
	}
	return out
}

// StartCycle PATCHes the player labels with the full required cycle
// schema (overwriting any prior cycle's keys) and returns the
// wall-clock instant the PATCH completed. The forwarder turns that
// PATCH into a `label_changed` control_event whose ts ≈ the returned
// time — those rows are what the dashboard's cycle-band overlay reads
// to draw bands across the play timeline.
//
// Cycles end IMPLICITLY when the next StartCycle overwrites cycle_id.
// To explicitly close the last cycle of a run (so the band has a
// trailing edge in the archive instead of trailing off-screen), call
// EndCycle after the last cycle's observation window.
//
// Returns the timestamp even on label error — the cycle is still
// happening; the only thing lost is the queryability of its boundary.
// Drivers SHOULD log the error and continue.
func StartCycle(ctx context.Context, sess *Session, cid CycleID) (time.Time, error) {
	now := time.Now()
	if sess == nil {
		return now, fmt.Errorf("StartCycle: nil session")
	}
	if cid.Test == "" {
		return now, fmt.Errorf("StartCycle: CycleID.Test is required")
	}
	return now, sess.LabelPlay(ctx, cid.labelSet())
}

// EndCycle clears the cycle_id label (sets it empty) so the cycle-band
// overlay knows the cycle has a definite closing edge. Other label
// keys (test, run_id, …) are left intact so the operator can still see
// what the LAST cycle was during the post-run cool-down period.
//
// Idempotent — calling twice in a row is a no-op since the second
// PATCH writes the same value. Safe to call before EVERY StartCycle
// when defensive (the next StartCycle will overwrite cycle_id again).
func EndCycle(ctx context.Context, sess *Session) error {
	if sess == nil {
		return fmt.Errorf("EndCycle: nil session")
	}
	return sess.LabelPlay(ctx, map[string]string{"cycle_id": ""})
}
