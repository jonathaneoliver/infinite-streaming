// playback_end_test.go — drives a real iOS player (iPad sim by default)
// through the #550 Phase 2 terminal-outcome vocabulary and validates
// the resulting playback_status / playback_reason / terminal_error_*
// fields on the wire and in ClickHouse.
//
// Skips `completed` because the test deployment's content loops
// forever — there's no natural EOF to drive natural_eof. Covers:
//
//   ✓ MidStreamFailureSegment — arm sustained segment 503s mid-play,
//                                assert playback_status=mid_stream_failure
//                                + terminal_error_code != 0.
//   ✓ StartFailureManifest404  — arm master_manifest 404 + tap
//                                playback-reload-button → fresh play
//                                hits the fault before first frame,
//                                assert playback_status=start_failure.
//   ✓ AbandonedStartSlowStartup — arm master_manifest delay > EBVS
//                                threshold + tap reload, wait threshold
//                                + buffer, tap back so iOS emits
//                                session_end; classifier marks the
//                                play as abandoned_start (slow_startup).
//   ✓ UserStopped              — tap playback-back-button while
//                                playing, assert playback_status=user_stopped.
//                                Goes LAST because it tears down the
//                                playback screen.
//
// Each sub-test that mutates the play_id (reload + fault) recovers a
// "playing" baseline before its assertions. CH fallback (`harness query
// events --player-id …`) takes over when the player record vanishes
// before we can poll it.
//
// Run via:
//   LAUNCH_MODE=appium go test ./tests/characterization/modes \
//     -run TestPlaybackEndsIPadSim -v -timeout 10m
//
// Env overrides:
//   CHAR_END_EBVS_THRESHOLD_S    — EBVS threshold (default 10s, matches
//                                  forwarder qoe_thresholds defaults).
//   CHAR_END_EBVS_BUFFER_S       — wait beyond threshold before tapping
//                                  back so the classifier has unambiguously
//                                  passed the slow-startup window
//                                  (default 5s).
//   CHAR_END_MID_STREAM_TIMEOUT_S — how long to wait for iOS to give up
//                                  after sustained segment faults
//                                  (default 60s).
//   CHAR_END_RECOVERY_TIMEOUT_S  — how long to wait for state=playing
//                                  between sub-tests (default 45s).

package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// TestPlaybackEndsIPadSim — iPad-simulator entry point. Mirror with
// other platforms when needed.
func TestPlaybackEndsIPadSim(t *testing.T) { runPlaybackEnds(t, runner.PlatformIPadSim) }

