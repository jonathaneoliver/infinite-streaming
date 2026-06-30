package generator

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/pkg/playlist"
	"github.com/jonathaneoliver/infinite-streaming/go-live/pkg/parser"
)

const (
	// Sliding live window. Must be comfortably larger than the player's
	// forward buffer so a throttled/stalled player doesn't fall off the
	// oldest edge before it can catch up. With a ~20s forward buffer,
	// 36s left only ~16s of real slack — any drift triggered
	// `-12642 No matching mediaFile found from playlist` on speculative
	// ABR playlist polls. 120s keeps the window well ahead of realistic
	// drift while staying bounded.
	MAX_LIVE_WINDOW_DURATION = 120.0 // 120 seconds
)

var (
	logOnceMuLL     sync.Mutex
	lastLogSecondLL int64
)

func logOncePerSecondLL(message string) {
	now := time.Now().Unix()
	logOnceMuLL.Lock()
	defer logOnceMuLL.Unlock()
	if lastLogSecondLL == now {
		return
	}
	lastLogSecondLL = now
	fmt.Fprintln(os.Stderr, message)
}

// LLHLSGenerator generates LL-HLS playlists
type LLHLSGenerator struct{}

func writeLoopDateRange(sb *strings.Builder, loopCount int, at time.Time, marker string) {
	timestamp := at.UTC().Format("2006-01-02T15:04:05.000Z")
	// Loop boundary marker for downstream analytics and loop counters.
	// sb.WriteString(fmt.Sprintf("#EXT-X-DATERANGE:ID=\"loop-%d-%s\",CLASS=\"com.infinite.loop\",START-DATE=\"%s\",X-LOOP-COUNT=\"%d\"\n", loopCount, marker, timestamp, loopCount))
	logOncePerSecondLL(fmt.Sprintf("[GO-LIVE:LOOP][LL] marker=%s loop_count=%d start_date=%s", marker, loopCount, timestamp))
}

