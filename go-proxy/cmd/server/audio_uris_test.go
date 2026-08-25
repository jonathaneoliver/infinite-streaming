package main

import (
	"bufio"
	"bytes"
	"testing"

	m3u8 "github.com/grafov/m3u8"
)

// TestExtractAudioURIs decodes a real go-live master (the insane_newer_p200_h264
// shape) and confirms extractAudioURIs pulls the EXT-X-MEDIA TYPE=AUDIO URI —
// deduped across the many variants that all reference the same audio GROUP-ID.
// This locks the grafov/m3u8 integration: the Alternatives must actually be
// populated for the #919 Tier 1 structure-driven audio classification to work.
func TestExtractAudioURIs(t *testing.T) {
	master := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="Audio",LANGUAGE="en",AUTOSELECT=YES,DEFAULT=YES,URI="playlist_6s_audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=988296,RESOLUTION=640x360,CODECS="avc1.64001e,mp4a.40.2",AUDIO="audio"
playlist_6s_360p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3774006,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2",AUDIO="audio"
playlist_6s_720p.m3u8
`
	pl, listType, err := m3u8.DecodeFrom(bufio.NewReader(bytes.NewReader([]byte(master))), true)
	if err != nil {
		t.Fatalf("decode master: %v", err)
	}
	if listType != m3u8.MASTER {
		t.Fatalf("listType = %v, want MASTER", listType)
	}
	uris := extractAudioURIs(pl.(*m3u8.MasterPlaylist))
	if len(uris) != 1 {
		t.Fatalf("audio URIs = %v, want exactly one (deduped)", uris)
	}
	if uris[0] != "playlist_6s_audio.m3u8" {
		t.Errorf("audio URI = %q, want playlist_6s_audio.m3u8", uris[0])
	}

	// End-to-end: with that URI cached, the classifier tags the audio playlist
	// request audio_manifest and leaves a video playlist alone.
	s := SessionData{"manifest_audio_uris": uris}
	if rc := classifyRequest(s, "/go-live/c/playlist_6s_audio.m3u8", false, true, false); rc.Kind != "audio_manifest" {
		t.Errorf("audio playlist Kind = %q, want audio_manifest", rc.Kind)
	}
	if rc := classifyRequest(s, "/go-live/c/playlist_6s_720p.m3u8", false, true, false); rc.Kind != "manifest" {
		t.Errorf("video playlist Kind = %q, want manifest", rc.Kind)
	}
}