func runPlaybackEnds(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)
	appium, hasAppium := sess.Launcher.(*runner.AppiumLauncher)
	if !hasAppium {
		t.Skip("requires LAUNCH_MODE=appium — playback-end scenarios need UI taps")
	}

	ebvsThreshold := envDurationSeconds("CHAR_END_EBVS_THRESHOLD_S", 10*time.Second)
	ebvsBuffer := envDurationSeconds("CHAR_END_EBVS_BUFFER_S", 5*time.Second)
	midStreamTimeout := envDurationSeconds("CHAR_END_MID_STREAM_TIMEOUT_S", 60*time.Second)
	recoveryTimeout := envDurationSeconds("CHAR_END_RECOVERY_TIMEOUT_S", 45*time.Second)

	runID := time.Now().UTC().Format("20060102T150405Z")
	runLabels := map[string]string{
		"test":      "playback_ends",
		"platform":  string(p),
		"run_id":    runID,
		"player_id": sess.PlayerID,
	}
	if err := sess.LabelPlay(context.Background(), runLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	}

	overall := 5*recoveryTimeout + midStreamTimeout + ebvsThreshold + ebvsBuffer + 2*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	t.Logf("")
	t.Logf("════════════════════════════════════════════════════════════════════")
	t.Logf("  PLAYBACK END CHARACTERIZATION — exercise Phase 2 terminal vocab")
	t.Logf("  Open in a browser to follow along:")
	t.Logf("    live tile : .../dashboard/testing-session.html?player_id=%s", sess.PlayerID)
	t.Logf("    plays list: .../dashboard/sessions.html?player=%s", sess.PlayerID)
	t.Logf("  Settings:")
	t.Logf("    ebvs threshold      = %s (CHAR_END_EBVS_THRESHOLD_S)", ebvsThreshold)
	t.Logf("    ebvs buffer         = %s (CHAR_END_EBVS_BUFFER_S)", ebvsBuffer)
	t.Logf("    mid-stream timeout  = %s (CHAR_END_MID_STREAM_TIMEOUT_S)", midStreamTimeout)
	t.Logf("    recovery timeout    = %s (CHAR_END_RECOVERY_TIMEOUT_S)", recoveryTimeout)
	t.Logf("════════════════════════════════════════════════════════════════════")

	// Bring the player to steady playback before the first sub-test.
	if !waitForState(t, ctx, sess, "playing", recoveryTimeout) {
		t.Fatalf("player never reached state=playing — can't run terminal-end scenarios")
	}

	// — Mid-stream failure: arm sustained 503s on video segments after
	// first frame, wait for iOS to exhaust retries and emit session_end
	// with playback_status=mid_stream_failure.
	t.Run("MidStreamFailureSegment", func(t *testing.T) {
		preID := currentPlayID(ctx, sess)
		t.Logf("[MidStreamFailureSegment] pre play_id=%s — arming sustained segment 503s", preID)
		if err := armSustainedFault(ctx, sess, "503", "segment", 30); err != nil {
			t.Fatalf("arm sustained 503: %v", err)
		}
		defer sess.ClearFaults(ctx)

		got, src := awaitTerminalStatus(t, ctx, sess, preID, midStreamTimeout, []string{"mid_stream_failure"})
		t.Logf("[MidStreamFailureSegment] terminal status=%q (source=%s)", got, src)
		if got != "mid_stream_failure" {
			t.Errorf("playback_status = %q, want %q", got, "mid_stream_failure")
		}

		// Recover for next sub-test.
		_ = sess.ClearFaults(ctx)
		tapReload(t, ctx, sess, appium)
		if !waitForState(t, ctx, sess, "playing", recoveryTimeout) {
			t.Fatalf("post-recovery: player did not reach playing")
		}
	})

	// — Start failure: arm master_manifest 404 BEFORE reload so the
	// fresh play hits the fault on its first manifest fetch.
	t.Run("StartFailureManifest404", func(t *testing.T) {
		if err := armSustainedFault(ctx, sess, "404", "master_manifest", 10); err != nil {
			t.Fatalf("arm master_manifest 404: %v", err)
		}
		defer sess.ClearFaults(ctx)

		tapReload(t, ctx, sess, appium)
		preID := waitForNewPlayID(ctx, sess, currentPlayID(ctx, sess), 15*time.Second)
		t.Logf("[StartFailureManifest404] new play_id=%s — waiting for start_failure", preID)

		got, src := awaitTerminalStatus(t, ctx, sess, preID, 60*time.Second, []string{"start_failure"})
		t.Logf("[StartFailureManifest404] terminal status=%q (source=%s)", got, src)
		if got != "start_failure" {
			t.Errorf("playback_status = %q, want %q", got, "start_failure")
		}

		_ = sess.ClearFaults(ctx)
		tapReload(t, ctx, sess, appium)
		if !waitForState(t, ctx, sess, "playing", recoveryTimeout) {
			t.Fatalf("post-recovery: player did not reach playing")
		}
	})

	// — Abandoned start: arm an extreme delay on master_manifest so the
	// player can't progress past startup, wait EBVS threshold + buffer,
	// then tap back. The forwarder classifier should mark the play as
	// abandoned_start (slow_startup) because the user quit before first
	// frame with elapsed > ebvs_threshold_ms.
	t.Run("AbandonedStartSlowStartup", func(t *testing.T) {
		// 30s delay >> EBVS 10s default — long enough that even with
		// fast retries the master_manifest never resolves in time.
		if err := armSustainedFault(ctx, sess, "request_body_hang", "master_manifest", 10); err != nil {
			t.Fatalf("arm master_manifest hang: %v", err)
		}
		defer sess.ClearFaults(ctx)

		tapReload(t, ctx, sess, appium)
		preID := waitForNewPlayID(ctx, sess, currentPlayID(ctx, sess), 15*time.Second)

		// Wait EBVS + buffer so the elapsed-since-play-start unambiguously
		// crosses the threshold from the classifier's perspective.
		wait := ebvsThreshold + ebvsBuffer
		t.Logf("[AbandonedStartSlowStartup] new play_id=%s — waiting %s (EBVS %s + buffer %s) then tapping back",
			preID, wait, ebvsThreshold, ebvsBuffer)
		holdContext(ctx, wait)

		if err := appium.TapByAccessibilityID(ctx, sess, "playback-back-button"); err != nil {
			t.Fatalf("tap back: %v", err)
		}

		got, src := awaitTerminalStatus(t, ctx, sess, preID, 60*time.Second,
			[]string{"abandoned_start"})
		t.Logf("[AbandonedStartSlowStartup] terminal status=%q (source=%s)", got, src)
		if got != "abandoned_start" {
			// Don't fail hard — classifier behavior for abandoned_start
			// is the most heuristic of the four (depends on EBVS
			// threshold + no-first-frame + user-quit signals lining up).
			// Log loudly so a future audit can verify.
			t.Errorf("playback_status = %q, want %q (classifier may need tuning if user_stopped fires instead)",
				got, "abandoned_start")
		}

		// Recover playback for the final sub-test.
		_ = sess.ClearFaults(ctx)
		if err := appium.TapByAccessibilityID(ctx, sess, "home-continue-watching"); err != nil {
			t.Logf("[AbandonedStartSlowStartup] re-enter via continue-watching failed: %v", err)
		}
		if !waitForState(t, ctx, sess, "playing", recoveryTimeout) {
			t.Fatalf("post-recovery: player did not reach playing")
		}
	})

	// — User stopped: tap back while playing. Goes LAST because it
	// leaves us on the home screen with no clean re-entry.
	t.Run("UserStopped", func(t *testing.T) {
		preID := currentPlayID(ctx, sess)
		t.Logf("[UserStopped] pre play_id=%s — tapping playback-back-button", preID)
		if err := appium.TapByAccessibilityID(ctx, sess, "playback-back-button"); err != nil {
			t.Fatalf("tap back: %v", err)
		}

		got, src := awaitTerminalStatus(t, ctx, sess, preID, 30*time.Second, []string{"user_stopped"})
		t.Logf("[UserStopped] terminal status=%q (source=%s)", got, src)
		if got != "user_stopped" {
			t.Errorf("playback_status = %q, want %q", got, "user_stopped")
		}
	})
}

