package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/jonathaneoliver/infinite-streaming/go-live/internal/manager"
)

// segmentMPD is a single-rung, segment-granularity static MPD of
// segments × segSeconds — the packaged-clip shape dash.LoadMPD reads.
func segmentMPD(segments, segSeconds int) string {
	const timescale = 30000
	urls := ""
	for i := 1; i <= segments; i++ {
		urls += fmt.Sprintf("\n          <SegmentURL media=\"360p/segment_%05d.m4s\"/>", i)
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="static" mediaPresentationDuration="PT%dS">
  <Period id="0">
    <AdaptationSet id="0" contentType="video">
      <Representation id="0" bandwidth="400000">
        <SegmentList timescale="%d">
          <Initialization sourceURL="360p/init.mp4"/>
          <SegmentTimeline>
            <S t="0" d="%d" r="%d"/>
          </SegmentTimeline>%s
        </SegmentList>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`, segments*segSeconds, timescale, segSeconds*timescale, segments-1, urls)
}

func serveDash(t *testing.T, h *Handler, content, path string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/go-live/"+content+"/"+path, nil)
	req = mux.SetURLVars(req, map[string]string{"content": content, "path": path})
	rec := httptest.NewRecorder()
	h.OnDemandDashManifest(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("GET %s: status=%d bytes=%d body=%q", path, rec.Code, rec.Body.Len(), rec.Body.String())
	}
}

func dashEntryUpdated(t *testing.T, content, variant string) time.Time {
	t.Helper()
	dashCacheMu.Lock()
	defer dashCacheMu.Unlock()
	entry := dashCache[dashCacheKey(content, filepath.Join(content, "manifest.mpd"), variant)]
	if entry == nil || entry.data == nil {
		t.Fatalf("no cached %s MPD for %s", variant, content)
	}
	return entry.updated
}

func ageDashEntry(content, variant string, by time.Duration) {
	dashCacheMu.Lock()
	defer dashCacheMu.Unlock()
	entry := dashCache[dashCacheKey(content, filepath.Join(content, "manifest.mpd"), variant)]
	entry.updated = entry.updated.Add(-by)
}

// #1033: a cached live MPD used to be generated once and served forever unless
// the HLS worker's tick refreshed it — which it never did for 1s, and never did
// at all for DASH-only content (no master.m3u8). Every variant must now be
// served from cache while fresh and regenerated once stale, and DASH-only
// content must not leave a dead worker registered as running.
func TestOnDemandDashManifestRefreshesStaleCache(t *testing.T) {
	root := t.TempDir()
	prev := infiniteOutputDir
	infiniteOutputDir = root
	t.Cleanup(func() { infiniteOutputDir = prev })

	content := "dashonly_refresh_p200_h264"
	if err := os.MkdirAll(filepath.Join(root, content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, content, "manifest.mpd"), []byte(segmentMPD(12, 6)), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &Handler{Manager: manager.NewProcessManager()}
	for _, c := range []struct{ path, variant string }{
		{"manifest_1s.mpd", "1s"},
		{"manifest_2s.mpd", "2s"},
		{"manifest_6s.mpd", "6s"},
		{"manifest.mpd", "ll"},
	} {
		serveDash(t, h, content, c.path)
		first := dashEntryUpdated(t, content, c.variant)

		serveDash(t, h, content, c.path)
		if got := dashEntryUpdated(t, content, c.variant); !got.Equal(first) {
			t.Errorf("%s: fresh cache was regenerated (updated %s -> %s)", c.variant, first, got)
		}

		ageDashEntry(content, c.variant, time.Minute)
		serveDash(t, h, content, c.path)
		if got := dashEntryUpdated(t, content, c.variant); !got.After(first) {
			t.Errorf("%s: stale cache was served, not regenerated (updated stayed at %s)", c.variant, got)
		}
	}

	if h.Manager.IsRunning("hls-worker-" + content) {
		t.Error("DASH-only content registered an HLS worker that can never run")
	}
}

func TestDashCacheMaxAge(t *testing.T) {
	cases := []struct {
		duration int
		llMode   bool
		want     time.Duration
	}{
		{6, true, time.Second},
		{1, false, time.Second},
		{2, false, 3 * time.Second},
		{6, false, 7 * time.Second},
	}
	for _, c := range cases {
		if got := dashCacheMaxAge(c.duration, c.llMode); got != c.want {
			t.Errorf("dashCacheMaxAge(%d, %v) = %s, want %s", c.duration, c.llMode, got, c.want)
		}
	}
}
