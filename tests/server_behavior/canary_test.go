// canary_test.go — package-level cross-session interference detector.
//
// Issue #816 taught us that the shaping bugs worth catching are
// CROSS-session: one session's lifecycle event (allocation sweep,
// rate-clear, teardown) silently wiping another live session's tc cap.
// Every test in this package is single-session, so none of them can see
// that class of failure in the sessions it isn't looking at — unless
// someone else is watching.
//
// TestMain therefore runs a background CANARY: one extra proxy session
// pinned at a low rate cap, pulling continuously for the whole `go test`
// invocation. At exit it checks its per-fetch throughput history: any
// fetch that ran ≥ SB_CANARY_MARGIN × its cap means some test's activity
// blew the canary's shaper away (the #816 signature), and the run fails
// even if every individual test passed. Breaches are printed with
// timestamps so they can be correlated against the -v test log to find
// the triggering operation.
//
//	SB_CANARY=0            disable
//	SB_CANARY_CAP_MBPS=5
//	SB_CANARY_MARGIN=3     breach factor (the bug signature is 10–100×)
//
// The canary is skipped in -short mode, when disabled, and when
// RESTART_CMD is set (TestRestartPersistence legitimately restarts the
// proxy, which would kill the canary's session and false-positive).
// Bootstrap failure downgrades to a warning — the canary must never be
// the reason an offline run can't start.
package server_behavior

import (
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMain(m *testing.M) {
	flag.Parse()
	var can *canary
	if !testing.Short() && envBool("SB_CANARY", true) && os.Getenv("RESTART_CMD") == "" {
		var err error
		can, err = startCanary()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[canary] disabled — bootstrap failed: %v\n", err)
		}
	}
	code := m.Run()
	if can != nil && can.stopAndReport() && code == 0 {
		code = 1
	}
	os.Exit(code)
}

type canary struct {
	c        *http.Client
	apiBase  string
	playerID string
	sess     sessionInfo
	capMbps  int
	margin   float64
	stop     chan struct{}
	done     chan []instSample
}

// startCanary bootstraps one extra session (same steps as newProbe, but
// t-free: discover → master fetch → session lookup → cap) and starts a
// continuous sampled pull against it.
func startCanary() (*canary, error) {
	host := env("THROUGHPUT_HOST", defaultHost)
	apiPort := env("THROUGHPUT_API_PORT", defaultAPIPort)
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)
	apiBase := host + ":" + apiPort
	shaperBase := host + ":" + shaperPortFromUI(apiPort)
	c := newClient(insecure)

	content, err := discoverContent(c, apiBase)
	if err != nil {
		return nil, fmt.Errorf("discover content: %w", err)
	}
	playerID := "canary-" + uuid.New().String()
	masterURL := fmt.Sprintf("https://%s/go-live/%s/master_6s.m3u8?player_id=%s",
		shaperBase, url.PathEscape(content), url.QueryEscape(playerID))
	masterBody, finalMasterURL, err := httpGet(c, masterURL)
	if err != nil {
		return nil, fmt.Errorf("master fetch: %w", err)
	}
	sess, err := findSession(c, apiBase, playerID)
	if err != nil {
		return nil, err
	}
	variants, err := parseMaster(masterBody, finalMasterURL)
	if err != nil {
		return nil, err
	}
	top := pickTopVariant(variants)

	can := &canary{
		c: c, apiBase: apiBase, playerID: playerID, sess: sess,
		capMbps: envInt("SB_CANARY_CAP_MBPS", 5),
		margin:  envFloat("SB_CANARY_MARGIN", 3),
		stop:    make(chan struct{}),
		done:    make(chan []instSample, 1),
	}
	if err := setRateLimit(c, apiBase, sess.InternalPort, can.capMbps); err != nil {
		return nil, fmt.Errorf("set canary cap: %w", err)
	}
	time.Sleep(settleKernel)
	go func() {
		// Quiet logf: transient pull errors self-heal via segment
		// refresh; only breaches matter, and those are found at exit.
		can.done <- sampledPull(c, top.URL, can.capMbps, nil, func(string, ...any) {}, can.stop)
	}()
	fmt.Fprintf(os.Stderr, "[canary] watching: player_id=%s port=%d cap=%d Mbps breach>%.1f×\n",
		playerID, sess.InternalPort, can.capMbps, can.margin)
	return can, nil
}

// stopAndReport ends the pull, tears the canary session down, and
// returns true if any fetch breached cap × margin.
func (can *canary) stopAndReport() bool {
	close(can.stop)
	samples := <-can.done
	_ = setRateLimit(can.c, can.apiBase, can.sess.InternalPort, 0)
	_ = deleteSession(can.c, can.apiBase, can.sess.SessionID)

	if len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "[canary] WARNING: collected 0 samples — canary saw nothing (not a failure)\n")
		return false
	}
	threshold := float64(can.capMbps) * can.margin
	avg, max, n := sampleStats(samples, samples[0].at, time.Now())
	breached := false
	for _, s := range samples {
		if s.mbps > threshold {
			breached = true
			fmt.Fprintf(os.Stderr, "[canary] BREACH at %s: %.2f Mbps under a %d Mbps cap (threshold %.2f) — some concurrent operation wiped the canary's shaper (#816 signature)\n",
				s.at.UTC().Format("15:04:05.000"), s.mbps, can.capMbps, threshold)
		}
	}
	fmt.Fprintf(os.Stderr, "[canary] done: fetches=%d avg=%.2f max=%.2f Mbps cap=%d threshold=%.2f breached=%v\n",
		n, avg, max, can.capMbps, threshold, breached)
	if breached {
		fmt.Fprintf(os.Stderr, "[canary] FAILING the run — correlate breach timestamps with the -v test log to find the trigger\n")
	}
	return breached
}
