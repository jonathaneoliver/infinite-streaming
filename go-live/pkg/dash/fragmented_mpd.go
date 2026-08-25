package dash

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/beevik/etree"
)

// fragmentedMPDName is the sibling manifest the encoder emits alongside
// manifest.mpd. It describes the same content at fragment granularity: one
// <SegmentURL> per fragment carrying @mediaRange, rather than one per segment.
const fragmentedMPDName = "manifest_fragmented.mpd"

// segmentMPDName is the manifest go-live always loads, whatever granularity it
// turns out to be written at. Granularity is detected from content — whether
// its <SegmentURL> entries carry @mediaRange — never from the filename, so one
// name serves both shapes and the serving path needs no special case.
const segmentMPDName = "manifest.mpd"

// fragmentRanges maps a segment's @media value (as it appears in manifest.mpd,
// e.g. "1080p/segment_00001.m4s") to that segment's fragments in playback order.
type fragmentRanges map[string][]byterangePayloadFragment

var (
	fragmentedCacheMu sync.RWMutex
	// fragmentedCache holds one parsed fragmented MPD per content directory. A
	// nil value is a negative result: we looked and there was nothing usable, so
	// don't re-read the file on every segment of every request.
	fragmentedCache = make(map[string]fragmentRanges)

	// noRangeSourceWarned keeps the "no fragment data at all" warning to one line
	// per content instead of one per segment per manifest generation.
	noRangeSourceWarned sync.Map
)

// normalizeFragmentedMPD collapses a fragment-granularity MPD in place to the
// segment-granularity form the rest of this package expects, returning the
// fragment byte-ranges it stripped out.
//
// This is the DASH equivalent of what the HLS side gets for free. A
// playlist.m3u8 marks both levels explicitly — EXTINF for segments,
// EXT-X-PART for fragments — so one file drives every variant. A fragmented
// MPD marks only the fragment level; segment boundaries are implicit in which
// @media each <SegmentURL> points at. Grouping by @media recovers them exactly,
// after which manifest_fragmented.mpd carries everything manifest.mpd does and
// go-live can generate 1s/2s/6s/LL from the single file.
//
// Returns (nil, false) when the document is already segment-granularity, so
// legacy content flows through untouched.
func normalizeFragmentedMPD(root *etree.Element) (fragmentRanges, bool) {
	ranges := make(fragmentRanges)
	collapsed := false

	for _, rep := range findAllByLocal(root, "Representation") {
		segList := findFirstByLocal(rep, "SegmentList")
		if segList == nil {
			continue
		}
		urls := findAllByLocal(segList, "SegmentURL")
		if len(urls) == 0 || !anyHasMediaRange(urls) {
			// Already segment-granularity — nothing to collapse.
			continue
		}

		timeline := findFirstByLocal(segList, "SegmentTimeline")
		durations := expandTimelineDurations(timeline)
		if timeline != nil && len(durations) != len(urls) {
			// The timeline and the URL list disagree about how many fragments
			// exist. Collapsing on a guess would silently shift every segment
			// boundary, so leave this representation alone and let the caller
			// fall back to the segment-granularity manifest.
			fmt.Fprintf(os.Stderr,
				"WARN: representation %q has %d SegmentURL entries but %d timeline entries; not collapsing\n",
				rep.SelectAttrValue("id", "?"), len(urls), len(durations))
			return nil, false
		}

		groups := groupFragmentsByMedia(urls, durations)
		if len(groups) == 0 {
			continue
		}
		for _, g := range groups {
			if len(g.fragments) > 0 {
				ranges[g.media] = append(ranges[g.media], g.fragments...)
			}
		}
		rewriteSegmentList(segList, timeline, groups)
		collapsed = true
	}

	if !collapsed {
		return nil, false
	}
	return ranges, true
}

// mediaGroup is the set of consecutive fragments belonging to one segment file.
type mediaGroup struct {
	media         string
	durationTicks int64
	fragments     []byterangePayloadFragment
}

func anyHasMediaRange(urls []*etree.Element) bool {
	for _, u := range urls {
		if u.SelectAttrValue("mediaRange", "") != "" {
			return true
		}
	}
	return false
}

