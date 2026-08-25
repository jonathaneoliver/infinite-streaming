package main

import "testing"

// full2sLadder mirrors the real insane_new master_2s.m3u8 (11 rungs).
func full2sLadder() []PlaylistInfo {
	return []PlaylistInfo{
		{URL: "playlist_2s_360p.m3u8", Bandwidth: 1063012, Resolution: "640x360"},
		{URL: "playlist_2s_432p.m3u8", Bandwidth: 1420320, Resolution: "768x432"},
		{URL: "playlist_2s_540p.m3u8", Bandwidth: 1893473, Resolution: "960x540"},
		{URL: "playlist_2s_648p.m3u8", Bandwidth: 2571743, Resolution: "1152x648"},
		{URL: "playlist_2s_720p.m3u8", Bandwidth: 3552350, Resolution: "1280x720"},
		{URL: "playlist_2s_900p.m3u8", Bandwidth: 5034445, Resolution: "1600x900"},
		{URL: "playlist_2s_1080p.m3u8", Bandwidth: 7182067, Resolution: "1920x1080"},
		{URL: "playlist_2s_1296p.m3u8", Bandwidth: 10530479, Resolution: "2304x1296"},
		{URL: "playlist_2s_1440p.m3u8", Bandwidth: 15484068, Resolution: "2560x1440"},
		{URL: "playlist_2s_1800p.m3u8", Bandwidth: 21525717, Resolution: "3200x1800"},
		{URL: "playlist_2s_2160p.m3u8", Bandwidth: 30333359, Resolution: "3840x2160"},
	}
}

func resOf(infos []PlaylistInfo) []string {
	out := make([]string, len(infos))
	for i, p := range infos {
		out[i] = p.Resolution
	}
	return out
}

func TestFilterPlaylistInfoByAllowed_everyOther(t *testing.T) {
	// The #820 case: every-other keep-set (resolution-keyed) must thin 11 → 6.
	allowed := []string{"640x360", "960x540", "1280x720", "1920x1080", "2560x1440", "3840x2160"}
	got := filterPlaylistInfoByAllowed(full2sLadder(), allowed)
	if len(got) != 6 {
		t.Fatalf("got %d variants, want 6: %v", len(got), resOf(got))
	}
	want := []string{"640x360", "960x540", "1280x720", "1920x1080", "2560x1440", "3840x2160"}
	for i, w := range want {
		if got[i].Resolution != w {
			t.Errorf("variant[%d] = %s, want %s", i, got[i].Resolution, w)
		}
	}
}

func TestFilterPlaylistInfoByAllowed_emptyAllowReturnsAll(t *testing.T) {
	got := filterPlaylistInfoByAllowed(full2sLadder(), nil)
	if len(got) != 11 {
		t.Fatalf("no allow-set should pass all 11 through, got %d", len(got))
	}
}

func TestFilterPlaylistInfoByAllowed_bareHeightAndURI(t *testing.T) {
	// Bare-height keep-set ("360"/"720") and exact-URI back-compat both match.
	got := filterPlaylistInfoByAllowed(full2sLadder(), []string{"360", "720p", "playlist_2s_2160p.m3u8"})
	if r := resOf(got); len(got) != 3 || r[0] != "640x360" || r[1] != "1280x720" || r[2] != "3840x2160" {
		t.Fatalf("bare-height/URI match: got %v, want [640x360 1280x720 3840x2160]", resOf(got))
	}
}

func TestVariantSelectorAllowed_unknownResolutionExcluded(t *testing.T) {
	m := map[string]bool{"640x360": true}
	if variantSelectorAllowed("", "unknown", m) {
		t.Error("unknown resolution with no URI match must be excluded")
	}
	if variantSelectorAllowed("", "", m) {
		t.Error("empty resolution with no URI match must be excluded")
	}
	if !variantSelectorAllowed("", "640x360", m) {
		t.Error("matching full resolution must be allowed")
	}
}
