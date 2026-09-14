package dash

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// usesVirtualSegments gates the fragment-regrouping path. The base package is 6s;
// LL and 6s serve the base SegmentList directly, while 1s and 2s are synthesized
// by regrouping the base fmp4 fragments. 1s was added alongside 2s (#931).
func TestUsesVirtualSegments(t *testing.T) {
	cases := map[int]bool{
		1: true,  // regrouped sub-base variant (#931)
		2: true,  // regrouped sub-base variant
		4: false, // not a served variant; init-prefix special-cased elsewhere
		6: false, // base segments, served directly
	}
	for duration, want := range cases {
		if got := usesVirtualSegments(duration); got != want {
			t.Errorf("usesVirtualSegments(%d) = %v, want %v", duration, got, want)
		}
	}
}

// staticSegmentMPD builds a single-rung, segment-granularity static MPD of
// `segments` × `segSeconds`, the shape LoadMPD reads from a packaged clip.
func staticSegmentMPD(segments, segSeconds int) string {
	const timescale = 30000
	urls := ""
	for i := 1; i <= segments; i++ {
		urls += fmt.Sprintf("\n          <SegmentURL media=\"360p/segment_%05d.m4s\"/>", i)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="static" mediaPresentationDuration="PT%dS">
  <Period id="0">
    <AdaptationSet id="0" contentType="video">
      <Representation id="0" bandwidth="400000">
        <SegmentList timescale="%d">
          <Initialization sourceURL="360p/init.mp4"/>
          <SegmentTimeline>
            <S t="0" d="%d" r="%d"/>
          </SegmentTimeline>%s
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, segments*segSeconds, timescale, segSeconds*timescale, segments-1, urls)
}

// generateAcrossLoops calls GenerateLiveMPD at every whole second across two
// full loops of the content (the first call fixes the stream epoch, so this
// sweeps every window position, including the first segment of the first loop
// where a short clip used to panic) and returns the rendered documents.
func generateAcrossLoops(t *testing.T, segments, segSeconds int) []string {
	t.Helper()
	data := loadContent(t, "manifest.mpd", staticSegmentMPD(segments, segSeconds), "")
	t0 := time.Date(2026, 9, 14, 22, 18, 10, 0, time.UTC)
	var docs []string
	for sec := 0; sec <= 2*segments*segSeconds; sec++ {
		out, err := GenerateLiveMPD(data, t0.Add(time.Duration(sec)*time.Second), "short", segSeconds, false)
		if err != nil {
			t.Fatalf("t+%ds: GenerateLiveMPD: %v", sec, err)
		}
		docs = append(docs, string(out))
	}
	return docs
}

// A clip shorter than the 36s live window must not panic, and its window must
// be clamped to the clip: never more than two Periods (one loop crossing), and
// a timeShiftBufferDepth that doesn't advertise more than the clip holds.
// Regression for a 24s encode crashing go-live with "index out of range [-1]".
func TestLiveMPDContentShorterThanWindow(t *testing.T) {
	docs := generateAcrossLoops(t, 4, 6) // 24s: 4 segments vs a 6-segment window
	wantDepth := `timeShiftBufferDepth="` + formatDuration(24) + `"`
	for i, doc := range docs {
		if periods := strings.Count(doc, "<Period"); periods < 1 || periods > 2 {
			t.Fatalf("t+%ds: %d Periods, want 1 or 2", i, periods)
		}
		if !strings.Contains(doc, "SegmentURL") {
			t.Fatalf("t+%ds: no SegmentURLs in live MPD", i)
		}
		if !strings.Contains(doc, wantDepth) {
			t.Fatalf("t+%ds: want %s in live MPD", i, wantDepth)
		}
	}
}

// Content at least as long as the window keeps the unclamped defaults.
func TestLiveMPDLongContentWindowUnchanged(t *testing.T) {
	docs := generateAcrossLoops(t, 12, 6) // 72s: 12 segments vs a 6-segment window
	wantDepth := `timeShiftBufferDepth="` + formatDuration(maxLiveWindowDurationSec) + `"`
	for i, doc := range docs {
		if !strings.Contains(doc, wantDepth) {
			t.Fatalf("t+%ds: want %s in live MPD", i, wantDepth)
		}
	}
}
