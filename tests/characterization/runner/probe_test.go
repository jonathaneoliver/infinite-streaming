package runner

import (
	"reflect"
	"testing"
)

func TestProbeLaunchArgs_Defaults(t *testing.T) {
	// The minimal config (just a player_id) must reproduce the launch-arg slice
	// TestSweepProbe built inline before the #811 extraction: the three pinned
	// launch-state flags plus an always-pinned live_offset_s of "0".
	got := ProbeLaunchArgs(ProbeConfig{PlayerID: "abc"})
	want := []string{
		"-is.player_id", "abc",
		"-is.flag.play_id_rotation_s", "0",
		"-is.flag.skip_home", "false",
		"-is.flag.dev_mode", "true",
		"-is.flag.live_offset_s", "0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults:\n got %q\nwant %q", got, want)
	}
}

func TestProbeLaunchArgs_AllKnobs(t *testing.T) {
	got := ProbeLaunchArgs(ProbeConfig{
		PlayerID:        "pid",
		Content:         "insane_new_p200_h264",
		Segment:         "s2",
		LiveOffsetS:     "24",
		Protocol:        "dash",
		PeakBitrateMbps: 3,
	})
	want := []string{
		"-is.player_id", "pid",
		"-is.flag.play_id_rotation_s", "0",
		"-is.flag.skip_home", "false",
		"-is.flag.dev_mode", "true",
		"-is.lastPlayed", "insane_new_p200_h264",
		"-is.segment", "s2",
		"-is.flag.live_offset_s", "24",
		"-is.protocol", "dash",
		"-is.flag.peak_bitrate_mbps", "3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("all knobs:\n got %q\nwant %q", got, want)
	}
}

func TestProbeLaunchArgs_EmptyLiveOffsetPinnedZero(t *testing.T) {
	// An empty LiveOffsetS must still emit "0" — a run never inherits the app's
	// persisted stepper value.
	got := ProbeLaunchArgs(ProbeConfig{PlayerID: "x", LiveOffsetS: ""})
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "-is.flag.live_offset_s" {
			if got[i+1] != "0" {
				t.Fatalf("live_offset_s = %q, want 0", got[i+1])
			}
			return
		}
	}
	t.Fatal("live_offset_s flag not present — it must always be pinned")
}

func TestProbeLaunchArgs_OmitsPeakWhenZero(t *testing.T) {
	got := ProbeLaunchArgs(ProbeConfig{PlayerID: "x", PeakBitrateMbps: 0})
	for _, a := range got {
		if a == "-is.flag.peak_bitrate_mbps" {
			t.Fatal("peak_bitrate_mbps must be omitted when 0")
		}
	}
}
