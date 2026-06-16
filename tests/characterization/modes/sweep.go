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
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// conservativeWarmupCap is the cold-start cap used when neither a prior
// heartbeating player nor a freshly-parsed master yields a floor. 1.5 Mbps is
// well above any realistic 360p variant rate and below most 540p rates, so the
// player picks the bottom variant on cold start regardless. Lives here (not a
// _test.go file) so the non-test helpers above can reference it.
const conservativeWarmupCap = 1.5

// resolveFloor picks the cold-start cap for config-on-connect (#714):
//  1. the prior play's manifest (preFloor) when a heartbeating player exists;
//  2. else the master parsed directly off the wire — no prior play needed;
//  3. else the conservative cap.
//
// floorFn maps a parsed ladder to the mode's floor (rampupFloorFrom /
// pyramidFloorFrom).
func resolveFloor(ctx context.Context, t *testing.T, preFloor float64, floorFn func([]runner.VariantRate) float64) float64 {
	t.Helper()
	if preFloor > 0 {
		return preFloor
	}
	if rates, err := runner.MasterLadder(ctx, ""); err == nil {
		if f := floorFn(rates); f > 0 {
			t.Logf("floor %.3f Mbps parsed from master (no prior play needed)", f)
			return f
		}
	} else {
		t.Logf("master parse for floor failed: %v", err)
	}
	t.Logf("no prior floor and master parse failed — conservative %.3f Mbps cap", conservativeWarmupCap)
	return conservativeWarmupCap
}

