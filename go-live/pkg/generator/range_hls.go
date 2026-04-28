package generator

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gohlslib/pkg/playlist"
	"github.com/jonathaneoliver/infinite-streaming/go-live/pkg/parser"
)

type rangeSegment struct {
	URI      string
	Offset   int
	Length   int
	Duration float64
}

// RangeHLSGenerator generates virtual 2s/6s playlists without partials.
type RangeHLSGenerator struct{}

var (
	logOnceMuRange     sync.Mutex
	lastLogSecondRange int64
)

func logOncePerSecondRange(message string) {
	now := time.Now().Unix()
	logOnceMuRange.Lock()
	defer logOnceMuRange.Unlock()
	if lastLogSecondRange == now {
		return
	}
	lastLogSecondRange = now
	fmt.Fprintln(os.Stderr, message)
}

func writeRangeLoopDateRange(sb *strings.Builder, loopCount int, at time.Time, marker string) {
	timestamp := at.UTC().Format("2006-01-02T15:04:05.000Z")
	// Loop boundary marker for downstream analytics and loop counters.
	// sb.WriteString(fmt.Sprintf("#EXT-X-DATERANGE:ID=\"loop-%d-%s\",CLASS=\"com.infinite.loop\",START-DATE=\"%s\",X-LOOP-COUNT=\"%d\"\n", loopCount, marker, timestamp, loopCount))
	logOncePerSecondRange(fmt.Sprintf("[GO-LIVE:LOOP][RANGE] marker=%s loop_count=%d start_date=%s", marker, loopCount, timestamp))
}

