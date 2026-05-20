package runner

import (
	"testing"
	"time"
)

func TestVariantRatesDescPrefersAverage(t *testing.T) {
	// Fixture matching the current test-dev master playlist: 5 variants,
	// all carrying AVERAGE-BANDWIDTH alongside BANDWIDTH.
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 724620, peak: 998009, url: "playlist_6s_360p.m3u8"},
		{res: "960x540", avg: 1307381, peak: 1840116, url: "playlist_6s_540p.m3u8"},
		{res: "1280x720", avg: 2467263, peak: 3463766, url: "playlist_6s_720p.m3u8"},
		{res: "1920x1080", avg: 4988561, peak: 7060425, url: "playlist_6s_1080p.m3u8"},
		{res: "3840x2160", avg: 10845181, peak: 15363854, url: "playlist_6s_2160p.m3u8"},
	})

	rates, err := VariantRatesDesc(rec, 5)
	if err != nil {
		t.Fatalf("VariantRatesDesc: %v", err)
	}
	if len(rates) != 5 {
		t.Fatalf("len=%d want 5", len(rates))
	}
	// Descending order check.
	for i := 1; i < len(rates); i++ {
		if rates[i-1].CapMbps <= rates[i].CapMbps {
			t.Errorf("rates not descending at %d: %.3f vs %.3f", i, rates[i-1].CapMbps, rates[i].CapMbps)
		}
	}
	// Top rung (2160p) should be 10.845 Mbps × 1.05 = 11.387 Mbps.
	if got := rates[0].CapMbps; got < 11.38 || got > 11.39 {
		t.Errorf("top cap %.3f Mbps, want ~11.387 (2160p avg × 1.05)", got)
	}
	// Bottom rung (360p) should be 0.725 Mbps × 1.05 = 0.761 Mbps.
	if got := rates[len(rates)-1].CapMbps; got < 0.76 || got > 0.762 {
		t.Errorf("bottom cap %.3f Mbps, want ~0.761 (360p avg × 1.05)", got)
	}
	// All five should report source=average since avg was populated.
	for _, r := range rates {
		if r.Source != "average" {
			t.Errorf("%s reported source=%q want average", r.Resolution, r.Source)
		}
	}
}

func TestVariantRatesDescFallsBackToPeak(t *testing.T) {
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 0, peak: 1000000, url: "p.m3u8"},
	})
	rates, err := VariantRatesDesc(rec, 0)
	if err != nil {
		t.Fatalf("VariantRatesDesc: %v", err)
	}
	if rates[0].Source != "peak" {
		t.Errorf("source=%q want peak (avg=0 forces fallback)", rates[0].Source)
	}
	if rates[0].CapMbps != 1.0 {
		t.Errorf("CapMbps=%.3f want 1.000 (0%% margin)", rates[0].CapMbps)
	}
}

func TestVariantRatesDescEmptyManifest(t *testing.T) {
	rec := mkPlayerWithVariants(t, nil)
	if _, err := VariantRatesDesc(rec, 5); err == nil {
		t.Error("expected error for empty variants")
	}
}

func TestVariantRatesDescNegativeMarginAccepted(t *testing.T) {
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 1000000, peak: 1500000, url: "p.m3u8"},
	})
	rates, err := VariantRatesDesc(rec, -5)
	if err != nil {
		t.Fatalf("VariantRatesDesc(-5): %v (negative margins must be allowed)", err)
	}
	if rates[0].CapMbps != 0.95 {
		t.Errorf("CapMbps=%.3f want 0.950 (1.0 Mbps × 0.95)", rates[0].CapMbps)
	}
}

