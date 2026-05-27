// fault_injection_test.go — issue #522.
//
// Verifies the per-kind HTTP fault rules (segment / manifest /
// master_manifest / all) inject failures at the configured frequency,
// with the configured status, in the configured kind ONLY.
//
// The proxy's count-based fault engine is deterministic: with
// consecutive=1 and frequency=N (units=requests) it fails exactly one
// request per N of that kind. So over K requests we expect ~K/N failures.
// We assert the observed count lands in a generous band around K/N (the
// playlist live-edge window + request-count phase make the exact count
// drift by a few), that the failures carry the configured status, and
// that a different request kind is untouched (cross-kind isolation). The
// `all` rule is checked to fault every kind at once.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerFault -timeout 10m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	FAULT_FREQUENCY=10    1-in-N target
//	FAULT_SAMPLES=120     requests pulled per kind (>= 10*freq recommended)
//	FAULT_TYPE=503        configured fault type. Numeric 4xx/5xx codes are
//	                      honored directly; named types: timeout→504,
//	                      connection_refused→503, dns_failure→502,
//	                      rate_limiting→429. Unrecognized → 500.
package server_behavior

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// faultStatusForType mirrors the proxy's switch (cmd/server/main.go ~5445):
// it maps a configured fault TYPE to the HTTP status the proxy emits.
//   - Any numeric 4xx/5xx type (e.g. "503", "429") is honored directly
//     by the proxy's generic numeric-status path.
//   - The named semantic types map as below.
//   - Anything else falls through to 500.
func faultStatusForType(faultType string) int {
	if code, err := strconv.Atoi(faultType); err == nil && code >= 400 && code <= 599 {
		return code
	}
	switch faultType {
	case "timeout":
		return 504
	case "connection_refused":
		return 503
	case "dns_failure":
		return 502
	case "rate_limiting":
		return 429
	default:
		return 500
	}
}

// faultSet builds the session-map keys that arm a count-based 1-in-freq
// fault for one request kind. Notes from the proxy's HandleRequest:
//   - units=requests on every axis forces the deterministic count path
//     (the proxy defaults frequency units to "seconds", the time engine).
//   - segment/manifest faults are gated by shouldApplyFailure(<prefix>_failure_urls,…),
//     which returns false for an empty list — the "All" sentinel (or a
//     matching URL) is required or nothing fires. master_manifest skips
//     that check; the all-rule skips it only when the list is empty.
//     Setting ["All"] everywhere matches all requests uniformly.
func faultSet(prefix, status string, freq, consec int) map[string]any {
	return map[string]any{
		prefix + "_failure_type":         status,
		prefix + "_failure_frequency":    freq,
		prefix + "_consecutive_failures": consec,
		prefix + "_failure_units":        "requests",
		prefix + "_frequency_units":      "requests",
		prefix + "_consecutive_units":    "requests",
		prefix + "_failure_urls":         []string{"All"},
	}
}

func faultClear(prefix string) map[string]any {
	return map[string]any{prefix + "_failure_type": "none"}
}

