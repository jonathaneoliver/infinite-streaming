package util

import (
	"os"
	"path/filepath"
	"testing"
)

// insane_new's real master.m3u8 head — note AVERAGE-BANDWIDTH precedes RESOLUTION
// and contains the substring "BANDWIDTH", which the BANDWIDTH match must NOT capture.
const sampleMaster = `#EXTM3U
#EXT-X-VERSION:7

#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="Audio",URI="audio/playlist.m3u8"

# Video variants
#EXT-X-STREAM-INF:BANDWIDTH=1063012,AVERAGE-BANDWIDTH=796599,RESOLUTION=640x360,CODECS="avc1.64001e,mp4a.40.2",AUDIO="audio"
360p/playlist.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=7182067,AVERAGE-BANDWIDTH=5075214,RESOLUTION=1920x1080,CODECS="avc1.640028,mp4a.40.2",AUDIO="audio"
1080p/playlist.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=30333359,AVERAGE-BANDWIDTH=21414076,RESOLUTION=3840x2160,CODECS="avc1.640033,mp4a.40.2",AUDIO="audio"
2160p/playlist.m3u8
`

func TestParseMasterVariants(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(sampleMaster), 0o644); err != nil {
		t.Fatal(err)
	}
	got := parseMasterVariants(dir)
	if len(got) != 3 {
		t.Fatalf("got %d variants, want 3: %+v", len(got), got)
	}
	// Ascending by bandwidth; BANDWIDTH (peak) must be the standalone value, NOT
	// AVERAGE-BANDWIDTH's digits.
	want := []Variant{
		{Resolution: "640x360", Height: 360, Bandwidth: 1063012, AverageBandwidth: 796599},
		{Resolution: "1920x1080", Height: 1080, Bandwidth: 7182067, AverageBandwidth: 5075214},
		{Resolution: "3840x2160", Height: 2160, Bandwidth: 30333359, AverageBandwidth: 21414076},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("variant[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestParseMasterVariantsAbsent(t *testing.T) {
	if got := parseMasterVariants(t.TempDir()); got != nil {
		t.Errorf("no master.m3u8 should yield nil, got %+v", got)
	}
}
