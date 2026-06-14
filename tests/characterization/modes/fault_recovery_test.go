package modes

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Fault-recovery probe — a DEDICATED investigative mode (not a characterization
// of normal playback). It warms a player to a steady playing state, arms a
// fault, and classifies the outcome — does AVPlayer go `.failed`, go STUCK
// (auto-paused mid-stall, rate 0), or ride it out — with time + faulted-request
// thresholds, then whether it recovers on clear.
//
// Two modes:
//   - FAULT_PROBE_ALL=1   sweep the full fault × kind matrix, one case after
//                         another (stop+restart playback between — NO sim
//                         reboot), and print a summary matrix at the end.
//   - FAULT_PROBE=<fault> run a SINGLE fault (default) for focused drilling.
//
// Env axes:
//
//	FAULT_PROBE              single-mode fault (default "corrupted"):
//	                           rate_choke / timeout / 404 / 500 / corrupted /
//	                           request_{connect,first_byte,body}_{hang,reset,delayed}
//	FAULT_PROBE_KIND         request kind (default "segment"); also manifest / master_manifest
//	FAULT_PROBE_ALL          =1 → sweep the whole matrix (ignores FAULT_PROBE/_KIND)
//	FAULT_PROBE_WARMUP_S     steady-play warmup before arming (default 20)
//	FAULT_PROBE_HOLD_S       max hold under the fault (default 60; breaks early once a state is reached)
//	FAULT_PROBE_RECOVER_S    observe window after clearing (default 20)
//	FAULT_PROBE_AUTORECOVERY auto-recovery flag (default "false" — observe the RAW state)
//	FAULT_PROBE_RESTART_DELAY_S  auto-recovery backoff (default 60)
//	FAULT_PROBE_CLEAR_ON_FAILED  =1 → clear the fault the moment a state is reached
//	                              (restart-into-clean-network testing; single mode)
//
// Appium launch mode only (skips otherwise). Honors CHARACTERIZATION_DEVICE_UDID.

func TestFaultRecoveryIPadSim(t *testing.T) { runFaultRecovery(t, runner.PlatformIPadSim) }
func TestFaultRecoveryIPhone(t *testing.T)  { runFaultRecovery(t, runner.PlatformIPhone) }

func runFaultRecovery(t *testing.T, p runner.Platform) {
	runFleet(t, p, runFaultRecoveryOnDevice)
}

type faultCase struct{ fault, kind string }

type caseResult struct {
	fault, kind  string
	playID       string // for post-hoc CH recovery-method attribution
	outcome      string // failed / stuck / rode_it_out / launch_failed
	timeToStateS int    // seconds to reach failed/stuck (-1 if never)
	faultedReqs  int    // proxy-applied faults at that point (-1 if unknown)
	termErr      int    // terminal_error_code if failed
	// Per-play residency at case end (cumulative): how much it stalled vs how
	// much it actually played before the fault killed (or it rode out) the play.
	stallCount uint32
	stallMs    uint32
	playCount  uint32
	playMs     uint32
	everStuck  bool // stall_stuck / rate-0-while-stalled seen at any point
	recovered  bool // returned to playing (sustained video) within the window
	// Aggregated AVPlayer errors over the whole play, pulled from the durable CH
	// event rows. errInfo = transient error_* (domain+code, e.g. "CoreMedia -16172");
	// termInfo = terminal_error_* (stamped only on a real terminal close). "" = none.
	errInfo  string
	termInfo string
	// recoveredVia names WHICH of the 4 recovery methods restored playback:
	//   failure      — METHOD 1: .failed → auto_recovery_failure restart
	//   stuck        — METHOD 2: stallStuck → auto_recovery_stuck restart
	//   live_seek    — METHOD 3: live-stall → jump-to-live seek (no restart)
	//   live_restart — METHOD 4: live-stall seek failed → auto_recovery_live_resync restart
	//   natural      — rode it out, resumed on clear with no intervention
	//   "-"          — did not recover
	recoveredVia string
}

// buildFaultCases returns the sweep matrix (FAULT_PROBE_ALL) or a single case.
func buildFaultCases() []faultCase {
	if envOr("FAULT_PROBE_ALL", "") != "" {
		faults := []string{
			"corrupted", "404", "500",
			"request_first_byte_reset", "request_connect_reset", "request_body_reset",
			"request_first_byte_hang", "request_body_hang",
			"rate_choke", "timeout",
		}
		cs := make([]faultCase, 0, len(faults)+1)
		for _, f := range faults {
			cs = append(cs, faultCase{f, "segment"})
		}
		// Also fault the media-playlist refresh — it IS re-fetched continuously on a
		// live stream, so the fault bites mid-stream. (No master_manifest case: the
		// multivariant playlist is fetched once at startup and never re-requested in
		// steady state, so arming it after warmup is a no-op — it's a startup-failure
		// concern, out of scope for this mid-stream recovery probe.)
		cs = append(cs, faultCase{"404", "manifest"})
		return cs
	}
	return []faultCase{{envOr("FAULT_PROBE", "corrupted"), envOr("FAULT_PROBE_KIND", "segment")}}
}

func runFaultRecoveryOnDevice(t *testing.T, p runner.Platform, dev runner.Device, _ *fleetBarriers) {
	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	appium, isAppium := launcher.(*runner.AppiumLauncher)
	if !isAppium {
		t.Skipf("fault-recovery probe requires LAUNCH_MODE=appium (got %s)", mode)
	}
	picked := &dev
	t.Logf("device: %s", picked)

	warmup := time.Duration(envInt("FAULT_PROBE_WARMUP_S", 20)) * time.Second
	hold := time.Duration(envInt("FAULT_PROBE_HOLD_S", 60)) * time.Second
	recover := time.Duration(envInt("FAULT_PROBE_RECOVER_S", 20)) * time.Second
	autoRec := envOr("FAULT_PROBE_AUTORECOVERY", "false")
	restartDelay := envOr("FAULT_PROBE_RESTART_DELAY_S", "60")
	restartBackoff := time.Duration(envInt("FAULT_PROBE_RESTART_DELAY_S", 60)) * time.Second

	cases := buildFaultCases()

	// Overall budget: per-case (warmup+hold + wait-for-restart + grace) + restart-playback
	// overhead. The dynamic recovery window can wait up to restartBackoff+margin for the
	// scheduled restart, then `recover` (grace) more after it fires.
	per := warmup + hold + restartBackoff + recover + 50*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(cases))*per+4*time.Minute)
	defer cancel()

	// --- launch ONCE to a steady playing state ---
	pid := runner.NewPlayerID()
	launchArgs := []string{
		"-is.player_id", pid,
		"-is.flag.auto_recovery", autoRec,
		"-is.flag.auto_recovery_base_delay_s", restartDelay,
		"-is.flag.skip_home", "false",
		"-is.flag.dev_mode", "true",
	}
	// CHAR_CONTENT pins the clip every case plays (e.g. insane_new_p200_h264) via
	// the is.lastPlayed NSArgumentDomain key — identical content + variant ladder
	// across cases, same as the other modes (sweep.go). Set once at launch;
	// persists across the stop/restart-playback cycles.
	if clip := envOr("CHAR_CONTENT", ""); clip != "" {
		launchArgs = append(launchArgs, "-is.lastPlayed", clip)
		t.Logf("content pinned: %s", clip)
	}
	appium.SetLaunchArgs(launchArgs)
	t.Logf("fault-recovery: %d case(s), autoRecovery=%s, restart_backoff=%ss, warmup/hold/recover=%s/%s/%s",
		len(cases), autoRec, restartDelay, warmup, hold, recover)
	launchCtx, lc := context.WithTimeout(ctx, 4*time.Minute)
	sess, lerr := appium.LaunchToHome(launchCtx, *picked)
	lc()
	if lerr != nil {
		t.Fatalf("LaunchToHome: %v", lerr)
	}
	sess.PlayerID = pid
	if err := resumeUntilHeartbeat(ctx, t, appium, sess, picked); err != nil {
		t.Fatalf("no heartbeat after resume: %v", err)
	}
	defer func() {
		_ = sess.ClearFaults(context.Background())
		_ = sess.ClearShape(context.Background())
		_ = sess.CloseViaUI(context.Background())
	}()

	results := make([]caseResult, 0, len(cases))
	for i, c := range cases {
		if i > 0 {
			// Stop + restart playback → fresh play_id, clean per-play state. The
			// app and simulator stay up — NO reboot, NO app relaunch.
			t.Logf("──── restart playback for next case: %s/%s ────", c.fault, c.kind)
			_ = sess.ClearFaults(ctx)
			_ = sess.ClearShape(ctx)
			_ = sess.CloseViaUI(ctx)
			if err := resumeUntilHeartbeat(ctx, t, appium, sess, picked); err != nil {
				t.Logf("case %s/%s: restart playback failed: %v — skipping", c.fault, c.kind, err)
				results = append(results, caseResult{fault: c.fault, kind: c.kind, outcome: "launch_failed", timeToStateS: -1, faultedReqs: -1})
				continue
			}
		}
		results = append(results, runOneCase(ctx, t, sess, c, warmup, hold, recover, restartBackoff))
	}

	if len(cases) > 1 {
		// Let the forwarder flush the last case's events to ClickHouse, then
		// attribute each recovery from the durable event rows (overrides the
		// lossy live hint) before rendering the table.
		_ = holdContext(ctx, 10*time.Second)
		attributeRecoveries(ctx, t, results)
		printSweepSummary(t, results, recover)
	}
}

