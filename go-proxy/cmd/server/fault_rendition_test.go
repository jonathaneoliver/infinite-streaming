package main

import (
	"bufio"
	"bytes"
	"testing"

	m3u8 "github.com/grafov/m3u8"
)

// TestMediaRenditionDir: parse a real media playlist and confirm the rendition
// dir comes from its EXT-X-MAP / segment paths (absolute in go-live output).
func TestMediaRenditionDir(t *testing.T) {
	media := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:6
#EXT-X-MAP:URI="/go-live/c/720p/init.mp4"
#EXTINF:6.0,
/go-live/c/720p/segment_00000.m4s
#EXTINF:6.0,
/go-live/c/720p/segment_00001.m4s
`
	pl, lt, err := m3u8.DecodeFrom(bufio.NewReader(bytes.NewReader([]byte(media))), true)
	if err != nil || lt != m3u8.MEDIA {
		t.Fatalf("decode: %v listType=%v", err, lt)
	}
	if got := mediaRenditionDir(pl.(*m3u8.MediaPlaylist)); got != "720p" {
		t.Errorf("mediaRenditionDir = %q, want 720p", got)
	}
}

// TestRenditionMap_AuthoritativeClassification is the #922 Tier 2 payoff: once a
// dir's media playlist has transited, a segment in that dir classifies with the
// TRUE resolution + rung (which matchVariantIndex can't derive, since the video
// variant URL is a playlist name that shares nothing with the segment dir), and
// a resolution-scoped fault rule finally matches the right rung only.
func TestRenditionMap_AuthoritativeClassification(t *testing.T) {
	s := SessionData{
		"manifest_variants": []PlaylistInfo{
			{URL: "playlist_6s_360p.m3u8", Bandwidth: 800_000, Resolution: "640x360"},
			{URL: "playlist_6s_720p.m3u8", Bandwidth: 4_000_000, Resolution: "1280x720"},
			{URL: "playlist_6s_2160p.m3u8", Bandwidth: 12_000_000, Resolution: "3840x2160"},
		},
		"manifest_audio_uris": []string{"playlist_6s_audio.m3u8"},
	}
	// Media playlists transit → record their segment dirs.
	recordRenditionDir(s, "/go-live/c/playlist_6s_720p.m3u8", "720p")
	recordRenditionDir(s, "/go-live/c/playlist_6s_2160p.m3u8", "2160p")
	recordRenditionDir(s, "/go-live/c/playlist_6s_audio.m3u8", "audio")

	rc := classifyRequest(s, "/go-live/c/720p/segment_00017.m4s", true, false, false)
	if rc.Kind != "segment" || rc.Resolution != "1280x720" || rc.RungIndex != 1 {
		t.Errorf("720p segment: kind=%q res=%q rung=%d, want segment/1280x720/1", rc.Kind, rc.Resolution, rc.RungIndex)
	}
	rc = classifyRequest(s, "/go-live/c/2160p/segment_00017.m4s", true, false, false)
	if rc.Resolution != "3840x2160" || rc.RungIndex != 2 {
		t.Errorf("2160p segment: res=%q rung=%d, want 3840x2160/2", rc.Resolution, rc.RungIndex)
	}
	rc = classifyRequest(s, "/go-live/c/audio/segment_00017.m4s", true, false, false)
	if rc.Kind != "audio_segment" || !rc.IsAudio || rc.RungIndex != -1 {
		t.Errorf("audio segment: kind=%q isAudio=%v rung=%d, want audio_segment/true/-1", rc.Kind, rc.IsAudio, rc.RungIndex)
	}

	// A resolution-scoped rule now targets 720p only — the whole point.
	rules := []any{map[string]any{"id": "r", "type": "500",
		"filter": map[string]any{"variant": map[string]any{"resolutions": []any{"1280x720"}}}}}
	if _, ok := matchFaultRule(rules, classifyRequest(s, "/go-live/c/720p/segment_1.m4s", true, false, false)); !ok {
		t.Error("720p segment should match resolutions=[1280x720]")
	}
	if _, ok := matchFaultRule(rules, classifyRequest(s, "/go-live/c/2160p/segment_1.m4s", true, false, false)); ok {
		t.Error("2160p segment must NOT match resolutions=[1280x720]")
	}
}

// TestRenditionMap_AudioDirNotNamedAudio proves the map is structure-driven, not
// spelling-driven: an audio rendition whose segments live under a dir NOT named
// "audio" still classifies as audio, because the map keyed it from the playlist
// that the master declared as EXT-X-MEDIA TYPE=AUDIO.
func TestRenditionMap_AudioDirNotNamedAudio(t *testing.T) {
	s := SessionData{
		"manifest_variants":   []PlaylistInfo{{URL: "v720.m3u8", Bandwidth: 4_000_000, Resolution: "1280x720"}},
		"manifest_audio_uris": []string{"soundtrack.m3u8"},
	}
	recordRenditionDir(s, "/go-live/c/soundtrack.m3u8", "aac_en") // segments under /aac_en/, not /audio/
	rc := classifyRequest(s, "/go-live/c/aac_en/segment_5.m4s", true, false, false)
	if rc.Kind != "audio_segment" || !rc.IsAudio {
		t.Errorf("aac_en segment: kind=%q isAudio=%v, want audio_segment/true (structure over spelling)", rc.Kind, rc.IsAudio)
	}
}
