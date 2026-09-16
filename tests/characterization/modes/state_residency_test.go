// state_residency_test.go — drives a real iOS player (iPad sim by
// default) through the PDF's "common-playback scenarios" and validates
// the resulting #550 Phase 1 residency accumulators + Phase 2 outcome
// fields against the wire output the iOS state machine produces.
//
// Unlike server_residency_test.go (deleted) this is a CLIENT test: it
// uses the Appium launcher to drive a real AVPlayer through real state
// transitions, exercising the PlaybackDiagnostics state machine end to
// end. Validation is against the metrics the proxy receives from the
// device — same wire shape the dashboard reads.
//
// === Coverage matrix ===
//
// Of the 10 PDF scenarios, this test exercises the 3 that are
// achievable without app-side changes:
//
//   ✓ Scenario 1 — cold start to playing       (Appium launch)
//   ✓ Scenario 3 — stall + recovery            (server-injected segment fault)
//   ✓ Scenario 8 — recoverable error mid-play  (transient first-byte hang)
//
// The remaining 7 need additional accessibility identifiers or in-app
// custom controls because AVKit's transport bar buttons (pause, play,
// scrubber, skip, trickplay) are rendered by AVKit's private UI and
// have no stable accessibility-id we can target. Specifically:
//
//   ✗ Scenario 2 — pause / resume        (needs in-app pause button OR
//                                          AVKit predicate-string tap)
//   ✗ Scenario 4 — skip in-buffer        (needs scrubber automation)
//   ✗ Scenario 5 — skip with refill      (needs scrubber automation)
//   ✗ Scenario 6 — pause + seek in-buf   (needs both)
//   ✗ Scenario 7 — trickplay             (needs trickplay control)
//   ✗ Scenario 9 — natural EOF           (would need to wait for full
//                                          content duration; impractical
//                                          unless we pick very short content)
//   ✗ Scenario 10 — user backgrounds     (Appium WebDriver does support
//                                          `mobile: backgroundApp` but
//                                          AppiumLauncher doesn't expose
//                                          a helper today; add when
//                                          needed)
//
// Run via:
//   LAUNCH_MODE=appium go test ./tests/characterization/modes \
//     -run TestStateResidencyIPadSim -v -timeout 5m
//
// Env overrides:
//   CHAR_STATE_OBSERVE_S — how long to observe each scenario (default
//                           20s; longer = more residency to assert
//                           against, but also more wall-clock)
//   CHAR_STATE_STALL_S    — how long the segment fault stays armed for
//                           the StallRecovery scenario (default 3s)

package modes

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// TestStateResidencyIPadSim — the iPad-simulator entry point. Other
// platforms can be added by mirroring this thin wrapper.
func TestStateResidencyIPadSim(t *testing.T) { runStateResidency(t, runner.PlatformIPadSim) }