func (g *RangeHLSGenerator) GenerateVariantPlaylist(
	pl *playlist.Media,
	byteranges map[string][]parser.ByteRange,
	relPath string,
	segmentMap string,
	timeNow float64,
	loopCount int,
	minDuration float64,
	maxDuration float64,
	targetDuration string,
	prefix string,
	content string,
	syncTotalDuration float64,
	syncTimeOffset float64,
) (string, error) {
	targetSeconds, err := parseDurationSeconds(targetDuration)
	if err != nil {
		return "", err
	}
	_ = maxDuration
	useByterange := targetSeconds < 6.0

	segments, totalDuration, err := buildRangeSegments(pl, byteranges, targetSeconds, useByterange)
	if err != nil {
		return "", err
	}
	if totalDuration <= 0 && minDuration > 0 {
		totalDuration = minDuration
	}
	if len(segments) == 0 || totalDuration <= 0 {
		return "", fmt.Errorf("no virtual segments available")
	}

	// Playlist position remains absolute-time based.
	timeOffset := math.Mod(timeNow, totalDuration)
	if syncTotalDuration > 0 {
		timeOffset = math.Mod(syncTimeOffset, syncTotalDuration)
	}

	var currentIdx, availableIdx, windowStartIdx, startLoop int
	var windowStartTime float64
	wrap := false
	if syncTotalDuration > 0 {
		idealSegmentDuration := syncTotalDuration / float64(len(segments))
		if idealSegmentDuration <= 0 {
			idealSegmentDuration = totalDuration / float64(len(segments))
		}
		currentIdx = int(timeOffset / idealSegmentDuration)
		if currentIdx >= len(segments) {
			currentIdx = len(segments) - 1
		}
		availableIdx = currentIdx - 1
		if availableIdx < 0 {
			availableIdx = len(segments) - 1
		}

		windowStartTime = timeOffset - MAX_LIVE_WINDOW_DURATION
		if windowStartTime < 0 {
			windowStartTime += syncTotalDuration
			wrap = true
		}
		windowStartIdx = int(windowStartTime / idealSegmentDuration)
		if windowStartIdx < 0 {
			windowStartIdx = 0
		}
		if windowStartIdx >= len(segments) {
			windowStartIdx = len(segments) - 1
		}

		startLoop = loopCount
		if wrap {
			startLoop = loopCount - 1
		}
		if startLoop < 0 {
			startLoop = 0
		}
	} else {
		segmentStarts := make([]float64, len(segments))
		accum := 0.0
		for i, seg := range segments {
			segmentStarts[i] = accum
			accum += seg.Duration
		}

		currentIdx = findSegmentIndex(segmentStarts, segments, timeOffset)
		availableIdx = currentIdx
		if currentIdx >= 0 && currentIdx < len(segments) {
			if timeOffset < segmentStarts[currentIdx]+segments[currentIdx].Duration {
				availableIdx = currentIdx - 1
			}
		}
		if availableIdx < 0 {
			availableIdx = len(segments) - 1
		}

		windowStartTime = timeOffset - MAX_LIVE_WINDOW_DURATION
		if windowStartTime < 0 {
			windowStartTime += totalDuration
			wrap = true
		}
		windowStartIdx = findSegmentIndex(segmentStarts, segments, windowStartTime)
		if windowStartIdx < 0 {
			windowStartIdx = 0
		}

		startLoop = loopCount
		if wrap {
			startLoop = loopCount - 1
		}
		if startLoop < 0 {
			startLoop = 0
		}
	}

	firstSeq := startLoop*len(segments) + windowStartIdx

	maxSegDuration := maxDurationInWindow(segments)

	// Program Date Time for the first segment in the sliding window.
	windowElapsed := timeOffset - windowStartTime
	if windowElapsed < 0 {
		if syncTotalDuration > 0 {
			windowElapsed += syncTotalDuration
		} else {
			windowElapsed += totalDuration
		}
	}
	pdtSeconds := timeNow - windowElapsed
	pdt := time.Unix(int64(pdtSeconds), int64((pdtSeconds-float64(int64(pdtSeconds)))*1e9)).UTC()

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	// Version 9 is required because the playlist emits EXT-X-SERVER-CONTROL
	// (below). AVPlayer rejects v7 playlists that carry v9-era tags with
	// -12646 "playlist parse error".
	sb.WriteString("#EXT-X-VERSION:9\n")
	sb.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(maxSegDuration))))
	sb.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", firstSeq))
	// EXT-X-DISCONTINUITY-SEQUENCE is REQUIRED by RFC 8216 §4.3.3.3 whenever
	// the playlist contains an EXT-X-DISCONTINUITY tag (see `wrap` path
	// below). It allows the player to synchronize across variant switches
	// over discontinuity boundaries; its absence causes AVPlayer to emit
	// -12642 "No matching mediaFile found from playlist" when probing a
	// higher variant during ABR evaluation. startLoop is the loop number of
	// the first segment in this window — i.e. the count of discontinuities
	// that have occurred before that first segment.
	sb.WriteString(fmt.Sprintf("#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", startLoop))
	// EXT-X-SERVER-CONTROL HOLD-BACK is a media-playlist-only tag (RFC 8216
	// §4.3.3.8). HLS spec requires HOLD-BACK >= 3× TARGETDURATION — note
	// TARGETDURATION is the integer ceiling (7 for 6.006s segments), not the
	// exact segment duration, so using 3× maxSegDuration would fall below
	// the minimum (18.018 < 21) and AVPlayer rejects with -12646 "playlist
	// parse error". Explicit declaration is preferred over relying on
	// player defaults; other players (hls.js, ExoPlayer, Shaka) may default
	// differently.
	targetDurationSecs := int(math.Ceil(maxSegDuration))
	liveHoldBack := 3.0 * float64(targetDurationSecs)
	sb.WriteString(fmt.Sprintf("#EXT-X-SERVER-CONTROL:HOLD-BACK=%.3f\n", liveHoldBack))
	// EXT-X-START in the variant matters because hls.js (and some other
	// players) only honor TIME-OFFSET in the *media* playlist; the master
	// inheritance is widely under-implemented. Without this, hls.js parks at
	// the oldest segment in the sliding window and never seeks to live —
	// the player ends up ~MAX_LIVE_WINDOW_DURATION seconds behind real time.
	// Match HOLD-BACK so the seek target and steady-state target agree.
	sb.WriteString(fmt.Sprintf("#EXT-X-START:TIME-OFFSET=-%.3f,PRECISE=YES\n", liveHoldBack))
	sb.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", pdt.Format("2006-01-02T15:04:05.000Z")))

	if segmentMap != "" {
		sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", absoluteSegmentURI(prefix, content, joinRelPath(relPath, segmentMap))))
	}

	writeSegmentRange := func(seg rangeSegment) {
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.Duration))
		if useByterange && seg.Length > 0 {
			sb.WriteString(fmt.Sprintf("#EXT-X-BYTERANGE:%d@%d\n", seg.Length, seg.Offset))
		}
		sb.WriteString(absoluteSegmentURI(prefix, content, joinRelPath(relPath, seg.URI)) + "\n")
	}

	if wrap {
		tailDuration := 0.0
		for i := windowStartIdx; i < len(segments); i++ {
			writeSegmentRange(segments[i])
			tailDuration += segments[i].Duration
		}
		boundaryAt := pdt.Add(time.Duration(tailDuration * float64(time.Second)))
		writeRangeLoopDateRange(&sb, loopCount, boundaryAt, "wrap")
		sb.WriteString("#EXT-X-DISCONTINUITY\n")
		if segmentMap != "" {
			sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", absoluteSegmentURI(prefix, content, joinRelPath(relPath, segmentMap))))
		}
		remainingDuration := MAX_LIVE_WINDOW_DURATION - tailDuration
		for i := 0; i <= availableIdx && remainingDuration > 0; i++ {
			writeSegmentRange(segments[i])
			remainingDuration -= segments[i].Duration
		}
	} else {
		windowDuration := 0.0
		for i := windowStartIdx; i <= availableIdx && windowDuration < MAX_LIVE_WINDOW_DURATION; i++ {
			writeSegmentRange(segments[i])
			windowDuration += segments[i].Duration
		}
	}

	result := sb.String()
	if len(result) > 5000 {
		fmt.Fprintf(os.Stderr, "WARNING: large range playlist (%d bytes) wrap=%t tailCount=%d availableIdx=%d segments=%d\n",
			len(result), wrap, len(segments)-windowStartIdx, availableIdx, len(segments))
	}
	return result, nil
}

