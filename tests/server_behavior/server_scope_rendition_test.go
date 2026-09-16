// server_scope_rendition_test.go — #922 Tier 2 end-to-end.
//
// Proves fault scoping by a VARIANT predicate (here bandwidth) matches real
// segments — which only works because the proxy builds an authoritative
// segmentDir→rendition map by parsing each media playlist as it transits
// (pullVariant fetches it). Before Tier 2 the classifier couldn't map a segment
// path to its bandwidth/resolution (the variant URL is a playlist name sharing
// nothing with the segment dir), so a variant-predicate fault matched nothing.
package server_behavior

import (
	"testing"
	"time"
)

func TestServerScopeByRendition(t *testing.T) {
	if testing.Short() {
		t.Skip("rendition scope check skipped in short mode")
	}
	p := newProbe(t)
	if len(p.variants) < 2 {
		t.Skipf("need >=2 variants, got %d", len(p.variants))
	}
	freq := envInt("SCOPE_FREQUENCY", 3)
	samples := envInt("SCOPE_SAMPLES", 30)
	status := env("FAULT_TYPE", "503")
	wantStatus := faultStatusForType(status)

	faultVar := p.variants[0]                 // top (highest bandwidth)
	otherVar := p.variants[len(p.variants)-1] // bottom
	if faultVar.BandwidthBps <= otherVar.BandwidthBps {
		t.Fatalf("variants not bandwidth-distinct: top=%d bottom=%d", faultVar.BandwidthBps, otherVar.BandwidthBps)
	}

	// Fetch both media playlists so the proxy records their rendition dirs
	// (segmentDir → {bandwidth,...}) — the map this feature is about.
	fseg := p.pullVariant(faultVar.URL)
	oseg := p.pullVariant(otherVar.URL)
	if len(fseg) == 0 || len(oseg) == 0 {
		t.Fatalf("could not pull segments: top=%d bottom=%d", len(fseg), len(oseg))
	}
	t.Logf("in-scope top=%.2f Mbps ; out-of-scope bottom=%.2f Mbps",
		float64(faultVar.BandwidthBps)/1e6, float64(otherVar.BandwidthBps)/1e6)

	// Scope a segment fault to variants ABOVE the bottom variant's bandwidth —
	// isolates the top variant. Matched by rc.BandwidthBps, resolved from the
	// rendition map, NOT by any URL token.
	rule := faultRuleV2("segment", status, freq, 1)
	rule["filter"].(map[string]any)["variant"] = map[string]any{
		"bandwidth_above": faultVar.BandwidthBps - 1,
	}
	if err := setFaultsV2(p, rule); err != nil {
		t.Fatalf("arm rendition-scoped fault: %v", err)
	}
	defer clearFaultsV2(p)
	time.Sleep(settleKernel)

	inHist := p.pullStatuses(t, func() []string { return p.pullVariant(faultVar.URL) }, samples)
	outHist := p.pullStatuses(t, func() []string { return p.pullVariant(otherVar.URL) }, samples)
	inFaults := inHist[wantStatus]
	outFaults := outHist[wantStatus]
	t.Logf("in-scope(top): hist=%v faults(%d)=%d ; out-of-scope(bottom): hist=%v faults=%d",
		inHist, wantStatus, inFaults, outHist, outFaults)

	if inFaults == 0 {
		t.Errorf("bandwidth-scoped fault produced ZERO faults on the in-scope (top) variant over %d samples — Tier 2 map not resolving segment bandwidth", samples)
	}
	if outFaults != 0 {
		t.Errorf("scope leaked: %d faults on the out-of-scope (bottom) variant, want 0", outFaults)
	}
}
