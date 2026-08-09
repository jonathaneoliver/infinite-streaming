package dash

import (
	"os"
	"path/filepath"
	"testing"
)

// Ranges lifted from the encoder sample
// insane_fpv_shots_hydrofoil_windsurfing_p200_h264_xs, 1080p/segment_00001.m4s.
// The MPD spells them as inclusive @mediaRange; the sidecar spells the same
// fragments as offset/length. Keeping both here is the point of the parity test.
//
// Three fragments for segment 1, one for segment 2, so grouping by @media has
// something to get wrong.
const sampleFragmentedMPD = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="static" mediaPresentationDuration="PT12S">
  <Period id="0">
    <AdaptationSet id="0" contentType="video">
      <Representation id="0" bandwidth="6779837">
        <SegmentList timescale="30000">
          <Initialization sourceURL="1080p/init.mp4"/>
          <SegmentTimeline>
            <S t="0" d="6006" r="2"/>
            <S d="18018"/>
          </SegmentTimeline>
          <SegmentURL media="1080p/segment_00001.m4s" mediaRange="432-150105"/>
          <SegmentURL media="1080p/segment_00001.m4s" mediaRange="150106-240493"/>
          <SegmentURL media="1080p/segment_00001.m4s" mediaRange="240494-434605"/>
          <SegmentURL media="1080p/segment_00002.m4s" mediaRange="432-67505"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