// wireConfigOnConnect selects the config-on-connect driver and sets the launch
// args (+ runs the curl for Approach A) for a single cold launch (#714).
//
// Which side puts the proxy.* config on a URL is chosen by CHAR_PROXY_CONFIG:
//
//   - "curl" (default): the HARNESS pre-configures the session via a bootstrap
//     curl (ConfigureOnConnect) and launches the app with a bare player_id.
//     Preferred — the harness fetches + parses the master itself before the app
//     streams a single segment, so it knows the variant ladder up front.
//   - "app": the PLAYER puts proxy.shape.rate_mbps on its own bootstrap URL via
//     the -is.proxy_query launch arg (no pre-flight curl).
//
// Two opt-in init knobs (0 / 0 ⇒ legacy rate-only behaviour, unchanged):
//
//   - xferTimeout>0 folds the server-side active transfer timeout into the proxy
//     config so it's armed before the first segment (not just via a post-bind
//     PATCH). Both drivers materialize it identically (ShapeConfig / appProxyQuery).
//   - peakClampMbps>0 sets the app's startup peak-bitrate clamp via the
//     -is.flag.peak_bitrate_mbps launch arg, so AVPlayer cold-starts on a variant
//     the cap can sustain instead of reaching for the top rung (#683 clamps then
//     releases after first frame). It's a client launch arg, independent of which
//     side carries the proxy config. Callers derive it with peakClampForCap.
//
// Modes that characterize the player's *natural* startup pick (e.g. startup)
// must pass 0 so they don't perturb what they measure.
//
// baseArgs are any other launch args (e.g. segment).
// groupBroadcast selects the group semantics when groupID is set: true is the
// pyramid-style auto-broadcast group (a member PATCH mirrors to all members);
// false is a display-only group (members share a group_id for dashboard compare
// but are shaped/labelled independently) — the startup fleet uses false.
// Ignored when groupID is empty.
// contentCfg carries this member's per-member content treatment (e.g.
// allowed_variants keep-set from an A/B arm), applied at ALLOCATE so it lands
// before the player's first master fetch. nil ⇒ no treatment (full ladder).
// Content is not broadcast-eligible, so it stays per-member regardless of the
// group's broadcast mode.
func wireConfigOnConnect(ctx context.Context, t *testing.T, appium *runner.AppiumLauncher, baseArgs []string, pid string, capMbps float64, xferTimeout time.Duration, peakClampMbps int, groupID string, groupBroadcast bool, contentCfg runner.BootstrapConfig) {
	t.Helper()
	launchArgs := append(append([]string{}, baseArgs...), "-is.player_id", pid)
	// Force play_id rotation OFF for every characterization launch
	// (is.flag.play_id_rotation_s). A device with a non-zero "Rotate play_id
	// (soak runs)" value saved in UserDefaults otherwise mints a fresh play
	// mid-run, fragmenting the run's data across multiple — often UNLABELLED —
	// plays (seen on a real iPhone: one pyramid split into 3 unlabelled plays,
	// making per-play ABR analysis impossible). The NSArgumentDomain override
	// wins over the persisted value, so this pins it off regardless of what the
	// device has saved.
	launchArgs = append(launchArgs, "-is.flag.play_id_rotation_s", "0")
	// Pin two more launch-state flags for appium-driven characterization
	// (NSArgumentDomain overrides any saved value; CLI mode never reaches here —
	// wireConfigOnConnect is appium-only):
	//   - skip_home OFF: land on Home so LaunchToHome + ResumePlayback drive the
	//     ONE intended play. With skip_home ON the app cold-launches straight into
	//     playback on lastPlayed — consuming the minted player_id on the wrong
	//     content — and the harness then backs out, minting an extra unlabelled
	//     play (same data-fragmentation class as rotation).
	//   - dev_mode ON: render the on-screen HUD (PlaybackScreen gates the metrics
	//     overlay on developerMode) for live observation during runs.
	launchArgs = append(launchArgs,
		"-is.flag.skip_home", "false",
		"-is.flag.dev_mode", "true",
	)
	// CHAR_CONTENT pins the clip EVERY device plays (e.g. "insane_new_p200_h264"),
	// overriding each device's own lastPlayed so the whole fleet streams identical
	// content — required for apples-to-apples ABR comparison (otherwise each device
	// resumes its own clip with its own variant ladder). Forced via the
	// is.lastPlayed NSArgumentDomain key: ResumePlayback's continue-watching hero
	// resolves to this clip. Value is the full catalogue `name`. Unset = today's
	// behaviour (each device resumes its own lastPlayed / the featured clip).
	if clip := os.Getenv("CHAR_CONTENT"); clip != "" {
		launchArgs = append(launchArgs, "-is.lastPlayed", clip)
	}
	if peakClampMbps > 0 {
		launchArgs = append(launchArgs, "-is.flag.peak_bitrate_mbps", strconv.Itoa(peakClampMbps))
	}
	if os.Getenv("CHAR_PROXY_CONFIG") == "app" {
		launchArgs = append(launchArgs, "-is.proxy_query", appProxyQuery(capMbps, xferTimeout, groupID, groupBroadcast, contentCfg))
		appium.SetLaunchArgs(launchArgs)
		t.Logf("config-on-connect VIA APP URL: rate=%.3f Mbps xfer_timeout=%s peak_clamp=%dMbps group=%q broadcast=%v content_keys=%d (no curl); player_id=%s", capMbps, xferTimeout, peakClampMbps, groupID, groupBroadcast, len(contentCfg), pid)
		return
	}
	appium.SetLaunchArgs(launchArgs)
	// capMbps<=0 ⇒ no shape (uncapped) — e.g. a born-grouped transient_shock
	// whose curl carries only player_id + group_id, so the session joins the
	// group at connect yet warms uncapped to its top variant.
	var cfg runner.BootstrapConfig
	if capMbps > 0 {
		cfg = runner.ShapeConfig(capMbps, xferTimeout)
	}
	// Merge the per-member content treatment into the allocate so it's live from
	// the player's first master fetch (e.g. an A/B arm's allowed_variants keep-set).
	for k, v := range contentCfg {
		if cfg == nil {
			cfg = runner.BootstrapConfig{}
		}
		cfg[k] = v
	}
	if err := runner.ConfigureOnConnect(ctx, pid, groupID, groupBroadcast, cfg); err != nil {
		t.Fatalf("ConfigureOnConnect (player_id=%s cap=%.3f Mbps group=%q): %v", pid, capMbps, groupID, err)
	}
	t.Logf("config-on-connect VIA CURL (default): session %s cap=%.3f Mbps xfer_timeout=%s peak_clamp=%dMbps group=%q broadcast=%v content_keys=%d before launch", pid, capMbps, xferTimeout, peakClampMbps, groupID, groupBroadcast, len(contentCfg))
}

// peakClampForCap maps a network cap (Mbps) to the app's whole-Mbps startup
// peak-bitrate clamp, never collapsing a real cap to 0 (which reads as "off").
func peakClampForCap(capMbps float64) int {
	m := int(math.Round(capMbps))
	if m < 1 && capMbps > 0 {
		m = 1
	}
	return m
}

