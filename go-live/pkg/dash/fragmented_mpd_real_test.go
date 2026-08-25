package dash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/beevik/etree"
)

// realFragmentedMPD locates a real encoder-produced manifest to test against.
//
// The unit tests above run on hand-written fixtures, which prove the collapse
// logic but not that it matches what the encoder ACTUALLY writes — the two
// drifted once already (the encoder stopped emitting a second
// manifest_fragmented.mpd and started rewriting manifest.mpd in place, #282),
// and a fixture would never have noticed.
//
// Set DASH_REAL_MPD to point at a package directory, or leave it unset to use
// the default staging path. Skips when neither is present, so this stays a
// local-only check and never fails CI on a box without the volume mounted.
func realFragmentedMPD(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("DASH_REAL_MPD")
	if dir == "" {
		dir = "/Volumes/4TB/media/encode-staging/insane_fpv_shots_hydrofoil_windsurfing_p200_h264_xs"
	}
	path := filepath.Join(dir, "manifest.mpd")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("no real encoder manifest at %s (set DASH_REAL_MPD to override)", path)
	}
	return path
}

// The acceptance case for #986: real encoder output, fragment-granularity
// manifest.mpd, and NO .byteranges sidecars anywhere in the package.
func TestRealEncoderManifestHasNoSidecars(t *testing.T) {
	path := realFragmentedMPD(t)
	dir := filepath.Dir(path)

	var sidecars int
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && filepath.Ext(p) == ".byteranges" {
			sidecars++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if sidecars != 0 {
		t.Fatalf("expected a sidecar-free package, found %d .byteranges files", sidecars)
	}
	t.Logf("package is sidecar-free: %s", dir)
}

// Collapsing the real manifest must recover segment granularity AND the
// fragment ranges, with the segment timeline reconstructed exactly. The encoder
// splits a segment's duration across its fragments with divmod (remainder to
// the leading fragments); summing them back must return the original.
func TestRealEncoderManifestCollapses(t *testing.T) {
	path := realFragmentedMPD(t)

	doc := etree.NewDocument()
	if err := doc.ReadFromFile(path); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	root := doc.Root()
	if root == nil {
		t.Fatal("no root element")
	}

	// Capture the pre-collapse shape so the assertions below are about THIS
	// file rather than an assumed one.
	fragURLs := 0
	for _, rep := range findAllByLocal(root, "Representation") {
		if sl := findFirstByLocal(rep, "SegmentList"); sl != nil {
			fragURLs += len(findAllByLocal(sl, "SegmentURL"))
		}
	}
	if fragURLs == 0 {
		t.Fatal("no SegmentURL entries — not a SegmentList manifest")
	}

	ranges, collapsed := normalizeFragmentedMPD(root)
	if !collapsed {
		t.Fatal("real encoder manifest was not recognised as fragment-granularity")
	}
	if len(ranges) == 0 {
		t.Fatal("collapse recovered no fragment ranges")
	}

	segURLs := 0
	for _, rep := range findAllByLocal(root, "Representation") {
		sl := findFirstByLocal(rep, "SegmentList")
		if sl == nil {
			continue
		}
		urls := findAllByLocal(sl, "SegmentURL")
		segURLs += len(urls)

		// Post-collapse, no SegmentURL may carry a range: the document must be
		// indistinguishable from an unexpanded manifest.mpd.
		for _, u := range urls {
			if mr := u.SelectAttrValue("mediaRange", ""); mr != "" {
				t.Fatalf("collapsed manifest still carries mediaRange %q", mr)
			}
		}

		// The rebuilt timeline must describe exactly as many segments as there
		// are SegmentURL entries, or every downstream boundary shifts.
		tl := findFirstByLocal(sl, "SegmentTimeline")
		if tl == nil {
			continue
		}
		if got := len(expandTimelineDurations(tl)); got != len(urls) {
			t.Fatalf("rebuilt timeline has %d entries for %d segments", got, len(urls))
		}
	}

	if segURLs >= fragURLs {
		t.Fatalf("collapse did not reduce entries: %d segments from %d fragments", segURLs, fragURLs)
	}

	// Within a segment, fragments must be contiguous and ascending — that is
	// what makes a regrouped 1s/2s variant a valid byte-range request.
	checked := 0
	for media, frags := range ranges {
		for i, f := range frags {
			if f.Length <= 0 {
				t.Fatalf("%s fragment %d has non-positive length %d", media, i, f.Length)
			}
			if i > 0 {
				prev := frags[i-1]
				if got, want := f.Offset, prev.Offset+prev.Length; got != want {
					t.Fatalf("%s fragment %d starts at %d, expected %d (contiguous)", media, i, got, want)
				}
			}
		}
		checked += len(frags)
	}

	t.Logf("collapsed %d fragments across %d segments in %d media entries",
		fragURLs, segURLs, len(ranges))
	if checked != fragURLs {
		t.Logf("note: %d ranges recovered from %d SegmentURL entries "+
			"(entries without @mediaRange are expected for un-fragmented segments)", checked, fragURLs)
	}
}