// pullStatuses fetches up to `samples` requests from the URL supplier and
// returns a status-code histogram. urlFn returns a (possibly refreshing)
// batch of URLs to cycle through.
func (p *probe) pullStatuses(t *testing.T, urlFn func() []string, samples int) map[int]int {
	t.Helper()
	hist := map[int]int{}
	got := 0
	stalls := 0
	for got < samples {
		urls := urlFn()
		if len(urls) == 0 {
			stalls++
			if stalls > 20 {
				t.Logf("no URLs available after 20 tries; stopping at %d/%d", got, samples)
				break
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		stalls = 0
		for _, u := range urls {
			if got >= samples {
				break
			}
			st, _, err := rawStatus(p.c, u)
			if err != nil {
				t.Logf("request error: %v", err)
				continue
			}
			hist[st]++
			got++
			p.heartbeat(0, 0, float64(got), "playing")
		}
		time.Sleep(40 * time.Millisecond)
	}
	return hist
}

type faultRow struct {
	kind       string
	configured string
	freq       int
	consec     int
	samples    int
	failures   int // -1 = not a single number (the all-rule row)
	expected   float64
	crossKind  string // per-other-kind leak counts; all should be =0
}

func TestServerFault(t *testing.T) {
	if testing.Short() {
		t.Skip("fault frequency skipped in short mode")
	}
	freq := envInt("FAULT_FREQUENCY", 10)
	samples := envInt("FAULT_SAMPLES", freq*12)
	// `status` holds the configured fault TYPE (goes into <kind>_failure_type);
	// the proxy maps it to an HTTP status we then assert. Default "503" —
	// honored directly now that the proxy has generic numeric-status support.
	status := env("FAULT_TYPE", "503")
	wantStatus := faultStatusForType(status)
	// Consecutive counts to sweep: 1-in-N and 2-in-N by default.
	consecValues, err := parseRates(env("FAULT_CONSECUTIVE", "1,2"))
	if err != nil {
		t.Fatalf("parse FAULT_CONSECUTIVE: %v", err)
	}

	p := newProbe(t)
	startedAt := time.Now()
	kinds := []struct {
		name string
		urls func() []string
	}{
		{"segment", func() []string { return p.pullOnce(t) }},
		{"manifest", func() []string { return []string{p.top.URL} }},
		{"master_manifest", func() []string { return []string{p.masterURL} }},
	}

	var rows []faultRow

	// Per-kind frequency fidelity + full cross-kind isolation, at each
	// consecutive count. With consecutive=C and frequency=N (units=requests)
	// the engine fails C of every N requests of that kind, so
	// expected = C*samples/N.
	for _, consec := range consecValues {
		for _, fk := range kinds {
			fk, consec := fk, consec
			t.Run(fmt.Sprintf("%s_%din%d", fk.name, consec, freq), func(t *testing.T) {
				if err := patchSession(p.c, p.apiBase, p.sess.SessionID, faultSet(fk.name, status, freq, consec)); err != nil {
					t.Fatalf("arm %s fault: %v", fk.name, err)
				}
				defer patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear(fk.name))
				time.Sleep(settleKernel)

				hist := p.pullStatuses(t, fk.urls, samples)
				failures := hist[wantStatus]
				expected := float64(consec) * float64(samples) / float64(freq)
				t.Logf("%s %d-in-%d: histogram=%v failures(%d)=%d expected~%.1f",
					fk.name, consec, freq, hist, wantStatus, failures, expected)
				if !withinFaultBand(float64(failures), expected) {
					t.Errorf("%s %d-in-%d: %d failures over %d outside band around %.1f",
						fk.name, consec, freq, failures, samples, expected)
				}

				// Cross-kind isolation: EVERY other kind must stay clean.
				var cross strings.Builder
				for _, ok := range kinds {
					if ok.name == fk.name {
						continue
					}
					oh := p.pullStatuses(t, ok.urls, 20)
					leak := oh[wantStatus]
					if leak > 0 {
						t.Errorf("cross-kind leak: %s fault produced %d %d-responses on %s requests (expected 0)",
							fk.name, leak, wantStatus, ok.name)
					}
					fmt.Fprintf(&cross, "%s=%d ", ok.name, leak)
				}
				rows = append(rows, faultRow{
					kind: fk.name, configured: status, freq: freq, consec: consec,
					samples: samples, failures: failures, expected: expected,
					crossKind: strings.TrimSpace(cross.String()),
				})
			})
		}
	}

	// "all" rule overrides per-kind and faults every request kind at once.
	t.Run("all", func(t *testing.T) {
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, faultSet("all", status, freq, 1)); err != nil {
			t.Fatalf("arm all fault: %v", err)
		}
		defer patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear("all"))
		time.Sleep(settleKernel)

		expected := float64(samples) / float64(freq)
		var perKind strings.Builder
		for _, fk := range kinds {
			h := p.pullStatuses(t, fk.urls, samples)
			fails := h[wantStatus]
			t.Logf("all: %s failures=%d (expected ~%.1f)", fk.name, fails, expected)
			if !withinFaultBand(float64(fails), expected) {
				t.Errorf("all-rule: %s failures=%d outside band around %.1f", fk.name, fails, expected)
			}
			fmt.Fprintf(&perKind, "%s=%d ", fk.name, fails)
		}
		rows = append(rows, faultRow{
			kind: "all", configured: status, freq: freq, consec: 1,
			samples: samples, failures: -1, expected: expected,
			crossKind: "faulted " + strings.TrimSpace(perKind.String()),
		})
	})

	// Failure-type selection: every supported HTTP fault type must produce
	// its mapped status at least once. This is independent of the frequency
	// math above — it catches a broken type→status switch (e.g. a renamed
	// case silently falling through to 500). corrupted/socket types live in
	// server_content / server_socket; here we cover the status-returning set:
	// the named types plus the generic numeric path.
	t.Run("type_coverage", func(t *testing.T) {
		coverTypes := []string{
			"timeout", "connection_refused", "dns_failure", "rate_limiting", // named
			"404", "403", "500", "502", "503", "429", // generic numeric
		}
		coverSamples := envInt("FAULT_TYPE_SAMPLES", 8)
		for _, ft := range coverTypes {
			want := faultStatusForType(ft)
			// consec=5/freq=1 → bursts of faults, so the type fires within a
			// few samples regardless of where the engine's cycle sits.
			if err := patchSession(p.c, p.apiBase, p.sess.SessionID, faultSet("segment", ft, 1, 5)); err != nil {
				t.Fatalf("arm type %s: %v", ft, err)
			}
			hist := p.pullStatuses(t, func() []string { return p.pullOnce(t) }, coverSamples)
			patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear("segment"))
			got := hist[want]
			t.Logf("type %-20s want_status=%d hist=%v hits=%d", ft, want, hist, got)
			if got < 1 {
				t.Errorf("failure type %q produced zero %d-responses over %d samples — type selection broken (hist=%v)",
					ft, want, coverSamples, hist)
			}
			rows = append(rows, faultRow{
				kind: "type:" + ft, configured: ft, freq: 1, consec: 1,
				samples: coverSamples, failures: got, expected: 1,
				crossKind: fmt.Sprintf("status=%d", want),
			})
		}
	})

	printFaultMatrix(t, rows)

	sm := serverMatrix{
		Title:   "Fault injection (per-kind C-in-N frequency + cross-kind isolation + failure-type coverage)",
		Columns: []string{"kind", "type", "rate", "samples", "failures", "expected", "other_kinds"},
	}
	for _, r := range rows {
		failStr := fmt.Sprintf("%d", r.failures)
		if r.failures < 0 {
			failStr = "—"
		}
		sm.Rows = append(sm.Rows, []string{
			r.kind,
			r.configured,
			fmt.Sprintf("%d-in-%d", r.consec, r.freq),
			fmt.Sprintf("%d", r.samples),
			failStr,
			fmt.Sprintf("%.1f", r.expected),
			r.crossKind,
		})
	}
	p.postServerReport(t, "server_fault", fmt.Sprintf("%s, 1-in-%d & 2-in-%d, +type coverage", status, freq, freq), startedAt, !t.Failed(), sm)
}

// withinFaultBand accepts the count-based engine's drift: the deterministic
// 1-in-N can be off by a couple over a finite window with live-edge churn.
func withinFaultBand(observed, expected float64) bool {
	if expected <= 0 {
		return observed == 0
	}
	lo := expected * 0.4
	hi := expected*1.6 + 1
	return observed >= lo && observed <= hi
}

func printFaultMatrix(t *testing.T, rows []faultRow) {
	t.Logf("\n=== fault injection matrix ===")
	t.Logf("%-18s %-8s %-10s %-9s %-10s %-10s %s",
		"kind", "type", "rate", "samples", "failures", "expected", "other_kinds")
	for _, r := range rows {
		failStr := fmt.Sprintf("%d", r.failures)
		if r.failures < 0 {
			failStr = "—"
		}
		t.Logf("%-18s %-8s %-10s %-9d %-10s %-10.1f %s",
			r.kind, r.configured, fmt.Sprintf("%d-in-%d", r.consec, r.freq),
			r.samples, failStr, r.expected, r.crossKind)
	}
}
