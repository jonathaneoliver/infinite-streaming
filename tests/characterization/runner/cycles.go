package runner

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
// In parallel, an OpenTelemetry span is started for the cycle
// (issue #493). Span attributes mirror the CycleID fields; the
// previous cycle's span (if any) is ended first so each test's run
// produces a clean parent → child → sibling-cycle trace shape.
//
// Cycles end IMPLICITLY when the next StartCycle overwrites cycle_id.
// To explicitly close the last cycle of a run (so the band has a
// trailing edge in the archive instead of trailing off-screen), call
// EndCycle after the last cycle's observation window.
//
// Returns the timestamp even on label error — the cycle is still
// happening; the only thing lost is the queryability of its boundary.
// Drivers SHOULD log the error and continue.
//
// The ctx passed in is used to PARENT the span. If callers want the
// cycle span to be a child of a `test_run` span, they must pass the
// context they got back from StartTestRunSpan. Otherwise the cycle
// is a root span — still useful, just unparented in the trace view.
func StartCycle(ctx context.Context, sess *Session, cid CycleID) (time.Time, error) {
	now := time.Now()
	if sess == nil {
		return now, fmt.Errorf("StartCycle: nil session")
	}
	if cid.Test == "" {
		return now, fmt.Errorf("StartCycle: CycleID.Test is required")
	}

	// Close the prior cycle's span first so siblings don't overlap in
	// the timeline view. EndCycle (or the next StartCycle) closes the
	// LAST one at run end — handled in EndCycle below.
	endActiveCycleSpan(codes.Ok, "")

	// Start the cycle span. Use the run-level context if the caller
	// passed one; otherwise this becomes a root span.
	_, span := Tracer().Start(ctx, "cycle", trace.WithAttributes(
		attribute.String("test", cid.Test),
		attribute.String("cycle_id", cid.ComposeID()),
		attribute.Int("cycle_idx", cid.Idx),
		attribute.Int("rep", cid.Rep),
		attribute.String("boundary", cid.Boundary),
		attribute.String("fault", cid.Fault),
		attribute.String("cap_mbps", cid.CapMbps),
	))
	rememberActiveCycleSpan(span)

	return now, sess.LabelPlay(ctx, cid.labelSet())
}

// EndCycle clears the cycle_id label (sets it empty) so the cycle-band
// overlay knows the cycle has a definite closing edge. Other label
// keys (test, run_id, …) are left intact so the operator can still see
// what the LAST cycle was during the post-run cool-down period.
//
// Also ends the active cycle span (issue #493). Idempotent — calling
// twice in a row is a no-op since the second PATCH writes the same
// value and the span is already ended.
func EndCycle(ctx context.Context, sess *Session) error {
	if sess == nil {
		return fmt.Errorf("EndCycle: nil session")
	}
	endActiveCycleSpan(codes.Ok, "")
	return sess.LabelPlay(ctx, map[string]string{"cycle_id": ""})
}

// EndCycleFailed is EndCycle for cycles that didn't meet pass
// criteria (e.g. never reached 5s buffer, player stalled, abort
// undetected). Sets the span status to Error so trace backends can
// surface failed cycles in their default filtering. The label PATCH
// is unchanged — failure is a property of the trace, not the label.
func EndCycleFailed(ctx context.Context, sess *Session, reason string) error {
	if sess == nil {
		return fmt.Errorf("EndCycleFailed: nil session")
	}
	endActiveCycleSpan(codes.Error, reason)
	return sess.LabelPlay(ctx, map[string]string{"cycle_id": ""})
}

// activeCycleSpan tracks the most recently-started cycle span so the
// next StartCycle (or EndCycle) can close it. One pointer per process
// — characterization tests run cycles strictly sequentially, never
// concurrently, so there's no ambiguity about which is "active."
var (
	activeCycleSpanMu sync.Mutex
	activeCycleSpan   trace.Span
)

func rememberActiveCycleSpan(s trace.Span) {
	activeCycleSpanMu.Lock()
	activeCycleSpan = s
	activeCycleSpanMu.Unlock()
}

func endActiveCycleSpan(code codes.Code, description string) {
	activeCycleSpanMu.Lock()
	s := activeCycleSpan
	activeCycleSpan = nil
	activeCycleSpanMu.Unlock()
	if s == nil {
		return
	}
	if code != codes.Ok {
		s.SetStatus(code, description)
	}
	s.End()
}

// StartTestRunSpan begins a top-level span for one test invocation.
// Cycle spans started under the returned context become its children
// — gives trace backends a clean per-run aggregation surface (look at
// the test_run span to see all cycles, their durations, and any
// failed status), instead of N unparented sibling spans.
//
// Caller MUST call the returned shutdown function (or span.End()
// directly via the returned span) at run end so the span duration
// closes and the trace flushes.
//
// runMeta becomes span attributes — typically {test, platform,
// run_id, clip_target} for startup, {test, platform, run_id} for
// abort. Run-scope LabelPlay calls write the same data into the
// player's labels[]; this writes it into the trace.
func StartTestRunSpan(ctx context.Context, name string, runMeta map[string]string) (context.Context, trace.Span) {
	attrs := make([]attribute.KeyValue, 0, len(runMeta))
	for k, v := range runMeta {
		if k == "" || v == "" {
			continue
		}
		attrs = append(attrs, attribute.String(k, v))
	}
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}