// resumeUntilHeartbeat starts playback and waits for the player to report,
// retrying through the transient empty-catalogue window after a (re)launch.
func resumeUntilHeartbeat(ctx context.Context, t *testing.T, appium *runner.AppiumLauncher, sess *runner.Session, picked *runner.Device) error {
	t.Helper()
	var herr error
	for attempt := 1; attempt <= 3; attempt++ {
		if rerr := appium.ResumePlayback(ctx, *picked); rerr != nil {
			t.Logf("resume attempt %d: %v", attempt, rerr)
		}
		if herr = sess.WaitForHeartbeat(ctx, 50*time.Second); herr == nil {
			return nil
		}
	}
	return herr
}

// runOneCase warms up, arms one fault, classifies the outcome (failed / stuck /
// rode_it_out) with time + faulted-request thresholds, then checks whether it
// recovers on clear. Logs per-tick; returns the case summary.
func runOneCase(ctx context.Context, t *testing.T, sess *runner.Session, c faultCase, warmup, hold, recover, restartBackoff time.Duration) caseResult {
	res := caseResult{fault: c.fault, kind: c.kind, outcome: "rode_it_out", timeToStateS: -1, faultedReqs: -1}
	clearOnFailed := envOr("FAULT_PROBE_CLEAR_ON_FAILED", "") != ""
	tag := fmt.Sprintf("[%s/%s]", c.fault, c.kind)

	if err := holdContext(ctx, warmup); err != nil {
		return res
	}
	sampleProbe(ctx, t, sess, tag+" baseline")
	playID, _ := sess.CurrentPlayID(ctx)
	res.playID = playID
	probeStart := time.Now()
	t.Logf("%s play_id: %s", tag, playID)

	// Recovery-method attribution. Track which trigger events the player emits
	// (error / player_stuck / live_resync) and whether a restart fired vs the
	// baseline — together these name which of the 4 methods restored playback.
	caseBaseRestarts := 0
	if rec, err := sess.PlayerState(ctx); err == nil && rec != nil && rec.PlayerMetrics != nil {
		caseBaseRestarts = rec.PlayerMetrics.PlayerRestarts
	}
	var sawError, sawStuck, sawLiveSeek bool
	noteEvent := func(ev string) {
		switch ev {
		case "error":
			sawError = true
		case "player_stuck":
			sawStuck = true
		case "live_resync":
			sawLiveSeek = true
		}
	}

	clear, desc := armProbeFault(ctx, t, sess, c.fault, c.kind)
	t.Logf("%s ARMED: %s", tag, desc)

	// --- observe under the fault ---
	deadline := time.Now().Add(hold)
	tick := 0
	prevReqs := -1
	cleared := false
	for time.Now().Before(deadline) {
		if err := holdContext(ctx, 2*time.Second); err != nil {
			break
		}
		tick += 2
		rec, err := sess.PlayerState(ctx)
		if err != nil || rec == nil || rec.PlayerMetrics == nil {
			t.Logf("  +%2ds (no state: %v)", tick, err)
			continue
		}
		m := rec.PlayerMetrics
		noteEvent(m.LastEvent)
		faulted, reqInfo := reqCadence(ctx, sess, playID, probeStart, &prevReqs)
		res.stallCount, res.stallMs = m.StallingCount, m.StallingTimeMs
		res.playCount, res.playMs = m.PlayingCount, m.PlayingTimeMs
		failed := m.TerminalErrorCode != 0 || m.ErrorCount > 0 || m.LastEvent == "error" ||
			m.PlaybackStatus == "mid_stream_failure" || m.PlaybackStatus == "start_failure"
		stuck := m.StallStuck || (m.PlaybackRate == 0 && m.State == "stalled")
		if stuck {
			res.everStuck = true
		}
		if res.timeToStateS < 0 && (failed || stuck) {
			res.timeToStateS = tick
			res.faultedReqs = faulted
			if failed {
				res.outcome = "failed"
				res.termErr = int(m.TerminalErrorCode)
			} else {
				res.outcome = "stuck"
			}
			if clearOnFailed && !cleared && clear != nil {
				clear()
				cleared = true
				t.Logf("  >>> CLEARED %s on %s", desc, res.outcome)
			}
		}
		t.Logf("  +%2ds state=%-9s rate=%.0f stuck=%t last_event=%-13s term_err=%d status=%-17s restarts=%d bufEnd=%.1f%s",
			tick, m.State, m.PlaybackRate, m.StallStuck, m.LastEvent, m.TerminalErrorCode, m.PlaybackStatus, m.PlayerRestarts, m.BufferEndS, reqInfo)
		// Once a terminal state is confirmed, no need to keep holding.
		if res.timeToStateS >= 0 && tick >= res.timeToStateS+6 {
			break
		}
	}

	// --- clear + observe recovery (does it come back without a restart?) ---
	if clear != nil {
		clear()
	}
	// Dynamic recovery window. Under auto-recovery the restart fires ~restartBackoff
	// after .failed (which happened mid-hold), so a fixed wall-clock window races a
	// moving target — it can close before the (variable-latency) restart even fires.
	// Instead: wait up to restartBackoff+margin for a restart to fire, then watch
	// `recover` (grace, e.g. 60s) from THAT restart. Each new restart in the backoff
	// chain re-arms the grace; we give up if no video by then.
	//
	// "Recovered" is NOT the first non-paused tick — a restart goes buffering→playing
	// and can re-stall. We require the video to play CONTINUOUSLY for playConfirm (10s)
	// with the playhead (position_s) actually advancing, so a rate=1-but-frozen blip
	// doesn't count. Once confirmed → recovered; if the grace expires first → gave up.
	grace := recover
	playConfirm := time.Duration(envInt("FAULT_PROBE_PLAY_CONFIRM_S", 10)) * time.Second
	waitForRestart := restartBackoff + 30*time.Second
	baseRestarts := -1
	if rec, err := sess.PlayerState(ctx); err == nil && rec != nil && rec.PlayerMetrics != nil {
		baseRestarts = rec.PlayerMetrics.PlayerRestarts
	}
	t.Logf("%s CLEARED — dynamic recovery window (wait ≤%s for restart, then %s grace; confirm=%s sustained video; base_restarts=%d)",
		tag, waitForRestart, grace, playConfirm, baseRestarts)
	deadline = time.Now().Add(waitForRestart)
	restartSeen := false
	playingSinceTick := -1 // start of the current uninterrupted playing+advancing streak
	lastPos := -1.0
	rtick := 0
	for time.Now().Before(deadline) {
		if err := holdContext(ctx, 2*time.Second); err != nil {
			break
		}
		rtick += 2
		rec, err := sess.PlayerState(ctx)
		if err != nil || rec == nil || rec.PlayerMetrics == nil {
			continue
		}
		m := rec.PlayerMetrics
		noteEvent(m.LastEvent)
		res.stallCount, res.stallMs = m.StallingCount, m.StallingTimeMs
		res.playCount, res.playMs = m.PlayingCount, m.PlayingTimeMs
		// A NEW restart fired → (re)arm the grace window from now and reset the
		// play-confirm streak (the rebuilt item must earn its 10s afresh).
		if baseRestarts >= 0 && m.PlayerRestarts > baseRestarts {
			restartSeen = true
			baseRestarts = m.PlayerRestarts
			deadline = time.Now().Add(grace)
			playingSinceTick = -1
			t.Logf("%s restart #%d fired at R+%ds — watching %s for %s of sustained video", tag, m.PlayerRestarts, rtick, grace, playConfirm)
		}
		// Real video = playing + rate + the playhead advancing since the last sample.
		advancing := lastPos >= 0 && m.PositionS > lastPos+0.1
		lastPos = m.PositionS
		if m.State == "playing" && m.PlaybackRate > 0.5 && advancing {
			if playingSinceTick < 0 {
				playingSinceTick = rtick
			}
		} else {
			playingSinceTick = -1 // streak broken — buffering, paused, or frozen
		}
		playingForS := 0
		if playingSinceTick >= 0 {
			playingForS = rtick - playingSinceTick
		}
		t.Logf("  R+%2ds state=%-9s rate=%.0f pos=%.1f adv=%-5t playing_for=%2ds last_event=%-13s restarts=%d",
			rtick, m.State, m.PlaybackRate, m.PositionS, advancing, playingForS, m.LastEvent, m.PlayerRestarts)
		// Recovered only once the video has played continuously for playConfirm.
		if playingSinceTick >= 0 && playingForS >= int(playConfirm/time.Second) {
			res.recovered = true
			res.recoveredVia = classifyRecovery(m.PlayerRestarts > caseBaseRestarts, sawLiveSeek, sawStuck, sawError)
			t.Logf("%s recovered at R+%ds via %s — video played %ds continuously (restarts=%d, restart_fired=%t)",
				tag, rtick, res.recoveredVia, playingForS, m.PlayerRestarts, restartSeen)
			break
		}
		// Sticky terminal: the app exhausted its retries and closed the play.
		if m.LastEvent == "play_end" {
			t.Logf("%s gave up (terminal) at R+%ds: status=%s term_err=%d", tag, rtick, m.PlaybackStatus, m.TerminalErrorCode)
			break
		}
	}
	t.Logf("%s => outcome=%s time=%ds faulted=%d term_err=%d recovered_on_clear=%t",
		tag, res.outcome, res.timeToStateS, res.faultedReqs, res.termErr, res.recovered)
	return res
}

