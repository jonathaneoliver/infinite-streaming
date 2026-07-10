// server_isolation_test.go — cross-session shaping isolation under
// lifecycle churn (issue #816 / fix #818).
//
// The contract every other test in this package takes for granted:
// "operator sets X on session A, the server delivers X on session A —
// and NOTHING ELSE changes X." Issue #816 broke the second half: the
// per-session u32 filters all share prio 1 on parent 1:0, and the
// config-on-start sweep deleted by match-spec, which can remove the
// WRONG filter. Starting session B silently wiped live session A's tc
// filter; A's traffic fell through to the uncapped 10 Gbps HTB
// `default 999` class (70–770 Mbps observed under a ~7 Mbps cap) until
// A's next rate-set reinstalled it. #818 fixed the delete to resolve
// the exact u32 handle first.
//
// No single-session test can see that failure mode, and no
// window-average assertion can either (a 2 s line-rate burst barely
// moves a 60 s average). So this test:
//
//  1. starts a VICTIM session pinned at a low cap, pulling continuously
//     with per-fetch throughput samples (the max transient is the
//     signal, not the average);
//  2. churns AGGRESSOR sessions through the full lifecycle around it —
//     allocation (the config-on-start sweep, #816's production
//     trigger), rate-set, rate-clear (the RemoveFilter path), and
//     DELETE teardown;
//  3. asserts the victim's single worst fetch never exceeded
//     cap × ISOLATION_MARGIN, and prints per-round windows so a breach
//     points at the lifecycle event that caused it.
//
// Bidirectional checks ride along: each aggressor's own cap must hold
// while the victim occupies its slot, and a rate-cleared (rate=0)
// aggressor must fall back to the deployment BASELINE cap, not to
// uncapped line rate (`clear_semantics` — the fail-open guard; see
// #816 remediation 2).
//
//	ISOLATION_VICTIM_CAP_MBPS=8
//	ISOLATION_AGGRESSOR_CAP_MBPS=3
//	ISOLATION_CHURN_ROUNDS=4
//	ISOLATION_MARGIN=1.5          breach factor (bug signature is 10–100×)
//	ISOLATION_BASELINE_WINDOW_S=8 quiet pre-churn measurement
//
// Skipped in short mode. Run:
//
//	cd tests/server_behavior && go test -v -run TestServerIsolation -timeout 10m
package server_behavior

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestServerIsolation(t *testing.T) {
	if testing.Short() {
		t.Skip("isolation churn skipped in short mode")
	}
	victimCap := envInt("ISOLATION_VICTIM_CAP_MBPS", 8)
	aggrCap := envInt("ISOLATION_AGGRESSOR_CAP_MBPS", 3)
	rounds := envInt("ISOLATION_CHURN_ROUNDS", 4)
	margin := envFloat("ISOLATION_MARGIN", 1.5)
	baselineWindow := time.Duration(envInt("ISOLATION_BASELINE_WINDOW_S", 8)) * time.Second
	startedAt := time.Now()

	victim := newProbe(t)
	baselineMbps := fetchDefaultRateMbps(victim.c, victim.apiBase)
	if err := setRateLimit(victim.c, victim.apiBase, victim.sess.InternalPort, victimCap); err != nil {
		t.Fatalf("set victim cap: %v", err)
	}
	time.Sleep(settleKernel)

	// Victim puller: continuous bounded fetches, per-fetch samples.
	stop := make(chan struct{})
	victimDone := make(chan []instSample, 1)
	go func() {
		victimDone <- sampledPull(victim.c, victim.top.URL, victimCap,
			func(mbps float64) { victim.heartbeat(round2(mbps), round2(mbps), time.Since(startedAt).Seconds(), "playing") },
			t.Logf, stop)
	}()

	type window struct {
		label      string
		from, to   time.Time
		aggrAvg    float64 // aggressor's own capped throughput (0 for baseline row)
		aggrMax    float64
		clearedMax float64 // aggressor's worst fetch after rate=0 (baseline fallback)
	}
	var windows []window

	// Quiet baseline: no churn, victim alone.
	base := window{label: "baseline (no churn)", from: time.Now()}
	time.Sleep(baselineWindow)
	base.to = time.Now()
	windows = append(windows, base)

	// Churn rounds: each full aggressor lifecycle is four distinct
	// trigger events against the victim's kernel state.
	for round := 1; round <= rounds; round++ {
		w := window{label: fmt.Sprintf("round %d churn", round), from: time.Now()}

		// (1) allocation — runs the config-on-start shaping sweep.
		aggr := newProbe(t)

		// (2) rate-set + verify the aggressor's own cap holds while the
		// victim occupies its slot (bidirectional isolation).
		if err := setRateLimit(aggr.c, aggr.apiBase, aggr.sess.InternalPort, aggrCap); err != nil {
			t.Errorf("round %d: set aggressor cap: %v", round, err)
		}
		time.Sleep(settleKernel)
		aggrStop := make(chan struct{})
		aggrDone := make(chan []instSample, 1)
		go func() {
			aggrDone <- sampledPull(aggr.c, aggr.top.URL, aggrCap, nil, t.Logf, aggrStop)
		}()
		time.Sleep(6 * time.Second)
		close(aggrStop)
		aggrSamples := <-aggrDone
		w.aggrAvg, w.aggrMax, _ = sampleStats(aggrSamples, w.from, time.Now())

		// (3) rate-clear — the RemoveFilter path. Cleared means "no
		// operator override": the session must fall back to the
		// deployment BASELINE cap, never to uncapped line rate.
		if err := setRateLimit(aggr.c, aggr.apiBase, aggr.sess.InternalPort, 0); err != nil {
			t.Errorf("round %d: clear aggressor cap: %v", round, err)
		}
		time.Sleep(settleKernel)
		clearCap := baselineMbps
		if clearCap <= 0 {
			clearCap = 100 // sizing only; the assertion is skipped below
		}
		clearedStop := make(chan struct{})
		clearedDone := make(chan []instSample, 1)
		go func() {
			clearedDone <- sampledPull(aggr.c, aggr.top.URL, clearCap, nil, t.Logf, clearedStop)
		}()
		time.Sleep(4 * time.Second)
		close(clearedStop)
		_, w.clearedMax, _ = sampleStats(<-clearedDone, w.from, time.Now())

		// (4) teardown.
		if err := deleteSession(aggr.c, aggr.apiBase, aggr.sess.SessionID); err != nil {
			t.Logf("round %d: delete aggressor session: %v", round, err)
		}
		time.Sleep(2 * time.Second)
		w.to = time.Now()
		windows = append(windows, w)
	}

	close(stop)
	samples := <-victimDone
	_ = setRateLimit(victim.c, victim.apiBase, victim.sess.InternalPort, 0)

	// --- assertions ---------------------------------------------------
	threshold := float64(victimCap) * margin
	runAvg, runMax, runN := sampleStats(samples, startedAt, time.Now())
	if runN == 0 {
		t.Fatalf("victim collected no samples — cannot assess isolation")
	}
	// Isolation: no single victim fetch may exceed the cap by margin.
	for _, s := range samples {
		if s.mbps > threshold {
			t.Errorf("ISOLATION BREACH: victim fetch at %s ran %.2f Mbps under a %d Mbps cap (threshold %.2f) — a churn event wiped the victim's shaper (#816 signature)",
				s.at.UTC().Format("15:04:05.000"), s.mbps, victimCap, threshold)
		}
	}
	// Sanity: the victim was genuinely capped and pulling (expect ~95% of cap).
	if runAvg < float64(victimCap)*0.5 {
		t.Errorf("victim busy-time avg %.2f Mbps is <50%% of its %d Mbps cap — probe wasn't saturating; isolation result not meaningful", runAvg, victimCap)
	}

	aggrThreshold := float64(aggrCap) * margin
	verdicts := make([]string, len(windows))
	for i, w := range windows {
		vAvg, vMax, vN := sampleStats(samples, w.from, w.to)
		verdict := "PASS"
		if vMax > threshold {
			verdict = "victim breach"
		}
		if i > 0 { // churn rounds only
			if w.aggrMax > aggrThreshold {
				verdict = "aggressor uncapped"
				t.Errorf("%s: aggressor's own worst fetch %.2f Mbps exceeded its %d Mbps cap (threshold %.2f) — new sessions aren't being shaped",
					w.label, w.aggrMax, aggrCap, aggrThreshold)
			}
			if baselineMbps > 0 && w.clearedMax > float64(baselineMbps)*margin {
				verdict = "clear fail-open"
				t.Errorf("%s: rate-cleared aggressor hit %.2f Mbps, above baseline %d Mbps × %.2f — rate=0 fell through to uncapped line rate instead of the baseline cap (fail-open, #816 remediation 2)",
					w.label, w.clearedMax, baselineMbps, margin)
			}
		}
		verdicts[i] = verdict
		t.Logf("%-22s victim avg=%.2f max=%.2f (n=%d)  aggr avg=%.2f max=%.2f  cleared max=%.2f  %s",
			w.label, vAvg, vMax, vN, w.aggrAvg, w.aggrMax, w.clearedMax, verdict)
	}
	t.Logf("victim whole-run: avg=%.2f Mbps max=%.2f Mbps fetches=%d cap=%d threshold=%.2f baseline=%d",
		runAvg, runMax, runN, victimCap, threshold, baselineMbps)

	// --- report ---------------------------------------------------------
	sm := serverMatrix{
		Title:   fmt.Sprintf("Cross-session isolation under churn (victim cap %d Mbps, margin ×%.2f)", victimCap, margin),
		Columns: []string{"window", "victim_avg_mbps", "victim_max_mbps", "victim_fetches", "aggr_avg_mbps", "aggr_max_mbps", "cleared_max_mbps", "verdict"},
	}
	for i, w := range windows {
		vAvg, vMax, vN := sampleStats(samples, w.from, w.to)
		aggrAvg, aggrMax, clearedMax := "—", "—", "—"
		if i > 0 {
			aggrAvg = fmt.Sprintf("%.2f", w.aggrAvg)
			aggrMax = fmt.Sprintf("%.2f", w.aggrMax)
			clearedMax = fmt.Sprintf("%.2f", w.clearedMax)
		}
		sm.Rows = append(sm.Rows, []string{
			w.label,
			fmt.Sprintf("%.2f", vAvg),
			fmt.Sprintf("%.2f", vMax),
			strconv.Itoa(vN),
			aggrAvg, aggrMax, clearedMax,
			verdicts[i],
		})
	}
	victim.postServerReport(t, "server_isolation",
		fmt.Sprintf("%d churn rounds, victim max %.2f/%.2f Mbps", rounds, runMax, threshold),
		startedAt, !t.Failed(), sm)
}
