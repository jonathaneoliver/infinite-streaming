package playlist

import (
	"strings"
	"testing"
)

const sampleMasterPlaylist = `#EXTM3U
#EXT-X-VERSION:6
#EXT-X-STREAM-INF:BANDWIDTH=1280000,AVERAGE-BANDWIDTH=1000000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2",FRAME-RATE=30.000
360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2560000,AVERAGE-BANDWIDTH=2000000,RESOLUTION=960x540,CODECS="avc1.4d401f,mp4a.40.2",FRAME-RATE=30.000
540p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5120000,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2",FRAME-RATE=30.000
720p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=10240000,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",FRAME-RATE=60.000
1080p.m3u8
`

const minimalMasterPlaylist = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1000000
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000
high.m3u8
`

func TestParseHLSMasterFromReader(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse master playlist: %v", err)
	}

	if len(ladder.Variants) != 4 {
		t.Errorf("Expected 4 variants, got %d", len(ladder.Variants))
	}

	// Check that variants are sorted by bandwidth
	for i := 1; i < len(ladder.Variants); i++ {
		if ladder.Variants[i].Bandwidth < ladder.Variants[i-1].Bandwidth {
			t.Errorf("Variants not sorted by bandwidth at index %d", i)
		}
	}
}

func TestParseBandwidth(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// First variant (after sorting) should be 360p with 1.28 Mbps
	v := ladder.Variants[0]
	if v.Bandwidth != 1280000 {
		t.Errorf("Expected bandwidth 1280000, got %d", v.Bandwidth)
	}
	if v.GetBandwidthMbps() < 1.27 || v.GetBandwidthMbps() > 1.29 {
		t.Errorf("Expected bandwidth ~1.28 Mbps, got %.2f", v.GetBandwidthMbps())
	}
}

func TestParseAverageBandwidth(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// 360p variant has AVERAGE-BANDWIDTH
	v := ladder.Variants[0]
	if v.AverageBandwidth != 1000000 {
		t.Errorf("Expected average bandwidth 1000000, got %d", v.AverageBandwidth)
	}

	// 1080p variant has no AVERAGE-BANDWIDTH
	v = ladder.Variants[3]
	if v.AverageBandwidth != 0 {
		t.Errorf("Expected no average bandwidth for 1080p, got %d", v.AverageBandwidth)
	}

	// GetEffectiveBandwidth should return AVERAGE-BANDWIDTH when available
	v = ladder.Variants[0]
	if v.GetEffectiveBandwidth() != 1000000 {
		t.Errorf("Expected effective bandwidth 1000000, got %d", v.GetEffectiveBandwidth())
	}

	// GetEffectiveBandwidth should fall back to BANDWIDTH when AVERAGE-BANDWIDTH not set
	v = ladder.Variants[3]
	if v.GetEffectiveBandwidth() != v.Bandwidth {
		t.Errorf("Expected effective bandwidth to equal bandwidth, got %d vs %d", v.GetEffectiveBandwidth(), v.Bandwidth)
	}
}

func TestParseResolution(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Check resolution of 360p variant
	v := ladder.Variants[0]
	if v.Resolution != "640x360" {
		t.Errorf("Expected resolution 640x360, got %s", v.Resolution)
	}
}

func TestParseCodecs(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	v := ladder.Variants[0]
	expected := "avc1.4d401e,mp4a.40.2"
	if v.Codecs != expected {
		t.Errorf("Expected codecs %s, got %s", expected, v.Codecs)
	}
}

func TestParseFrameRate(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// 360p has 30fps
	v := ladder.Variants[0]
	if v.FrameRate < 29.9 || v.FrameRate > 30.1 {
		t.Errorf("Expected frame rate ~30, got %.2f", v.FrameRate)
	}

	// 1080p has 60fps
	v = ladder.Variants[3]
	if v.FrameRate < 59.9 || v.FrameRate > 60.1 {
		t.Errorf("Expected frame rate ~60, got %.2f", v.FrameRate)
	}
}

func TestMinimalPlaylist(t *testing.T) {
	reader := strings.NewReader(minimalMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse minimal playlist: %v", err)
	}

	if len(ladder.Variants) != 2 {
		t.Errorf("Expected 2 variants, got %d", len(ladder.Variants))
	}

	// First variant should be low (1 Mbps)
	if ladder.Variants[0].Bandwidth != 1000000 {
		t.Errorf("Expected first variant bandwidth 1000000, got %d", ladder.Variants[0].Bandwidth)
	}

	// Second variant should be high (5 Mbps)
	if ladder.Variants[1].Bandwidth != 5000000 {
		t.Errorf("Expected second variant bandwidth 5000000, got %d", ladder.Variants[1].Bandwidth)
	}
}

func TestFindVariantByBandwidth(t *testing.T) {
	reader := strings.NewReader(sampleMasterPlaylist)
	ladder, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err != nil {
		t.Fatalf("Failed to parse: %v", err)
	}

	// Find variant closest to 1.5 Mbps (should be 360p with avg 1.0 Mbps)
	v := ladder.FindVariantByBandwidth(1.5)
	if v == nil {
		t.Fatal("Expected to find a variant")
	}
	if v.GetEffectiveBandwidth() != 1000000 {
		t.Errorf("Expected to find 1.0 Mbps variant, got %.2f Mbps", v.GetAverageBandwidthMbps())
	}

	// Find variant closest to 10 Mbps (should be 1080p)
	v = ladder.FindVariantByBandwidth(10.0)
	if v == nil {
		t.Fatal("Expected to find a variant")
	}
	if v.Bandwidth != 10240000 {
		t.Errorf("Expected to find 10.24 Mbps variant, got %.2f Mbps", v.GetBandwidthMbps())
	}
}

func TestEmptyPlaylist(t *testing.T) {
	reader := strings.NewReader("#EXTM3U\n")
	_, err := ParseHLSMasterFromReader(reader, "https://example.com/master.m3u8")
	if err == nil {
		t.Error("Expected error for empty playlist")
	}
}