// The segment-granularity equivalent of the document above: two segments, the
// first 3*6006 ticks, the second 18018. This is what the encoder writes into
// manifest.mpd, and what collapsing the fragmented file must reproduce.
const sampleSegmentMPD = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="static" mediaPresentationDuration="PT12S">
  <Period id="0">
    <AdaptationSet id="0" contentType="video">
      <Representation id="0" bandwidth="6779837">
        <SegmentList timescale="30000">
          <Initialization sourceURL="1080p/init.mp4"/>
          <SegmentTimeline>
            <S t="0" d="18018" r="1"/>
          </SegmentTimeline>
          <SegmentURL media="1080p/segment_00001.m4s"/>
          <SegmentURL media="1080p/segment_00002.m4s"/>
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`

const sampleSidecar = `{
  "fragments": [
    {"offset": 432, "length": 149674, "independent": true},
    {"offset": 150106, "length": 90388, "independent": false},
    {"offset": 240494, "length": 194112, "independent": false}
  ]
}`

// writeContent lays out a content directory and loads the named manifest the
// way production does, so the tests exercise the real load path.
func loadContent(t *testing.T, name, manifest, sidecar string) *MPDData {
	t.Helper()
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(manifest), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	if sidecar != "" {
		if err := os.MkdirAll(filepath.Join(dir, "1080p"), 0o755); err != nil {
			t.Fatalf("creating rung dir: %v", err)
		}
		p := filepath.Join(dir, "1080p", "segment_00001.m4s.byteranges")
		if err := os.WriteFile(p, []byte(sidecar), 0o644); err != nil {
			t.Fatalf("writing sidecar: %v", err)
		}
	}

	data, err := LoadMPD(dir, name)
	if err != nil {
		t.Fatalf("LoadMPD(%s): %v", name, err)
	}
	return data
}

func TestParseMediaRangeIsInclusive(t *testing.T) {
	// 432-150105 is 149674 bytes, not 149673: @mediaRange includes both ends.
	offset, length, err := parseMediaRange("432-150105")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if offset != 432 {
		t.Errorf("offset = %d, want 432", offset)
	}
	if length != 149674 {
		t.Errorf("length = %d, want 149674 (inclusive range)", length)
	}
}

func TestParseMediaRangeRejectsMalformed(t *testing.T) {
	for _, in := range []string{"", "432", "abc-def", "150105-432", "-", "432-"} {
		if _, _, err := parseMediaRange(in); err == nil {
			t.Errorf("parseMediaRange(%q) succeeded, want error", in)
		}
	}
}

// The claim that makes single-input DASH possible: collapsing the fragmented
// manifest reproduces the segment-granularity manifest exactly. If this drifts,
// every segment boundary shifts and nothing downstream notices.
func TestCollapsedFragmentedMatchesSegmentManifest(t *testing.T) {
	fragmented := loadContent(t, fragmentedMPDName, sampleFragmentedMPD, "")
	segmented := loadContent(t, segmentMPDName, sampleSegmentMPD, "")

	if fragmented.SegmentCount != segmented.SegmentCount {
		t.Errorf("SegmentCount = %d, want %d", fragmented.SegmentCount, segmented.SegmentCount)
	}
	if fragmented.SegmentDuration != segmented.SegmentDuration {
		t.Errorf("SegmentDuration = %v, want %v", fragmented.SegmentDuration, segmented.SegmentDuration)
	}
	if fragmented.TotalDuration != segmented.TotalDuration {
		t.Errorf("TotalDuration = %v, want %v", fragmented.TotalDuration, segmented.TotalDuration)
	}

	fw, sw := fragmented.Timelines["0"], segmented.Timelines["0"]
	if fw == nil || sw == nil {
		t.Fatalf("missing timeline: fragmented=%v segmented=%v", fw != nil, sw != nil)
	}
	if len(fw.SegmentDurations) != len(sw.SegmentDurations) {
		t.Fatalf("timeline length = %d, want %d", len(fw.SegmentDurations), len(sw.SegmentDurations))
	}
	for i := range fw.SegmentDurations {
		if fw.SegmentDurations[i] != sw.SegmentDurations[i] {
			t.Errorf("segment %d duration = %d, want %d", i, fw.SegmentDurations[i], sw.SegmentDurations[i])
		}
	}
}

// Collapsing must keep the byte-ranges it strips out, or the 1s/2s/LL variants
// lose their partials.
func TestCollapseRetainsFragmentRanges(t *testing.T) {
	data := loadContent(t, fragmentedMPDName, sampleFragmentedMPD, "")

	first, err := loadByterangesForSegment(data, "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("segment 1: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("segment 1 has %d fragments, want 3", len(first))
	}
	// Document order is playback order; offsets must ascend.
	for i := 1; i < len(first); i++ {
		if first[i].Offset <= first[i-1].Offset {
			t.Errorf("fragment %d offset %d does not follow %d", i, first[i].Offset, first[i-1].Offset)
		}
	}
	if first[0].Offset != 432 || first[0].Length != 149674 {
		t.Errorf("fragment 0 = {%d,%d}, want {432,149674}", first[0].Offset, first[0].Length)
	}

	// Grouping is by @media, so segment 2 must not absorb segment 1's fragments.
	second, err := loadByterangesForSegment(data, "1080p/segment_00002.m4s")
	if err != nil {
		t.Fatalf("segment 2: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("segment 2 has %d fragments, want 1", len(second))
	}
	if second[0].Offset != 432 || second[0].Length != 67074 {
		t.Errorf("segment 2 fragment = {%d,%d}, want {432,67074}", second[0].Offset, second[0].Length)
	}
}

// Ranges recovered from the collapse must equal what the sidecar reported.
func TestCollapsedRangesMatchSidecar(t *testing.T) {
	fromMPD, err := loadByterangesForSegment(
		loadContent(t, fragmentedMPDName, sampleFragmentedMPD, ""), "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("loading from fragmented MPD: %v", err)
	}
	fromSidecar, err := loadByterangesForSegment(
		loadContent(t, segmentMPDName, sampleSegmentMPD, sampleSidecar), "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("loading from sidecar: %v", err)
	}

	if len(fromMPD) != len(fromSidecar) {
		t.Fatalf("fragment count differs: MPD %d, sidecar %d", len(fromMPD), len(fromSidecar))
	}
	for i := range fromMPD {
		if fromMPD[i].Offset != fromSidecar[i].Offset || fromMPD[i].Length != fromSidecar[i].Length {
			t.Errorf("fragment %d differs: MPD {%d,%d}, sidecar {%d,%d}",
				i, fromMPD[i].Offset, fromMPD[i].Length,
				fromSidecar[i].Offset, fromSidecar[i].Length)
		}
	}
}

// Legacy content has a segment-granularity manifest and sidecars; it must be
// left completely alone.
func TestSegmentManifestIsNotCollapsed(t *testing.T) {
	data := loadContent(t, segmentMPDName, sampleSegmentMPD, sampleSidecar)
	if len(data.InlineRanges) != 0 {
		t.Errorf("segment-granularity manifest produced %d inline ranges, want 0", len(data.InlineRanges))
	}
	fragments, err := loadByterangesForSegment(data, "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fragments) != 3 {
		t.Fatalf("got %d fragments, want the sidecar's 3", len(fragments))
	}
	if !fragments[0].Independent {
		t.Error("sidecar path dropped the independent flag")
	}
}

// Inline ranges win over a sidecar. Values differ so a silent fall-through
// would fail rather than coincidentally pass.
func TestInlineRangesTakePriorityOverSidecar(t *testing.T) {
	divergent := `{"fragments": [{"offset": 999999, "length": 1, "independent": true}]}`
	data := loadContent(t, fragmentedMPDName, sampleFragmentedMPD, divergent)

	fragments, err := loadByterangesForSegment(data, "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fragments) != 3 {
		t.Fatalf("got %d fragments, want 3 from the manifest", len(fragments))
	}
	if fragments[0].Offset == 999999 {
		t.Error("read the sidecar when the manifest carried ranges")
	}
}

func TestErrorsWhenNoRangeSourceExists(t *testing.T) {
	data := loadContent(t, segmentMPDName, sampleSegmentMPD, "")
	if _, err := loadByterangesForSegment(data, "1080p/segment_00001.m4s"); err == nil {
		t.Error("expected an error when no range source exists")
	}
}

func TestLeadingSlashInMediaPathStillMatches(t *testing.T) {
	data := loadContent(t, fragmentedMPDName, sampleFragmentedMPD, "")
	fragments, err := loadByterangesForSegment(data, "/1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fragments) != 3 {
		t.Fatalf("got %d fragments, want 3", len(fragments))
	}
}

// A timeline that disagrees with the SegmentURL count means we cannot know
// where segments end; collapsing on a guess would silently move every boundary.
func TestMismatchedTimelineIsNotCollapsed(t *testing.T) {
	bad := `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><Representation id="0">
  <SegmentList timescale="30000">
    <SegmentTimeline><S t="0" d="6006"/></SegmentTimeline>
    <SegmentURL media="1080p/segment_00001.m4s" mediaRange="432-150105"/>
    <SegmentURL media="1080p/segment_00001.m4s" mediaRange="150106-240493"/>
  </SegmentList>
