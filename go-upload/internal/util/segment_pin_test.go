package util

import "testing"

// The catalogue's view of a pin must agree with go-live's, or /api/content
// advertises a segment length the manifest handler will 404. Kept as its own
// table rather than shared code because the two live in separate Go modules —
// see the note on segmentPinPattern in content.go.
func TestSegmentPin(t *testing.T) {
	cases := []struct {
		name string
		want int
	}{
		{"fpv_p200_h264_1s", 1},
		{"fpv_p200_h264_2s", 2},
		{"fpv_p200_h264_6s", 6},
		{"insane_fpv_shots_hydrofoil_windsurfing_p200_h264_6s", 6},

		{"fpv_p200_h264_xs", 0},
		{"insane_fpv_shots_hydrofoil_windsurfing_p200_h264_xs", 0},
		{"bucks_bunny_p200_h264", 0},
		{"redbull_p200_av1", 0},

		{"insane_fpv_shots_hydrofoil_windsurfing_p200_h264__6s", 6},

		{"clip_p200_h264_3s", 0},
		{"clip_p200_h264_10s", 0},
		{"clip_p200_h264_0s", 0},

		{"fpv_6s_p200_h264", 0},
		{"my_2s_clip_p200_h264", 0},

		{"clip_p200_h264_6S", 6},
		{"", 0},
	}
	for _, c := range cases {
		if got := SegmentPin(c.name); got != c.want {
			t.Errorf("SegmentPin(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestAvailableSegmentDurationsPinned(t *testing.T) {
	// A pinned clip reports exactly its own length, regardless of what the
	// on-disk probe would otherwise allow.
	for _, c := range []struct {
		name string
		want int
	}{
		{"fpv_p200_h264_1s", 1},
		{"fpv_p200_h264_2s", 2},
		{"fpv_p200_h264_6s", 6},
	} {
		got := availableSegmentDurations(c.name, t.TempDir(), true, nil)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("availableSegmentDurations(%q) = %v, want [%d]", c.name, got, c.want)
		}
	}
}

func TestAvailableSegmentDurationsUnpinnedKeepsRepackaging(t *testing.T) {
	// _xs and unsuffixed content keep today's behaviour. No partials on disk
	// here, so 1s is absent but 2s and 6s are still composed.
	for _, name := range []string{"fpv_p200_h264_xs", "bucks_bunny_p200_h264"} {
		got := availableSegmentDurations(name, t.TempDir(), true, nil)
		if len(got) != 2 || got[0] != 2 || got[1] != 6 {
			t.Errorf("availableSegmentDurations(%q) = %v, want [2 6]", name, got)
		}
	}
}

// A native duration of 0 means detection failed. It used to be advertised
// verbatim, producing segment_durations [0 1 2 6] on a real clip.
func TestAvailableSegmentDurationsDropsZeroNative(t *testing.T) {
	zero := 0
	got := availableSegmentDurations("bucks_bunny_p200_h264", t.TempDir(), true, &zero)
	for _, d := range got {
		if d == 0 {
			t.Fatalf("advertised a 0s segment length: %v", got)
		}
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 6 {
		t.Errorf("got %v, want [2 6]", got)
	}
}

func TestAvailableSegmentDurationsKeepsRealNative(t *testing.T) {
	four := 4
	got := availableSegmentDurations("clip_p200_h264", t.TempDir(), false, &four)
	if len(got) != 1 || got[0] != 4 {
		t.Errorf("DASH-only clip: got %v, want [4]", got)
	}
}
