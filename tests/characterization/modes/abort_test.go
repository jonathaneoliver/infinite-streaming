package modes

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Abort — characterize player response to mid-segment fetch aborts.
//
// Five fault shapes per run, one cycle each by default:
//
//	1. server_timeout         — 5s active timeout + 0.1 Mbps throttle
//	2. request_first_byte_hang
//	3. request_first_byte_delayed   (10s delay before first byte)
//	4. request_body_reset
//	5. request_body_hang
//
// Each cycle: ApplyRate(high) → WaitForTopAndBuffer → arm fault →
// observe 30s → release → wait for recovery → next.
//
// Override the rep count with CHAR_ABORT_REPS=N to run each shape N
// times (default 1). Five shapes × 1 rep = ~5 min wall clock;
// five × 3 reps = ~15 min.
//
// Pass condition: every armed cycle produced an observable abort
// (AbortDetected=true) AND the player did not stall (PlayerStalled
// stays false). Downshift behavior is reported but not asserted —
// each platform may legitimately handle aborts differently.
//
// Plan: ~/.claude/plans/abort-characterization-test.md.

type abortShape struct {
	name        string // matches the proxy's fault-type vocab (or "server_timeout" for the throttle case)
	useThrottle bool   // true → ApplyRate(0.1) + SetSegmentTimeout(5s); false → ArmFault
	delayHint   int    // for *_delayed shapes; informational only (the proxy chooses its own delay)
}

var abortShapes = []abortShape{
	{name: "server_timeout", useThrottle: true},
	{name: "request_first_byte_hang"},
	{name: "request_first_byte_delayed", delayHint: 10},
	{name: "request_body_reset"},
	{name: "request_body_hang"},
}

const (
	abortHighRateMbps     = 100.0
	abortThrottleRateMbps = 0.1
	abortServerTimeout    = 5 * time.Second
	abortFillBufferS      = 15.0
	abortRecoveryBufferS  = 15.0
	abortFillDeadline     = 90 * time.Second
	abortObserveWindow    = 30 * time.Second
	abortPollPeriod       = 500 * time.Millisecond
	abortSamplerPeriod    = 1 * time.Second
)

func TestAbortIPadSim(t *testing.T)   { runAbort(t, runner.PlatformIPadSim) }
func TestAbortIPhone(t *testing.T)    { runAbort(t, runner.PlatformIPhone) }
func TestAbortAppleTV(t *testing.T)   { runAbort(t, runner.PlatformAppleTV) }
func TestAbortAndroidTV(t *testing.T) { runAbort(t, runner.PlatformAndroidTV) }
func TestAbortWeb(t *testing.T)       { runAbort(t, runner.PlatformWeb) }

