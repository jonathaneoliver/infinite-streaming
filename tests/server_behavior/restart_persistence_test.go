// restart_persistence_test.go — shaping state must survive a proxy
// restart (issue #686).
//
// Session + shaping state became memory-only in the #470 refactor, which
// stranded restoreShapeApplication() iterating an always-empty list
// (`server_start` reports restored=0 on every boot). Contract this test
// pins down: a session capped at N Mbps BEFORE a restart is still capped
// at N Mbps AFTER it, without any operator/harness re-apply — a
// reconnecting client mid-stream never refetches the master, so nothing
// re-registers the session for it. Today the client instead runs at the
// deployment baseline (~100 Mbps) until the next rate-set: the
// "restore-window rate spike".
//
// This is the ACCEPTANCE test for #686 — it is EXPECTED TO FAIL until
// that fix ships. It is opt-in (skips unless RESTART_CMD is set) because
// restarting a shared deployment kills every live session on the box;
// only run it against a stack you own, e.g.:
//
//	RESTART_CMD="ssh $TEST_SSH 'cd ~/test-dev && docker compose restart go-server'" \
//	  go test -v -run TestRestartPersistence -timeout 15m
//
//	RESTART_WAIT_S=180   how long to wait for the API to come back
//	RESTART_CAPS=5,20    one probe per cap
//	RESTART_MARGIN=1.5   per-fetch breach factor post-restart
//
// Setting RESTART_CMD also disables the package canary (its session is
// legitimately killed by the restart and would false-positive).
package server_behavior

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestRestartPersistence(t *testing.T) {
	if testing.Short() {
		t.Skip("restart persistence skipped in short mode")
	}
	restartCmd := os.Getenv("RESTART_CMD")
	if restartCmd == "" {
		t.Skip("RESTART_CMD not set — this test restarts the target proxy and kills every live session on it; " +
			`opt in with e.g. RESTART_CMD="ssh $TEST_SSH 'cd ~/test-dev && docker compose restart go-server'"`)
	}
	waitS := envInt("RESTART_WAIT_S", 180)
	margin := envFloat("RESTART_MARGIN", 1.5)
	caps, err := parseRates(env("RESTART_CAPS", "5,20"))
	if err != nil {
		t.Fatalf("parse RESTART_CAPS: %v", err)
	}
	measureWindow := 8 * time.Second
	startedAt := time.Now()

	// --- 1. Allocate one probe per cap and verify each is enforced
	// pre-restart (a broken post-restart assertion means nothing if the
	// cap never held in the first place).
	type arm struct {
		p                *probe
		capMbps          int
		preAvg, preMax   float64
		survived         bool
		postAvg, postMax float64
		postN            int
		verdict          string
	}
	arms := make([]*arm, 0, len(caps))
	for _, capMbps := range caps {
		a := &arm{p: newProbe(t), capMbps: capMbps}
		if err := setRateLimit(a.p.c, a.p.apiBase, a.p.sess.InternalPort, capMbps); err != nil {
			t.Fatalf("set cap %d: %v", capMbps, err)
		}
		time.Sleep(settleKernel)
		stop := make(chan struct{})
		done := make(chan []instSample, 1)
		go func() { done <- sampledPull(a.p.c, a.p.top.URL, capMbps, nil, t.Logf, stop) }()
		time.Sleep(measureWindow)
		close(stop)
		a.preAvg, a.preMax, _ = sampleStats(<-done, startedAt, time.Now())
		if a.preMax > float64(capMbps)*margin {
			t.Fatalf("pre-restart: cap %d not enforced (max %.2f Mbps) — fix that before testing persistence", capMbps, a.preMax)
		}
		t.Logf("pre-restart: cap=%d avg=%.2f max=%.2f Mbps", capMbps, a.preAvg, a.preMax)
		arms = append(arms, a)
	}

	// --- 2. Restart the proxy.
	t.Logf("restarting proxy: %s", restartCmd)
	out, err := exec.Command("sh", "-c", restartCmd).CombinedOutput()
	if err != nil {
		t.Fatalf("RESTART_CMD failed: %v\n%s", err, out)
	}
	t.Logf("restart command completed: %s", string(out))

	// Wait for the API to come back — on a FRESH client, so we aren't
	// fooled by stale keep-alive sockets to the dead process.
	fresh := newClient(envBool("THROUGHPUT_INSECURE", defaultInsecure))
	deadline := time.Now().Add(time.Duration(waitS) * time.Second)
	up := false
	for time.Now().Before(deadline) {
		if _, _, err := httpGet(fresh, fmt.Sprintf("https://%s/api/sessions", arms[0].p.apiBase)); err == nil {
			up = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !up {
		t.Fatalf("proxy did not come back within %ds of RESTART_CMD", waitS)
	}
	t.Logf("proxy is back (%.1fs after restart command)", time.Since(startedAt).Seconds())
	time.Sleep(3 * time.Second) // let listeners + any restore path finish

	// --- 3. Post-restart, WITHOUT re-registering anything: is the
	// session still known, and is its cap still enforced on the wire?
	// Pulls go straight at the per-session port (the mid-stream client's
	// view — no master refetch, no config-on-connect).
	for _, a := range arms {
		if _, err := getSessionMap(fresh, a.p.apiBase, a.p.playerID); err == nil {
			a.survived = true
		}
		stop := make(chan struct{})
		done := make(chan []instSample, 1)
		go func() { done <- sampledPull(fresh, a.p.top.URL, a.capMbps, nil, t.Logf, stop) }()
		time.Sleep(measureWindow)
		close(stop)
		a.postAvg, a.postMax, a.postN = sampleStats(<-done, startedAt, time.Now())

		a.verdict = "PASS"
		if !a.survived {
			a.verdict = "session lost"
			t.Errorf("cap %d: session for player %s is gone from /api/sessions after restart — state was not persisted (#686)", a.capMbps, a.p.playerID)
		}
		if a.postN == 0 {
			a.verdict = "port dead"
			t.Errorf("cap %d: no successful pulls on the session port after restart — reconnecting clients are broken outright", a.capMbps)
		} else if a.postMax > float64(a.capMbps)*margin {
			a.verdict = "cap lost"
			t.Errorf("cap %d: post-restart worst fetch %.2f Mbps (threshold %.2f) — shaping did not survive the restart; client rides the baseline until the next rate-set (#686 restore-window spike)",
				a.capMbps, a.postMax, float64(a.capMbps)*margin)
		}
		t.Logf("post-restart: cap=%d survived=%v avg=%.2f max=%.2f (n=%d) %s",
			a.capMbps, a.survived, a.postAvg, a.postMax, a.postN, a.verdict)
	}

	// --- 4. Report.
	sm := serverMatrix{
		Title:   "Shaping persistence across proxy restart (#686)",
		Columns: []string{"cap_mbps", "pre_avg_mbps", "pre_max_mbps", "session_survived", "post_avg_mbps", "post_max_mbps", "post_fetches", "verdict"},
	}
	for _, a := range arms {
		sm.Rows = append(sm.Rows, []string{
			strconv.Itoa(a.capMbps),
			fmt.Sprintf("%.2f", a.preAvg), fmt.Sprintf("%.2f", a.preMax),
			strconv.FormatBool(a.survived),
			fmt.Sprintf("%.2f", a.postAvg), fmt.Sprintf("%.2f", a.postMax),
			strconv.Itoa(a.postN),
			a.verdict,
		})
	}
	arms[0].p.postServerReport(t, "server_restart_persistence",
		fmt.Sprintf("%d capped sessions across a restart", len(arms)),
		startedAt, !t.Failed(), sm)
}
