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
		"-is.flag.reset_advanced", "true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults:\n got %q\nwant %q", got, want)
	}
}

func TestProbeLaunchArgs_AllKnobs(t *testing.T) {
	got := ProbeLaunchArgs(ProbeConfig{
		PlayerID:           "pid",
		Content:            "insane_new_p200_h264",
		Segment:            "s2",
		LiveOffsetS:        "24",
		Protocol:           "dash",
		Codec:              "hevc",
		PeakBitrateMbps:    3,
		StartsFirstVariant: "true",
		Muted:              "false",
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
		"-is.codec", "hevc",
		"-is.flag.peak_bitrate_mbps", "3",
		"-is.flag.starts_first_variant", "true",
		"-is.flag.muted", "false",
		"-is.flag.reset_advanced", "true",
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

func TestProbeLaunchArgs_OmitsMutedWhenEmpty(t *testing.T) {
	// Empty Muted leaves the app's default-mute in force (#838) — no flag emitted.
	got := ProbeLaunchArgs(ProbeConfig{PlayerID: "x", Muted: ""})
	for _, a := range got {
		if a == "-is.flag.muted" {
			t.Fatal("muted must be omitted when empty (app default-mutes)")
		}
	}
}

func TestProbeLaunchArgs_EmitsMutedFalse(t *testing.T) {
	// `false` is meaningful (force audible) and must reach the launch args.
	got := ProbeLaunchArgs(ProbeConfig{PlayerID: "x", Muted: "false"})
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "-is.flag.muted" {
			if got[i+1] != "false" {
				t.Fatalf("muted = %q, want false", got[i+1])
			}
			return
		}
	}
	t.Fatal("muted=false must be emitted")
}

// TestProbeLaunchArgs_ServerURL: the per-launch server override (#942) is emitted
// as -is.server_url right after the pinned launch-state flags when set, and
// omitted entirely when empty (so non-fleet callers keep the app's saved server).
func TestProbeLaunchArgs_ServerURL(t *testing.T) {
	got := ProbeLaunchArgs(ProbeConfig{PlayerID: "pid", ServerURL: "https://dev.jeoliver.com:21000"})
	// find the flag/value pair
	var val string
	found := false
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "-is.server_url" {
			val, found = got[i+1], true
			break
		}
	}
	if !found {
		t.Fatalf("-is.server_url not emitted when ServerURL set: %q", got)
	}
	if val != "https://dev.jeoliver.com:21000" {
		t.Errorf("-is.server_url = %q, want the configured URL", val)
	}

	// Empty ServerURL must not emit the flag.
	none := ProbeLaunchArgs(ProbeConfig{PlayerID: "pid"})
	for _, a := range none {
		if a == "-is.server_url" {
			t.Fatalf("-is.server_url emitted with empty ServerURL: %q", none)
		}
	}
}