func runAbort(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	runID := time.Now().UTC().Format("20060102T150405Z")
	// Run-scope labels — constant across all cycles. Per-cycle
	// identity (cycle_id, cycle_idx, rep, fault, cap_mbps) is
	// written by runner.StartCycle inside runOneAbortCycle below.
	// See .claude/standards/characterization-principles.md § 9.
	runLabels := map[string]string{
		"test":     "abort",
		"platform": string(p),
		"run_id":   runID,
	}
	if err := sess.LabelPlay(context.Background(), runLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	} else {
		t.Logf("labeled play with %v", runLabels)
	}
	playID, err := sess.CurrentPlayID(context.Background())
	if err != nil {
		t.Logf("CurrentPlayID: %v (test continues)", err)
	} else {
		t.Logf("play_id: %s   (find later: harness query play %s)", playID, playID)
	}

	reps := 1
	if raw := os.Getenv("CHAR_ABORT_REPS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			reps = n
		}
	}
	cycleCount := len(abortShapes) * reps
	// Each cycle: fill (≤90s) + observe (30s) + recovery (≤90s) = ~3 min worst case.
	overall := time.Duration(cycleCount)*3*time.Minute + 2*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	// Warmup at the high rate so the player picks its top variant and
	// builds an initial buffer. After warmup we read the manifest to
	// learn the top resolution string.
	if err := sess.ApplyRate(ctx, abortHighRateMbps); err != nil {
		t.Fatalf("apply high rate: %v", err)
	}
	if err := sess.ClearFaults(ctx); err != nil {
		t.Logf("clear faults (initial): %v (continuing)", err)
	}
	if err := sess.SetSegmentTimeout(ctx, 0); err != nil {
		t.Logf("clear segment timeout (initial): %v (continuing)", err)
	}
	t.Logf("warmup: %.0f Mbps for 15s", abortHighRateMbps)
	if err := holdContext(ctx, 15*time.Second); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	rec, err := sess.PlayerState(ctx)
	if err != nil {
		t.Fatalf("PlayerState (post-warmup): %v", err)
	}
	variants, err := runner.VariantRatesDesc(rec, 0)
	if err != nil {
		t.Fatalf("VariantRatesDesc: %v", err)
	}
	topResolutions := []string{variants[0].Resolution}
	t.Logf("top variant: %s (%.3f Mbps avg)", topResolutions[0], float64(variants[0].AvgBps)/1_000_000)

	// Scope the fault injections to ONLY video segments by enumerating
	// the video variant directory names from the manifest. Without this
	// scope, faults fire on whichever segment hits the proxy first —
	// often an audio segment (smaller / more frequent), which doesn't
	// give the video-side characterization we want. See plan and issue
	// #491 for the long-term variant-identity filter.
	videoDirs, err := runner.VideoVariantDirs(rec)
	if err != nil {
		t.Fatalf("VideoVariantDirs: %v", err)
	}
	t.Logf("video variant scope: %v (audio + other surfaces excluded)", videoDirs)

	// Start a sampler so each cycle has post-armedAt samples to scan
	// for downshift / stall.
	sampler := runner.NewSampler(sess, abortSamplerPeriod)
	sampler.Start(ctx)
	defer sampler.Stop()
	sampler.SetAppliedRate(abortHighRateMbps)

	var allCycles []runner.AbortCycleResult
	cycleIdx := 0
	t.Logf("cycle plan: %d cycles (%d shape%s × %d rep%s)",
		cycleCount, len(abortShapes), plural(len(abortShapes)), reps, plural(reps))
	for rep := 0; rep < reps; rep++ {
		for _, shape := range abortShapes {
			cycleIdx++
			result := runOneAbortCycle(ctx, t, sess, sampler, shape, topResolutions, videoDirs, cycleIdx, rep)
			allCycles = append(allCycles, result)
			t.Logf("  [%2d] %-32s  abort=%t kind=%-25s retry=%t range=%t downshift=%q stalled=%t recoveryS=%.1f",
				cycleIdx, shape.name, result.AbortDetected, result.AbortKind,
				result.RetryFound, result.RetryHadRange, result.DownshiftedTo,
				result.PlayerStalled, result.RecoveryS)
		}
	}

	// Build the report. Samples come from the sampler; AbortCycles
	// from the per-cycle observations above.
	samples := sampler.Stop()
	report := runner.Report{
		Mode:        "abort",
		Platform:    p,
		Device:      sess.Device,
		PlayerID:    sess.PlayerID,
		PlayIDs:     []string{},
		StartedAt:   time.Now().Add(-overall),
		EndedAt:     time.Now(),
		Samples:     samples,
		AbortCycles: allCycles,
	}
	if playID != "" {
		report.PlayIDs = []string{playID}
	}
	if len(samples) > 0 {
		report.StartedAt = samples[0].Ts
		report.EndedAt = samples[len(samples)-1].Ts
	}
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	base := fmt.Sprintf("abort-%s-%s-%s", p, playerShort, runID)
	jsonPath, err := runner.WriteReport(out, base, &report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	LogReport(t, jsonPath)
	if htmlPath, err := runner.WriteChart(out, base, &report); err == nil {
		t.Logf("chart: %s", htmlPath)
	} else {
		t.Logf("chart write skipped: %v", err)
	}

	// Close the last cycle's band so the dashboard overlay renders
	// a trailing edge in the archive.
	if err := runner.EndCycle(context.Background(), sess); err != nil {
		t.Logf("EndCycle: %v", err)
	}
	endLabels := map[string]string{
		"completed":     time.Now().UTC().Format("20060102T150405Z"),
		"cycle_count":   fmt.Sprintf("%d", len(allCycles)),
		"abort_success": fmt.Sprintf("%d", countAborts(allCycles)),
		"stalled":       fmt.Sprintf("%d", countStalls(allCycles)),
	}
	if err := sess.LabelPlay(context.Background(), endLabels); err != nil {
		t.Logf("label play (end): %v", err)
	}

	// Pass criteria.
	if len(allCycles) == 0 {
		t.Fatal("no cycles ran")
	}
	missed := []string{}
	stalled := []string{}
	for _, c := range allCycles {
		if !c.AbortDetected {
			missed = append(missed, fmt.Sprintf("#%d %s", c.CycleIdx, c.FaultShape))
		}
		if c.PlayerStalled {
			stalled = append(stalled, fmt.Sprintf("#%d %s", c.CycleIdx, c.FaultShape))
		}
	}
	if len(missed) > 0 {
		t.Errorf("%d cycle(s) did not produce an observable abort: %v", len(missed), missed)
	}
	if len(stalled) > 0 {
		t.Errorf("%d cycle(s) stalled (position frozen >5s post-arm): %v", len(stalled), stalled)
	}
}

