package main

import (
	"strings"
	"testing"
)

// A small HLS master playlist authored ascending (lowest BANDWIDTH first),
// mirroring our own ladder. URIs double as variant identifiers.
const sampleMaster = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=426x240
v240.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=854x480
v480.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4200000,RESOLUTION=1280x720
v720.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=8000000,RESOLUTION=1920x1080
v1080.m3u8
`

// streamInfURIOrder returns the variant URIs in the order they appear in the
// (re-encoded) master playlist — i.e. the order AVPlayer reads them.
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

func TestManipulateHLSMaster_VariantOrder(t *testing.T) {
	cases := []struct {
		order string
		want  []string
	}{
		// default / unset → passthrough, authored ascending order preserved.
		{"", []string{"v240.m3u8", "v480.m3u8", "v720.m3u8", "v1080.m3u8"}},
		{"default", []string{"v240.m3u8", "v480.m3u8", "v720.m3u8", "v1080.m3u8"}},
		{"ascending", []string{"v240.m3u8", "v480.m3u8", "v720.m3u8", "v1080.m3u8"}},
		{"descending", []string{"v1080.m3u8", "v720.m3u8", "v480.m3u8", "v240.m3u8"}},
		// nearest 4 Mbps is the 4.2 Mbps 720p variant → first; rest ascending.
		{"first_4mbps", []string{"v720.m3u8", "v240.m3u8", "v480.m3u8", "v1080.m3u8"}},
	}
	for _, tc := range cases {
		t.Run(tc.order, func(t *testing.T) {
			out, err := manipulateHLSMaster([]byte(sampleMaster), false, false, false, false, 0, nil, tc.order)
			if err != nil {
				t.Fatalf("manipulateHLSMaster(%q): %v", tc.order, err)
			}
			got := streamInfURIOrder(t, out)
			if len(got) != len(tc.want) {
				t.Fatalf("order %q: got %v variants, want %v", tc.order, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("order %q: variant order = %v, want %v", tc.order, got, tc.want)
				}
			}
		})
	}
}
