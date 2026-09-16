package api

import "testing"

// TestParseDashVariant covers the DASH manifest routing, including the 1s
// variant added in #931. The three flat manifest_Ns.mpd forms and the nested
// {duration}/…/manifest.mpd form must all resolve to the right (variant,
// duration, llMode, lookupBase); everything else falls back to LL.
func TestParseDashVariant(t *testing.T) {
	cases := []struct {
		path     string
		variant  string
		duration int
		llMode   bool
		lookup   string
	}{
		{"insane/manifest_1s.mpd", "1s", 1, false, "insane/manifest.mpd"},
		{"insane/manifest_2s.mpd", "2s", 2, false, "insane/manifest.mpd"},
		{"insane/manifest_6s.mpd", "6s", 6, false, "insane/manifest.mpd"},
		{"insane/manifest.mpd", "ll", 6, true, "insane/manifest.mpd"},
		// bare (no content dir) still parses
		{"manifest_1s.mpd", "1s", 1, false, "manifest.mpd"},
		// nested duration-prefixed form (non-manifest.mpd base reaches the
		// default branch, where 1s now sits alongside 2s/6s)
		{"1s/insane.mpd", "1s", 1, false, "insane.mpd"},
	}
	for _, c := range cases {
		variant, duration, llMode, lookup := parseDashVariant(c.path)
		if variant != c.variant || duration != c.duration || llMode != c.llMode || lookup != c.lookup {
			t.Errorf("parseDashVariant(%q) = (%q,%d,%v,%q), want (%q,%d,%v,%q)",
				c.path, variant, duration, llMode, lookup,
				c.variant, c.duration, c.llMode, c.lookup)
		}
	}
}
