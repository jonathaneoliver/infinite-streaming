// server_dash_rendition_test.go — #926 live integration.
//
// Fetching the DASH .mpd through the proxy must populate the session's
// _rendition_map (segmentDir → video resolution/rung | audio), parsed from the
// MPD's AdaptationSet/Representation/SegmentList. That's what gives DASH the
// same structure-driven fault scoping as HLS (the classification + scoping
// logic on top is covered by fault_dash_test.go and TestServerScopeByRendition).
package server_behavior

import (
	"strings"
	"testing"
	"time"
)

func TestDASHRenditionMapPopulates(t *testing.T) {
	if testing.Short() {
		t.Skip("DASH rendition check skipped in short mode")
	}
	p := newProbe(t)
	mpdURL := strings.Replace(p.masterURL, "master_6s.m3u8", "manifest_6s.mpd", 1)
	if mpdURL == p.masterURL {
		t.Skipf("no DASH manifest URL derivable from %q", p.masterURL)
	}
	if _, _, err := httpGet(p.c, mpdURL); err != nil {
		t.Fatalf("fetch .mpd through proxy: %v", err)
	}
	time.Sleep(750 * time.Millisecond) // let the session write persist

	sm, err := getSessionMap(p.c, p.apiBase, p.playerID)
	if err != nil {
		t.Fatalf("session map: %v", err)
	}
	rm, ok := sm["_rendition_map"].(map[string]any)
	if !ok || len(rm) == 0 {
		t.Fatalf("_rendition_map not populated from the DASH .mpd; got %v", sm["_rendition_map"])
	}

	var video, audio int
	var sawResolution bool
	for _, raw := range rm {
		m, _ := raw.(map[string]any)
		switch m["kind"] {
		case "video":
			video++
			if res, _ := m["resolution"].(string); res != "" {
				sawResolution = true
			}
		case "audio":
			audio++
		}
	}
	t.Logf("_rendition_map from .mpd: %d entries (video=%d audio=%d)", len(rm), video, audio)
	if video == 0 {
		t.Error("no video renditions mapped from the DASH .mpd")
	}
	if audio == 0 {
		t.Error("no audio rendition mapped from the DASH .mpd")
	}
	if !sawResolution {
		t.Error("video renditions have no resolution — width/height not parsed from the MPD")
	}
}