// reqCadence reads the play's network rows: returns the faulted-request count
// and a log fragment with the cumulative total + per-tick delta (flat = silent).
func reqCadence(ctx context.Context, sess *runner.Session, playID string, probeStart time.Time, prevReqs *int) (int, string) {
	if playID == "" {
		return -1, ""
	}
	rows, ferr := runner.FetchNetworkRows(ctx, sess.PlayerID, playID, probeStart, time.Now(), 5000)
	if ferr != nil {
		return -1, ""
	}
	faulted := 0
	for _, r := range rows {
		if r.FaultAction != "" || r.FaultType != "" {
			faulted++
		}
	}
	delta := 0
	silent := ""
	if *prevReqs >= 0 {
		delta = len(rows) - *prevReqs
		if delta == 0 {
			silent = " SILENT"
		}
	}
	*prevReqs = len(rows)
	return faulted, fmt.Sprintf(" reqs=%d(+%d) faulted=%d%s", len(rows), delta, faulted, silent)
}

// armProbeFault applies one fault and returns a clear func + human description.
func armProbeFault(ctx context.Context, t *testing.T, sess *runner.Session, probe, kind string) (func(), string) {
	t.Helper()
	switch probe {
	case "rate_choke":
		if err := sess.ApplyRate(ctx, 0.01); err != nil {
			t.Fatalf("rate_choke: %v", err)
		}
		return func() { _ = sess.ApplyRate(context.Background(), 0) }, "rate cap 0.01 Mbps"
	case "timeout":
		if err := sess.SetSegmentTimeout(ctx, 3*time.Second); err != nil {
			t.Fatalf("timeout arm: %v", err)
		}
		_ = sess.ApplyRate(ctx, 0.1)
		return func() {
			_ = sess.SetSegmentTimeout(context.Background(), 0)
			_ = sess.ApplyRate(context.Background(), 0)
		}, "segment timeout 3s + 0.1 Mbps"
	default:
		// Any HTTP/socket fault shape, applied continuously to `kind` requests.
		if err := sess.ArmFaultRepeating(ctx, probe, kind, 100000, 1); err != nil {
			t.Fatalf("arm fault %s/%s: %v", probe, kind, err)
		}
		return func() { _ = sess.ClearFaults(context.Background()) }, fmt.Sprintf("%s on %s", probe, kind)
	}
}