// appProxyQuery builds the -is.proxy_query value for CHAR_PROXY_CONFIG=app: the
// rate cap plus, when xferTimeout>0, the segment transfer timeout. Mirrors the
// curl-mode ShapeConfig so both drivers materialize the same session.
func appProxyQuery(capMbps float64, xferTimeout time.Duration, groupID string, groupBroadcast bool, contentCfg runner.BootstrapConfig) string {
	var parts []string
	if capMbps > 0 {
		parts = append(parts, fmt.Sprintf("proxy.shape.rate_mbps=%g", capMbps))
		if xferTimeout > 0 {
			parts = append(parts,
				fmt.Sprintf("proxy.transfer_timeouts.active_timeout_seconds=%d", int(xferTimeout.Seconds())),
				"proxy.transfer_timeouts.applies_segments=true")
		}
	}
	// Per-member content treatment rides the same proxy.<path> form (keys are
	// stable-sorted so the URL is deterministic).
	keys := make([]string, 0, len(contentCfg))
	for k := range contentCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, "proxy."+k+"="+contentCfg[k])
	}
	if groupID != "" {
		parts = append(parts, "group_id="+groupID)
		if !groupBroadcast {
			parts = append(parts, "group_broadcast=false")
		}
	}
	return strings.Join(parts, "&")
}

// clipIDFromContent derives the app's clip_id from a catalogue name by stripping
// the _p200_<codec> suffix (mirrors the iOS ContentItem.deriveClipId and
// go-upload's splitClipIDAndCodec). "insane_new_p200_h264" → "insane_new". Used
// to tap the clip-specific home-tile-<clip_id> so a pinned-content run lands on
// the intended clip rather than the racy continue-watching hero.
func clipIDFromContent(name string) string {
	lower := strings.ToLower(name)
	if i := strings.Index(lower, "_p200_"); i >= 0 {
		return lower[:i]
	}
	return lower
}

// armContentConfig resolves this fleet member's per-member content treatment for
// an A/B run, applied at ALLOCATE (left of the group barrier — content is not
// broadcast-eligible, so it stays per-member, even inside a CHAR_FLEET_GROUP).
// Two independent, combinable knobs:
//
//   - CHAR_ARM_<idx>_STRIP_AVG_BW=1|true strips the AVERAGE-BANDWIDTH attribute
//     from this member's master playlist (proxy key content.strip_average_bandwidth).
//     Lets an A/B group compare the SAME shaping with vs without AVERAGE-BANDWIDTH
//     present, to see whether the player's ABR (and thus the charts) diverges when
//     it can only see peak BANDWIDTH.
//   - CHAR_ARM_<idx>_VARIANT_KEEP (e.g. "every_other") selects a variant keep-rule;
//     the keep-set is computed from the catalogue ladder for CHAR_CONTENT — read
//     straight from /api/content, which is fault-immune and session-independent —
//     and resolved to RESOLUTION terms (the proxy matches allowed_variants by
//     resolution).
//
// Returns nil when neither knob is set for this index (member streams the full
// ladder, AVERAGE-BANDWIDTH intact).
func armContentConfig(ctx context.Context, t *testing.T, fleetIndex int) runner.BootstrapConfig {
	cfg := runner.BootstrapConfig{}

	if v := strings.TrimSpace(os.Getenv(fmt.Sprintf("CHAR_ARM_%d_STRIP_AVG_BW", fleetIndex))); v == "1" || strings.EqualFold(v, "true") {
		cfg["content.strip_average_bandwidth"] = "true"
		t.Logf("arm[%d] strip_average_bandwidth=true", fleetIndex)
	}

	rule := strings.TrimSpace(os.Getenv(fmt.Sprintf("CHAR_ARM_%d_VARIANT_KEEP", fleetIndex)))
	if rule != "" && rule != "all" {
		clip := strings.TrimSpace(os.Getenv("CHAR_CONTENT"))
		if clip == "" {
			t.Logf("arm[%d] variant_keep=%q ignored: CHAR_CONTENT unset (can't resolve the ladder)", fleetIndex, rule)
		} else if variants, err := runner.FetchContentVariants(ctx, clip); err != nil {
			t.Logf("arm[%d] variant_keep=%q: %v — falling back to full ladder", fleetIndex, rule, err)
		} else if keep := runner.ApplyKeep(rule, variants); len(keep) == 0 {
			t.Logf("arm[%d] variant_keep=%q resolved to no thinning (full ladder)", fleetIndex, rule)
		} else {
			t.Logf("arm[%d] variant_keep=%q → kept %d/%d rungs: %v", fleetIndex, rule, len(keep), len(variants), keep)
			for k, v := range runner.ContentAllowedVariantsConfig(keep) {
				cfg[k] = v
			}
		}
	}

	if len(cfg) == 0 {
		return nil
	}
	return cfg
}

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
	return OpenSessionOnDevice(t, platform, nil, nil)
}

