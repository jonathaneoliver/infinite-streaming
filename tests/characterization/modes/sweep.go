// Package modes contains the per-characterization sweep implementations.
// Each *_test.go drives runner.Session through a specific network shape
// pattern (smooth / steps / shock / …) and writes one Report per platform.
//
// Sweep helpers shared across modes live in this non-test file so they can
// be reused without re-exporting via t.Helper().
package modes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// OpenSession picks a launcher per $LAUNCH_MODE, discovers a device for
// the requested platform, launches the player, and returns the bound
// Session. Skips the test cleanly when:
//   - the harness binary isn't installed
//   - no device of the requested platform is reachable
//   - the player doesn't heartbeat within the launcher's timeout
//
// On success registers a t.Cleanup that clears the shape so we don't
// leave the proxy stuck in a degraded state between tests.
func OpenSession(t *testing.T, platform runner.Platform) *runner.Session {
	t.Helper()
	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	// CLI launcher doesn't know how to launch Chrome — auto-downgrade to
	// Manual for web so the operator opens the testing page themselves.
	if platform == runner.PlatformWeb && mode == runner.LaunchCLI {
		t.Logf("launch mode: cli requested, but web platform → switching to manual")
		mode = runner.LaunchManual
		launcher = runner.NewManualLauncher()
		t.Logf("open Chrome to https://dev.jeoliver.com:21000/dashboard/testing-session.html (or your test-dev base) and start playback before the heartbeat timeout fires")
	} else {
		t.Logf("launch mode: %s", mode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	devs, err := launcher.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// Device selection. When CHARACTERIZATION_DEVICE_UDID is set the test
	// targets ONLY that specific device — required for parallel runs
	// across multiple sims of the same platform (each terminal exports
	// its own UDID, no race for the same device). When unset we fall
	// back to "first matching platform" — the simple default.
	wantUDID := strings.TrimSpace(os.Getenv("CHARACTERIZATION_DEVICE_UDID"))
	var picked *runner.Device
	for i := range devs {
		if devs[i].Platform != platform {
			continue
		}
		if wantUDID != "" && !strings.EqualFold(devs[i].UDID, wantUDID) {
			continue
		}
		picked = &devs[i]
		break
	}
	if picked == nil {
		if wantUDID != "" {
			t.Skipf("no %s device with UDID=%s discovered (mode=%s)", platform, wantUDID, mode)
		}
		t.Skipf("no %s device discovered (mode=%s)", platform, mode)
	}
	t.Logf("picked device: %s", picked)

	sess, err := launcher.Launch(ctx, *picked)
	if err != nil {
		t.Fatalf("launch %s: %v", picked, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := sess.ClearShape(ctx); err != nil {
			t.Logf("clear shape: %v", err)
		}
		if err := launcher.Close(); err != nil {
			t.Logf("close launcher: %v", err)
		}
	})
	return sess
}

// LinearSteps generates a descending series of evenly-spaced rate steps.
// "smooth" callers use a small step (~10) over a wide range; "shock"
// callers use 2 steps. count must be ≥ 2 — the slice is undefined otherwise.
func LinearSteps(fromMbps, toMbps float64, count int, hold time.Duration) []runner.Step {
	if count < 2 {
		count = 2
	}
	steps := make([]runner.Step, count)
	delta := (fromMbps - toMbps) / float64(count-1)
	for i := 0; i < count; i++ {
		steps[i] = runner.Step{
			RateMbps: fromMbps - delta*float64(i),
			Hold:     hold,
		}
	}
	return steps
}

// RunSweep applies each step's rate in turn, holds for the step duration
// while the sampler accumulates state, and stamps StartedAt / EndedAt on
// each Step. Returns the populated Report ready for Finalize+Write.
//
// Always clears the shape at the end so a failing test doesn't leak
// network caps onto the next one.
func RunSweep(ctx context.Context, t *testing.T, sess *runner.Session,
	mode string, steps []runner.Step, samplePeriod time.Duration) *runner.Report {
	t.Helper()
	r := &runner.Report{
		Mode:      mode,
		Platform:  sess.Device.Platform,
		Device:    sess.Device,
		PlayerID:  sess.PlayerID,
		StartedAt: time.Now(),
	}
	s := runner.NewSampler(sess, samplePeriod)
	s.Start(ctx)
	defer func() {
		r.Samples = s.Stop()
		_ = sess.ClearShape(context.Background())
	}()

	for i := range steps {
		st := &steps[i]
		st.StartedAt = time.Now()
		if err := sess.ApplyRate(ctx, st.RateMbps); err != nil {
			t.Errorf("step %d: apply rate %.3f Mbps: %v", i, st.RateMbps, err)
			return r
		}
		s.SetAppliedRate(st.RateMbps)
		t.Logf("step %d/%d: %.3f Mbps for %s", i+1, len(steps), st.RateMbps, st.Hold)
		if err := holdContext(ctx, st.Hold); err != nil {
			t.Logf("step %d: hold cancelled: %v", i, err)
			st.EndedAt = time.Now()
			r.Steps = append(r.Steps, *st)
			return r
		}
		st.EndedAt = time.Now()
		r.Steps = append(r.Steps, *st)
	}
	return r
}

func holdContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// RunVariantSweep is the variant-aware step runner the smooth mode uses.
// For each step it:
//   - applies the cap and annotates the sampler
//   - enforces minHold (operator-confirmed: at least 15 s on every cap)
//   - polls every pollPeriod past minHold and exits the step early when
//     the buffer has been stable across the early-exit window (see
//     bufferStable below) — stable buffer means this cap is safe for this
//     variant and there's no point waiting the full maxHold for a stall
//     that won't happen
//   - stops at maxHold regardless
//   - rolls up per-step min buffer + stalls into the Step record so
//     Finalize can compute LowestSustainableCapMbps
//
// Returns the populated Report (Samples/Steps filled). Caller is expected
// to set Report.Variants and call Finalize.
func RunVariantSweep(ctx context.Context, t *testing.T, sess *runner.Session, mode string,
	steps []runner.Step, samplePeriod, minHold, maxHold, earlyExitWindow time.Duration,
	earlyExitTol float64) *runner.Report {
	t.Helper()
	r := &runner.Report{
		Mode:      mode,
		Platform:  sess.Device.Platform,
		Device:    sess.Device,
		PlayerID:  sess.PlayerID,
		StartedAt: time.Now(),
	}
	s := runner.NewSampler(sess, samplePeriod)
	s.Start(ctx)
	defer func() {
		r.Samples = s.Stop()
		_ = sess.ClearShape(context.Background())
	}()

	pollPeriod := 2 * time.Second

	for i := range steps {
		st := &steps[i]
		st.StartedAt = time.Now()
		if err := sess.ApplyRate(ctx, st.RateMbps); err != nil {
			t.Errorf("step %d: apply rate %.3f Mbps: %v", i, st.RateMbps, err)
			st.EndedAt = time.Now()
			st.ExitReason = "cancelled"
			r.Steps = append(r.Steps, *st)
			return r
		}
		s.SetAppliedRate(st.RateMbps)

		startCount := len(s.Recent(0)) // snapshot count before step
		exit := holdWithEarlyExit(ctx, s, st, minHold, maxHold, pollPeriod,
			earlyExitWindow, earlyExitTol)
		st.EndedAt = time.Now()
		st.ExitReason = exit

		stepSamples := s.Recent(0)
		if len(stepSamples) > startCount {
			st.PopulateStepResult(stepSamples[startCount:])
		}
		annotateLog(t, i, len(steps), st)
		r.Steps = append(r.Steps, *st)
		if exit == "cancelled" {
			return r
		}
		// Wedge detection: if the player's playhead hasn't moved over
		// the last 10 s, it's given up. Remaining caps are strictly
		// lower (descending sweep) so we'd just stack more stalls onto
		// a dead player. Mark the rest skipped and exit so the report
		// reflects "we stopped here because the player wedged."
		//
		// #632: skip this on the FIRST step. The entry step is where the
		// player resumes (often probing high — e.g. 4K with no
		// preferredPeakBitRate ceiling — then downshifting to the capped
		// rung) and rebuffers from empty. Position is legitimately frozen
		// during that downshift+rebuffer, which is NOT a wedge — declaring
		// one here aborts a player that's actively recovering. Later steps
		// start from a settled buffer, so the check is valid there.
		if i > 0 && playerWedged(s, 10*time.Second) {
			t.Logf("  player wedged (position not advancing) — skipping remaining %d step(s)",
				len(steps)-i-1)
			for k := i + 1; k < len(steps); k++ {
				steps[k].ExitReason = "skipped-player-wedged"
				steps[k].StartedAt = time.Now()
				steps[k].EndedAt = steps[k].StartedAt
				r.Steps = append(r.Steps, steps[k])
			}
			return r
		}
	}
	return r
}

// playerWedged returns true when the player has given up and isn't
// recovering — distinct from "currently stalled but climbing back".
// Two-signal check:
//
//  1. position_s hasn't advanced more than 0.5 s over the lookback
//     window (the player isn't moving through content)
//  2. AND the buffer never reached SustainableBufferS anywhere in the
//     window (it stayed pinned near-empty — if it climbed back at any
//     point the player is alive and cycling, not dead, so don't wedge)
//
// The second check prevents the false-positives we saw on iPad sim: a
// buffer that drains to 0 mid-step but recovers to >20 s — the player is
// coming back, not wedged. #632 widened it from "last sample" to "any
// sample in the window": on a thin-margin peak-rung upshift the buffer
// oscillates (drains fetching big segments, refills, may drain again), so
// the LAST sample can read ~0 even though the player hit a healthy buffer
// seconds earlier. Keying on the window max spares that dip-and-recover
// while still catching a player pinned empty throughout. Returns false
// when there's not enough sample data yet to judge.
func playerWedged(s *runner.Sampler, lookback time.Duration) bool {
	n := int(lookback / time.Second)
	if n < 5 {
		n = 5
	}
	samples := s.Recent(n)
	if len(samples) < n {
		return false
	}
	first := samples[0].PositionS
	last := samples[len(samples)-1]
	// Position advancing? Not wedged.
	if last.PositionS > first+0.5 {
		return false
	}
	// Position not advancing, but did the buffer reach a healthy level
	// anywhere in the window? Then the player is cycling/recovering, not
	// dead — hold off on declaring wedge.
	for _, smp := range samples {
		if smp.BufferDepthS >= runner.SustainableBufferS {
			return false
		}
	}
	return true
}

// holdWithEarlyExit sleeps up to maxHold, polling every pollPeriod after
// minHold. Returns the exit reason: "full" / "early-stable" / "cancelled".
func holdWithEarlyExit(ctx context.Context, s *runner.Sampler, st *runner.Step,
	minHold, maxHold, pollPeriod, earlyExitWindow time.Duration,
	earlyExitTol float64) string {
	start := time.Now()
	for {
		elapsed := time.Since(start)
		if elapsed >= maxHold {
			return "full"
		}
		remaining := maxHold - elapsed
		wait := pollPeriod
		if wait > remaining {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return "cancelled"
		case <-time.After(wait):
		}
		elapsed = time.Since(start)
		if elapsed < minHold {
			continue
		}
		if stepOnTarget(s, st, earlyExitWindow, earlyExitTol) {
			return "early-stable"
		}
	}
}

// stepOnTarget is the combined early-exit predicate: it fires only when
// the step is going as we expected, i.e. buffer is stable AND the player
// is fetching the variant the cap was built for. If the player has
// downshifted (or upshifted, or is mid-transition) we hold longer to
// collect more data on the recovery path — early-exit on a wrong-variant
// step would short-change the diagnostic value of those steps.
//
// When Step.Variant is nil (non-variant-aware sweep), the variant check
// is bypassed and we fall back to pure buffer-trend stability.
func stepOnTarget(s *runner.Sampler, st *runner.Step, window time.Duration, tol float64) bool {
	if !bufferStable(s, window, tol) {
		return false
	}
	if st == nil || st.Variant == nil {
		return true
	}
	// Player reports the variant's BANDWIDTH attribute (peak per HLS
	// spec) as video_bitrate_mbps, exactly. Match the last few samples
	// to the expected peak within a 10% tolerance — that's generous
	// enough for noise but tight enough that an adjacent rung (which is
	// >40% lower or higher) doesn't accidentally match.
	expected := float64(st.Variant.PeakBps) / 1_000_000
	if expected <= 0 {
		expected = float64(st.Variant.RawBps) / 1_000_000
	}
	if expected <= 0 {
		return true
	}
	// Sample window — the latest third of the buffer-stability window.
	// We only care that the player is on-target NOW, not throughout the
	// transition.
	n := int(window / time.Second)
	if n < 10 {
		n = 10
	}
	samples := s.Recent(n)
	if len(samples) < 5 {
		return false
	}
	third := len(samples) / 3
	if third < 3 {
		third = 3
	}
	recent := samples[len(samples)-third:]
	tolMbps := expected * 0.10
	for _, smp := range recent {
		if smp.VideoBitrateMbps <= 0 {
			return false
		}
		if abs(smp.VideoBitrateMbps-expected) > tolMbps {
			return false
		}
	}
	return true
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// bufferStable returns true when the buffer hasn't trended down across the
// minStableBufferS is the floor a buffer must clear before bufferStable
// will call a step "stable" — guards against a flat near-empty buffer
// reading as settled (#632). Low enough not to disturb genuinely-stable
// steps, which sit well above it.
const minStableBufferS = 1.0

// last earlyExitWindow seconds — defined as (mean of latest third) ≥
// (mean of earliest third) - tolerance. Requires at least 10 samples in
// the window to fire (avoids declaring stability from noise).
func bufferStable(s *runner.Sampler, window time.Duration, tol float64) bool {
	// 1 Hz sampling assumed — the smooth mode uses samplePeriod=1s.
	n := int(window / time.Second)
	if n < 10 {
		n = 10
	}
	samples := s.Recent(n)
	if len(samples) < 10 {
		return false
	}
	third := len(samples) / 3
	if third < 3 {
		third = 3
	}
	early := samples[:third]
	late := samples[len(samples)-third:]
	earlyMean := meanBuffer(early)
	lateMean := meanBuffer(late)
	// #632: a buffer pinned near empty is stalled, not "stable" — a flat
	// 0→0 trace would otherwise satisfy the trend test below and trigger a
	// premature early-exit (we saw a floor step exit "early-stable" at 20s
	// with buf=0 while the player was still rebuffering off a 4K→360p
	// downshift). Require a minimally healthy buffer before calling it
	// stable, so such a step rides toward maxHold and lets the buffer fill.
	if lateMean < minStableBufferS {
		return false
	}
	return lateMean >= earlyMean-tol
}

func meanBuffer(ss []runner.Sample) float64 {
	if len(ss) == 0 {
		return 0
	}
	var sum float64
	for _, s := range ss {
		sum += s.BufferDepthS
	}
	return sum / float64(len(ss))
}

func annotateLog(t *testing.T, i, total int, st *runner.Step) {
	t.Helper()
	dur := st.EndedAt.Sub(st.StartedAt).Round(time.Second)
	tag := ""
	if st.Variant != nil {
		tag = fmt.Sprintf(" %s/%+d%% (avg=%.3f peak=%.3f)",
			st.Variant.Resolution, st.Variant.MarginPct,
			float64(st.Variant.AvgBps)/1_000_000,
			float64(st.Variant.PeakBps)/1_000_000)
	}
	t.Logf("  step %2d/%d  cap=%6.3f Mbps%s  held=%s  exit=%s  buf=%.1f→%.1f  stalls=%d  shifts=%d  bitrate=%.2f Mbps",
		i+1, total, st.RateMbps, tag, dur, st.ExitReason, st.MinBufferS, st.MaxBufferS,
		st.StallsDelta, st.ProfileShiftsDelta, st.MeanBitrateMbps)
}

// LogReport prints a one-line "where the artifacts went" so the operator
// can find them after the test exits.
func LogReport(t *testing.T, jsonPath string) {
	t.Helper()
	t.Logf("report: %s", jsonPath)
}

// RunMode is the shared shape for every characterization mode test. It
// opens a session against the requested platform, applies the supplied
// step plan, samples at samplePeriod, and writes the report to
// $CHARACTERIZATION_OUTDIR (or t.TempDir() when unset).
//
// Before applying the first step we hold at "no cap" for `warmup` so the
// player buffer settles to its natural depth — without this the first
// downshift is contaminated by "buffer was already full from launch".
//
// To run a mode with a non-standard step list, call RunSweep directly.
func RunMode(t *testing.T, platform runner.Platform, mode string,
	steps []runner.Step, samplePeriod, warmup time.Duration) {
	t.Helper()
	sess := OpenSession(t, platform)

	overall := timeoutForSteps(steps, warmup, samplePeriod)
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	if warmup > 0 {
		if err := sess.ApplyRate(ctx, 0); err != nil {
			t.Fatalf("warmup uncap: %v", err)
		}
		if err := holdContext(ctx, warmup); err != nil {
			t.Fatalf("warmup hold: %v", err)
		}
	}

	report := RunSweep(ctx, t, sess, mode, steps, samplePeriod)
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	base := mode + "-" + string(platform) + "-" + time.Now().UTC().Format("20060102T150405Z")
	jsonPath, err := runner.WriteReport(out, base, report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	LogReport(t, jsonPath)

	if report.Summary.SampleCount == 0 {
		t.Errorf("no samples collected")
	}
	if len(report.Steps) == 0 {
		t.Errorf("no steps recorded")
	}
}

// timeoutForSteps adds a 60s slack on top of the planned sweep time so
// the per-test deadline doesn't fire just because the player paused briefly.
func timeoutForSteps(steps []runner.Step, warmup, samplePeriod time.Duration) time.Duration {
	total := warmup
	for _, s := range steps {
		total += s.Hold
	}
	// Slack covers launch + the cleanup ClearShape call + any harness latency.
	total += 60 * time.Second
	if total < 90*time.Second {
		total = 90 * time.Second
	}
	return total
}
