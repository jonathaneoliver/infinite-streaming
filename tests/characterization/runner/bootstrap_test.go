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

// TestConfigOnConnectCapacity allocates + releases more sessions than the
// proxy's pool holds (4 slots) to prove Session.Release frees the slot
// immediately. If Release didn't work, the 5th ConfigureOnConnect would 503
// "session limit reached" within the 5-min reap window (#714). Skips when the
// server is unreachable.
func TestConfigOnConnectCapacity(t *testing.T) {
	const iters = 6 // > the 4-slot pool
	for i := 0; i < iters; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		pid := NewPlayerID()
		err := ConfigureOnConnect(ctx, pid, "", ShapeRateConfig(2.0))
		if err != nil && i == 0 {
			cancel()
			t.Skipf("ConfigureOnConnect unavailable (server down?): %v", err)
		}
		if err != nil {
			cancel()
			t.Fatalf("iter %d/%d: ConfigureOnConnect failed — pool exhausted? Release not freeing slots: %v", i+1, iters, err)
		}
		if rerr := (&Session{PlayerID: pid}).Release(ctx); rerr != nil {
			t.Errorf("iter %d/%d: release %s failed (slot will leak): %v", i+1, iters, pid, rerr)
		} else {
			t.Logf("iter %d/%d: allocated + released %s", i+1, iters, pid)
		}
		cancel()
	}
	t.Logf("%d allocate+release cycles against the 4-slot pool — no exhaustion", iters)
}