// expandTimelineDurations flattens <S d=... r=.../> runs into one duration per
// entry, matching the order of the SegmentURL list.
func expandTimelineDurations(timeline *etree.Element) []int64 {
	if timeline == nil {
		return nil
	}
	var out []int64
	for _, s := range findAllByLocal(timeline, "S") {
		d := parseInt64(s.SelectAttrValue("d", "0"), 0)
		for i := int64(0); i <= parseInt64(s.SelectAttrValue("r", "0"), 0); i++ {
			out = append(out, d)
		}
	}
	return out
}

// groupFragmentsByMedia walks the fragment list in document order and starts a
// new group each time @media changes. Document order is playback order, so
// consecutive runs are exactly the segments.
func groupFragmentsByMedia(urls []*etree.Element, durations []int64) []mediaGroup {
	var groups []mediaGroup
	for i, u := range urls {
		media := normalizeMediaKey(u.SelectAttrValue("media", ""))
		if media == "" {
			continue
		}
		if len(groups) == 0 || groups[len(groups)-1].media != media {
			groups = append(groups, mediaGroup{media: media})
		}
		g := &groups[len(groups)-1]
		if i < len(durations) {
			g.durationTicks += durations[i]
		}
		if mr := u.SelectAttrValue("mediaRange", ""); mr != "" {
			if offset, length, err := parseMediaRange(mr); err == nil {
				g.fragments = append(g.fragments, byterangePayloadFragment{
					Offset: offset,
					Length: length,
				})
			}
		}
	}
	return groups
}

// rewriteSegmentList replaces the fragment-level SegmentURL and SegmentTimeline
// children with their segment-level equivalents, producing the same shape the
// encoder writes into manifest.mpd.
func rewriteSegmentList(segList, timeline *etree.Element, groups []mediaGroup) {
	for _, u := range findAllByLocal(segList, "SegmentURL") {
		if parent := u.Parent(); parent != nil {
			parent.RemoveChild(u)
		}
	}
	for _, g := range groups {
		segURL := segList.CreateElement("SegmentURL")
		segURL.CreateAttr("media", g.media)
	}

	if timeline == nil {
		return
	}
	for _, s := range findAllByLocal(timeline, "S") {
		if parent := s.Parent(); parent != nil {
			parent.RemoveChild(s)
		}
	}
	// Run-length encode equal-duration runs, and stamp @t on the first entry,
	// exactly as a segment-granularity manifest would.
	start := int64(0)
	for i := 0; i < len(groups); {
		j := i
		for j+1 < len(groups) && groups[j+1].durationTicks == groups[i].durationTicks {
			j++
		}
		s := timeline.CreateElement("S")
		if i == 0 {
			s.CreateAttr("t", strconv.FormatInt(start, 10))
		}
		s.CreateAttr("d", strconv.FormatInt(groups[i].durationTicks, 10))
		if j > i {
			s.CreateAttr("r", strconv.Itoa(j-i))
		}
		for k := i; k <= j; k++ {
			start += groups[k].durationTicks
		}
		i = j + 1
	}
}

// loadFragmentedRanges returns the fragment byte-ranges declared in the
// manifest_fragmented.mpd sitting next to mpdPath, or nil if that file is
// absent or carries no usable ranges.
//
// This is the preferred source of fragment byte-ranges. The per-segment
// .byteranges JSON sidecars carry the same offsets and lengths, but the encoder
// is dropping them now that the manifests are self-describing.
func loadFragmentedRanges(mpdPath string) fragmentRanges {
	dir := filepath.Dir(mpdPath)

	fragmentedCacheMu.RLock()
	cached, ok := fragmentedCache[dir]
	fragmentedCacheMu.RUnlock()
	if ok {
		return cached
	}

	ranges := parseFragmentedMPD(filepath.Join(dir, fragmentedMPDName))

	fragmentedCacheMu.Lock()
	fragmentedCache[dir] = ranges
	fragmentedCacheMu.Unlock()

	return ranges
}

