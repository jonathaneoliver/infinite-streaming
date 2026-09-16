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
	// Fragments holds the underlying ~200ms fragment byteranges that make up
	// this 1s segment, in order. Populated only on the byterange path (the
	// only path that has fragment-level data). Consumed by the LL-HLS variant
	// to emit one EXT-X-PART tag per fragment; the non-partials 2s/6s/1s path
	// ignores it, so this field is purely additive.
	Fragments []parser.ByteRange
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

	return generateVariantPlaylistCore(segments, totalDuration, segmentMap, relPath, timeNow, loopCount, minDuration, syncTotalDuration, syncTimeOffset, variantPlaylistOptions{
		emitPartials: false,
		useByterange: useByterange,
		renderURI: func(uri string) string {
			return absoluteSegmentURI(prefix, content, joinRelPath(relPath, uri))
		},
		loopMarker: "RANGE",
	})
}

// variantPlaylistOptions captures the per-variant differences between the
// non-partials range playlists (2s/6s/1s) and the LL-HLS playlist. Everything
// else — the flat-segment list, the MEDIA-SEQUENCE / DISCONTINUITY-SEQUENCE /
// sliding-window / loop-wrap math, and the PDT — is shared in
// generateVariantPlaylistCore so the LL playlist is structurally identical to
// the 1s range playlist (same `len(segments)`-derived sequence) PLUS partials.
type variantPlaylistOptions struct {
	// emitPartials turns on the LL-HLS behaviour: an EXT-X-PART tag per
	// underlying fragment before each segment's EXTINF, the LL-only header
	// tags (EXT-X-PART-INF, PART-HOLD-BACK), and the live-edge tail of bare
	// EXT-X-PART lines for the still-incomplete current segment.
	emitPartials bool
	// useByterange controls whether the closed segments carry an
	// EXT-X-BYTERANGE line (only the byterange path — targetSeconds < 6 — has
	// per-segment offsets).
	useByterange bool
	// renderURI maps a parsed segment/init URI to the form the playlist emits
	// (absolute prefix/content path for range, bare filename for LL).
	renderURI func(string) string
	// partTarget / partHoldBack are only consulted when emitPartials is true.
	partTarget   float64
	partHoldBack float64
	// loopMarker tags the once-per-second loop-boundary log line (RANGE / LL).
	loopMarker string
}