// OpenSessionOnDevice is the device-explicit, optionally fleet-synchronized
// form of OpenSession. It is the launch path for modes that start the player
// HIGH/uncapped (rampdown, startup) and need to run as a fleet.
//
//   - dev == nil  → resolve the single device the legacy way (Discover →
//     first-match, honouring CHARACTERIZATION_DEVICE_UDID). This is exactly
//     OpenSession's behaviour and keeps the single-device callers
//     (abort / startup_caps / state_residency / playback_end) unchanged.
//   - dev != nil  → launch against THAT explicit device (no Discover-pick).
//
// When bars != nil AND the launcher is Appium, the launch is split so the home
// barrier gates the simultaneous playback start across the fleet:
// LaunchToHome → bars.home.arriveAndWait → ResumePlayback → WaitForHeartbeat.
// Otherwise it uses launcher.Launch (the single-shot home+resume+heartbeat),
// byte-for-byte identical to the legacy path. The caller wires the SWEEP
// barrier itself (right before its shaping sweep) — this helper owns the HOME
// barrier (including dropping itself from it if bring-up fails).
func OpenSessionOnDevice(t *testing.T, platform runner.Platform, dev *runner.Device, bars *fleetBarriers) *runner.Session {
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

	var picked *runner.Device
	if dev != nil {
		// Device-explicit (fleet or honoured single dev) — no Discover pick.
		d := *dev
		picked = &d
		t.Logf("device: %s (fleet index %d)", picked, d.FleetIndex)
	} else {
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
	}

	var sess *runner.Session
	if appium, isAppium := launcher.(*runner.AppiumLauncher); isAppium && bars != nil {
		// Fleet path: split the launch so the home barrier can gate the
		// simultaneous playback start. The launcher already carries any launch
		// args the caller set (none for the high-start modes). A generous
		// setup window — an early sim holds at the home barrier until the last
		// (most-staggered) sim arrives.
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer setupCancel()
		// If we die before reaching the home barrier (e.g. LaunchToHome fails),
		// drop ourselves from its expected set so the survivors still release
		// together. t.Fatalf runs deferred funcs via runtime.Goexit.
		homeArrived := false
		defer func() {
			if !homeArrived {
				bars.home.giveUp()
			}
		}()
		s, lerr := appium.LaunchToHome(setupCtx, *picked)
		if lerr != nil {
			t.Fatalf("LaunchToHome: %v", lerr)
		}
		// Fleet sync #1 (home): hold until every sim is at home, then all start
		// playback at once.
		homeArrived = true
		t.Logf("at home — waiting at fleet HOME barrier (playback starts together)")
		bars.home.arriveAndWait(setupCtx)
		t.Logf("HOME barrier released — starting playback")
		if rerr := appium.ResumePlayback(setupCtx, *picked); rerr != nil {
			t.Fatalf("ResumePlayback: %v", rerr)
		}
		if herr := s.WaitForHeartbeat(setupCtx, 90*time.Second); herr != nil {
			t.Fatalf("WaitForHeartbeat: %v", herr)
		}
		sess = s
	} else {
		s, lerr := launcher.Launch(ctx, *picked)
		if lerr != nil {
			t.Fatalf("launch %s: %v", picked, lerr)
		}
		sess = s
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := sess.ClearShape(ctx); err != nil {
			t.Logf("clear shape: %v", err)
		}
		// #627: close the play the way a user does — drive the app's own
		// back navigation so it emits a real client play_end and the play
		// shows up cleanly ended in the sessions view. No-op for CLI /
		// Manual launchers. Must run before Close() tears the session down.
		if err := sess.CloseViaUI(ctx); err != nil {
			t.Logf("close playback via UI: %v", err)
		}
		// #714: free the session slot immediately (don't wait for the 5-min
		// reaper) — config-on-connect mints a fresh player_id per run, so a few
		// back-to-back runs would otherwise exhaust the small session pool.
		if err := sess.Release(ctx); err != nil {
			t.Logf("release session: %v", err)
		}
		if err := launcher.Close(); err != nil {
			t.Logf("close launcher: %v", err)
		}
		// #627: optionally release the device (terminate WDA so the iOS
		// "Automation Running" overlay clears). Opt-in via
		// CHAR_RELEASE_DEVICE=1; default keeps WDA resident for fast reuse.
		if err := sess.ReleaseDevice(ctx); err != nil {
			t.Logf("release device: %v", err)
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
		if i == 0 {
			t.Logf("PATTERN-START %s first step ApplyRate=%.3f Mbps utc=%s", mode, st.RateMbps, time.Now().UTC().Format("15:04:05.000"))
		}
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

// RunCycledVariantSweep runs `steps` as a variant sweep `reps` times on the
// SAME live play — no relaunch between cycles. Each cycle is bracketed by
// StartCycle/EndCycle so the dashboard's cycle-band overlay separates the
// reps, and the cap jump between the last step of one cycle and the first
// step of the next is a real, observable transition: rampup drops top→floor,
// rampdown jumps floor→top, pyramid floor→floor. Watching the player
// re-converge across that jump — with buffer + current variant carried over,
// not reset by a relaunch — is the point of running multiple cycles.
//
// Returns one *Report per cycle, each with its own samples (RunVariantSweep
// starts/stops a fresh Sampler per call). `variants` is applied to every
// report's .Variants; `playID` (when non-empty) to .PlayIDs. Finalize runs
// per cycle. The caller writes + validates each returned report — the
// per-mode pass criteria differ, so they stay out here.
func RunCycledVariantSweep(
	ctx context.Context, t *testing.T, sess *runner.Session, mode string,
	steps []runner.Step, reps int, variants []runner.VariantRate, playID string,
	samplePeriod, minHold, maxHold, earlyExitWindow time.Duration, earlyExitTol float64,
) []*runner.Report {
	t.Helper()
	if reps < 1 {
		reps = 1
	}
	reports := make([]*runner.Report, 0, reps)
	for rep := 1; rep <= reps; rep++ {
		// cap_mbps is "none" at the cycle level — the cap VARIES within
		// the sweep, so the cycle label can't pin one value. Idx and Rep
		// both track the rep counter (these cycles repeat the same sweep,
		// not a (boundary,fault,cap) tuple).
		if _, err := runner.StartCycle(ctx, sess, runner.CycleID{
			Test: mode, Idx: rep, Rep: rep,
		}); err != nil {
			t.Logf("[%s cycle %d/%d] StartCycle: %v (band may be missing)", mode, rep, reps, err)
		}
		t.Logf("════ %s cycle %d/%d ════", mode, rep, reps)
		// Fresh copy per cycle — RunVariantSweep mutates Step timing /
		// exit fields in place, so reps must not share the backing array.
		cycleSteps := make([]runner.Step, len(steps))
		copy(cycleSteps, steps)
		report := RunVariantSweep(ctx, t, sess, mode, cycleSteps, samplePeriod,
			minHold, maxHold, earlyExitWindow, earlyExitTol)
		report.Variants = variants
		if playID != "" {
			report.PlayIDs = []string{playID}
		}
		report.Finalize(time.Now())
		reports = append(reports, report)
	}
	// Close the trailing cycle band so the archive shows a definite edge.
	if err := runner.EndCycle(ctx, sess); err != nil {
		t.Logf("EndCycle: %v", err)
	}
	return reports
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
// minHold. Returns the exit reason: "full" / "early-stable" / "cancelled" /
// "const".
func holdWithEarlyExit(ctx context.Context, s *runner.Sampler, st *runner.Step,
	minHold, maxHold, pollPeriod, earlyExitWindow time.Duration,
	earlyExitTol float64) string {
	// CHAR_STEP_S forces a CONSTANT step duration across every variant-sweep
	// mode (rampup / rampdown / pyramid): hold each step exactly this many
	// seconds and disable early-exit entirely. This matters for group/leader
	// runs — early-exit advances on the LEADER's settling, then the broadcast
	// imposes that leader-timed cadence on every member, so observers see
	// identical caps but leader-dependent durations. A fixed duration makes the
	// sweep a pure open-loop stimulus: same caps AND same dwell for all devices,
	// so any divergence is genuine per-device ABR response. Unset / <=0 keeps
	// the historical leader-gated early-exit behaviour below.
	if secs := envInt("CHAR_STEP_S", 0); secs > 0 {
		select {
		case <-ctx.Done():
			return "cancelled"
		case <-time.After(time.Duration(secs) * time.Second):
			return "const"
		}
	}
	// CHAR_NO_EARLY_EXIT disables the early-exit predicate ONLY (orthogonal to
	// CHAR_STEP_S): every step then runs to its full maxHold instead of
	// advancing on the leader's settling. Use it to hold the existing per-step
	// maxHold without committing to a single fixed duration. CHAR_STEP_S, if
	// set, already returned above and takes precedence.
	noEarlyExit := envInt("CHAR_NO_EARLY_EXIT", 0) > 0
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
		if !noEarlyExit && stepOnTarget(s, st, earlyExitWindow, earlyExitTol) {
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

// env helpers — small generic readers shared across modes. They live in this
// NON-test file (not startup_test.go) so the non-test fleet.go can call envInt
// for CHAR_FLEET_COUNT / CHAR_FLEET_STAGGER_SEC.

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