// parseFragmentedMPD reads a fragmented MPD and groups its <SegmentURL>
// entries by @media, preserving document order within each segment.
//
// @mediaRange is an inclusive byte range ("first-last"), so a fragment's length
// is last-first+1. That matches what the .byteranges sidecars report: the first
// fragment of a segment starts at 432, not 0, because the leading bytes are the
// segment's own styp/sidx header and belong to no fragment.
func parseFragmentedMPD(path string) fragmentRanges {
	if _, err := os.Stat(path); err != nil {
		return nil
	}

	doc := etree.NewDocument()
	if err := doc.ReadFromFile(path); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: %s is unreadable, falling back to .byteranges sidecars: %v\n", path, err)
		return nil
	}
	root := doc.Root()
	if root == nil {
		fmt.Fprintf(os.Stderr, "WARN: %s has no root element, falling back to .byteranges sidecars\n", path)
		return nil
	}

	ranges := make(fragmentRanges)
	skipped := 0
	for _, segURL := range findAllByLocal(root, "SegmentURL") {
		media := segURL.SelectAttrValue("media", "")
		mediaRange := segURL.SelectAttrValue("mediaRange", "")
		if media == "" || mediaRange == "" {
			// A fragmented MPD should carry a range on every entry. One without
			// is either a segment-granularity manifest handed to us by mistake
			// or a malformed entry; either way there's nothing to take from it.
			skipped++
			continue
		}
		offset, length, err := parseMediaRange(mediaRange)
		if err != nil {
			skipped++
			continue
		}
		key := normalizeMediaKey(media)
		ranges[key] = append(ranges[key], byterangePayloadFragment{
			Offset: offset,
			Length: length,
			// @mediaRange carries no independence flag. Nothing in this package
			// reads Independent — only the HLS side needs it, and it takes it
			// from #EXT-X-PART's INDEPENDENT attribute.
		})
	}

	if len(ranges) == 0 {
		fmt.Fprintf(os.Stderr, "WARN: %s declares no fragment ranges, falling back to .byteranges sidecars\n", path)
		return nil
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "WARN: %s: skipped %d SegmentURL entries without a usable @mediaRange\n", path, skipped)
	}

	fragmentCount := 0
	for _, frags := range ranges {
		fragmentCount += len(frags)
	}
	fmt.Fprintf(os.Stderr, "INFO: loaded %d fragment ranges across %d segments from %s\n",
		fragmentCount, len(ranges), path)

	return ranges
}

// parseMediaRange converts an inclusive "first-last" byte range into the
// offset/length pair the rest of this package works in.
func parseMediaRange(mediaRange string) (int64, int64, error) {
	first, last, found := strings.Cut(mediaRange, "-")
	if !found {
		return 0, 0, fmt.Errorf("mediaRange %q is not a range", mediaRange)
	}
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("mediaRange %q has an unparseable start: %w", mediaRange, err)
	}
	end, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("mediaRange %q has an unparseable end: %w", mediaRange, err)
	}
	if end < start {
		return 0, 0, fmt.Errorf("mediaRange %q ends before it starts", mediaRange)
	}
	return start, end - start + 1, nil
}

// normalizeMediaKey strips the leading slash that @media values sometimes
// carry, so lookups match however the calling manifest spelled the path. This
// mirrors how the sidecar path is resolved from @media.
func normalizeMediaKey(media string) string {
	return strings.TrimPrefix(media, "/")
}

// warnNoRangeSource reports, once per content, that neither the fragmented MPD
// nor the .byteranges sidecars could supply fragment ranges.
//
// This case is worth a log line because it is not a failure the caller sees:
// callers fall back to whole-segment granularity, so DASH partials silently
// stop existing while the manifests still look well-formed.
func warnNoRangeSource(mpdPath string) {
	dir := filepath.Dir(mpdPath)
	if _, loaded := noRangeSourceWarned.LoadOrStore(dir, true); loaded {
		return
	}
	fmt.Fprintf(os.Stderr,
		"WARN: no fragment byte-ranges for %s — neither %s nor .byteranges sidecars are usable; "+
			"DASH partials are unavailable and manifests will fall back to whole-segment granularity\n",
		dir, fragmentedMPDName)
}