// sampleProbe logs a single state snapshot with a label.
func sampleProbe(ctx context.Context, t *testing.T, sess *runner.Session, label string) {
	t.Helper()
	rec, err := sess.PlayerState(ctx)
	if err != nil || rec == nil || rec.PlayerMetrics == nil {
		t.Logf("%s: (no state: %v)", label, err)
		return
	}
	m := rec.PlayerMetrics
	t.Logf("%s: state=%s rate=%.0f bufEnd=%.1fs vbr=%.1f restarts=%d", label, m.State, m.PlaybackRate, m.BufferEndS, m.VideoBitrateMbps, m.PlayerRestarts)
}

// printSweepSummary renders the full fault → outcome matrix.
func printSweepSummary(t *testing.T, results []caseResult, recover time.Duration) {
	t.Logf("════════════════════════════ FAULT-RECOVERY SWEEP SUMMARY ════════════════════════════")
	hdr := "%-24s %-15s %-11s %6s %7s %6s %7s %5s %6s %6s %9s %-12s %-20s %-20s"
	t.Logf(hdr, "FAULT", "KIND", "OUTCOME", "TIME_S", "FAULTED", "STALLS", "STALL_S", "PLAYS", "PLAY_S", "STUCK", "RECOVERED", "RECOVERED_VIA", "ERROR", "TERM_ERROR")
	t.Logf(hdr, "─────", "────", "───────", "──────", "───────", "──────", "───────", "─────", "──────", "─────", "─────────", "─────────────", "─────", "──────────")
	for _, r := range results {
		ts, fa, via := "-", "-", r.recoveredVia
		errInfo, termInfo := r.errInfo, r.termInfo
		if r.timeToStateS >= 0 {
			ts = fmt.Sprintf("%d", r.timeToStateS)
		}
		if r.faultedReqs >= 0 {
			fa = fmt.Sprintf("%d", r.faultedReqs)
		}
		if via == "" {
			via = "-"
		}
		if errInfo == "" {
			errInfo = "-"
		}
		if termInfo == "" {
			termInfo = "-"
		}
		t.Logf("%-24s %-15s %-11s %6s %7s %6d %7.1f %5d %6.1f %6t %9t %-12s %-20s %-20s",
			r.fault, r.kind, r.outcome, ts, fa,
			r.stallCount, float64(r.stallMs)/1000, r.playCount, float64(r.playMs)/1000, r.everStuck, r.recovered, via, errInfo, termInfo)
	}
	t.Logf("──────────────────────────────────────────────────────────────────────")
	t.Logf("outcome: failed = AVPlayer terminal (.failed); stuck = auto-paused mid-stall")
	t.Logf("         (rate 0, needs restart); rode_it_out = stalled but recovered on clear.")
	t.Logf("recovered = video played continuously (playhead advancing) for the confirm")
	t.Logf("           window within the grace (grace = %s after each restart).", recover)
	t.Logf("recovered_via = which of the 4 methods restored playback:")
	t.Logf("           failure      = METHOD 1  .failed → auto_recovery_failure restart")
	t.Logf("           stuck        = METHOD 2  stallStuck → auto_recovery_stuck restart")
	t.Logf("           live_seek    = METHOD 3  live-stall → jump-to-live seek (no restart)")
	t.Logf("           live_restart = METHOD 4  seek failed → auto_recovery_live_resync restart")
	t.Logf("           natural      = rode it out, resumed on clear; '-' = did not recover")
	t.Logf("ERROR = transient AVPlayer error_* (domain+code) seen at any point — sticky")
	t.Logf("        last_player_error; TERM_ERROR = terminal_error_* (stamped only on a")
	t.Logf("        real terminal close — empty under auto-recovery). Both from CH rows.")
	t.Logf("══════════════════════════════════════════════════════════════════════")
}