</Representation></AdaptationSet></Period></MPD>`

	data := loadContent(t, fragmentedMPDName, bad, "")
	if len(data.InlineRanges) != 0 {
		t.Errorf("collapsed a manifest whose timeline disagrees with its URL list (%d ranges)", len(data.InlineRanges))
	}
}

// Granularity is decided by content, not by filename: the same fragment-level
// document collapses identically whether it is called manifest.mpd or
// manifest_fragmented.mpd. That is what lets the serving path keep one name.
func TestGranularityIsDetectedFromContentNotFilename(t *testing.T) {
	underStandardName := loadContent(t, segmentMPDName, sampleFragmentedMPD, "")
	underFragmentedName := loadContent(t, fragmentedMPDName, sampleFragmentedMPD, "")

	if len(underStandardName.InlineRanges) == 0 {
		t.Fatal("a fragment-granularity document named manifest.mpd was not collapsed")
	}
	if underStandardName.SegmentCount != underFragmentedName.SegmentCount {
		t.Errorf("SegmentCount differs by filename: %d vs %d",
			underStandardName.SegmentCount, underFragmentedName.SegmentCount)
	}

	got, err := loadByterangesForSegment(underStandardName, "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d fragments, want 3", len(got))
	}
}

// The reverse: a segment-granularity document keeps working under either name,
// sourcing its ranges from the sidecars.
func TestSegmentGranularityUnderEitherName(t *testing.T) {
	data := loadContent(t, fragmentedMPDName, sampleSegmentMPD, sampleSidecar)
	if len(data.InlineRanges) != 0 {
		t.Errorf("collapsed a segment-granularity document because of its filename")
	}
	got, err := loadByterangesForSegment(data, "1080p/segment_00001.m4s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d fragments, want the sidecar's 3", len(got))
	}
}
