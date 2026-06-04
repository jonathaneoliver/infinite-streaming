package runner

import (
	"testing"
	"time"
)

func TestStandardLadderRatesDualRungFilled(t *testing.T) {
	// Fixture matching the current test-dev master playlist: 5 variants,
	// all carrying AVERAGE-BANDWIDTH alongside BANDWIDTH.
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 724620, peak: 998009, url: "playlist_6s_360p.m3u8"},
		{res: "960x540", avg: 1307381, peak: 1840116, url: "playlist_6s_540p.m3u8"},
		{res: "1280x720", avg: 2467263, peak: 3463766, url: "playlist_6s_720p.m3u8"},
		{res: "1920x1080", avg: 4988561, peak: 7060425, url: "playlist_6s_1080p.m3u8"},
		{res: "3840x2160", avg: 10845181, peak: 15363854, url: "playlist_6s_2160p.m3u8"},
	})

	rates, err := StandardLadderRates(rec)
	if err != nil {
		t.Fatalf("StandardLadderRates: %v", err)
	}
	// 10 anchors (5 variants × {peak,avg}) + geometric fills.
	if len(rates) <= 10 {
		t.Fatalf("len=%d, want >10 (10 anchors + fills)", len(rates))
	}
	// Descending, within the target step ratio (slack for 3-dp rounding).
	maxStep := LadderMaxStep()
	for i := 1; i < len(rates); i++ {
		if rates[i-1].CapMbps <= rates[i].CapMbps {
			t.Errorf("not descending at %d: %.3f vs %.3f", i, rates[i-1].CapMbps, rates[i].CapMbps)
		}
		if r := rates[i-1].CapMbps / rates[i].CapMbps; r > maxStep+0.005 {
			t.Errorf("step %d ratio %.3fx exceeds %.2fx", i, r, maxStep)
		}
	}
	// Top rung is the top variant's PEAK anchor ×1.05.
	if rates[0].Source != "peak" || rates[0].Resolution != "3840x2160" {
		t.Errorf("top rung = %s/%s, want 3840x2160/peak", rates[0].Resolution, rates[0].Source)
	}
	if got := rates[0].CapMbps; got < 16.12 || got > 16.14 {
		t.Errorf("top cap %.3f Mbps, want ~16.132 (top peak × 1.05)", got)
	}
	// Bottom rung is attributed to the bottom variant; cap = bottom avg ×1.05.
	last := rates[len(rates)-1]
	if last.Resolution != "640x360" {
		t.Errorf("bottom rung attributed to %s, want 640x360", last.Resolution)
	}
	if got := last.CapMbps; got < 0.755 || got > 0.766 {
		t.Errorf("bottom cap %.3f Mbps, want ~0.761 (bottom avg × 1.05)", got)
	}
	// Every rung is attributed to a real variant + carries its peak.
	for _, r := range rates {
		if r.Resolution == "" || r.PeakBps <= 0 {
			t.Errorf("rung cap=%.3f not attributed to a variant (res=%q peak=%d)", r.CapMbps, r.Resolution, r.PeakBps)
		}
		if r.Source != "peak" && r.Source != "avg" && r.Source != "fill" {
			t.Errorf("rung cap=%.3f bad source %q", r.CapMbps, r.Source)
		}
	}
}

func TestStandardLadderRatesPeakOnly(t *testing.T) {
	// AVERAGE-BANDWIDTH absent ⇒ a single (peak) anchor per variant.
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 0, peak: 1000000, url: "p.m3u8"},
		{res: "1920x1080", avg: 0, peak: 6000000, url: "q.m3u8"},
	})
	rates, err := StandardLadderRates(rec)
	if err != nil {
		t.Fatalf("StandardLadderRates: %v", err)
	}
	if rates[0].Source != "peak" {
		t.Errorf("top source=%q want peak", rates[0].Source)
	}
	// Top cap = 6.0 Mbps peak × 1.05 = 6.300.
	if got := rates[0].CapMbps; got < 6.29 || got > 6.31 {
		t.Errorf("top cap=%.3f want ~6.300 (peak 6.0 × 1.05)", got)
	}
}

func TestStandardLadderRatesEnvBump(t *testing.T) {
	t.Setenv("CHAR_LADDER_BUMP_PCT", "0")
	rec := mkPlayerWithVariants(t, []variantSeed{
		{res: "640x360", avg: 0, peak: 1000000, url: "p.m3u8"},
	})
	rates, err := StandardLadderRates(rec)
	if err != nil {
		t.Fatalf("StandardLadderRates: %v", err)
	}
	// bump 0 ⇒ cap = peak exactly (1.000 Mbps).
	if got := rates[0].CapMbps; got < 0.999 || got > 1.001 {
		t.Errorf("cap=%.3f want 1.000 (peak × bump 0%%)", got)
	}
}

func TestStandardLadderRatesEmptyManifest(t *testing.T) {
	rec := mkPlayerWithVariants(t, nil)
	if _, err := StandardLadderRates(rec); err == nil {
		t.Error("expected error for empty variants")
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
		ID        string `json:"id"`
		AttemptID int    `json:"attempt_id"`
		Manifest  struct {
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