func TestVariantSweepProducesStrictDescent(t *testing.T) {
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 725000, peak: 1000000, url: "p.m3u8"},
		{res: "960x540", avg: 1307000, peak: 1800000, url: "p.m3u8"},
		{res: "1920x1080", avg: 4989000, peak: 7000000, url: "p.m3u8"},
	})
	margins := []int{50, 25, 10, 5, 0, -5}
	sweep, err := VariantSweep(rec, margins)
	if err != nil {
		t.Fatalf("VariantSweep: %v", err)
	}
	// 3 variants × 6 margins = 18 candidates; with this widely-spaced
	// ladder all should survive the strict-descent prune.
	if len(sweep) != 18 {
		t.Errorf("len=%d want 18 (no candidates should be dropped)", len(sweep))
	}
	for i := 1; i < len(sweep); i++ {
		if sweep[i].CapMbps >= sweep[i-1].CapMbps {
			t.Errorf("strict descent violated at %d: %.3f → %.3f",
				i, sweep[i-1].CapMbps, sweep[i].CapMbps)
		}
	}
	// First cap = 1080p × 1.50 = 7.484 Mbps; last = 360p × 0.95 = 0.689 Mbps.
	if got := sweep[0].CapMbps; got < 7.48 || got > 7.49 {
		t.Errorf("first cap %.3f Mbps, want ~7.484", got)
	}
	if got := sweep[len(sweep)-1].CapMbps; got < 0.688 || got > 0.690 {
		t.Errorf("last cap %.3f Mbps, want ~0.689", got)
	}
}

func TestVariantSweepDropsAscendingOnTightLadder(t *testing.T) {
	// Adjacent variants very close: 540p avg 1.0 Mbps, 720p avg 1.1 Mbps.
	// 720p × 0.95 = 1.045; 540p × 1.50 = 1.500 — the lower variant's high
	// margin would *increase* the cap vs. the higher variant's low margin,
	// so the prune must drop it.
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "960x540", avg: 1000000, peak: 1200000, url: "p.m3u8"},
		{res: "1280x720", avg: 1100000, peak: 1300000, url: "p.m3u8"},
	})
	sweep, err := VariantSweep(rec, []int{50, 25, 10, 5, 0, -5})
	if err != nil {
		t.Fatalf("VariantSweep: %v", err)
	}
	// Some candidates must have been dropped; check strict descent.
	for i := 1; i < len(sweep); i++ {
		if sweep[i].CapMbps >= sweep[i-1].CapMbps {
			t.Errorf("strict descent violated at %d: %s/%+d%% %.3f → %s/%+d%% %.3f",
				i,
				sweep[i-1].Resolution, sweep[i-1].MarginPct, sweep[i-1].CapMbps,
				sweep[i].Resolution, sweep[i].MarginPct, sweep[i].CapMbps)
		}
	}
}

func TestStepsFromVariants(t *testing.T) {
	rates := []VariantRate{
		{Resolution: "2160p", CapMbps: 11.387},
		{Resolution: "360p", CapMbps: 0.761},
	}
	steps := StepsFromVariants(rates, 60*time.Second)
	if len(steps) != 2 {
		t.Fatalf("len=%d want 2", len(steps))
	}
	if steps[0].RateMbps != 11.387 || steps[0].Hold != 60*time.Second {
		t.Errorf("step 0 = %+v", steps[0])
	}
	if steps[1].RateMbps != 0.761 {
		t.Errorf("step 1 rate = %.3f", steps[1].RateMbps)
	}
}

// --- fixtures ---------------------------------------------------------------

type variantSeed struct {
	res, url  string
	avg, peak int
}

// mkPlayerWithVariants builds a minimum PlayerRecord carrying the supplied
// variant fixtures. Used by the variant-helper tests; keeps them
// independent of the live harness.
func mkPlayerWithVariants(t *testing.T, seeds []variantSeed) *PlayerRecord {
	t.Helper()
	rec := &PlayerRecord{
		ID: "00000000-0000-0000-0000-000000000001",
	}
	rec.CurrentPlay = &struct {
		ID       string `json:"id"`
		Manifest struct {
			MasterURL string `json:"master_url"`
			Variants  []struct {
				Bandwidth        int    `json:"bandwidth"`
				AverageBandwidth int    `json:"average_bandwidth"`
				Resolution       string `json:"resolution"`
				URL              string `json:"url"`
			} `json:"variants"`
		} `json:"manifest"`
	}{}
	rec.CurrentPlay.ID = "play-xyz"
	for _, s := range seeds {
		rec.CurrentPlay.Manifest.Variants = append(rec.CurrentPlay.Manifest.Variants, struct {
			Bandwidth        int    `json:"bandwidth"`
			AverageBandwidth int    `json:"average_bandwidth"`
			Resolution       string `json:"resolution"`
			URL              string `json:"url"`
		}{
			Bandwidth:        s.peak,
			AverageBandwidth: s.avg,
			Resolution:       s.res,
			URL:              s.url,
		})
	}
	return rec
}
