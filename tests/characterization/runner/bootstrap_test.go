package runner

import (
	"context"
	"testing"
	"time"
)

// TestMasterLadderSmoke parses the live master playlist off the configured
// base URL (HARNESS_BASE_URL, default test-dev) and confirms the ladder builds
// — proving config-on-connect can compute the cold-start floor without a prior
// play. Skips when the server is unreachable so it's CI-safe.
func TestMasterLadderSmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rates, err := MasterLadder(ctx, "")
	if err != nil {
		t.Skipf("MasterLadder unavailable (server down?): %v", err)
	}
	if len(rates) == 0 {
		t.Fatal("MasterLadder returned no rungs")
	}
	// Bottom rung = lowest cap; second-from-bottom variant's lowest surviving
	// cap is the rampup floor. Log the bottom few so the operator can eyeball it.
	n := len(rates)
	for i := n - 1; i >= 0 && i >= n-4; i-- {
		r := rates[i]
		t.Logf("rung[%d] %-9s cap=%.3f Mbps avg=%.3f peak=%.3f src=%s",
			i, r.Resolution, r.CapMbps, float64(r.AvgBps)/1e6, float64(r.PeakBps)/1e6, r.Source)
	}
	t.Logf("parsed %d rungs from master (no prior play needed)", n)
}
