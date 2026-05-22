package modes

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Startup — characterize the player's cold-start behavior across two
// lifecycle boundaries (app kill+launch vs channel-change to a
// different content item). See:
//
//	.claude/standards/startup-characterization-test.md
//
// for the full data-model + how-to-read-outcomes documentation. Don't
// reason about what a field means from name alone; read the doc.

// Target clip and a distinct "setup" clip. EVERY cycle measures the
// startup of the SAME target clip — keeping the destination constant
// is what lets us compare across boundary types without content
// variance polluting the result. The setup clip exists only to put
// the player in a "currently playing something else" state before a
// channel_change cycle measures the switch TO the target.
//
// Override via CHAR_STARTUP_CLIP_TARGET / CHAR_STARTUP_CLIP_SETUP env.
// Both must exist in `/api/content`; the setup clip MUST be distinct
// from the target.
const (
	defaultStartupClipTarget = "insane_fpv_shots_hydrofoil_windsurfing"
	defaultStartupClipSetup  = "bucks_bunny"
)

type startupBoundary string

const (
	startupAppCold       startupBoundary = "app_cold"
	startupChannelChange startupBoundary = "channel_change"
)

var startupBoundaries = []startupBoundary{startupAppCold, startupChannelChange}

const (
	startupObserveWindow      = 30 * time.Second
	startupBufferReach5SLimit = 20 * time.Second // pass threshold (informational, not asserted)
	startupSamplerPeriod      = 500 * time.Millisecond
)

// startupCapMarginFraction is the headroom multiplied onto each
// variant's AVERAGE-BANDWIDTH to derive its "just-pick-this-variant"
// cap. avg × (1 + headroom) × (1 + TCPOverhead) sits below the next
// variant's avg, so the player's variant-selection algorithm should
// land on this rung. Tuned to match what AVPlayer settles on in
// practice — too low and the player won't pick the intended variant;
// too high and it overshoots.
const startupCapMarginFraction = 0.20

// computeStartupCaps derives one cap per video variant from the
// manifest. Each cap is variant.avg × 1.20 × 1.07 (TCP overhead) —
// enough headroom that the player picks that variant comfortably,
// but below the next variant's avg.
//
// Returns the caps in DESCENDING order (top variant first) so the
// most interesting case (no-constraint) runs first.
//
// Override CHAR_STARTUP_CAPS=30,8,3 to bypass and use a literal list.
func computeStartupCaps(bws map[string]runner.VariantBandwidth) []float64 {
	avgs := make([]float64, 0, len(bws))
	for _, bw := range bws {
		if bw.AvgMbps > 0 {
			avgs = append(avgs, bw.AvgMbps)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(avgs)))
	out := make([]float64, 0, len(avgs))
	for _, avg := range avgs {
		cap := avg * (1 + startupCapMarginFraction) * (1 + float64(runner.TCPOverheadPct)/100)
		out = append(out, math.Round(cap*1000)/1000)
	}
	return out
}

func TestStartupIPadSim(t *testing.T)   { runStartup(t, runner.PlatformIPadSim) }
func TestStartupIPhone(t *testing.T)    { runStartup(t, runner.PlatformIPhone) }
func TestStartupAppleTV(t *testing.T)   { runStartup(t, runner.PlatformAppleTV) }
func TestStartupAndroidTV(t *testing.T) { runStartup(t, runner.PlatformAndroidTV) }
func TestStartupWeb(t *testing.T)       { runStartup(t, runner.PlatformWeb) }