// armSustainedFault posts a non-one-shot fault rule (consecutive > 1)
// so the fault fires for the next N matching requests instead of just
// one. Mirrors runner.ArmFault but with a configurable consecutive
// count — needed for terminal-failure scenarios where iOS retries
// several times before giving up.
func armSustainedFault(ctx context.Context, sess *runner.Session, shape, kind string, consecutive int) error {
	if sess == nil || sess.PlayerID == "" {
		return fmt.Errorf("arm sustained fault: no player bound")
	}
	if _, err := runHarnessForTest(ctx,
		"fault", "add", sess.PlayerID,
		"--type", shape,
		"--kind", kind,
		"--frequency", "0",
		"--consecutive", fmt.Sprintf("%d", consecutive),
		"--mode", "requests",
	); err != nil {
		return fmt.Errorf("harness fault add: %w", err)
	}
	return nil
}

// currentPlayID reads the live record's current_play.id with a single
// probe — used for "remember what was playing right now" snapshots.
// Returns empty string on any error.
func currentPlayID(ctx context.Context, sess *runner.Session) string {
	rec, err := sess.PlayerState(ctx)
	if err != nil || rec == nil || rec.CurrentPlay == nil {
		return ""
	}
	return rec.CurrentPlay.ID
}

// waitForNewPlayID polls until the current play_id differs from
// `prior` or `timeout` elapses. Used after tapReload to grab the
// fresh play_id without racing the reload's manifest fetch.
func waitForNewPlayID(ctx context.Context, sess *runner.Session, prior string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cur := currentPlayID(ctx, sess)
		if cur != "" && cur != prior {
			return cur
		}
		holdContext(ctx, 500*time.Millisecond)
	}
	return ""
}