func runOneAbortCycle(
	ctx context.Context, t *testing.T,
	sess *runner.Session, sampler *runner.Sampler,
	shape abortShape, topResolutions []string,
	videoDirs []string, idx, rep int,
) runner.AbortCycleResult {
	// Stamp cycle identity onto the player BEFORE the fault arms so
	// the dashboard's cycle-band overlay shows the band starting at
	// boundary-time. CapMbps reflects the throttled rate when the
	// shape uses throttling; "none" otherwise (raw fault types).
	capLabel := "none"
	if shape.useThrottle {
		capLabel = fmt.Sprintf("%g", abortThrottleRateMbps)
	}
	if _, err := runner.StartCycle(ctx, sess, runner.CycleID{
		Test:    "abort",
		Idx:     idx,
		Rep:     rep,
		Fault:   shape.name,
		CapMbps: capLabel,
	}); err != nil {
		t.Logf("[%d] StartCycle: %v (band rendering may be missing)", idx, err)
	}
	// Pre-cycle: ensure clean state, then fill buffer at the high rate.
	if err := sess.ClearFaults(ctx); err != nil {
		t.Logf("[%d] clear faults pre-cycle: %v (continuing)", idx, err)
	}
	if err := sess.SetSegmentTimeout(ctx, 0); err != nil {
		t.Logf("[%d] clear segment timeout pre-cycle: %v (continuing)", idx, err)
	}
	if err := sess.ApplyRate(ctx, abortHighRateMbps); err != nil {
		t.Fatalf("[%d] apply high rate: %v", idx, err)
	}
	sampler.SetAppliedRate(abortHighRateMbps)
	if err := runner.WaitForTopAndBuffer(ctx, sess, topResolutions, abortFillBufferS, abortFillDeadline, abortPollPeriod); err != nil {
		t.Logf("[%d] WaitForTopAndBuffer (pre): %v — proceeding anyway", idx, err)
	}

	// Snapshot just before arming.
	pre := runner.AbortCycleResult{CycleIdx: idx, FaultShape: shape.name}
	rec, err := sess.PlayerState(ctx)
	if err == nil && rec.PlayerMetrics != nil {
		pm := rec.PlayerMetrics
		pre.PreVariant = pm.VideoResolution
		pre.PreBufferS = pm.BufferDepthS
		pre.PreBwEstMbps = pm.NetworkBitrateMbps
	}

	// Arm the fault.
	armedAt := time.Now()
	if shape.useThrottle {
		if err := sess.SetSegmentTimeout(ctx, abortServerTimeout); err != nil {
			t.Fatalf("[%d] set segment timeout: %v", idx, err)
		}
		if err := sess.ApplyRate(ctx, abortThrottleRateMbps); err != nil {
			t.Fatalf("[%d] throttle rate: %v", idx, err)
		}
		sampler.SetAppliedRate(abortThrottleRateMbps)
	} else {
		if err := sess.ArmFault(ctx, shape.name, "segment", videoDirs...); err != nil {
			t.Fatalf("[%d] arm fault %s: %v", idx, shape.name, err)
		}
	}
	t.Logf("[%d] %s armed at %s (pre: var=%s buf=%.1fs)",
		idx, shape.name, armedAt.Format("15:04:05.000"), pre.PreVariant, pre.PreBufferS)

	// Observe.
	if err := holdContext(ctx, abortObserveWindow); err != nil {
		t.Logf("[%d] observation window cut short: %v", idx, err)
	}

	// Release the fault BEFORE querying — getting the proxy back to a
	// clean state ASAP so recovery starts. Query happens against the
	// archive a moment later (forwarder is near-real-time).
	if err := sess.ClearFaults(ctx); err != nil {
		t.Logf("[%d] clear faults post-cycle: %v (continuing)", idx, err)
	}
	if err := sess.SetSegmentTimeout(ctx, 0); err != nil {
		t.Logf("[%d] clear segment timeout post-cycle: %v (continuing)", idx, err)
	}
	if err := sess.ApplyRate(ctx, abortHighRateMbps); err != nil {
		t.Logf("[%d] release rate: %v (continuing)", idx, err)
	}
	sampler.SetAppliedRate(abortHighRateMbps)

	// Wait briefly for the forwarder to absorb the network rows that
	// just landed, then query them.
	time.Sleep(2 * time.Second)
	queryFrom := armedAt.Add(-2 * time.Second)
	queryTo := time.Now()
	playID, _ := sess.CurrentPlayID(ctx)
	rows, err := runner.FetchNetworkRows(ctx, sess.PlayerID, playID, queryFrom, queryTo, 500)
	if err != nil {
		t.Logf("[%d] FetchNetworkRows: %v — abort detection may be incomplete", idx, err)
	}
	samples := runner.SamplesAfter(sampler.Recent(120), armedAt)

	result := runner.ObserveAbortCycle(shape.name, pre, armedAt, rows, samples)

	// Wait for recovery — top variant + healthy buffer again.
	recoveryStart := time.Now()
	if err := runner.WaitForTopAndBuffer(ctx, sess, topResolutions, abortRecoveryBufferS, abortFillDeadline, abortPollPeriod); err != nil {
		t.Logf("[%d] WaitForTopAndBuffer (recovery): %v", idx, err)
	}
	result.RecoveryS = time.Since(recoveryStart).Seconds()
	return result
}

func countAborts(cs []runner.AbortCycleResult) int {
	n := 0
	for _, c := range cs {
		if c.AbortDetected {
			n++
		}
	}
	return n
}

func countStalls(cs []runner.AbortCycleResult) int {
	n := 0
	for _, c := range cs {
		if c.PlayerStalled {
			n++
		}
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