// GenerateVariantPlaylist generates a single variant playlist with byte-range partials
func (g *LLHLSGenerator) GenerateVariantPlaylist(
	pl *playlist.Media,
	byteranges map[string][]parser.ByteRange,
	relPath string,
	segmentMap string,
	timeNow float64,
	loopCount int,
	minDuration float64,
	maxDuration float64,
) (string, error) {

	// Auto-detect content characteristics from first segment
	segmentDuration, partialsPerSegment, partialDuration := detectContentCharacteristics(pl, byteranges)

	// Determine TARGETDURATION from the actual GOP-aligned sub-segment sizes.
	// We split each underlying segment into sub-segments that start at each
	// INDEPENDENT fragment (≈1s GOPs), so the largest sub-segment governs
	// TARGETDURATION (ceil of the max group duration → ~1). This drops the
	// AVPlayer HOLD-BACK floor from 18s (with a 6s EXTINF) to 3s.
	subSegTargetDuration := computeSubSegmentTargetDuration(pl, byteranges, partialDuration, segmentDuration)
	// HOLD-BACK is 3 × TARGETDURATION per RFC 8216 recommendation.
	holdBack := 3.0 * float64(subSegTargetDuration)

	// Playlist position remains absolute-time based.
	timeOffset := math.Mod(timeNow, minDuration)

	// Calculate PDT (Program Date Time)
	pdtSeconds := timeNow - maxDuration
	pdt := time.Unix(int64(pdtSeconds), 0).UTC()

	// Calculate which segment we're on using ideal segment duration
	// This matches live.py approach and handles variable-length final segments correctly
	numSegments := countSegments(pl)
	idealSegmentDuration := minDuration / float64(numSegments)
	currentSegmentIdx := int(timeOffset / idealSegmentDuration)

	// Clamp to valid range
	if currentSegmentIdx >= numSegments {
		currentSegmentIdx = numSegments - 1
	}
	if currentSegmentIdx < 0 {
		currentSegmentIdx = 0
	}

	// Calculate which partial within the current segment
	segmentStartTime := float64(currentSegmentIdx) * idealSegmentDuration
	timeWithinSegment := timeOffset - segmentStartTime
	currentPartialIdx := int(timeWithinSegment / partialDuration)

	// Clamp partial index to valid range
	if currentPartialIdx > partialsPerSegment-1 {
		currentPartialIdx = partialsPerSegment - 1
	}
	if currentPartialIdx < 0 {
		currentPartialIdx = 0
	}

	// Calculate media sequence (segment number)
	sequence := (loopCount * numSegments) + currentSegmentIdx

	// Pre-compute the window wrap decision here so we can emit the correct
	// EXT-X-DISCONTINUITY-SEQUENCE in the playlist header below. The actual
	// window-building logic further down uses the same computation.
	segmentsNeededForDiscSeq := int(MAX_LIVE_WINDOW_DURATION / idealSegmentDuration)
	wrapAroundForDiscSeq := currentSegmentIdx < segmentsNeededForDiscSeq-1
	firstSegLoop := loopCount
	if wrapAroundForDiscSeq {
		// Wrapping: first segment in window comes from the previous loop,
		// and this playlist contains the discontinuity marker itself.
		firstSegLoop = loopCount - 1
		if firstSegLoop < 0 {
			firstSegLoop = 0
		}
	}

	// Build playlist
	var sb strings.Builder

	// LL-HLS playlist header
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:10\n") // Version 10 for LL-HLS
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", subSegTargetDuration))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequence))
	// EXT-X-DISCONTINUITY-SEQUENCE is REQUIRED by RFC 8216 §4.3.3.3 when the
	// playlist contains an EXT-X-DISCONTINUITY tag. Required for cross-variant
	// synchronization during ABR evaluation; absence causes AVPlayer -12642
	// "No matching mediaFile found from playlist".
	sb.WriteString(fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", firstSegLoop))

	// LL-HLS specific tags
	sb.WriteString(fmt.Sprintf("#EXT-X-PART-INF:PART-TARGET=%.3f\n", partialDuration))
	sb.WriteString(fmt.Sprintf("#EXT-X-SERVER-CONTROL:HOLD-BACK=%.3f,PART-HOLD-BACK=3.0\n", holdBack))

	// Program date time
	sb.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", pdt.Format("2006-01-02T15:04:05.000Z")))

	// Start time-offset: 3.0 seconds to match PART-HOLD-BACK
	sb.WriteString("#EXT-X-START:TIME-OFFSET=-3.0,PRECISE=YES\n")

	// Init segment (fMP4 initialization)
	if segmentMap != "" {
		sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", baseURI(segmentMap)))
	}

	// Add discontinuity at loop boundary
	if currentSegmentIdx == 0 {
		writeLoopDateRange(&sb, loopCount, pdt, "head")
		sb.WriteString("#EXT-X-DISCONTINUITY\n")
		if segmentMap != "" {
			sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", baseURI(segmentMap)))
		}
	}

	// Calculate sliding window - how many past segments to show
	// Use idealSegmentDuration so audio/video windows align on segment count.
	segmentsNeeded := int(MAX_LIVE_WINDOW_DURATION / idealSegmentDuration)

	// Handle wrap-around at loop boundary
	var startIdx int
	var wrapAround bool

	if currentSegmentIdx < segmentsNeeded-1 {
		// Need to wrap around - show segments from end of previous loop
		segmentsFromEnd := segmentsNeeded - 1 - currentSegmentIdx
		startIdx = numSegments - segmentsFromEnd
		wrapAround = true
	} else {
		// Normal case - enough segments in current position
		startIdx = currentSegmentIdx - segmentsNeeded + 1
		wrapAround = false
	}

	accumulatedDuration := 0.0

	// FIRST: Write all PAST segments (fully available, in chronological order)
	// Handle wrap-around case: segments from end of video, then segments from beginning
	if wrapAround {
		// Write segments from end of video (previous loop)
		for segIdx := startIdx; segIdx < numSegments; segIdx++ {
			segment := getSegment(pl, segIdx)
			if segment == nil {
				continue
			}
			segmentURI := baseURI(segment.URI)
			fragments := byteranges[segment.URI]

			// Emit GOP-aligned sub-segments (each ≈1s) with their partials.
			writeCompleteSegmentSubSegments(&sb, segmentURI, fragments, partialDuration)

			accumulatedDuration += segment.Duration.Seconds()
		}

		// Add discontinuity before switching from previous loop to current loop segments
		boundaryAt := pdt.Add(time.Duration(accumulatedDuration * float64(time.Second)))
		writeLoopDateRange(&sb, loopCount, boundaryAt, "wrap")
		sb.WriteString("#EXT-X-DISCONTINUITY\n")
		if segmentMap != "" {
			sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", baseURI(segmentMap)))
		}
	}

	// Write segments from current loop (0 to current_segment_idx)
	startSegIdx := 0
	if !wrapAround {
		startSegIdx = startIdx
	}

	for segIdx := startSegIdx; segIdx < currentSegmentIdx; segIdx++ {
		segment := getSegment(pl, segIdx)
		if segment == nil {
			continue
		}
		segmentURI := baseURI(segment.URI)
		fragments := byteranges[segment.URI]

		// Emit GOP-aligned sub-segments (each ≈1s) with their partials.
		writeCompleteSegmentSubSegments(&sb, segmentURI, fragments, partialDuration)

		accumulatedDuration += segment.Duration.Seconds()

		if accumulatedDuration >= MAX_LIVE_WINDOW_DURATION {
			break
		}
	}

	// LAST: Write the CURRENT segment (live edge - may be incomplete)
	currentSegment := getSegment(pl, currentSegmentIdx)
	if currentSegment != nil {
		segmentURI := baseURI(currentSegment.URI)
		fragments := byteranges[currentSegment.URI]

		// Only fragments up to current_partial_idx (inclusive) are "live".
		availableCount := currentPartialIdx + 1
		if availableCount > len(fragments) {
			availableCount = len(fragments)
		}
		segmentComplete := currentPartialIdx >= len(fragments)-1

		// Walk the available fragments, grouping into GOP-aligned sub-segments
		// that start at each INDEPENDENT fragment. A group is emitted as a
		// closed sub-segment (parts + #EXT-X-BYTERANGE + #EXTINF) only once its
		// closing boundary is known — either the next INDEPENDENT fragment has
		// appeared, or the whole segment is complete. The trailing, not-yet-
		// closed group stays as bare #EXT-X-PART lines (no #EXTINF yet),
		// preserving the existing live-edge behaviour.
		groups := groupFragmentsByGOP(fragments[:availableCount])
		for gi, group := range groups {
			// A group is closed if it is not the last available group, or if
			// it is the last group AND the segment is complete (no more
			// fragments will arrive to extend it).
			closed := gi < len(groups)-1 || segmentComplete

			// Emit every part in the group (unchanged part formatting).
			for _, fragment := range group {
				partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
					partialDuration, segmentURI, fragment.Length, fragment.Offset)
				if fragment.Independent {
					partTag += ",INDEPENDENT=YES"
				}
				sb.WriteString(partTag + "\n")
			}

			if closed {
				writeSubSegmentTail(&sb, segmentURI, group, partialDuration)
			}
		}
	}

	return sb.String(), nil
}