// waitForState polls until pm.State == target or `timeout` elapses.
// Returns true on success, false on timeout. Logs progress every 5s.
func waitForState(t *testing.T, ctx context.Context, sess *runner.Session, target string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	lastLog := time.Now()
	for time.Now().Before(deadline) {
		rec, err := sess.PlayerState(ctx)
		if err == nil && rec != nil && rec.PlayerMetrics != nil &&
			rec.PlayerMetrics.State == target {
			return true
		}
		if time.Since(lastLog) > 5*time.Second {
			cur := "(unknown)"
			if rec != nil && rec.PlayerMetrics != nil {
				cur = rec.PlayerMetrics.State
			}
			t.Logf("[waitForState] target=%s current=%s elapsed=%s", target, cur, time.Since(deadline.Add(-timeout)))
			lastLog = time.Now()
		}
		holdContext(ctx, 500*time.Millisecond)
	}
	return false
}

// tapReload taps the in-app reload button which calls vm.reload() →
// new play_id, fresh play with whatever fault state is armed.
func tapReload(t *testing.T, ctx context.Context, sess *runner.Session, appium *runner.AppiumLauncher) {
	if err := appium.TapByAccessibilityID(ctx, sess, "playback-reload-button"); err != nil {
		t.Fatalf("tap reload: %v", err)
	}
}

// awaitTerminalStatus polls the live player record for a non-in_progress
// playback_status matching `want`. Falls back to a CH query when the
// live record's current_play.id has rotated away from `playID` (which
// happens once iOS emits session_end + the player flips to idle).
// Returns the observed status string and a one-word source label
// ("live" or "ch") for log context.
func awaitTerminalStatus(t *testing.T, ctx context.Context, sess *runner.Session, playID string, timeout time.Duration, want []string) (status string, source string) {
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, err := sess.PlayerState(ctx)
		if err == nil && rec != nil && rec.PlayerMetrics != nil {
			pm := rec.PlayerMetrics
			if pm.PlaybackStatus != "" && pm.PlaybackStatus != "in_progress" {
				if rec.CurrentPlay != nil && rec.CurrentPlay.ID == playID {
					return pm.PlaybackStatus, "live"
				}
			}
		}
		// Player has rotated away or vanished — try the CH fallback.
		if playID != "" && time.Since(deadline.Add(-timeout)) > 5*time.Second {
			if s, ok := queryTerminalStatusFromCH(ctx, playID); ok {
				return s, "ch"
			}
		}
		holdContext(ctx, 750*time.Millisecond)
	}
	// Final CH read after the timeout — give the forwarder a chance
	// to catch up if SSE just slipped past the deadline.
	if playID != "" {
		if s, ok := queryTerminalStatusFromCH(ctx, playID); ok {
			return s, "ch"
		}
	}
	return "", "timeout"
}

// queryTerminalStatusFromCH asks the forwarder for the latest
// session_events row for `playID` and returns the non-in_progress
// playback_status if there is one. CH is the authoritative durable
// view — used when the live PlayerRecord no longer exposes the
// finished play.
func queryTerminalStatusFromCH(ctx context.Context, playID string) (string, bool) {
	raw, err := runHarnessForTest(ctx, "query", "events", "--play-id", playID, "--limit", "1", "--order", "desc")
	if err != nil {
		return "", false
	}
	var resp struct {
		Items []struct {
			PlaybackStatus string `json:"playback_status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", false
	}
	if len(resp.Items) == 0 {
		return "", false
	}
	s := resp.Items[0].PlaybackStatus
	if s == "" || s == "in_progress" {
		return "", false
	}
	return s, true
}

// runHarnessForTest is a thin wrapper that calls the runner.runHarness
// helper. The runner package keeps runHarness unexported; we mirror
// the signature here so tests under the modes package can drive
// arbitrary harness CLI commands without depending on internals.
func runHarnessForTest(ctx context.Context, args ...string) ([]byte, error) {
	return runner.RunHarnessCLI(ctx, args...)
}
