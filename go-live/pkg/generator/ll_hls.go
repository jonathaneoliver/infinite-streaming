package generator

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/pkg/playlist"
	"github.com/boss/go-live/pkg/parser"
)

const (
	MAX_LIVE_WINDOW_DURATION = 36.0 // 36 seconds
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

// LLHLSGenerator generates LL-HLS playlists matching Python ll_live.py behavior
type LLHLSGenerator struct{}

// GenerateVariantPlaylist generates a single variant playlist with byte-range partials
// This matches the Python generate_ll_playlist() function exactly
func (g *LLHLSGenerator) GenerateVariantPlaylist(
	pl *playlist.Media,
	byteranges map[string][]parser.ByteRange,
	relPath string,
	segmentMap string,
	timeNow float64,
	minDuration float64,
	maxDuration float64,
) (string, error) {

	// Auto-detect content characteristics from first segment
	segmentDuration, partialsPerSegment, partialDuration := detectContentCharacteristics(pl, byteranges)

	// Calculate live position
	timeOffset := math.Mod(timeNow, minDuration)
	loopCount := int(timeNow / minDuration)

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

	// Build playlist
	var sb strings.Builder

	// LL-HLS playlist header
	sb.WriteString("#EXTM3U\n")
	sb.WriteString("#EXT-X-VERSION:10\n") // Version 10 for LL-HLS
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(segmentDuration)))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", sequence))

	// LL-HLS specific tags
	sb.WriteString(fmt.Sprintf("#EXT-X-PART-INF:PART-TARGET=%.3f\n", partialDuration))
	sb.WriteString("#EXT-X-SERVER-CONTROL:PART-HOLD-BACK=3.0\n")

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

			// Write all partials for this complete segment
			for _, fragment := range fragments {
				partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
					partialDuration, segmentURI, fragment.Length, fragment.Offset)

				if fragment.Independent {
					partTag += ",INDEPENDENT=YES"
				}

				sb.WriteString(partTag + "\n")
			}

			sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", segment.Duration.Seconds()))
			sb.WriteString(segmentURI + "\n")

			accumulatedDuration += segment.Duration.Seconds()
		}

		// Add discontinuity before switching from previous loop to current loop segments
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

		// Write all partials for this complete segment
		for _, fragment := range fragments {
			partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
				partialDuration, segmentURI, fragment.Length, fragment.Offset)

			if fragment.Independent {
				partTag += ",INDEPENDENT=YES"
			}

			sb.WriteString(partTag + "\n")
		}

		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", segment.Duration.Seconds()))
		sb.WriteString(segmentURI + "\n")

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

		// For current segment: show partials from 0 up to current_partial_idx (inclusive)
		for partialIdx := 0; partialIdx <= currentPartialIdx && partialIdx < len(fragments); partialIdx++ {
			fragment := fragments[partialIdx]
			partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
				partialDuration, segmentURI, fragment.Length, fragment.Offset)

			if fragment.Independent {
				partTag += ",INDEPENDENT=YES"
			}

			sb.WriteString(partTag + "\n")
		}

		// Only write EXTINF if current segment is complete (all partials available)
		if currentPartialIdx >= len(fragments)-1 {
			sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", currentSegment.Duration.Seconds()))
			sb.WriteString(segmentURI + "\n")
		}
	}

	return sb.String(), nil
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
// Matches Python detect_content_characteristics()
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