// groupFragmentsByGOP splits a contiguous run of fragments into GOP-aligned
// sub-segment groups. A new group begins at every INDEPENDENT fragment (a
// keyframe boundary); following non-independent fragments accumulate into the
// current group until the next INDEPENDENT fragment or end of input. If the
// first fragment is not independent (e.g. mid-GOP at a window edge), it seeds
// the first group so no fragment is dropped.
func groupFragmentsByGOP(fragments []parser.ByteRange) [][]parser.ByteRange {
	var groups [][]parser.ByteRange
	var current []parser.ByteRange
	for _, fragment := range fragments {
		if fragment.Independent && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, fragment)
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// writeSubSegmentTail emits the #EXT-X-BYTERANGE + #EXTINF + URI lines that
// close one GOP-aligned sub-segment. The byterange spans the contiguous run of
// the group's fragments (length = sum of fragment lengths, offset = first
// fragment offset); the EXTINF duration = sum of the group's part durations
// (≈ len(group) × partialDuration).
func writeSubSegmentTail(sb *strings.Builder, segmentURI string, group []parser.ByteRange, partialDuration float64) {
	if len(group) == 0 {
		return
	}
	totalLength := 0
	for _, fragment := range group {
		totalLength += fragment.Length
	}
	firstOffset := group[0].Offset
	groupDuration := float64(len(group)) * partialDuration

	sb.WriteString(fmt.Sprintf("#EXT-X-BYTERANGE:%d@%d\n", totalLength, firstOffset))
	sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", groupDuration))
	sb.WriteString(segmentURI + "\n")
}

// writeCompleteSegmentSubSegments emits all GOP-aligned sub-segments for a
// fully-available underlying segment: every part, then a closing
// #EXT-X-BYTERANGE + #EXTINF per sub-segment group.
func writeCompleteSegmentSubSegments(sb *strings.Builder, segmentURI string, fragments []parser.ByteRange, partialDuration float64) {
	for _, group := range groupFragmentsByGOP(fragments) {
		for _, fragment := range group {
			partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
				partialDuration, segmentURI, fragment.Length, fragment.Offset)
			if fragment.Independent {
				partTag += ",INDEPENDENT=YES"
			}
			sb.WriteString(partTag + "\n")
		}
		writeSubSegmentTail(sb, segmentURI, group, partialDuration)
	}
}

