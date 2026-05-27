// server_socket_test.go — socket-phase fault coverage (epic #518, issue #523).
//
// The proxy's nine socket-phase faults each hijack the client TCP socket
// and emit a SPECIFIC on-the-wire shape, defined by TWO orthogonal axes:
//
//	PHASE  — how far the response got before the cut:
//	  connect_*     → nothing written; client never sees a status line.
//	  first_byte_*  → "HTTP/1.1 200 OK" + chunked headers, then cut; 0 body.
//	  body_*        → headers + ~64KB of REAL upstream bytes, then cut.
//	TERMINATOR — how the socket dies:
//	  *_reset       → SO_LINGER=0 close → RST, immediately (elapsed ≈ 0).
//	  *_delayed     → hold socketDelayDuration (12s) → graceful FIN.
//	  *_hang        → hold socketHangDuration (90s) → our probe deadline fires.
//
// We arm each fault as a segment fault and drive a raw HTTP GET with a
// bounded deadline, then classify what the client saw against the contract
// in .claude/standards/fault-injection-wire-contract.md. We assert BOTH
// axes — phase (did headers / body arrive?) and terminator — because the
// point of nine distinct faults is that they are distinguishable.
//
// Two behaviours of the environment shape how we measure:
//
//   - The count-based engine at consec=C/freq=N faults C of every N requests
//     of a kind; there is NO "every request" setting (freq=1 is 1-in-2 — the
//     recovery window itself consumes a count). So a single probe may land in
//     a clean slot. We therefore RETRY the probe until a fault actually fires
//     (a complete fetch — terminator "ok" — means no fault landed).
//   - An RST surfaces as EOF / "unexpected EOF" through the TLS layer here,
//     not "connection reset", so reset-vs-FIN can't be told apart by errno.
//     We classify the terminator by TIMING instead: reset ≈ immediate,
//     delayed ≈ socketDelayDuration, hang = our deadline firing.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerSocketFaults -timeout 10m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	SOCKET_PROBE_TIMEOUT_S=20   per-attempt client deadline. Must sit ABOVE the
//	                            proxy's socketDelayDuration (12s) so a *_delayed
//	                            fault closes before the deadline, and BELOW
//	                            socketHangDuration (90s) so a *_hang fault is
//	                            seen as our deadline firing.
//	SOCKET_FAULT_ATTEMPTS=5     max probes per fault before giving up on a fire.
package server_behavior

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// socketObs is the raw outcome of one faulted-or-not request.
type socketObs struct {
	gotHeaders bool          // a status line/headers were received
	status     int           // HTTP status, if headers arrived
	bodyBytes  int64         // body bytes read before the cut
	completed  bool          // body read to a clean EOF → NO fault fired
	timedOut   bool          // our probe deadline fired (the *_hang signature)
	elapsed    time.Duration // wall time until the outcome
	rawErr     string        // underlying error text, for the matrix
}

// probeSocketFault drives one raw GET with a bounded deadline and reports
// what the client observed. No status code is treated as an error — the
// fault IS the result we're measuring.
func probeSocketFault(c *http.Client, u string, deadline time.Duration) socketObs {
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return socketObs{rawErr: err.Error()}
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("User-Agent", "server-behavior-probe/1.0")

	resp, err := c.Do(req)
	if err != nil {
		// No response → connect-phase fault (cut before the status line).
		return socketObs{
			gotHeaders: false,
			timedOut:   ctx.Err() == context.DeadlineExceeded,
			elapsed:    time.Since(start),
			rawErr:     err.Error(),
		}
	}
	defer resp.Body.Close()
	n, rerr := io.Copy(io.Discard, resp.Body)
	obs := socketObs{
		gotHeaders: true,
		status:     resp.StatusCode,
		bodyBytes:  n,
		completed:  rerr == nil,
		timedOut:   ctx.Err() == context.DeadlineExceeded,
		elapsed:    time.Since(start),
	}
	if rerr != nil {
		obs.rawErr = rerr.Error()
	}
	return obs
}

// terminatorOf labels a faulted outcome by TIMING (see file header). Only
// meaningful when the fault fired (obs.completed == false).
func terminatorOf(o socketObs, delayThreshold time.Duration) string {
	switch {
	case o.timedOut:
		return "hang"
	case o.elapsed >= delayThreshold:
		return "delayed"
	default:
		return "reset"
	}
}

func TestServerSocketFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("socket faults skipped in short mode")
	}
	deadline := time.Duration(envInt("SOCKET_PROBE_TIMEOUT_S", 20)) * time.Second
	maxAttempts := envInt("SOCKET_FAULT_ATTEMPTS", 5)
	// Halfway between an immediate reset and the 12s delayed-close cleanly
	// separates the two; hang is caught earlier by the deadline flag.
	const delayThreshold = 6 * time.Second

	p := newProbe(t)
	startedAt := time.Now()

	// Dedicated client: a faulted socket is always cut, so keep-alive reuse
	// buys nothing and a poisoned pooled connection could bleed into the
	// next probe. Fresh transport, no idle reuse.
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)
	probeClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: insecure},
			DisableKeepAlives: true,
		},
		// No client.Timeout — the per-request context deadline governs.
	}

	type phase int
	const (
		phaseConnect phase = iota
		phaseFirstByte
		phaseBody
	)
	cases := []struct {
		faultType string
		phase     phase
		term      string
	}{
		{"request_connect_reset", phaseConnect, "reset"},
		{"request_connect_hang", phaseConnect, "hang"},
		{"request_connect_delayed", phaseConnect, "delayed"},
		{"request_first_byte_reset", phaseFirstByte, "reset"},
		{"request_first_byte_hang", phaseFirstByte, "hang"},
		{"request_first_byte_delayed", phaseFirstByte, "delayed"},
		{"request_body_reset", phaseBody, "reset"},
		{"request_body_hang", phaseBody, "hang"},
		{"request_body_delayed", phaseBody, "delayed"},
	}

	phaseName := func(p phase) string {
		switch p {
		case phaseConnect:
			return "connect(no-headers)"
		case phaseFirstByte:
			return "first_byte(headers,0body)"
		default:
			return "body(headers,+bytes)"
		}
	}
	obsPhase := func(o socketObs) phase {
		if !o.gotHeaders {
			return phaseConnect
		}
		if o.bodyBytes == 0 {
			return phaseFirstByte
		}
		return phaseBody
	}

	var rows [][]string
	for _, tc := range cases {
		tc := tc
		t.Run(strings.TrimPrefix(tc.faultType, "request_"), func(t *testing.T) {
			// Arm as a segment fault. A short consecutive burst (consec=5,
			// freq=1) biases toward an immediate fire; the retry loop covers
			// the slots where the engine's cycle skips a request.
			if err := patchSession(p.c, p.apiBase, p.sess.SessionID, faultSet("segment", tc.faultType, 1, 5)); err != nil {
				t.Fatalf("arm %s: %v", tc.faultType, err)
			}
			defer patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear("segment"))
			time.Sleep(settleKernel)

			var obs socketObs
			attempts := 0
			for attempts = 1; attempts <= maxAttempts; attempts++ {
				segs := p.pullOnce(t)
				if len(segs) == 0 {
					t.Fatalf("no segment URLs to probe")
				}
				obs = probeSocketFault(probeClient, segs[len(segs)-1], deadline)
				p.heartbeat(0, 0, 0, "rebuffering")
				if !obs.completed {
					break // a fault fired — this is the observation we want
				}
			}
			if obs.completed {
				t.Fatalf("%s: no fault fired in %d attempts (engine cycle never landed on our probe)", tc.faultType, maxAttempts)
			}

			gotPhase := obsPhase(obs)
			gotTerm := terminatorOf(obs, delayThreshold)
			phaseOK := gotPhase == tc.phase
			termOK := gotTerm == tc.term
			if !phaseOK {
				t.Errorf("%s: phase = %s, want %s (gotHeaders=%v status=%d body=%d)",
					tc.faultType, phaseName(gotPhase), phaseName(tc.phase),
					obs.gotHeaders, obs.status, obs.bodyBytes)
			}
			if !termOK {
				t.Errorf("%s: terminator = %q, want %q (elapsed=%.1fs timedOut=%v err=%q)",
					tc.faultType, gotTerm, tc.term, obs.elapsed.Seconds(), obs.timedOut, obs.rawErr)
			}

			// Server-side cross-check: the proxy bumps a per-type counter.
			proxyCount := "—"
			if sm, err := getSessionMap(p.c, p.apiBase, p.playerID); err == nil {
				if v, ok := mapFloat(sm, "fault_count_"+tc.faultType); ok {
					proxyCount = strconv.Itoa(int(v))
				}
			}

			t.Logf("%-28s phase=%-26s term=%-8s %8db %5.1fs attempts=%d proxy_count=%s",
				tc.faultType, phaseName(gotPhase), gotTerm,
				obs.bodyBytes, obs.elapsed.Seconds(), attempts, proxyCount)

			pass := "yes"
			if !phaseOK || !termOK {
				pass = "NO"
			}
			rows = append(rows, []string{
				strings.TrimPrefix(tc.faultType, "request_"),
				phaseName(tc.phase), phaseName(gotPhase),
				tc.term, gotTerm,
				fmt.Sprintf("%d", obs.bodyBytes),
				obs.elapsed.Round(100 * time.Millisecond).String(),
				proxyCount,
				pass,
			})
		})
	}

	t.Logf("\n=== socket-phase fault matrix ===")
	for _, r := range rows {
		t.Logf("%-26s exp[%s|%s] got[%s|%s] %sb %s proxy=%s %s",
			r[0], r[1], r[3], r[2], r[4], r[5], r[6], r[7], r[8])
	}

	p.postServerReport(t, "server_socket", "9 socket-phase faults (phase × terminator)", startedAt, !t.Failed(),
		serverMatrix{
			Title:   "Socket-phase faults (client-observed wire shape vs contract)",
			Columns: []string{"fault", "exp_phase", "obs_phase", "exp_term", "obs_term", "body_bytes", "elapsed", "proxy_count", "ok"},
			Rows:    rows,
		})
}