func parseDurationSeconds(raw string) (float64, error) {
	val := strings.TrimSuffix(raw, "s")
	parsed, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("invalid duration: %s", raw)
	}
	return float64(parsed), nil
}

func buildRangeSegments(pl *playlist.Media, byteranges map[string][]parser.ByteRange, targetSeconds float64, useByterange bool) ([]rangeSegment, float64, error) {
	if pl == nil || len(pl.Segments) == 0 {
		return nil, 0, fmt.Errorf("playlist missing segments")
	}

	var segments []rangeSegment
	totalDuration := 0.0

	for _, seg := range pl.Segments {
		if seg == nil {
			continue
		}
		fragments := byteranges[seg.URI]
		if !useByterange || len(fragments) == 0 {
			segments = append(segments, rangeSegment{
				URI:      seg.URI,
				Offset:   0,
				Length:   0,
				Duration: seg.Duration.Seconds(),
			})
			totalDuration += seg.Duration.Seconds()
			continue
		}

		segmentDuration := seg.Duration.Seconds()
		fragmentDuration := segmentDuration / float64(len(fragments))
		groupSize := int(math.Round(targetSeconds / fragmentDuration))
		if groupSize < 1 {
			groupSize = 1
		}

		for i := 0; i < len(fragments); i += groupSize {
			end := i + groupSize
			if end > len(fragments) {
				end = len(fragments)
			}
			group := fragments[i:end]
			if len(group) == 0 {
				continue
			}
			length := 0
			for _, frag := range group {
				length += frag.Length
			}
			duration := fragmentDuration * float64(len(group))
			segments = append(segments, rangeSegment{
				URI:      seg.URI,
				Offset:   group[0].Offset,
				Length:   length,
				Duration: duration,
			})
			totalDuration += duration
		}
	}

	return segments, totalDuration, nil
}

func findSegmentIndex(starts []float64, segments []rangeSegment, timeOffset float64) int {
	for i := len(starts) - 1; i >= 0; i-- {
		if timeOffset >= starts[i] {
			return i
		}
	}
	return 0
}

func maxDurationInWindow(segments []rangeSegment) float64 {
	maxDur := 0.0
	for _, seg := range segments {
		if seg.Duration > maxDur {
			maxDur = seg.Duration
		}
	}
	return maxDur
}

func absoluteSegmentURI(prefix, content, uri string) string {
	if uri == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(uri, "/")
	return fmt.Sprintf("%s/%s/%s", prefix, content, trimmed)
}

func joinRelPath(relPath, uri string) string {
	if uri == "" {
		return ""
	}
	if strings.Contains(uri, "/") || relPath == "" {
		return uri
	}
	return fmt.Sprintf("%s/%s", relPath, uri)
}
