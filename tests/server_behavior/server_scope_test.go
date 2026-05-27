// server_scope_test.go — fault scope-checkbox coverage (epic #518, issue #522).
//
// The dashboard lets an operator scope an HTTP fault to specific URLs /
// variants (the "scope" checkboxes), not just a whole request kind. The
// proxy implements this in shouldApplyFailure: a fault only fires when the
// request's variant directory (pathParent) or filename matches one of the
// configured *_failure_urls entries ("All" matches everything).
//
// This test proves the variant scope actually isolates: arm a segment fault
// scoped to ONE variant's directory token, then
//
//   - pull the in-scope variant  → faults appear at the configured rate;
//   - pull an out-of-scope variant → ZERO faults (scope held).
//
// The proxy advances the fault cycle ONLY for in-scope requests
// (handleSegmentFailure returns "none" before HandleFailure when the URL
// doesn't match), so the out-of-scope variant is clean by construction —
// this verifies that wiring end to end.
//
// Sibling coverage of the other scope axes lives elsewhere:
//   - request-KIND scope (segment vs manifest vs master)  → server_fault.
//   - transfer-timeout applies_segments scope             → server_transfer.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerScope -timeout 5m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	SCOPE_FREQUENCY=3    1-in-N fault rate on the in-scope variant
//	SCOPE_SAMPLES=30     segment requests pulled per variant
//	FAULT_TYPE=503       configured fault type (shared with server_fault)
package server_behavior

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// variantDir returns the variant directory token from a segment URL — the
// path element the proxy uses as `variant` in shouldApplyFailure (its
// pathParent). e.g. ".../2160p/segment_00027.m4s" → "2160p".
func variantDir(segURL string) string {
	u, err := url.Parse(segURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}

// pullVariant fetches a specific variant's media playlist and returns its
// segment URLs (pullOnce is hardwired to the top variant).
func (p *probe) pullVariant(variantURL string) []string {
	body, final, err := httpGet(p.c, variantURL)
	if err != nil {
		return nil
	}
	segs, err := parseMediaPlaylist(body, final)
	if err != nil {
		return nil
	}
	return segs
}

func TestServerScope(t *testing.T) {
	if testing.Short() {
		t.Skip("scope checks skipped in short mode")
	}
	p := newProbe(t)
	if len(p.variants) < 2 {
		t.Skipf("need >=2 variants to test variant scoping, got %d", len(p.variants))
	}
	startedAt := time.Now()
	freq := envInt("SCOPE_FREQUENCY", 3)
	samples := envInt("SCOPE_SAMPLES", 30)
	status := env("FAULT_TYPE", "503")
	wantStatus := faultStatusForType(status)

	// p.variants is sorted descending by pickTopVariant — fault the top
	// variant, leave the bottom one as the out-of-scope control.
	faultVar := p.variants[0]
	otherVar := p.variants[len(p.variants)-1]

	fseg := p.pullVariant(faultVar.URL)
	oseg := p.pullVariant(otherVar.URL)
	if len(fseg) == 0 || len(oseg) == 0 {
		t.Fatalf("could not pull segments: in-scope=%d out-of-scope=%d", len(fseg), len(oseg))
	}
	faultTok := variantDir(fseg[0])
	otherTok := variantDir(oseg[0])
	if faultTok == "" || otherTok == "" || faultTok == otherTok {
		t.Fatalf("indistinct variant tokens: in-scope=%q out-of-scope=%q", faultTok, otherTok)
	}
	// The scope token must NOT also appear in the out-of-scope variant's
	// URL, or shouldApplyFailure's substring match would leak the fault.
	if strings.Contains(oseg[0], faultTok) {
		t.Fatalf("scope token %q also appears in out-of-scope URL %q — can't isolate", faultTok, oseg[0])
	}
	t.Logf("in-scope variant=%.2f Mbps token=%q ; out-of-scope variant=%.2f Mbps token=%q",
		float64(faultVar.BandwidthBps)/1e6, faultTok,
		float64(otherVar.BandwidthBps)/1e6, otherTok)

	// Arm a segment fault scoped to ONLY the in-scope variant's directory.
	set := faultSet("segment", status, freq, 1)
	set["segment_failure_urls"] = []string{faultTok}
	if err := patchSession(p.c, p.apiBase, p.sess.SessionID, set); err != nil {
		t.Fatalf("arm scoped fault: %v", err)
	}
	defer patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear("segment"))
	time.Sleep(settleKernel)

	inHist := p.pullStatuses(t, func() []string { return p.pullVariant(faultVar.URL) }, samples)
	inFaults := inHist[wantStatus]
	outHist := p.pullStatuses(t, func() []string { return p.pullVariant(otherVar.URL) }, samples)
	outFaults := outHist[wantStatus]

	t.Logf("in-scope(%s): hist=%v faults(%d)=%d ; out-of-scope(%s): hist=%v faults=%d",
		faultTok, inHist, wantStatus, inFaults, otherTok, outHist, outFaults)

	if inFaults == 0 {
		t.Errorf("scoped fault produced ZERO %d-responses on in-scope variant %q (over %d samples) — scope too narrow or fault not firing",
			wantStatus, faultTok, samples)
	}
	if outFaults != 0 {
		t.Errorf("scope LEAK: %d %d-responses on out-of-scope variant %q (expected 0)",
			outFaults, wantStatus, otherTok)
	}

	pass := func(ok bool) string {
		if ok {
			return "yes"
		}
		return "NO"
	}
	rows := [][]string{
		{"in-scope/" + faultTok, fmt.Sprintf("%d-in-%d", 1, freq),
			fmt.Sprintf("%d", samples), fmt.Sprintf("%d", inFaults), ">0", pass(inFaults > 0)},
		{"out-of-scope/" + otherTok, "scoped out",
			fmt.Sprintf("%d", samples), fmt.Sprintf("%d", outFaults), "0", pass(outFaults == 0)},
	}

	p.postServerReport(t, "server_scope",
		fmt.Sprintf("variant scope: %s faulted, %s clean", faultTok, otherTok),
		startedAt, !t.Failed(),
		serverMatrix{
			Title:   "Fault scope checkboxes (segment fault scoped to one variant directory)",
			Columns: []string{"variant", "rate", "samples", "faults", "expected", "ok"},
			Rows:    rows,
		})
}