// classifyRecovery names which of the 4 recovery methods restored playback, from
// whether a restart fired (vs the case baseline) and which trigger events the
// player emitted. `live_resync` is the most specific marker (only the live-stall
// ladder emits it) so it wins: seen WITHOUT a restart means the cheap jump-to-live
// seek did it (method 3); WITH a restart means the seek failed and escalated to the
// rebuild (method 4). Otherwise a restart preceded by player_stuck/error attributes
// to the stuck/failure paths; a recovery with no restart at all is natural.
func classifyRecovery(restarted, sawLiveSeek, sawStuck, sawError bool) string {
	switch {
	case sawLiveSeek && !restarted:
		return "live_seek"
	case sawLiveSeek && restarted:
		return "live_restart"
	case restarted && sawStuck:
		return "stuck"
	case restarted && sawError:
		return "failure"
	case restarted:
		return "restart"
	default:
		return "natural"
	}
}

// chEventsResp is the minimal shape of `harness query events <play_id>` — one
// item per metrics POST, each carrying the player_metrics.last_event for that row.
type chEventsResp struct {
	Items []struct {
		PlayerMetrics struct {
			LastEvent           string `json:"last_event"`
			Error               string `json:"error"`
			ErrorCode           int    `json:"error_code"`
			ErrorDomain         string `json:"error_domain"`
			TerminalErrorCode   int    `json:"terminal_error_code"`
			TerminalErrorDomain string `json:"terminal_error_domain"`
		} `json:"player_metrics"`
	} `json:"items"`
}