// computeSubSegmentTargetDuration returns the integer ceiling of the largest
// GOP-aligned sub-segment duration across the playlist's segments. This is the
// EXT-X-TARGETDURATION for the sub-segmented LL playlist (≈1 for ~1s GOPs).
// Falls back to the segment duration if no fragment byteranges are available.
func computeSubSegmentTargetDuration(pl *playlist.Media, byteranges map[string][]parser.ByteRange, partialDuration float64, segmentDuration float64) int {
	maxGroupDuration := 0.0
	if pl != nil {
		for _, seg := range pl.Segments {
			if seg == nil {
				continue
			}
			for _, group := range groupFragmentsByGOP(byteranges[seg.URI]) {
				if d := float64(len(group)) * partialDuration; d > maxGroupDuration {
					maxGroupDuration = d
				}
			}
		}
	}
	if maxGroupDuration <= 0 {
		// No byterange data — preserve prior behaviour (whole-segment EXTINF).
		return int(segmentDuration)
	}
	return int(math.Ceil(maxGroupDuration))
}

func joinURI(relPath, uri string) string {
	if uri == "" {
		return ""
	}
	if strings.Contains(uri, "/") {
		parts := strings.Split(uri, "/")
		return parts[len(parts)-1]
	}
	return uri
}

func baseURI(uri string) string {
	return joinURI("", uri)
}

// detectContentCharacteristics auto-detects segment duration, fragment count, and partial duration
func detectContentCharacteristics(pl *playlist.Media, byteranges map[string][]parser.ByteRange) (float64, int, float64) {
	const DEFAULT_SEGMENT_DURATION = 6.0
	const DEFAULT_PARTIALS_PER_SEGMENT = 6
	const DEFAULT_PARTIAL_DURATION = 1.0

	// Get first segment duration from playlist
	if pl == nil || len(pl.Segments) == 0 || pl.Segments[0] == nil {
		fmt.Fprintf(os.Stderr, "WARN: No segments found, using defaults\n")
		return DEFAULT_SEGMENT_DURATION, DEFAULT_PARTIALS_PER_SEGMENT, DEFAULT_PARTIAL_DURATION
	}

	firstSegment := pl.Segments[0]
	segmentDuration := firstSegment.Duration.Seconds()

	// Get fragment count from byteranges
	fragments := byteranges[firstSegment.URI]
	fragmentCount := len(fragments)

	if fragmentCount == 0 {
		fmt.Fprintf(os.Stderr, "WARN: No fragments found for %s, using defaults\n", firstSegment.URI)
		return DEFAULT_SEGMENT_DURATION, DEFAULT_PARTIALS_PER_SEGMENT, DEFAULT_PARTIAL_DURATION
	}

	// Calculate partial duration
	partialDuration := segmentDuration / float64(fragmentCount)

	logOncePerSecondLL(fmt.Sprintf("Content characteristics detected:\n  Segment duration: %.3fs\n  Fragments per segment: %d\n  Partial duration: %.3fs",
		segmentDuration, fragmentCount, partialDuration))

	return segmentDuration, fragmentCount, partialDuration
}

// countSegments counts non-nil segments in playlist
func countSegments(pl *playlist.Media) int {
	count := 0
	for _, seg := range pl.Segments {
		if seg != nil {
			count++
		}
	}
	return count
}

// getSegment safely gets a segment by index
func getSegment(pl *playlist.Media, idx int) *playlist.MediaSegment {
	if idx < 0 || idx >= len(pl.Segments) {
		return nil
	}
	return pl.Segments[idx]
}
