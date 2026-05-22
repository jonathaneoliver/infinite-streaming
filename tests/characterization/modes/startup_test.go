package modes

import (
	"context"
	"fmt"
	"os"
	"regexp"
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

// Default clip_ids for the channel_change cycles. Override via env
// when the test-dev content roster changes. Both must exist in
// `/api/content`. Without two distinct clip_ids, channel_change can't
// run — the test skips that boundary and reports only app_cold.
const (
	defaultStartupClipA = "insane_fpv_shots_hydrofoil_windsurfing"
	defaultStartupClipB = "bucks_bunny"
)

type startupBoundary string

const (
	startupAppCold       startupBoundary = "app_cold"
	startupChannelChange startupBoundary = "channel_change"
)

var startupBoundaries = []startupBoundary{startupAppCold, startupChannelChange}

const (
	startupCapMbpsDefault     = 30.0 // wide enough to let the player pick top variant most of the time
	startupObserveWindow      = 30 * time.Second
	startupBufferReach5SLimit = 20 * time.Second // pass threshold (informational, not asserted)
	startupSamplerPeriod      = 500 * time.Millisecond
)

func TestStartupIPadSim(t *testing.T)   { runStartup(t, runner.PlatformIPadSim) }
func TestStartupIPhone(t *testing.T)    { runStartup(t, runner.PlatformIPhone) }
func TestStartupAppleTV(t *testing.T)   { runStartup(t, runner.PlatformAppleTV) }
func TestStartupAndroidTV(t *testing.T) { runStartup(t, runner.PlatformAndroidTV) }
func TestStartupWeb(t *testing.T)       { runStartup(t, runner.PlatformWeb) }

func runStartup(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	clipA := envOr("CHAR_STARTUP_CLIP_A", defaultStartupClipA)
	clipB := envOr("CHAR_STARTUP_CLIP_B", defaultStartupClipB)
	capMbps := envFloat("CHAR_STARTUP_CAP_MBPS", startupCapMbpsDefault)
	reps := envInt("CHAR_STARTUP_REPS", 3)
	if reps <= 0 {
		reps = 1
	}

	runID := time.Now().UTC().Format("20060102T150405Z")
	startLabels := map[string]string{
		"test":     "startup",
		"platform": string(p),
		"run_id":   runID,
		"clip_a":   clipA,
		"clip_b":   clipB,
		"cap_mbps": fmt.Sprintf("%.3f", capMbps),
	}
	if err := sess.LabelPlay(context.Background(), startLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	} else {
		t.Logf("labeled play with %v", startLabels)
	}

	picked := pickedDevice(sess)
	launcher := sess.Launcher
	appium, isAppium := launcher.(*runner.AppiumLauncher)
	if !isAppium {
		t.Skip("startup test requires -launch-mode=appium")
	}

	cycleCount := len(startupBoundaries) * reps
	// Per-cycle budget: setup (~30s app_cold, ~5s channel) + observe
	// (30s) + cleanup. Worst case ~90s × 6 cycles + slack.
	overall := time.Duration(cycleCount)*90*time.Second + 3*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	t.Logf("cycle plan: %d cycles (%d boundaries × %d reps), cap=%.3f Mbps, clip_a=%s clip_b=%s",
		cycleCount, len(startupBoundaries), reps, capMbps, clipA, clipB)

	var allCycles []runner.StartupCycleResult
	cycleIdx := 0

	for rep := 0; rep < reps; rep++ {
		for _, boundary := range startupBoundaries {
			cycleIdx++
			// For channel_change cycles, swap the target each rep so
			// we test both directions (A→B then B→A then A→B…).
			targetClip := clipA
			if boundary == startupChannelChange && rep%2 == 0 {
				targetClip = clipB
			}
			result := runStartupCycle(ctx, t, sess, appium, *picked, boundary, targetClip, capMbps, cycleIdx)
			allCycles = append(allCycles, result)
			t.Logf("  [%2d] %-16s clip=%-40s first_var=%-12s ttff=%.2fs reach5sbuf=%.1fs settled=%s shifts=+%d/-%d stalls=%d",
				cycleIdx, boundary, result.ContentClipID, result.FirstVariantPicked,
				result.TimeToFirstFrameS, result.ReachedFiveSBufferAtS,
				result.SettledVariant, result.UpshiftsIn30S, result.DownshiftsIn30S, result.StallsIn30S)
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

	// Post-run summary labels — one per boundary's median TTFF / reach.
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
	boundary startupBoundary, targetClip string, capMbps float64, idx int,
) runner.StartupCycleResult {
	result := runner.StartupCycleResult{
		CycleIdx:      idx,
		BoundaryType:  string(boundary),
		ContentClipID: targetClip,
		CapMbps:       capMbps,
	}

	// Clear any leftover proxy state from the previous cycle (or test).
	_ = sess.ClearFaults(ctx)
	_ = sess.SetSegmentTimeout(ctx, 0)

	// Boundary-specific setup. After this block the player should be
	// transitioning into a fresh play (or about to). StartedAt marks
	// t=0 for every per-cycle measurement.
	switch boundary {
	case startupAppCold:
		// Kill the app outright so the next launch is genuinely cold —
		// no in-process AVPlayer state, no learned bandwidth estimate.
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
		if err := s.ApplyRate(ctx, capMbps); err != nil {
			t.Fatalf("[%d] ApplyRate %.2f: %v", idx, capMbps, err)
		}
		// tc-settle gap — the proxy needs ~1-2s for kernel rules to
		// actually engage after the HTTP PATCH returns.
		time.Sleep(2 * time.Second)
		result.StartedAt = time.Now()
		if err := appium.ResumePlayback(ctx, dev); err != nil {
			t.Fatalf("[%d] ResumePlayback: %v", idx, err)
		}
		// Bind to whatever player_id ends up reporting.
		sess.PlayerID = pid

	case startupChannelChange:
		// Player should currently be playing (set up by the previous
		// cycle or by OpenSession). Tap back → home, then tap the
		// target tile to start a fresh play on a (presumably)
		// different content item.
		//
		// Note: the AX scope for the back button only matches when
		// PlaybackScreen is mounted, so a best-effort tap is fine — if
		// we're already on Home (no in-flight playback) the tap is a
		// no-op.
		if err := tapPlaybackBack(ctx, appium, dev); err != nil {
			t.Logf("[%d] back-to-home: %v (continuing — may already be on home)", idx, err)
		}
		time.Sleep(500 * time.Millisecond)
		// Reapply the cap before the next play starts so the new play
		// fetches under the throttle. PlayerID survives kill+launch on
		// iOS (UserDefaults-backed) but channel_change doesn't relaunch
		// the app, so the same player_id is fine.
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

	result = populateStartupCycleResult(result, samples, rows)
	return result
}

// populateStartupCycleResult walks the per-cycle observations (samples
// + network rows) and fills the StartupCycleResult fields. Pure — no
// network calls or proxy state mutations — so it's testable in
// isolation.
func populateStartupCycleResult(r runner.StartupCycleResult, samples []runner.Sample, rows []runner.NetworkRow) runner.StartupCycleResult {
	t0 := r.StartedAt

	// First-request kinds. Rows are typically newest-first; flip so
	// we walk chronologically.
	chronoRows := make([]runner.NetworkRow, len(rows))
	copy(chronoRows, rows)
	// Stable sort would be nicer; for now do a simple bubble (n is small).
	for i := 0; i < len(chronoRows); i++ {
		for j := i + 1; j < len(chronoRows); j++ {
			if chronoRows[j].Ts.Before(chronoRows[i].Ts) {
				chronoRows[i], chronoRows[j] = chronoRows[j], chronoRows[i]
			}
		}
	}
	var firstReqs []runner.NetworkRow
	for _, row := range chronoRows {
		if row.Ts.Before(t0) {
			continue
		}
		if r.FirstMasterAtS == 0 && row.RequestKind == "master_manifest" {
			r.FirstMasterAtS = row.Ts.Sub(t0).Seconds()
		}
		if r.FirstVariantAtS == 0 && row.RequestKind == "manifest" {
			r.FirstVariantAtS = row.Ts.Sub(t0).Seconds()
			r.FirstVariantPicked = variantFromURL(row.URL)
		}
		if r.FirstSegmentAtS == 0 && row.RequestKind == "segment" {
			r.FirstSegmentAtS = row.Ts.Sub(t0).Seconds()
		}
		if len(firstReqs) < 5 {
			firstReqs = append(firstReqs, row)
		}
	}
	if len(firstReqs) > 0 {
		r.FirstReqDNSMs = medianFloat(firstReqs, func(n runner.NetworkRow) float64 { return 0 })  // dns_ms not in NetworkRow yet
		r.FirstReqConnectMs = medianFloat(firstReqs, func(n runner.NetworkRow) float64 { return 0 }) // connect_ms not in NetworkRow yet
		r.FirstReqTLSMs = medianFloat(firstReqs, func(n runner.NetworkRow) float64 { return 0 })     // tls_ms not in NetworkRow yet
	}

	// Sample-derived fields. Walk samples and extract trajectory.
	var stallStart, upStart int
	var prevStalls, prevShifts int
	var firstNB float64
	var lastNB float64
	var firstTTFF float64
	resAt5, resAt15, resAt30 := "", "", ""
	cntByRes := map[string]int{}
	last10sCnts := map[string]int{}
	var maxBuffer float64
	for _, s := range samples {
		if s.Ts.Before(t0) {
			continue
		}
		dt := s.Ts.Sub(t0).Seconds()
		if firstNB == 0 && s.NetworkBitrateMbps > 0 {
			firstNB = s.NetworkBitrateMbps
		}
		lastNB = s.NetworkBitrateMbps
		// initial buffer thresholds
		if s.BufferDepthS > maxBuffer {
			maxBuffer = s.BufferDepthS
		}
		if r.ReachedFiveSBufferAtS == 0 && s.BufferDepthS >= 5.0 {
			r.ReachedFiveSBufferAtS = dt
		}
		if r.ReachedFifteenSBufferAtS == 0 && s.BufferDepthS >= 15.0 {
			r.ReachedFifteenSBufferAtS = dt
		}
		if firstTTFF == 0 && s.VideoBitrateMbps > 0 {
			firstTTFF = dt
		}
		// variant trajectory
		if s.VideoResolution != "" {
			cntByRes[s.VideoResolution]++
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
		// counter deltas — captured at end via prev-tracking
		if upStart == 0 {
			prevShifts = s.ProfileShiftCount
			prevStalls = s.Stalls
			upStart = 1
		}
		_ = stallStart
	}
	if upStart > 0 && len(samples) > 0 {
		last := samples[len(samples)-1]
		r.UpshiftsIn30S = 0 // we don't separate upshifts vs downshifts in the sample; profile_shift_count covers both
		// Compute combined shift delta (rampdown's per-step machinery has up/down separation; we don't here).
		r.UpshiftsIn30S = max0(last.ProfileShiftCount - prevShifts)
		r.StallsIn30S = max0(last.Stalls - prevStalls)
		r.DroppedFramesIn30S = max0(last.DroppedFrames)
	}
	r.NetworkBitrateAtStartMbps = firstNB
	r.NetworkBitrateAt30SMbps = lastNB
	r.TimeToFirstFrameS = firstTTFF
	r.VariantAt5S = resAt5
	r.VariantAt15S = resAt15
	r.VariantAt30S = resAt30
	// settled variant = majority in last 10s
	var best string
	var bestN int
	for k, n := range last10sCnts {
		if n > bestN {
			best = k
			bestN = n
		}
	}
	r.SettledVariant = best

	return r
}

func variantFromURL(url string) string {
	// e.g. ".../playlist_6s_2160p.m3u8" → "2160p"
	re := regexp.MustCompile(`playlist[_-]?\d*s?[_-]?([A-Za-z0-9]+)\.m3u8`)
	if m := re.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
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

func medianFloat(rows []runner.NetworkRow, pick func(runner.NetworkRow) float64) float64 {
	if len(rows) == 0 {
		return 0
	}
	xs := make([]float64, len(rows))
	for i, r := range rows {
		xs[i] = pick(r)
	}
	// simple sort
	for i := 0; i < len(xs); i++ {
		for j := i + 1; j < len(xs); j++ {
			if xs[j] < xs[i] {
				xs[i], xs[j] = xs[j], xs[i]
			}
		}
	}
	return xs[len(xs)/2]
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
