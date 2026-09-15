package app

import (
	"reflect"
	"testing"
)

// Lines exactly as create_abr_ladder.sh prints them, colour codes included.
const (
	tick = "\x1b[0;32m✓\x1b[0m "
	ts   = "\x1b[0;32m[11:37:58]\x1b[0m "
)

func TestResultTrackerRecordsEncodersActuallyUsed(t *testing.T) {
	r := NewResultTracker()
	if got := r.Parse("j", tick+"HEVC encoder: libx265 (software)"); got == nil {
		t.Fatal("HEVC encoder line: want a result, got nil")
	}
	got := r.Parse("j", tick+"H.264 encoder: VideoToolbox (hardware) - 5x faster")
	want := map[string]interface{}{"encoders": map[string]interface{}{
		"hevc": "libx265 (software)",
		"h264": "VideoToolbox (hardware) - 5x faster",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
	got = r.Parse("j", tick+"AV1 encoder: libsvtav1 (software)")
	if enc := got["encoders"].(map[string]interface{}); enc["av1"] != "libsvtav1 (software)" || len(enc) != 3 {
		t.Fatalf("after AV1 line encoders = %#v", enc)
	}
}

func TestResultTrackerPaddingModes(t *testing.T) {
	cases := []struct {
		line string
		want map[string]interface{}
	}{
		// The script prints this whenever padding is off, including the default
		// (no flag); padding is opt-in.
		{ts + "Padding disabled (padding is opt-in: pass --padding or --padding-pink)", map[string]interface{}{"padding": "none"}},
		{ts + "Padding disabled via --no-padding flag", map[string]interface{}{"padding": "none"}}, // older scripts
		{ts + "Video padding: 1.234s (118.766s → 120.000s)", map[string]interface{}{"padding": "applied", "padding_video_s": 1.234}},
		{ts + "Video padding: 0.000s (120.000s → 120.000s)", map[string]interface{}{"padding": "none"}},
	}
	for _, c := range cases {
		r := NewResultTracker()
		if got := r.Parse("j", c.line); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q) = %#v, want %#v", c.line, got, c.want)
		}
	}
}

func TestResultTrackerIgnoresUnrelatedAndRepeatedLines(t *testing.T) {
	r := NewResultTracker()
	for _, l := range []string{
		ts + "Encoding: HEVC 540p | 30.00fps @ 1.60Mbps (1600kbps target, preset medium) - libx265/libx264 (software)",
		"out_time=00:00:12.000000",
		ts + "Padding applied successfully",
	} {
		if got := r.Parse("j", l); got != nil {
			t.Errorf("Parse(%q) = %#v, want nil (not a result line)", l, got)
		}
	}
	if r.Parse("j", tick+"HEVC encoder: libx265 (software)") == nil {
		t.Fatal("first encoder line should produce a result")
	}
	if got := r.Parse("j", tick+"HEVC encoder: libx265 (software)"); got != nil {
		t.Fatalf("repeated identical line = %#v, want nil (nothing new to persist)", got)
	}
}

func TestResultTrackerKeepsJobsSeparate(t *testing.T) {
	r := NewResultTracker()
	r.Parse("a", tick+"HEVC encoder: libx265 (software)")
	got := r.Parse("b", tick+"H.264 encoder: libx264 (software)")
	if enc := got["encoders"].(map[string]interface{}); len(enc) != 1 || enc["h264"] == nil {
		t.Fatalf("job b encoders = %#v, want only h264", enc)
	}
	r.Forget("a")
	if got := r.Parse("a", tick+"HEVC encoder: libx265 (software)"); got == nil {
		t.Fatal("after Forget, job a should start fresh and report its encoder again")
	}
}