func runStartup(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	target := envOr("CHAR_STARTUP_CLIP_TARGET", defaultStartupClipTarget)
	setupClip := envOr("CHAR_STARTUP_CLIP_SETUP", defaultStartupClipSetup)
	if target == setupClip {
		t.Fatalf("target clip and setup clip must differ (both = %q)", target)
	}
	reps := envInt("CHAR_STARTUP_REPS", 3)
	if reps <= 0 {
		reps = 1
	}

	picked := pickedDevice(sess)
	launcher := sess.Launcher
	appium, isAppium := launcher.(*runner.AppiumLauncher)
	if !isAppium {
		t.Skip("startup test requires -launch-mode=appium")
	}

	// Capture the manifest's variant bandwidths up front. They drive
	// the cap matrix (one cap per variant unless CHAR_STARTUP_CAPS
	// overrides) AND annotate every variant mention in the cycle logs.
	// OpenSession should leave us with a player that has its manifest
	// fetched; if not, computeStartupCaps below will return an empty
	// list and the test fails fast with a clear message.
	bws := map[string]runner.VariantBandwidth{}
	if rec, err := sess.PlayerState(context.Background()); err == nil {
		bws = runner.VariantBandwidthByResolution(rec)
	}

	// Caps: env override has priority; otherwise derive one cap per
	// variant from the manifest (variant.avg × 1.20 × 1.07 — just
	// above sustainable for THAT variant, below the next variant's
	// avg). Sorted descending so the no-constraint case runs first.
	caps := envFloatList("CHAR_STARTUP_CAPS", nil)
	if len(caps) == 0 {
		caps = computeStartupCaps(bws)
	}
	if len(caps) == 0 {
		t.Fatalf("no caps to test (manifest variants unavailable AND CHAR_STARTUP_CAPS not set)")
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	// Run-scope labels (test identity, platform, clip selection) are
	// stamped via LabelPlay directly — they're constant across all
	// cycles and only need to land ONCE. Per-cycle identity
	// (cycle_id, cycle_idx, rep, boundary, cap_mbps) is written by
	// runner.StartCycle inside runStartupCycle below; that overwrite
	// is what the dashboard's cycle-band overlay reads. See
	// .claude/standards/characterization-principles.md § 9.
	//
	// `caps_mbps` (the WHOLE list) is deliberately omitted — the
	// forwarder rejects label values containing `,` (silent drop;
	// see reference_labelplay_value_encoding.md). The active cap for
	// each cycle is carried on `cap_mbps` instead.
	runLabels := map[string]string{
		"test":        "startup",
		"platform":    string(p),
		"run_id":      runID,
		"clip_target": target,
		"clip_setup":  setupClip,
	}
	if err := sess.LabelPlay(context.Background(), runLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	} else {
		t.Logf("labeled play with %v", runLabels)
	}

	cycleCount := len(caps) * len(startupBoundaries) * reps
	// Per-cycle budget: setup (~30s app_cold, ~5s channel) + observe
	// (30s) + cleanup. Worst case ~90s per cycle + slack.
	overall := time.Duration(cycleCount)*90*time.Second + 3*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	t.Logf("cycle plan: %d cycles (%d caps × %d boundaries × %d reps), target=%s setup=%s",
		cycleCount, len(caps), len(startupBoundaries), reps, target, setupClip)
	t.Logf("caps (Mbps): %v", caps)
	t.Logf("every cycle measures startup OF %q — boundary type AND cap are the independent variables", target)

	if len(bws) > 0 {
		t.Logf("variant ladder (manifest):")
		for res, bw := range bws {
			t.Logf("  %-12s avg=%.3f peak=%.3f Mbps", res, bw.AvgMbps, bw.PeakMbps)
		}
	}

	var allCycles []runner.StartupCycleResult
	cycleIdx := 0

	for _, capMbps := range caps {
		t.Logf("== cap=%.3f Mbps ==", capMbps)
		for rep := 0; rep < reps; rep++ {
			for _, boundary := range startupBoundaries {
				cycleIdx++
				result := runStartupCycle(ctx, t, sess, appium, *picked, boundary, target, setupClip, capMbps, cycleIdx, rep, bws)
				allCycles = append(allCycles, result)
				firstAnnot := runner.AnnotateVariant(bws, result.FirstVariantPicked, capMbps)
				settledAnnot := runner.AnnotateVariant(bws, result.SettledVariant, capMbps)
				t.Logf("  [%2d] %-16s cap=%6.3f first_var=%-10s %s settled=%-10s %s ttff=%.2fs reach5sbuf=%.1fs shifts=%d stalls=%d",
					cycleIdx, boundary, capMbps,
					result.FirstVariantPicked, firstAnnot,
					result.SettledVariant, settledAnnot,
					result.TimeToFirstFrameS, result.ReachedFiveSBufferAtS,
					result.UpshiftsIn30S, result.StallsIn30S)
			}
		}
	}

	report := runner.Report{
		Mode:          "startup",
		Platform:      p,
		Device:        sess.Device,
		PlayerID:      sess.PlayerID,
		StartedAt:     time.Now().Add(-overall),
		EndedAt:       time.Now(),
		StartupCycles: allCycles,
	}
	playID, _ := sess.CurrentPlayID(context.Background())
	if playID != "" {
		report.PlayIDs = []string{playID}
	}
	if len(allCycles) > 0 {
		report.StartedAt = allCycles[0].StartedAt
		report.EndedAt = time.Now()
	}
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	base := fmt.Sprintf("startup-%s-%s-%s", p, playerShort, runID)
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

	// Close the last cycle's band explicitly so the dashboard overlay
	// renders a trailing edge. Without this, the final cycle's band
	// extends past the end-of-run in the archived view.
	if err := runner.EndCycle(context.Background(), sess); err != nil {
		t.Logf("EndCycle: %v", err)
	}
	endLabels := map[string]string{
		"completed":     time.Now().UTC().Format("20060102T150405Z"),
		"cycle_count":   fmt.Sprintf("%d", len(allCycles)),
		"settle_misses": fmt.Sprintf("%d", countSettleMisses(allCycles)),
	}
	if err := sess.LabelPlay(context.Background(), endLabels); err != nil {
		t.Logf("label play (end): %v", err)
	}

	if len(allCycles) == 0 {
		t.Fatal("no startup cycles ran")
	}
	// Informational pass criterion: every cycle reached 5s buffer
	// within the configured window. Failures here aren't necessarily
	// player bugs — could be cap-too-tight for the picked variant.
	// The detail goes in the report; we surface as t.Errorf so a
	// dashboard can flag the run for review.
	slow := []string{}
	for _, c := range allCycles {
		if c.ReachedFiveSBufferAtS == 0 || c.ReachedFiveSBufferAtS > startupBufferReach5SLimit.Seconds() {
			slow = append(slow, fmt.Sprintf("#%d %s", c.CycleIdx, c.BoundaryType))
		}
	}
	if len(slow) > 0 {
		t.Errorf("%d cycle(s) took >%.0fs to reach 5s of buffer (or never did): %v",
			len(slow), startupBufferReach5SLimit.Seconds(), slow)
	}
}

func runStartupCycle(
	ctx context.Context, t *testing.T,
	sess *runner.Session, appium *runner.AppiumLauncher, dev runner.Device,
	boundary startupBoundary, targetClip, setupClip string, capMbps float64, idx, rep int,
	bws map[string]runner.VariantBandwidth,
) runner.StartupCycleResult {
	result := runner.StartupCycleResult{
		CycleIdx:      idx,
		BoundaryType:  string(boundary),
		ContentClipID: targetClip,
		CapMbps:       capMbps,
	}

	// Stamp the cycle identity onto the player via the standard
	// schema. The label PATCH lands BEFORE the boundary fires so
	// the dashboard's cycle-band overlay shows the band starting
	// exactly at boundary-time, not after observation begins.
	if _, err := runner.StartCycle(ctx, sess, runner.CycleID{
		Test:     "startup",
		Idx:      idx,
		Rep:      rep,
		Boundary: string(boundary),
		CapMbps:  fmt.Sprintf("%g", capMbps),
	}); err != nil {
		t.Logf("[%d] StartCycle: %v (band rendering may be missing)", idx, err)
	}

	// Capture the play_id active BEFORE this cycle's boundary fires.
	// populateStartupCycleResult uses this to detect the new-play
	// transition for accurate TTFF measurement (filters out residual
	// metrics from the previous play). Best-effort — empty on the very
	// first cycle when no prior play exists.
	if priorPlayID, err := sess.CurrentPlayID(ctx); err == nil {
		result.PrePlayID = priorPlayID
	}

	// Clear any leftover proxy state from the previous cycle (or test).
	_ = sess.ClearFaults(ctx)
	_ = sess.SetSegmentTimeout(ctx, 0)

	// Boundary-specific setup. After this block the player should be
	// about to BEGIN playback of targetClip. Every cycle lands on the
	// same target — boundary type is the only experimental variable.
	// StartedAt marks t=0 for every per-cycle measurement.
	switch boundary {
	case startupAppCold:
		// Kill the app so the next launch is genuinely cold — no
		// in-process AVPlayer state, no learned bandwidth estimate.
		if err := appium.Kill(ctx, dev); err != nil {
			t.Logf("[%d] Kill: %v (continuing — app may already be killed)", idx, err)
		}
		time.Sleep(1 * time.Second)
		s, err := appium.LaunchToHome(ctx, dev)
		if err != nil {
			t.Fatalf("[%d] LaunchToHome: %v", idx, err)
		}
		pid, err := appium.ReadPlayerID(ctx, s)
		if err != nil {
			t.Fatalf("[%d] ReadPlayerID: %v (rebuild iOS app — needs home-player-id AX node)", idx, err)
		}
		s.PlayerID = pid
		result.PlayerID = pid
		sess.PlayerID = pid
		if err := s.ApplyRate(ctx, capMbps); err != nil {
			t.Fatalf("[%d] ApplyRate %.2f: %v", idx, capMbps, err)
		}
		// tc-settle gap — the proxy needs ~1-2s for kernel rules to
		// actually engage after the HTTP PATCH returns.
		time.Sleep(2 * time.Second)
		result.StartedAt = time.Now()
		// Land on the SAME target_clip every time (not Continue
		// Watching, which would be path-dependent on the last play).
		if err := appium.TapTileByClipID(ctx, s, targetClip); err != nil {
			t.Fatalf("[%d] TapTileByClipID %s: %v (LiveRow may not have rendered the tile yet)", idx, targetClip, err)
		}

	case startupChannelChange:
		// Two-phase: (a) put the player in a "currently playing
		// setupClip" state, then (b) measure the switch TO targetClip.
		// Without phase (a) we can't reliably reproduce the
		// channel_change scenario from inside a single test run.
		if err := tapPlaybackBack(ctx, appium, dev); err != nil {
			t.Logf("[%d] back-to-home (pre-setup): %v (continuing)", idx, err)
		}
		time.Sleep(500 * time.Millisecond)
		if err := appium.TapTileByClipID(ctx, sess, setupClip); err != nil {
			t.Fatalf("[%d] setup TapTileByClipID %s: %v", idx, setupClip, err)
		}
		// Let setupClip start playing properly — manifest fetched,
		// first segment in buffer — so the channel_change is a clean
		// warm-AVPlayer transition rather than "cancelled before it
		// even started." 4 s is enough on a healthy network; bump if
		// the test runs on a slower path.
		time.Sleep(4 * time.Second)

		// Now the channel change measurement begins.
		if err := tapPlaybackBack(ctx, appium, dev); err != nil {
			t.Logf("[%d] back-to-home (pre-target): %v (continuing)", idx, err)
		}
		time.Sleep(500 * time.Millisecond)
		if err := sess.ApplyRate(ctx, capMbps); err != nil {
			t.Fatalf("[%d] ApplyRate %.2f: %v", idx, capMbps, err)
		}
		time.Sleep(2 * time.Second)
		result.StartedAt = time.Now()
		if err := appium.TapTileByClipID(ctx, sess, targetClip); err != nil {
			t.Fatalf("[%d] TapTileByClipID %s: %v", idx, targetClip, err)
		}
	}

	// Start a sampler immediately after the boundary fires; observe
	// for the full window. Sampler reads via PlayerState — works as
	// long as the player is heartbeating (which it should be within
	// 2-3s of resume).
	sampler := runner.NewSampler(sess, startupSamplerPeriod)
	sampler.Start(ctx)
	defer sampler.Stop()

	if err := holdContext(ctx, startupObserveWindow); err != nil {
		t.Logf("[%d] observation window cut short: %v", idx, err)
	}

	samples := sampler.Stop()

	// Pull network rows for the window. Wait 2s for the forwarder to
	// catch up before querying. PlayID may have changed (new play),
	// so re-fetch it.
	time.Sleep(2 * time.Second)
	playID, _ := sess.CurrentPlayID(ctx)
	queryFrom := result.StartedAt.Add(-2 * time.Second)
	queryTo := time.Now()
	rows, err := runner.FetchNetworkRows(ctx, sess.PlayerID, playID, queryFrom, queryTo, 500)
	if err != nil {
		t.Logf("[%d] FetchNetworkRows: %v — first-request timings will be empty", idx, err)
	}

	result = populateStartupCycleResult(result, samples, rows, bws)
	return result
}

// populateStartupCycleResult walks the per-cycle observations (samples
// + network rows) and fills the StartupCycleResult fields. Pure — no
// network calls or proxy state mutations — so it's testable in
// isolation.
//
// Key rules (each one was a known measurement bug we fixed):
//   - First-variant + settled-variant are derived from SEGMENT fetches,
//     not playlist fetches. A playlist fetch means "the player
//     considered this variant"; a segment fetch means "the player
//     COMMITTED to it." Audio playlists are excluded from the
//     first-variant signal (they're a separate media group, not a
//     video variant).
//   - TTFF is read from the iOS player's own video_first_frame_time_s
//     measurement, sampled AFTER the new play_id has appeared. This
//     filters out residual metrics from the previous play.
//   - Shifts/stalls deltas use the LAST pre-armed sample as baseline
//     (not the FIRST post-armed sample, which already reflects any
//     shifts that happened during boundary setup).
func populateStartupCycleResult(r runner.StartupCycleResult, samples []runner.Sample, rows []runner.NetworkRow, bws map[string]runner.VariantBandwidth) runner.StartupCycleResult {
	t0 := r.StartedAt

	// Chrono-sort network rows for the rest of the walk.
	chronoRows := make([]runner.NetworkRow, len(rows))
	copy(chronoRows, rows)
	sort.Slice(chronoRows, func(i, j int) bool { return chronoRows[i].Ts.Before(chronoRows[j].Ts) })

	// First-request kinds: master / variant playlist / first segment.
	// We DON'T set FirstVariantPicked here — that comes from the
	// FIRST SEGMENT (committed) not the first playlist (peek).
	for _, row := range chronoRows {
		if row.Ts.Before(t0) {
			continue
		}
		if r.FirstMasterAtS == 0 && row.RequestKind == "master_manifest" {
			r.FirstMasterAtS = row.Ts.Sub(t0).Seconds()
		}
		if r.FirstVariantAtS == 0 && row.RequestKind == "manifest" {
			r.FirstVariantAtS = row.Ts.Sub(t0).Seconds()
		}
		if r.FirstSegmentAtS == 0 && row.RequestKind == "segment" {
			r.FirstSegmentAtS = row.Ts.Sub(t0).Seconds()
		}
	}

	// Per-variant activity (segment fetches + playlist fetches per dir).
	// Skip audio (its dir is conventionally "audio").
	activity := map[string]*runner.VariantActivity{}
	getOrInit := func(dir string) *runner.VariantActivity {
		v, ok := activity[dir]
		if !ok {
			v = &runner.VariantActivity{VariantDir: dir}
			activity[dir] = v
		}
		return v
	}
	for _, row := range chronoRows {
		if row.Ts.Before(t0) {
			continue
		}
		dir := runner.VariantDirFromPath(row.URL)
		if dir == "" || dir == "audio" {
			continue
		}
		dt := row.Ts.Sub(t0).Seconds()
		switch row.RequestKind {
		case "manifest":
			a := getOrInit(dir)
			a.PlaylistFetches++
		case "segment":
			a := getOrInit(dir)
			a.SegmentFetches++
			if a.FirstSegmentAtS == 0 {
				a.FirstSegmentAtS = dt
			}
			a.LastSegmentAtS = dt
		}
	}

	// Resolve VariantActivity to a stable, dashboard-friendly slice
	// sorted by first-segment-time (most-recent first uses first then last).
	for _, a := range activity {
		a.ActiveDurationS = math.Max(0, a.LastSegmentAtS-a.FirstSegmentAtS)
		a.PeekedButNeverUsed = a.PlaylistFetches > 0 && a.SegmentFetches == 0
		// Resolve resolution + bandwidth from the manifest if we can.
		// VariantDir is e.g. "2160p"; map it to a manifest resolution
		// by suffix matching. Heuristic: variant dir "2160p" matches
		// resolution "3840x2160".
		for res, bw := range bws {
			// resolution "3840x2160" → height "2160"; variant dir "2160p"
			// → strip trailing "p". Match on that.
			height := resolutionHeight(res)
			vd := strings.TrimSuffix(a.VariantDir, "p")
			if height != "" && height == vd {
				a.Resolution = res
				a.AvgMbps = bw.AvgMbps
				a.PeakMbps = bw.PeakMbps
				break
			}
		}
	}
	r.VariantActivity = make([]runner.VariantActivity, 0, len(activity))
	for _, a := range activity {
		r.VariantActivity = append(r.VariantActivity, *a)
	}
	sort.Slice(r.VariantActivity, func(i, j int) bool {
		// Variants that fetched ANY segment come first, ordered by
		// first-segment time. Peeked-only variants come after.
		ai, aj := r.VariantActivity[i], r.VariantActivity[j]
		if ai.SegmentFetches > 0 && aj.SegmentFetches == 0 {
			return true
		}
		if aj.SegmentFetches > 0 && ai.SegmentFetches == 0 {
			return false
		}
		return ai.FirstSegmentAtS < aj.FirstSegmentAtS
	})

	// First variant: FIRST variant the player fetched a SEGMENT from.
	// Bandwidth context from the manifest lookup. Skip audio.
	for _, a := range r.VariantActivity {
		if a.SegmentFetches > 0 {
			r.FirstVariantPicked = a.Resolution
			if r.FirstVariantPicked == "" {
				r.FirstVariantPicked = a.VariantDir
			}
			r.FirstVariantAvgMbps = a.AvgMbps
			r.FirstVariantPeakMbps = a.PeakMbps
			break
		}
	}

	// Detect new-play transition by play_id change. Pre-armed samples
	// (or post-armed samples that still carry the OLD play_id) are
	// the previous play's metrics. Find the first sample whose play_id
	// differs from PrePlayID — that's the moment the new play's
	// metrics are authoritative.
	var newPlayFirstSampleIdx = -1
	for i, s := range samples {
		if s.Ts.Before(t0) {
			continue
		}
		if r.PrePlayID == "" || (s.PlayID != "" && s.PlayID != r.PrePlayID) {
			newPlayFirstSampleIdx = i
			if s.PlayID != "" {
				r.PlayID = s.PlayID
			}
			break
		}
	}

	// Pre-arm baseline samples (the LAST sample before t0) for the
	// shifts/stalls/dropped counter deltas.
	var baselineSample *runner.Sample
	for i := range samples {
		s := samples[len(samples)-1-i] // walk reverse
		if s.Ts.Before(t0) {
			baselineSample = &s
			break
		}
	}
	if baselineSample == nil && newPlayFirstSampleIdx > 0 {
		// No pre-armed samples — use the LAST sample from the OLD play
		// (the one before the new-play transition) as the baseline.
		baselineSample = &samples[newPlayFirstSampleIdx-1]
	}

	// Sample-walk for buffer + variant trajectory + counters.
	//
	// IMPORTANT: use BufferDepthS, not BufferEndS. BufferEndS is the
	// ABSOLUTE playhead-time of the buffer end (e.g. 972.906 means
	// "the buffered data ends at second 972.906 of the stream"), not
	// the depth. On a stream that's been playing for many seconds,
	// BufferEndS is naturally hundreds of seconds and trivially
	// exceeds any depth threshold. BufferDepthS (the actual seconds
	// of buffer ahead of playhead) is the correct field — and it IS
	// reliable on the iPad sim per the runs we've inspected, despite
	// the avplayer-quirks note (which may apply only to certain iOS
	// versions or specific failure modes).
	bufField := func(s runner.Sample) float64 {
		return s.BufferDepthS
	}
	var firstNB, lastNB float64
	resAt5, resAt15, resAt30 := "", "", ""
	last10sCnts := map[string]int{}
	for _, s := range samples {
		if s.Ts.Before(t0) {
			continue
		}
		dt := s.Ts.Sub(t0).Seconds()
		if firstNB == 0 && s.NetworkBitrateMbps > 0 {
			firstNB = s.NetworkBitrateMbps
		}
		lastNB = s.NetworkBitrateMbps
		bd := bufField(s)
		if r.ReachedFiveSBufferAtS == 0 && bd >= 5.0 {
			r.ReachedFiveSBufferAtS = dt
		}
		if r.ReachedFifteenSBufferAtS == 0 && bd >= 15.0 {
			r.ReachedFifteenSBufferAtS = dt
		}
		if dt >= 5 && resAt5 == "" {
			resAt5 = s.VideoResolution
		}
		if dt >= 15 && resAt15 == "" {
			resAt15 = s.VideoResolution
		}
		if dt >= 30 && resAt30 == "" {
			resAt30 = s.VideoResolution
		}
		if dt >= 20 {
			last10sCnts[s.VideoResolution]++
		}
	}

	// TTFF: read iOS's own first-frame measurement from the FIRST
	// sample after the new-play transition that has a non-zero value.
	// This is per-play (resets when play_id changes) so it's clean
	// even when the previous play's samples were still in flight.
	var ttff float64
	if newPlayFirstSampleIdx >= 0 {
		for i := newPlayFirstSampleIdx; i < len(samples); i++ {
			if samples[i].VideoFirstFrameTimeS > 0 {
				ttff = samples[i].VideoFirstFrameTimeS
				break
			}
		}
	}
	r.TimeToFirstFrameS = ttff

	// Counter deltas — last-sample-minus-baseline. If no baseline
	// (no pre-armed sample, no new-play transition), report 0.
	if len(samples) > 0 && baselineSample != nil {
		last := samples[len(samples)-1]
		shiftDelta := max0(last.ProfileShiftCount - baselineSample.ProfileShiftCount)
		stallDelta := max0(last.Stalls - baselineSample.Stalls)
		droppedDelta := max0(last.DroppedFrames - baselineSample.DroppedFrames)
		// ProfileShiftCount is a combined counter (up + down). We don't
		// have per-direction info; lump into UpshiftsIn30S and leave
		// DownshiftsIn30S=0. Standards doc notes this.
		r.UpshiftsIn30S = shiftDelta
		r.StallsIn30S = stallDelta
		r.DroppedFramesIn30S = droppedDelta
	}

	r.NetworkBitrateAtStartMbps = firstNB
	r.NetworkBitrateAt30SMbps = lastNB
	r.VariantAt5S = resAt5
	r.VariantAt15S = resAt15
	r.VariantAt30S = resAt30

	// Settled variant: majority of SEGMENT fetches in the last 10 s.
	// Falls back to the majority sample-reported resolution if no
	// segment activity. Audio-skipped (variantDirFromPath returns
	// "audio" for audio segments; we don't include them in the
	// segment-fetch tally below).
	var settledFromSegments string
	segmentsBy := map[string]int{}
	windowEnd := t0.Add(30 * time.Second)
	windowStart := windowEnd.Add(-10 * time.Second)
	for _, row := range chronoRows {
		if row.Ts.Before(windowStart) || row.Ts.After(windowEnd) {
			continue
		}
		if row.RequestKind != "segment" {
			continue
		}
		dir := runner.VariantDirFromPath(row.URL)
		if dir == "" || dir == "audio" {
			continue
		}
		segmentsBy[dir]++
	}
	if len(segmentsBy) > 0 {
		var best string
		var bestN int
		for k, n := range segmentsBy {
			if n > bestN {
				best = k
				bestN = n
			}
		}
		// Map dir → resolution via the activity lookup we already built.
		settledFromSegments = best
		for _, a := range r.VariantActivity {
			if a.VariantDir == best && a.Resolution != "" {
				settledFromSegments = a.Resolution
				r.SettledVariantAvgMbps = a.AvgMbps
				r.SettledVariantPeakMbps = a.PeakMbps
				break
			}
		}
	}
	if settledFromSegments == "" {
		// Fallback: majority sample resolution in last-10s window.
		var best string
		var bestN int
		for k, n := range last10sCnts {
			if n > bestN {
				best = k
				bestN = n
			}
		}
		settledFromSegments = best
	}
	r.SettledVariant = settledFromSegments

	return r
}

// resolutionHeight returns the "height" portion of a manifest
// resolution string. "3840x2160" → "2160". Used to match variant
// directory names like "2160p" back to manifest entries.
func resolutionHeight(res string) string {
	i := strings.Index(res, "x")
	if i < 0 {
		return ""
	}
	return res[i+1:]
}

func tapPlaybackBack(ctx context.Context, appium *runner.AppiumLauncher, dev runner.Device) error {
	// Tap the back button on the playback screen. The button only
	// renders when PlaybackScreen is mounted, so this is best-effort.
	// We don't expose the W3C tap helper publicly on AppiumLauncher;
	// for now the back-tap is achieved via the existing LaunchToHome
	// path which itself best-effort taps back. Use that.
	_, err := appium.LaunchToHome(ctx, dev)
	return err
}

// helpers — small enough to keep alongside the test.

func pickedDevice(s *runner.Session) *runner.Device {
	if s == nil {
		return nil
	}
	d := s.Device
	return &d
}

func envOr(key, dflt string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return dflt
}

// envFloatList parses a comma-separated list of floats from env.
// Empty / unset / malformed → returns dflt unchanged.
func envFloatList(key string, dflt []float64) []float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return dflt
	}
	out := []float64{}
	for _, p := range strings.Split(v, ",") {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return dflt
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return dflt
	}
	return out
}

func envFloat(key string, dflt float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return dflt
}

func envInt(key string, dflt int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return dflt
}

func countSettleMisses(cs []runner.StartupCycleResult) int {
	n := 0
	for _, c := range cs {
		if c.SettledVariant == "" {
			n++
		}
	}
	return n
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
