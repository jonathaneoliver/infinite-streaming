package main

import "testing"

// A trimmed go-live SegmentList MPD: a video AdaptationSet (3 rungs) + an audio
// AdaptationSet, segment/init paths under the same CMAF dirs HLS uses.
const sampleMPD = `<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="dynamic">
  <Period id="loop_3" start="PT1003S">
    <AdaptationSet id="0" contentType="video" par="16:9">
      <Representation id="0" bandwidth="775917" codecs="avc1.64001e" mimeType="video/mp4" width="640" height="360">
        <SegmentList>
          <Initialization sourceURL="360p/init.mp4"/>
          <SegmentURL media="360p/segment_00038.m4s"/>
        </SegmentList>
      </Representation>
      <Representation id="3" bandwidth="3561627" codecs="avc1.64001f" mimeType="video/mp4" width="1280" height="720">
        <SegmentList>
          <Initialization sourceURL="720p/init.mp4"/>
          <SegmentURL media="720p/segment_00038.m4s"/>
        </SegmentList>
      </Representation>
      <Representation id="8" bandwidth="33595654" codecs="avc1.640033" mimeType="video/mp4" width="3840" height="2160">
        <SegmentList>
          <Initialization sourceURL="2160p/init.mp4"/>
          <SegmentURL media="2160p/segment_00038.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="1" contentType="audio" lang="en">
      <Representation id="9" bandwidth="212379" codecs="mp4a.40.2" mimeType="audio/mp4">
        <SegmentList>
          <Initialization sourceURL="audio/init.mp4"/>
          <SegmentURL media="audio/segment_00038.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

func TestParseDASHManifest(t *testing.T) {
	variants, renditions := parseDASHManifest([]byte(sampleMPD))
	if len(variants) != 3 {
		t.Fatalf("video variants = %d, want 3", len(variants))
	}
	// Rendition map: 3 video dirs + 1 audio.
	if len(renditions) != 4 {
		t.Fatalf("rendition map = %d entries, want 4; got %v", len(renditions), renditions)
	}
	// Bandwidth-ascending rungs: 360p=0, 720p=1, 2160p=2.
	for dir, wantRung := range map[string]int{"360p": 0, "720p": 1, "2160p": 2} {
		r := renditions[dir]
		if r.Kind != "video" || r.Rung != wantRung {
			t.Errorf("%s: kind=%q rung=%d, want video/%d", dir, r.Kind, r.Rung, wantRung)
		}
	}
	if renditions["720p"].Resolution != "1280x720" {
		t.Errorf("720p resolution = %q, want 1280x720", renditions["720p"].Resolution)
	}
	if renditions["audio"].Kind != "audio" {
		t.Errorf("audio dir kind = %q, want audio", renditions["audio"].Kind)
	}
}

// TestDASHClassificationEndToEnd: parse the MPD → merge onto the session → DASH
// segments (same CMAF dirs) classify with authoritative resolution/rung/audio,
// and a resolution-scoped fault matches the right rung only.
func TestDASHClassificationEndToEnd(t *testing.T) {
	s := SessionData{}
	_, renditions := parseDASHManifest([]byte(sampleMPD))
	mergeRenditionMap(s, renditions)

	rc := classifyRequest(s, "/go-live/c/720p/segment_00040.m4s", true, false, false)
	if rc.Kind != "segment" || rc.Resolution != "1280x720" || rc.RungIndex != 1 {
		t.Errorf("DASH 720p seg: kind=%q res=%q rung=%d, want segment/1280x720/1", rc.Kind, rc.Resolution, rc.RungIndex)
	}
	rc = classifyRequest(s, "/go-live/c/audio/segment_00040.m4s", true, false, false)
	if rc.Kind != "audio_segment" || !rc.IsAudio {
		t.Errorf("DASH audio seg: kind=%q isAudio=%v, want audio_segment/true", rc.Kind, rc.IsAudio)
	}

	rules := []any{map[string]any{"id": "r", "type": "500",
		"filter": map[string]any{"variant": map[string]any{"resolutions": []any{"1280x720"}}}}}
	if _, ok := matchFaultRule(rules, classifyRequest(s, "/go-live/c/720p/segment_1.m4s", true, false, false)); !ok {
		t.Error("DASH 720p segment should match resolutions=[1280x720]")
	}
	if _, ok := matchFaultRule(rules, classifyRequest(s, "/go-live/c/2160p/segment_1.m4s", true, false, false)); ok {
		t.Error("DASH 2160p segment must NOT match resolutions=[1280x720]")
	}
}
