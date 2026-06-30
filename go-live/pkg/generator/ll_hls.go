package generator

import (
	"fmt"
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

// GenerateVariantPlaylist generates a single LL-HLS variant playlist.
//
// The LL playlist is, by construction, the 1s range playlist (the `s1` rung
// built by buildRangeSegments with targetSeconds=1) PLUS per-segment
// EXT-X-PART tags and the LL-only header. It shares ALL of the
// MEDIA-SEQUENCE / DISCONTINUITY-SEQUENCE / sliding-window / loop-wrap math
// with the range generator via generateVariantPlaylistCore, so the sequence
// number is derived from len(segments) — the flat 1s-segment count — and can
// never drift (the historical bug where the underlying-6s-segment count was
// used for MEDIA-SEQUENCE while ~6× as many EXTINFs aged out of the window).
func (g *LLHLSGenerator) GenerateVariantPlaylist(
	pl *playlist.Media,
	byteranges map[string][]parser.ByteRange,
	relPath string,
	segmentMap string,
	timeNow float64,
	loopCount int,
	minDuration float64,
	maxDuration float64,
	syncTotalDuration float64,
	syncTimeOffset float64,
) (string, error) {
	_ = maxDuration

	// PART-TARGET (the ~200ms partial duration). Detected from the first
	// underlying segment's fragment count, matching prior behaviour.
	_, _, partialDuration := detectContentCharacteristics(pl, byteranges)

	// Build the SAME flat 1s segments the s1 range rung uses (group ~5
	// fragments → one 1s segment), so the LL's complete segments are
	// byte-identical to s1. Each rangeSegment now also carries its Fragments,
	// which the core emits as EXT-X-PART tags.
	const llTargetSeconds = 1.0
	segments, totalDuration, err := buildRangeSegments(pl, byteranges, llTargetSeconds, true)
	if err != nil {
		return "", err
	}

	return generateVariantPlaylistCore(segments, totalDuration, segmentMap, relPath, timeNow, loopCount, minDuration, syncTotalDuration, syncTimeOffset, variantPlaylistOptions{
		emitPartials: true,
		useByterange: true,
		renderURI: func(uri string) string {
			return baseURI(uri)
		},
		partTarget:   partialDuration,
		partHoldBack: 3.0,
		loopMarker:   "LL",
	})
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