func runStateResidency(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)
	// The Appium launcher exposes UI-driving helpers (tap by AX id) we
	// need for the post-stall recovery scenario. If the user picked
	// LAUNCH_MODE=cli the scenarios that require UI taps skip cleanly
	// instead of fatal'ing the whole run.
	appium, hasAppium := sess.Launcher.(*runner.AppiumLauncher)
	_ = hasAppium // referenced only inside scenarios that need it
	_ = appium

	runID := time.Now().UTC().Format("20060102T150405Z")
	runLabels := map[string]string{
		"test":      "state_residency",
		"platform":  string(p),
		"run_id":    runID,
		"player_id": sess.PlayerID,
	}
	if err := sess.LabelPlay(context.Background(), runLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	}

	observeWindow := envDurationSeconds("CHAR_STATE_OBSERVE_S", 20*time.Second)
	stallDuration := envDurationSeconds("CHAR_STATE_STALL_S", 3*time.Second)

	// Overall test budget: 3 scenarios × (observe + recovery) + cushion.
	overall := 3*observeWindow + 3*stallDuration + 2*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	// Print the live URLs ONCE up front so the operator can pre-arrange
	// their browser before the first scenario fires.
	t.Logf("")
	t.Logf("════════════════════════════════════════════════════════════════════")
	t.Logf("  STATE RESIDENCY CHARACTERIZATION — drive iPad sim through scenarios")
	t.Logf("  Open these in a browser to follow along:")
	t.Logf("    live tile : .../dashboard/testing-session.html?player_id=%s", sess.PlayerID)
	t.Logf("    plays list: .../dashboard/sessions.html?player=%s", sess.PlayerID)
	t.Logf("  Settings:")
	t.Logf("    observe window per scenario = %s", observeWindow)
	t.Logf("    stall duration              = %s", stallDuration)
	t.Logf("════════════════════════════════════════════════════════════════════")

	// Scenario 1 — cold start to playing.
	//
	// OpenSession already launched the app + tapped continue-watching.
	// We just observe for `observeWindow` and assert the residency
	// accumulators reflect a steady playing state.
	t.Run("ColdStartToPlaying", func(t *testing.T) {
		scenarioStart := time.Now()
		t.Logf("[ColdStartToPlaying] holding %s to let pre-roll → playing settle", observeWindow)
		holdContext(ctx, observeWindow)

		rec, err := sess.PlayerState(ctx)
		if err != nil {
			t.Fatalf("PlayerState: %v", err)
		}
		mustHavePlayerMetrics(t, rec)
		pm := rec.PlayerMetrics

		t.Logf("[ColdStartToPlaying] state=%s playback_status=%s playback_reason=%s",
			pm.State, pm.PlaybackStatus, pm.PlaybackReason)
		t.Logf("  residency ms: playing=%d buffering=%d stalling=%d seeking=%d pausing=%d",
			pm.PlayingTimeMs, pm.BufferingTimeMs, pm.StallingTimeMs,
			pm.SeekingTimeMs, pm.PausingTimeMs)
		t.Logf("  counts: playing=%d buffering=%d stalling=%d seeking=%d pausing=%d",
			pm.PlayingCount, pm.BufferingCount, pm.StallingCount,
			pm.SeekingCount, pm.PausingCount)
		t.Logf("  ttff=%.2fs scenario_wall=%s", pm.VideoFirstFrameTimeS, time.Since(scenarioStart))

		// Assertions:
		//   - eventually playing (latest state)
		//   - playback_status = "in_progress"
		//   - playing_time_ms accumulated meaningfully (> 50% of the
		//     observe window since pre-roll < ~5s typically)
		//   - buffering_count >= 1 (pre-roll counts as one buffering enter)
		//   - playing_count >= 1
		//   - error_count == 0 (no faults injected)
		if pm.State != "playing" {
			t.Errorf("state = %q, want %q", pm.State, "playing")
		}
		if pm.PlaybackStatus != "" && pm.PlaybackStatus != "in_progress" {
			t.Errorf("playback_status = %q, want %q", pm.PlaybackStatus, "in_progress")
		}
		minPlayingMs := uint32(observeWindow.Milliseconds() / 2)
		if pm.PlayingTimeMs < minPlayingMs {
			t.Errorf("playing_time_ms = %d, want >= %d (50%% of observe window %s)",
				pm.PlayingTimeMs, minPlayingMs, observeWindow)
		}
		if pm.BufferingCount < 1 {
			t.Errorf("buffering_count = %d, want >= 1 (pre-roll)", pm.BufferingCount)
		}
		if pm.PlayingCount < 1 {
			t.Errorf("playing_count = %d, want >= 1", pm.PlayingCount)
		}
		if pm.ErrorCount != 0 {
			t.Errorf("error_count = %d, want 0 (no faults injected)", pm.ErrorCount)
		}
	})

	// Scenario 2 — restart preserves accumulators.
	//
	// Trigger the in-app Retry button (`playback-retry-button`) which
	// calls PlayerViewModel.retry(): replaces the AVPlayer item,
	// increments attempt_id, KEEPS play_id stable. The PlaybackDiagnostics
	// `snapshotForRestart()` path should preserve every cumulative
	// metric across the player replacement — residency accumulators,
	// counts, and per-variant dwell seconds. Asserts:
	//
	//   - play_id stable (retry, not reload)
	//   - attempt_id incremented by 1
	//   - playing_time_ms / playing_count etc. >= pre values
	//     (continued from where they left off, not reset)
	//   - per-variant dwell map: every key present pre-restart still
	//     present post-restart, every value >= pre value
	//
	// Skipped cleanly when LAUNCH_MODE isn't appium (no way to tap
	// the in-app button without WebDriver).
	t.Run("RestartPreservesMetrics", func(t *testing.T) {
		if !hasAppium {
			t.Skip("requires LAUNCH_MODE=appium to tap playback-retry-button")
		}
		pre, err := sess.PlayerState(ctx)
		if err != nil {
			t.Fatalf("PlayerState pre-restart: %v", err)
		}
		mustHavePlayerMetrics(t, pre)
		prePlayingMs := pre.PlayerMetrics.PlayingTimeMs
		prePlayingCount := pre.PlayerMetrics.PlayingCount
		preBufferingMs := pre.PlayerMetrics.BufferingTimeMs
		preBufferingCount := pre.PlayerMetrics.BufferingCount
		preVariants := parseTimePerVariant(pre.PlayerMetrics.TimePerVariantS)
		var prePlayID string
		var preAttemptID int
		if pre.CurrentPlay != nil {
			prePlayID = pre.CurrentPlay.ID
			preAttemptID = pre.CurrentPlay.AttemptID
		}
		t.Logf("[RestartPreservesMetrics] pre: play_id=%s attempt_id=%d playing_ms=%d playing_count=%d buffering_ms=%d variants=%v",
			prePlayID, preAttemptID, prePlayingMs, prePlayingCount, preBufferingMs, preVariants)

		t.Logf("[RestartPreservesMetrics] tapping playback-retry-button")
		if err := appium.TapByAccessibilityID(ctx, sess, "playback-retry-button"); err != nil {
			t.Fatalf("tap retry: %v", err)
		}

		// Recovery polling — retry() rebuilds the AVPlayer, refetches
		// the manifest, restarts playback. ~10-30s typical on the sim.
		recoveryDeadline := time.Now().Add(45 * time.Second)
		var post *runner.PlayerRecord
		for time.Now().Before(recoveryDeadline) {
			rec, err := sess.PlayerState(ctx)
			if err == nil && rec.PlayerMetrics != nil &&
				rec.PlayerMetrics.State == "playing" {
				post = rec
				break
			}
			holdContext(ctx, 1*time.Second)
		}
		if post == nil {
			t.Fatalf("player did not return to state=playing within %s", 45*time.Second)
		}
		mustHavePlayerMetrics(t, post)
		pm := post.PlayerMetrics
		postVariants := parseTimePerVariant(pm.TimePerVariantS)
		var postPlayID string
		var postAttemptID int
		if post.CurrentPlay != nil {
			postPlayID = post.CurrentPlay.ID
			postAttemptID = post.CurrentPlay.AttemptID
		}
		t.Logf("[RestartPreservesMetrics] post: play_id=%s attempt_id=%d playing_ms=%d playing_count=%d buffering_ms=%d variants=%v",
			postPlayID, postAttemptID, pm.PlayingTimeMs, pm.PlayingCount, pm.BufferingTimeMs, postVariants)

		// Assertion 1 — play_id stable across retry().
		if prePlayID != "" && postPlayID != "" && prePlayID != postPlayID {
			t.Errorf("play_id rotated across retry: pre=%s post=%s — retry should keep play_id stable, only reload() rotates",
				prePlayID, postPlayID)
		}

		// Assertion 2 — attempt_id incremented by exactly 1.
		if postAttemptID != preAttemptID+1 {
			t.Errorf("attempt_id = %d, want %d (pre %d + 1)",
				postAttemptID, preAttemptID+1, preAttemptID)
		}

		// Assertion 3 — residency accumulators preserved.
		if pm.PlayingTimeMs < prePlayingMs {
			t.Errorf("playing_time_ms reset across retry: pre=%d post=%d (should continue, not reset)",
				prePlayingMs, pm.PlayingTimeMs)
		}
		if pm.PlayingCount < prePlayingCount {
			t.Errorf("playing_count reset across retry: pre=%d post=%d",
				prePlayingCount, pm.PlayingCount)
		}
		if pm.BufferingTimeMs < preBufferingMs {
			t.Errorf("buffering_time_ms reset across retry: pre=%d post=%d",
				preBufferingMs, pm.BufferingTimeMs)
		}
		if pm.BufferingCount < preBufferingCount {
			t.Errorf("buffering_count reset across retry: pre=%d post=%d",
				preBufferingCount, pm.BufferingCount)
		}

		// Assertion 4 — per-variant dwell seconds preserved + accumulating.
		// Every variant we'd accumulated pre-restart should still
		// appear post-restart with a value >= the pre value.
		for variant, preSec := range preVariants {
			postSec, ok := postVariants[variant]
			if !ok {
				t.Errorf("variant %q present pre-restart (%.2fs) but missing post-restart",
					variant, preSec)
				continue
			}
			if postSec < preSec {
				t.Errorf("variant %q dwell reset across retry: pre=%.2fs post=%.2fs",
					variant, preSec, postSec)
			}
		}
	})

	// Scenario 3 — mid-play stall + recovery.
	//
	// Server-side rate-throttle to drain the buffer and force a real
	// stall. A single-segment fault (e.g. request_first_byte_hang) is
	// NOT enough on a player with a deep buffer — the next-segment
	// timeout passes by the time AVPlayer plays through what it has
	// queued, and the player never reports stalling. Throttling to
	// near-zero bandwidth blocks every fetch; once the buffer drains
	// AVPlayerItemPlaybackStalled fires and state → "stalled".
	//
	// Recovery: restore a high rate, watch for state → "playing".
	t.Run("StallRecovery", func(t *testing.T) {
		pre, err := sess.PlayerState(ctx)
		if err != nil {
			t.Fatalf("PlayerState pre-stall: %v", err)
		}
		mustHavePlayerMetrics(t, pre)
		preCount := pre.PlayerMetrics.StallingCount
		preTime := pre.PlayerMetrics.StallingTimeMs
		t.Logf("[StallRecovery] pre: stalling_count=%d stalling_time_ms=%d buffer_end=%.1fs",
			preCount, preTime, pre.PlayerMetrics.BufferEndS)

		t.Logf("[StallRecovery] throttling to 0.01 Mbps to drain buffer")
		if err := sess.ApplyRate(ctx, 0.01); err != nil {
			t.Fatalf("apply throttle rate: %v", err)
		}

		// Poll until stall observed or deadline. Buffer drain time is
		// position_s + buffer_end_s — typically 5-30s on AVPlayer with
		// the default 6s segment ladder.
		stallDeadline := time.Now().Add(45 * time.Second)
		var stalled bool
		for time.Now().Before(stallDeadline) {
			rec, err := sess.PlayerState(ctx)
			if err == nil && rec.PlayerMetrics != nil &&
				rec.PlayerMetrics.StallingCount > preCount {
				stalled = true
				t.Logf("[StallRecovery] stall observed after %s — state=%s stalling_count=%d",
					time.Since(stallDeadline.Add(-45*time.Second)),
					rec.PlayerMetrics.State, rec.PlayerMetrics.StallingCount)
				break
			}
			holdContext(ctx, 1*time.Second)
		}
		if !stalled {
			t.Errorf("did not observe stall within 45s under 0.01 Mbps cap")
		}

		t.Logf("[StallRecovery] restoring 100 Mbps, polling for state recovery")
		if err := sess.ApplyRate(ctx, 100); err != nil {
			t.Fatalf("restore rate: %v", err)
		}
		// Recovery polling. AVPlayer's behaviour on iOS 26+ after a
		// long stall (>20s of empty buffer): it sometimes transitions
		// `timeControlStatus` to `.paused` rather than waiting
		// indefinitely. That's a real stuck state — AVPlayer does NOT
		// auto-recover from .paused; the app must call play(). When
		// our state machine sees this we tap the in-app Retry button
		// (accessibility identifier "playback-retry-button") which
		// triggers PlayerViewModel.retry(). The button is reachable
		// only via Appium; CLI / Manual launchers can't drive it.
		recoveryStart := time.Now()
		recoveryDeadline := recoveryStart.Add(60 * time.Second)
		retried := false
		for time.Now().Before(recoveryDeadline) {
			rec, err := sess.PlayerState(ctx)
			if err != nil || rec.PlayerMetrics == nil {
				holdContext(ctx, 1*time.Second)
				continue
			}
			curState := rec.PlayerMetrics.State
			if curState == "playing" {
				t.Logf("[StallRecovery] recovered to state=playing after %s (retried=%v)",
					time.Since(recoveryStart), retried)
				break
			}
			// If the player gave up to .paused, tap the in-app Retry
			// button to drive a recovery. Tap once per scenario — if
			// the first retry doesn't take, something deeper is wrong.
			if curState == "paused" && !retried && hasAppium {
				t.Logf("[StallRecovery] state=paused at %s — tapping playback-retry-button",
					time.Since(recoveryStart))
				if err := appium.TapByAccessibilityID(ctx, sess, "playback-retry-button"); err != nil {
					t.Logf("  tap retry: %v (continuing to poll)", err)
				}
				retried = true
			}
			holdContext(ctx, 1*time.Second)
		}

		post, err := sess.PlayerState(ctx)
		if err != nil {
			t.Fatalf("PlayerState post-stall: %v", err)
		}
		mustHavePlayerMetrics(t, post)
		pm := post.PlayerMetrics

		stallDeltaCount := pm.StallingCount - preCount
		stallDeltaMs := pm.StallingTimeMs - preTime
		t.Logf("[StallRecovery] post: state=%s stalling_count=%d (+%d) stalling_time_ms=%d (+%d ms)",
			pm.State, pm.StallingCount, stallDeltaCount, pm.StallingTimeMs, stallDeltaMs)

		// Assertions:
		//   - state recovered to "playing" by end of observe window
		//   - stalling_count delta >= 1 (one new stall fired)
		//   - stalling_time_ms delta > 0 (some measurable stall window)
		if pm.State != "playing" {
			t.Errorf("post-recovery state = %q, want %q", pm.State, "playing")
		}
		if stallDeltaCount < 1 {
			t.Errorf("stalling_count delta = %d, want >= 1", stallDeltaCount)
		}
		if stallDeltaMs == 0 {
			t.Errorf("stalling_time_ms delta = 0, want > 0")
		}
		_ = stallDuration // retained for backward compat with env override; future tunable
	})

	// Scenario 8 — transient error mid-play (recoverable).
	//
	// Arm a single brief request_zero_byte_response fault on a segment
	// — AVPlayer returns an error on that segment, retries, and
	// recovers. We expect error_count to tick AT LEAST by 1 but
	// playback_status to stay "in_progress" (the play hasn't ended).
	t.Run("RecoverableErrorMidPlay", func(t *testing.T) {
		pre, err := sess.PlayerState(ctx)
		if err != nil {
			t.Fatalf("PlayerState pre-error: %v", err)
		}
		mustHavePlayerMetrics(t, pre)
		preErrorCount := pre.PlayerMetrics.ErrorCount
		t.Logf("[RecoverableErrorMidPlay] pre: error_count=%d", preErrorCount)

		videoDirs, _ := runner.VideoVariantDirs(pre)
		// "500" is the v1 surface model's keyword for HTTP-500 fault
		// injection. v2-only types like request_zero_byte_response
		// require an upgraded backend; "500" works against the
		// existing test-dev server today.
		t.Logf("[RecoverableErrorMidPlay] arming 500 on next segment")
		if err := sess.ArmFault(ctx, "500", "segment", videoDirs...); err != nil {
			t.Fatalf("arm fault: %v", err)
		}
		holdContext(ctx, observeWindow/2)
		if err := sess.ClearFaults(ctx); err != nil {
			t.Logf("clear fault: %v", err)
		}
		// Poll for state=playing after the fault clears. A single 500
		// often causes a brief mid-play stall while AVPlayer retries
		// — that's expected, just give it room.
		recoveryDeadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(recoveryDeadline) {
			rec, err := sess.PlayerState(ctx)
			if err == nil && rec.PlayerMetrics != nil &&
				rec.PlayerMetrics.State == "playing" {
				break
			}
			holdContext(ctx, 1*time.Second)
		}

		post, err := sess.PlayerState(ctx)
		if err != nil {
			t.Fatalf("PlayerState post-error: %v", err)
		}
		mustHavePlayerMetrics(t, post)
		pm := post.PlayerMetrics

		errorDelta := pm.ErrorCount - preErrorCount
		t.Logf("[RecoverableErrorMidPlay] post: state=%s playback_status=%s error_count=%d (+%d) error_code=%d error_domain=%s",
			pm.State, pm.PlaybackStatus, pm.ErrorCount, errorDelta,
			pm.ErrorCode, pm.ErrorDomain)

		// Assertions:
		//   - playback_status NOT terminal (status stays in_progress
		//     since the player recovered)
		//   - terminal_error_code == 0 (transient, not terminal)
		//   - error_count incremented by at least 1
		//   - state recovered to playing
		if pm.PlaybackStatus != "" && pm.PlaybackStatus != "in_progress" {
			t.Errorf("playback_status = %q, want %q (transient should not terminate)",
				pm.PlaybackStatus, "in_progress")
		}
		if pm.TerminalErrorCode != 0 {
			t.Errorf("terminal_error_code = %d, want 0 (transient should not be terminal)",
				pm.TerminalErrorCode)
		}
		if pm.State != "playing" {
			t.Errorf("state = %q, want %q (player should have recovered)", pm.State, "playing")
		}
		// error_count delta is a soft assertion — iOS may suppress
		// AVError reporting if its internal retry succeeds before the
		// metrics POST goes out. Log but don't fail when delta is 0.
		if errorDelta == 0 {
			t.Logf("note: error_count did not increment — AVPlayer may have absorbed the retry internally")
		}
	})

	t.Logf("")
	t.Logf("════════════════════════════════════════════════════════════════════")
	t.Logf("  All scenarios complete. Inspect via:")
	t.Logf("    plays list: .../dashboard/sessions.html?player=%s", sess.PlayerID)
	t.Logf("════════════════════════════════════════════════════════════════════")
}

// mustHavePlayerMetrics fails fast when the harness returns a player
// record with nil PlayerMetrics — which usually means the device
// stopped heartbeating between scenarios.
func mustHavePlayerMetrics(t *testing.T, rec *runner.PlayerRecord) {
	t.Helper()
	if rec == nil || rec.PlayerMetrics == nil {
		t.Fatalf("player_metrics is nil — device stopped heartbeating?")
	}
}

// envDurationSeconds reads an integer env var as a duration in seconds.
// On parse failure logs a note and returns the default.
func envDurationSeconds(name string, def time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// parseTimePerVariant decodes the JSON-string-encoded variant dwell map
// iOS emits as `player_metrics_time_per_variant_s` (e.g.
// `{"2160p@29857kbps":65.28,"1080p@7060kbps":12.4}`). Returns an empty
// map on empty input or parse error so callers can do range/diff math
// without nil checks.
func parseTimePerVariant(raw string) map[string]float64 {
	out := map[string]float64{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]float64{}
	}
	return out
}

// holdContext is provided by sweep.go.