// errStrRe pulls "<Foo>ErrorDomain error -16172" out of the player-reported
// error string — the numeric error_code field is usually 0 even when the string
// carries the code, so the string is the reliable source.
var errStrRe = regexp.MustCompile(`([A-Za-z]+ErrorDomain) error (-?\d+)`)

// shortDomain trims the verbose "...ErrorDomain" suffix for table width
// (CoreMediaErrorDomain → CoreMedia, NSURLErrorDomain → NSURL).
func shortDomain(d string) string { return strings.TrimSuffix(d, "ErrorDomain") }

// attributeRecoveries overwrites recoveredVia from the DURABLE ClickHouse event
// rows (every POST is a row) rather than the 2s-sampled live state, which drops
// the brief error/player_stuck/live_resync trigger events. Authoritative for the
// summary table. Run AFTER a short flush delay so the last case's rows have landed.
func attributeRecoveries(ctx context.Context, t *testing.T, results []caseResult) {
	for i := range results {
		r := &results[i]
		if r.playID == "" {
			continue
		}
		raw, err := runner.RunHarnessCLI(ctx, "query", "events", r.playID, "--limit", "1000")
		if err != nil {
			t.Logf("attribution: %s/%s events query failed: %v", r.fault, r.kind, err)
			continue
		}
		var resp chEventsResp
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Logf("attribution: %s/%s decode failed: %v", r.fault, r.kind, err)
			continue
		}
		var sawSeek, sawStuck, sawError, sawRestart bool
		var errDom, termDom string
		var errCode, termCode int
		for _, it := range resp.Items {
			pm := it.PlayerMetrics
			switch pm.LastEvent {
			case "live_resync":
				sawSeek = true
			case "player_stuck":
				sawStuck = true
			case "error":
				sawError = true
			case "restart":
				sawRestart = true
			}
			// Aggregate the last non-empty transient error: prefer the numeric
			// fields, else parse the (sticky) error string.
			if pm.ErrorCode != 0 {
				errCode, errDom = pm.ErrorCode, pm.ErrorDomain
			} else if errCode == 0 && pm.Error != "" {
				if m := errStrRe.FindStringSubmatch(pm.Error); m != nil {
					fmt.Sscanf(m[2], "%d", &errCode)
					errDom = m[1]
				}
			}
			if pm.TerminalErrorCode != 0 {
				termCode, termDom = pm.TerminalErrorCode, pm.TerminalErrorDomain
			}
		}
		if errCode != 0 {
			r.errInfo = fmt.Sprintf("%s %d", shortDomain(errDom), errCode)
		}
		if termCode != 0 {
			r.termInfo = fmt.Sprintf("%s %d", shortDomain(termDom), termCode)
		}
		if r.recovered {
			via := classifyRecovery(sawRestart, sawSeek, sawStuck, sawError)
			if via != r.recoveredVia {
				t.Logf("attribution: %s/%s via %s (CH) — live hint was %q", r.fault, r.kind, via, r.recoveredVia)
			}
			r.recoveredVia = via
		}
	}
}
