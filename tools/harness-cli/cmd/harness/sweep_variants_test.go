package main

import "testing"

const sampleMaster = `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=800000,AVERAGE-BANDWIDTH=600000,RESOLUTION=640x360,CODECS="avc1"
playlist_6s_360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2500000,AVERAGE-BANDWIDTH=1800000,RESOLUTION=1920x1080,CODECS="avc1"
playlist_6s_1080p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=16000000,AVERAGE-BANDWIDTH=12000000,RESOLUTION=3840x2160,CODECS="avc1"
playlist_6s_2160p.m3u8
`

func TestParseMasterRungs(t *testing.T) {
	rungs := parseMasterRungs(sampleMaster)
	if len(rungs) != 3 {
		t.Fatalf("want 3 rungs, got %d", len(rungs))
	}
	// The bug: matching the whole line made `(?:^|,)BANDWIDTH=` miss the first
	// attribute (preceded by ':') and pick up nothing → bandwidth 0. Assert the
	// REAL BANDWIDTH (not AVERAGE-BANDWIDTH) is parsed for each rung.
	want := map[int]int{360: 800000, 1080: 2500000, 2160: 16000000}
	for _, r := range rungs {
		if want[r.height] != r.bandwidth {
			t.Errorf("height %d: bandwidth=%d want %d", r.height, r.bandwidth, want[r.height])
		}
	}
}

func TestLadderKeepSetDropTopRung(t *testing.T) {
	rungs := parseMasterRungs(sampleMaster)
	got, err := ladderKeepSet("drop-top-rung", rungs)
	if err != nil {
		t.Fatal(err)
	}
	// Drops the TOP (2160, highest bandwidth), keeps the rest as heights,
	// descending.
	want := []string{"1080", "360"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("drop-top-rung = %v, want %v", got, want)
	}
}

func TestLadderKeepSetVariants(t *testing.T) {
	rungs := parseMasterRungs(sampleMaster)
	// drop-top-2 → only the lowest survives.
	if got, _ := ladderKeepSet("drop-top-2", rungs); len(got) != 1 || got[0] != "360" {
		t.Fatalf("drop-top-2 = %v, want [360]", got)
	}
	// keep-bottom-1 → only the lowest.
	if got, _ := ladderKeepSet("keep-bottom-1", rungs); len(got) != 1 || got[0] != "360" {
		t.Fatalf("keep-bottom-1 = %v, want [360]", got)
	}
	// alternating_variants → keep every 2nd rung (#820 keep-every-other); 3 rungs
	// (2160,1080,360 desc) → [2160, 360] (top + bottom kept, middle dropped).
	if got, _ := ladderKeepSet("alternating_variants", rungs); len(got) != 2 || got[0] != "2160" || got[1] != "360" {
		t.Fatalf("alternating_variants = %v, want [2160 360]", got)
	}
	// drop-all is an error, not an empty (which would break the player).
	if _, err := ladderKeepSet("drop-top-9", rungs); err == nil {
		t.Fatal("dropping all rungs should error")
	}
}
