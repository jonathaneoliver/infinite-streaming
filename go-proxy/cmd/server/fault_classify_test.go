package main

import "testing"

// ladderSession builds a session whose cached master playlist mirrors the
// insane_newer_p200_h264 ladder from the #918 investigation: a few video
// renditions keyed by resolution directory, authored low→high. Bandwidths are
// deliberately out of authored order for one entry so rung-index tests prove
// the sort, not the authored order.
func ladderSession() SessionData {
	return SessionData{
		"manifest_variants": []PlaylistInfo{
			{URL: "360p/playlist.m3u8", Bandwidth: 800_000, Resolution: "640x360"},
			{URL: "432p/playlist.m3u8", Bandwidth: 1_400_000, Resolution: "768x432"},
			{URL: "540p/playlist.m3u8", Bandwidth: 2_200_000, Resolution: "960x540"},
			{URL: "720p/playlist.m3u8", Bandwidth: 4_000_000, Resolution: "1280x720"},
			{URL: "1080p/playlist.m3u8", Bandwidth: 8_000_000, Resolution: "1920x1080"},
		},
	}
}

func TestClassifyRequest_Kind(t *testing.T) {
	s := ladderSession()
	cases := []struct {
		name                        string
		path                        string
		isSeg, isManifest, isMaster bool
		wantKind                    string
		wantInit, wantAudio         bool
	}{
		{"master", "/go-live/c/master.m3u8", false, false, true, "master_manifest", false, false},
		{"video media manifest (under dir)", "/go-live/c/720p/playlist.m3u8", false, true, false, "manifest", false, false},
		{"audio media manifest (under dir)", "/go-live/c/audio/playlist.m3u8", false, true, false, "audio_manifest", false, true},
		// Real go-live naming: media playlists sit in the content root named
		// playlist_<dur>_<variant>.m3u8. The audio one is NOT under /audio/.
		{"video media manifest (root, named)", "/go-live/c/playlist_6s_2160p.m3u8", false, true, false, "manifest", false, false},
		{"audio media manifest (root, named)", "/go-live/c/playlist_6s_audio.m3u8", false, true, false, "audio_manifest", false, true},
		{"audio media manifest (1s, named)", "/go-live/c/playlist_1s_audio.m3u8", false, true, false, "audio_manifest", false, true},
		{"video segment", "/go-live/c/720p/segment_00017.m4s", true, false, false, "segment", false, false},
		{"audio segment", "/go-live/c/audio/segment_00017.m4s", true, false, false, "audio_segment", false, true},
		{"video init", "/go-live/c/720p/init.mp4", true, false, false, "init", true, false},
		{"audio init", "/go-live/c/audio/init.mp4", true, false, false, "init", true, true},
		{"partial", "/go-live/c/720p/segment_00017.part.3.m4s", true, false, false, "partial", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := classifyRequest(s, tc.path, tc.isSeg, tc.isManifest, tc.isMaster)
			if rc.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", rc.Kind, tc.wantKind)
			}
			if rc.IsInit != tc.wantInit {
				t.Errorf("IsInit = %v, want %v", rc.IsInit, tc.wantInit)
			}
			if rc.IsAudio != tc.wantAudio {
				t.Errorf("IsAudio = %v, want %v", rc.IsAudio, tc.wantAudio)
			}
		})
	}
}

// TestClassifyRequest_AudioByManifestStructure proves the #919 Tier 1 win: when
// the master declares audio rendition URIs (manifest_audio_uris), the classifier
// identifies the audio playlist by that DECLARATION — even when the filename
// carries no "audio" token (here "sound/track.m3u8") — and does NOT mis-tag a
// video playlist. This is the structure-driven replacement for name guessing.
func TestClassifyRequest_AudioByManifestStructure(t *testing.T) {
	s := ladderSession()
	s["manifest_audio_uris"] = []string{"sound/track.m3u8"}

	rc := classifyRequest(s, "/go-live/c/sound/track.m3u8", false, true, false)
	if rc.Kind != "audio_manifest" || !rc.IsAudio {
		t.Errorf("declared audio URI: Kind=%q IsAudio=%v, want audio_manifest/true", rc.Kind, rc.IsAudio)
	}
	// A video media playlist not in the audio set stays a plain manifest.
	rcv := classifyRequest(s, "/go-live/c/playlist_6s_2160p.m3u8", false, true, false)
	if rcv.Kind != "manifest" || rcv.IsAudio {
		t.Errorf("video playlist: Kind=%q IsAudio=%v, want manifest/false", rcv.Kind, rcv.IsAudio)
	}
	// Even a filename that WOULD trip the name heuristic must defer to structure:
	// with audio URIs known, a "..._audio.m3u8" not in the set is NOT audio.
	rcx := classifyRequest(s, "/go-live/c/playlist_6s_audio.m3u8", false, true, false)
	if rcx.IsAudio {
		t.Errorf("unlisted _audio playlist: IsAudio=true, want false (structure overrides name)")
	}
}

func TestClassifyRequest_VideoVariantRung(t *testing.T) {
	s := ladderSession()
	cases := []struct {
		path        string
		wantRung    int
		wantResolun string
	}{
		{"/go-live/c/360p/segment_1.m4s", 0, "640x360"},
		{"/go-live/c/432p/segment_1.m4s", 1, "768x432"},
		{"/go-live/c/540p/segment_1.m4s", 2, "960x540"},
		{"/go-live/c/720p/segment_1.m4s", 3, "1280x720"},
		{"/go-live/c/1080p/segment_1.m4s", 4, "1920x1080"},
	}
	for _, tc := range cases {
		rc := classifyRequest(s, tc.path, true, false, false)
		if rc.RungIndex != tc.wantRung {
			t.Errorf("%s: RungIndex = %d, want %d", tc.path, rc.RungIndex, tc.wantRung)
		}
		if rc.Resolution != tc.wantResolun {
			t.Errorf("%s: Resolution = %q, want %q", tc.path, rc.Resolution, tc.wantResolun)
		}
	}
}

// Audio and init requests carry no video-variant identity — RungIndex stays -1
// even though the path sits under a rendition-shaped directory. This is what
// keeps a variant-scoped (rung/resolution) fault rule from matching audio/init,
// the #917 fix.
func TestClassifyRequest_NonVideoHasNoRung(t *testing.T) {
	s := ladderSession()
	for _, path := range []string{
		"/go-live/c/audio/segment_1.m4s",
		"/go-live/c/720p/init.mp4",
		"/go-live/c/audio/init.mp4",
	} {
		rc := classifyRequest(s, path, true, false, false)
		if rc.RungIndex != -1 {
			t.Errorf("%s: RungIndex = %d, want -1 (non-video)", path, rc.RungIndex)
		}
	}
}

// With no cached master playlist the classifier still assigns Kind (from path
// shape) but leaves the variant fields unresolved — a fault rule filtering only
// on request_kind still works before the master is fetched.
func TestClassifyRequest_NoLadder(t *testing.T) {
	rc := classifyRequest(SessionData{}, "/go-live/c/720p/segment_1.m4s", true, false, false)
	if rc.Kind != "segment" {
		t.Errorf("Kind = %q, want segment", rc.Kind)
	}
	if rc.RungIndex != -1 {
		t.Errorf("RungIndex = %d, want -1 (no ladder)", rc.RungIndex)
	}
}