// generateVariantPlaylistCore builds a media playlist from a flat list of 1s
// segments. The MEDIA-SEQUENCE / DISCONTINUITY-SEQUENCE / window / wrap math is
// derived from len(segments) — the flat sub-segment count — so a given segment
// keeps a stable, monotonic sequence number across reloads (the drift bug that
// the old underlying-segment counting LL path suffered from is impossible here
// by construction). opts.emitPartials adds the EXT-X-PART tags + LL header on
// top of the otherwise-identical structure.
func generateVariantPlaylistCore(
	segments []rangeSegment,
	totalDuration float64,
	segmentMap string,
	relPath string,
	timeNow float64,
	loopCount int,
	minDuration float64,
	syncTotalDuration float64,
	syncTimeOffset float64,
	opts variantPlaylistOptions,
) (string, error) {
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
	if opts.emitPartials {
		sb.WriteString("#EXT-X-VERSION:10\n") // Version 10 for LL-HLS
	} else {
		// Version 9 is required because the playlist emits EXT-X-SERVER-CONTROL
		// (below). AVPlayer rejects v7 playlists that carry v9-era tags with
		// -12646 "playlist parse error".
		sb.WriteString("#EXT-X-VERSION:9\n")
	}
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
	// TARGETDURATION is the integer ceiling (2 for 1.001s sub-segments), not
	// the exact segment duration, so using 3× maxSegDuration would fall below
	// the minimum and AVPlayer rejects with -12646 "playlist parse error".
	// Explicit declaration is preferred over relying on player defaults; other
	// players (hls.js, ExoPlayer, Shaka) may default differently.
	targetDurationSecs := int(math.Ceil(maxSegDuration))
	liveHoldBack := 3.0 * float64(targetDurationSecs)
	if opts.emitPartials {
		// LL-HLS header: PART-INF advertises the partial target, and the
		// SERVER-CONTROL line carries PART-HOLD-BACK in addition to HOLD-BACK.
		sb.WriteString(fmt.Sprintf("#EXT-X-PART-INF:PART-TARGET=%.3f\n", opts.partTarget))
		sb.WriteString(fmt.Sprintf("#EXT-X-SERVER-CONTROL:HOLD-BACK=%.3f,PART-HOLD-BACK=%.1f\n", liveHoldBack, opts.partHoldBack))
	} else {
		sb.WriteString(fmt.Sprintf("#EXT-X-SERVER-CONTROL:HOLD-BACK=%.3f\n", liveHoldBack))
	}
	// EXT-X-START in the variant matters because hls.js (and some other
	// players) only honor TIME-OFFSET in the *media* playlist; the master
	// inheritance is widely under-implemented. Without this, hls.js parks at
	// the oldest segment in the sliding window and never seeks to live —
	// the player ends up ~MAX_LIVE_WINDOW_DURATION seconds behind real time.
	// Match HOLD-BACK so the seek target and steady-state target agree — for BOTH
	// the range rungs and LL. We previously joined LL at PART-HOLD-BACK (closer to
	// live), but without CAN-BLOCK-RELOAD AVPlayer cannot sustain its buffer riding
	// the parts that close and rebuffers; joining at HOLD-BACK (the complete-segment
	// edge, same as the partial-less 1s rung) keeps the parts available without
	// yanking the player into the zone it stalls in.
	sb.WriteString(fmt.Sprintf("#EXT-X-START:TIME-OFFSET=-%.3f,PRECISE=YES\n", liveHoldBack))
	sb.WriteString(fmt.Sprintf("#EXT-X-PROGRAM-DATE-TIME:%s\n", pdt.Format("2006-01-02T15:04:05.000Z")))

	if segmentMap != "" {
		sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", opts.renderURI(joinRelPath(relPath, segmentMap))))
	}

	// writeSegment emits one CLOSED segment: when emitPartials is on, the
	// segment's EXT-X-PART fragment tags precede its EXT-X-BYTERANGE / EXTINF /
	// URI; otherwise it is exactly the non-partials range output.
	writeSegment := func(seg rangeSegment) {
		if opts.emitPartials {
			uri := opts.renderURI(joinRelPath(relPath, seg.URI))
			for _, frag := range seg.Fragments {
				partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
					opts.partTarget, uri, frag.Length, frag.Offset)
				if frag.Independent {
					partTag += ",INDEPENDENT=YES"
				}
				sb.WriteString(partTag + "\n")
			}
		}
		sb.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", seg.Duration))
		if opts.useByterange && seg.Length > 0 {
			sb.WriteString(fmt.Sprintf("#EXT-X-BYTERANGE:%d@%d\n", seg.Length, seg.Offset))
		}
		sb.WriteString(opts.renderURI(joinRelPath(relPath, seg.URI)) + "\n")
	}

	if wrap {
		tailDuration := 0.0
		for i := windowStartIdx; i < len(segments); i++ {
			writeSegment(segments[i])
			tailDuration += segments[i].Duration
		}
		boundaryAt := pdt.Add(time.Duration(tailDuration * float64(time.Second)))
		writeRangeLoopDateRange(&sb, loopCount, boundaryAt, opts.loopMarker)
		sb.WriteString("#EXT-X-DISCONTINUITY\n")
		if segmentMap != "" {
			sb.WriteString(fmt.Sprintf("#EXT-X-MAP:URI=\"%s\"\n", opts.renderURI(joinRelPath(relPath, segmentMap))))
		}
		remainingDuration := MAX_LIVE_WINDOW_DURATION - tailDuration
		for i := 0; i <= availableIdx && remainingDuration > 0; i++ {
			writeSegment(segments[i])
			remainingDuration -= segments[i].Duration
		}
	} else {
		windowDuration := 0.0
		for i := windowStartIdx; i <= availableIdx && windowDuration < MAX_LIVE_WINDOW_DURATION; i++ {
			writeSegment(segments[i])
			windowDuration += segments[i].Duration
		}
	}

	// LL live-edge tail: the current still-incomplete segment is emitted as
	// bare EXT-X-PART lines with NO #EXTINF, exactly as the old LL path did at
	// the live edge. availableIdx is the last CLOSED segment that was written
	// above; currentIdx is the live one. Emitting it only when
	// currentIdx == availableIdx+1 guarantees it was NOT already written as a
	// closed segment (no duplication) — and this holds in both the wrap and
	// non-wrap branches, since in both the closed loops stop at availableIdx.
	if opts.emitPartials && currentIdx == availableIdx+1 && currentIdx >= 0 && currentIdx < len(segments) {
		live := segments[currentIdx]
		uri := opts.renderURI(joinRelPath(relPath, live.URI))
		for _, frag := range live.Fragments {
			partTag := fmt.Sprintf("#EXT-X-PART:DURATION=%.3f,URI=\"%s\",BYTERANGE=\"%d@%d\"",
				opts.partTarget, uri, frag.Length, frag.Offset)
			if frag.Independent {
				partTag += ",INDEPENDENT=YES"
			}
			sb.WriteString(partTag + "\n")
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
				URI:       seg.URI,
				Offset:    group[0].Offset,
				Length:    length,
				Duration:  duration,
				Fragments: group,
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
