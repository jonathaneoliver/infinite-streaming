package main

import (
	"regexp"
	"strings"
	"testing"
)

// A small HLS master playlist authored ascending (lowest BANDWIDTH first),
// mirroring our own ladder. URIs double as variant identifiers. Each variant
// carries CODECS and AVERAGE-BANDWIDTH so the strip/overstate axes have
// something to act on.
const sampleMaster = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-STREAM-INF:BANDWIDTH=800000,AVERAGE-BANDWIDTH=700000,CODECS="avc1.640015,mp4a.40.2",RESOLUTION=426x240
v240.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,AVERAGE-BANDWIDTH=1800000,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=854x480
v480.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4200000,AVERAGE-BANDWIDTH=3800000,CODECS="avc1.640028,mp4a.40.2",RESOLUTION=1280x720
v720.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=8000000,AVERAGE-BANDWIDTH=7200000,CODECS="avc1.640033,mp4a.40.2",RESOLUTION=1920x1080
v1080.m3u8
`

// streamInfURIOrder returns the variant URIs in the order they appear in the
// (re-encoded) master playlist — i.e. the order AVPlayer reads them. Doubles as
// the surviving-variant set for allowed_variants filtering.
func streamInfURIOrder(t *testing.T, body []byte) []string {
	t.Helper()
	var uris []string
	expectURI := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			expectURI = true
			continue
		}
		if expectURI && line != "" && !strings.HasPrefix(line, "#") {
			uris = append(uris, line)
			expectURI = false
		}
	}
	return uris
}

// streamInfLines returns the raw #EXT-X-STREAM-INF attribute lines.
func streamInfLines(body []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#EXT-X-STREAM-INF:") {
			lines = append(lines, line)
		}
	}
	return lines
}

// bandwidthAttrRE captures peak BANDWIDTH while the leading-`[^-]` guard keeps
// the match off AVERAGE-BANDWIDTH.
var bandwidthAttrRE = regexp.MustCompile(`[^-]BANDWIDTH=(\d+)`)

// streamInfBandwidths returns the peak BANDWIDTH of each variant, in playlist
// order.
func streamInfBandwidths(t *testing.T, body []byte) []int {
	t.Helper()
	var bws []int
	for _, line := range streamInfLines(body) {
		m := bandwidthAttrRE.FindStringSubmatch(" " + line)
		if m == nil {
			t.Fatalf("no BANDWIDTH in line: %q", line)
		}
		bws = append(bws, atoiOrFatal(t, m[1]))
	}
	return bws
}

func atoiOrFatal(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("non-numeric %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func wantURIOrder(t *testing.T, out []byte, want string) {
	t.Helper()
	got := streamInfURIOrder(t, out)
	if !eqStrings(got, strings.Fields(want)) {
		t.Fatalf("variant URIs = %v, want %v", got, strings.Fields(want))
	}
}

// TestManipulateHLSMaster drives every content-manipulation axis through the
// single ContentManipulation struct. Adding a new option = a new row here.
func TestManipulateHLSMaster(t *testing.T) {
	const allURIsAscending = "v240.m3u8 v480.m3u8 v720.m3u8 v1080.m3u8"

	cases := []struct {
		name  string
		cm    ContentManipulation
		check func(t *testing.T, out []byte)
	}{
		// ---- variant_order (issue #682) ----
		{
			name:  "order/unset_passthrough",
			cm:    ContentManipulation{},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, allURIsAscending) },
		},
		{
			name:  "order/default",
			cm:    ContentManipulation{VariantOrder: "default"},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, allURIsAscending) },
		},
		{
			name:  "order/ascending",
			cm:    ContentManipulation{VariantOrder: "ascending"},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, allURIsAscending) },
		},
		{
			name:  "order/descending",
			cm:    ContentManipulation{VariantOrder: "descending"},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v1080.m3u8 v720.m3u8 v480.m3u8 v240.m3u8") },
		},
		{
			// nearest 4 Mbps is the 4.2 Mbps 720p variant → first; rest ascending.
			name:  "order/first_4mbps",
			cm:    ContentManipulation{VariantOrder: "first_4mbps"},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v720.m3u8 v240.m3u8 v480.m3u8 v1080.m3u8") },
		},

		// ---- allowed_variants (the resolution-match gap #766 calls out) ----
		{
			name:  "allowed/by_uri",
			cm:    ContentManipulation{AllowedVariants: []string{"v480.m3u8"}},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v480.m3u8") },
		},
		{
			name:  "allowed/by_resolution",
			cm:    ContentManipulation{AllowedVariants: []string{"1280x720"}},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v720.m3u8") },
		},
		{
			name:  "allowed/by_height",
			cm:    ContentManipulation{AllowedVariants: []string{"240"}},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v240.m3u8") },
		},
		{
			name:  "allowed/by_height_p_suffix",
			cm:    ContentManipulation{AllowedVariants: []string{"1080p"}},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v1080.m3u8") },
		},
		{
			// mixed match keys, authored ascending order preserved among survivors.
			name:  "allowed/mixed_keys",
			cm:    ContentManipulation{AllowedVariants: []string{"v240.m3u8", "720"}},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "v240.m3u8 v720.m3u8") },
		},
		{
			// no whitelist entry matches → every variant is filtered out,
			// leaving an empty master. Documents current behaviour (#767 is a
			// pure refactor; this is not asserting it's the *desired* policy).
			name:  "allowed/no_match_strips_all",
			cm:    ContentManipulation{AllowedVariants: []string{"4320"}},
			check: func(t *testing.T, out []byte) { wantURIOrder(t, out, "") },
		},

		// ---- strip_codecs (#486 family) ----
		{
			name: "strip_codecs",
			cm:   ContentManipulation{StripCodecs: true},
			check: func(t *testing.T, out []byte) {
				for _, line := range streamInfLines(out) {
					if strings.Contains(line, "CODECS=") {
						t.Fatalf("CODECS still present: %q", line)
					}
				}
			},
		},

		// ---- strip_average_bandwidth ----
		{
			name: "strip_avg_bandwidth",
			cm:   ContentManipulation{StripAvgBandwidth: true},
			check: func(t *testing.T, out []byte) {
				for _, line := range streamInfLines(out) {
					if strings.Contains(line, "AVERAGE-BANDWIDTH=") {
						t.Fatalf("AVERAGE-BANDWIDTH still present: %q", line)
					}
				}
				// peak BANDWIDTH untouched.
				if got := streamInfBandwidths(t, out); !eqInts(got, []int{800000, 2000000, 4200000, 8000000}) {
					t.Fatalf("peak BANDWIDTH changed: %v", got)
				}
			},
		},

		// ---- strip_resolution (#486) ----
		{
			name: "strip_resolution",
			cm:   ContentManipulation{StripResolution: true},
			check: func(t *testing.T, out []byte) {
				for _, line := range streamInfLines(out) {
					if strings.Contains(line, "RESOLUTION=") {
						t.Fatalf("RESOLUTION still present: %q", line)
					}
				}
			},
		},

		// ---- overstate_bandwidth (+10% on BANDWIDTH and AVERAGE-BANDWIDTH) ----
		{
			name: "overstate_bandwidth",
			cm:   ContentManipulation{OverstateBandwidth: true},
			check: func(t *testing.T, out []byte) {
				// 800000*1.10 etc, truncated to uint32 by the implementation.
				want := []int{880000, 2200000, 4620000, 8800000}
				if got := streamInfBandwidths(t, out); !eqInts(got, want) {
					t.Fatalf("BANDWIDTH after overstate = %v, want %v", got, want)
				}
			},
		},

		// ---- live_offset (EXT-X-START injection) ----
		{
			name: "live_offset",
			cm:   ContentManipulation{LiveOffset: 6},
			check: func(t *testing.T, out []byte) {
				if !strings.Contains(string(out), "#EXT-X-START:TIME-OFFSET=-6") {
					t.Fatalf("expected EXT-X-START TIME-OFFSET=-6, got:\n%s", out)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := manipulateHLSMaster([]byte(sampleMaster), tc.cm)
			if err != nil {
				t.Fatalf("manipulateHLSMaster(%+v): %v", tc.cm, err)
			}
			tc.check(t, out)
		})
	}
}
