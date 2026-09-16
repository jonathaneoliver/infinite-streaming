package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// #630: SetSegmentLength must drive the settings UI in order — settings
// button → Segment-length row → the chosen value → back out — using the
// accessibility ids the app exposes. This pins the find-element id sequence
// against a mock Appium server.
func TestSetSegmentLength(t *testing.T) {
	var mu sync.Mutex
	var findIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/element") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Value string `json:"value"`
			}
			_ = json.Unmarshal(body, &req)
			mu.Lock()
			findIDs = append(findIDs, req.Value)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"value":{"element-6066-11e4-a52e-4f735466cecf":"elem-1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":null}`))
	}))
	defer srv.Close()

	l := NewAppiumLauncher()
	l.URL = srv.URL
	l.hc = srv.Client()
	l.sessions = map[string]string{"udid-1": "sess-1"}

	d := Device{Platform: PlatformIPadSim, UDID: "udid-1"}
	if err := l.SetSegmentLength(context.Background(), d, "2s"); err != nil {
		t.Fatalf("SetSegmentLength: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), findIDs...)
	mu.Unlock()
	want := []string{"playback-settings-button", "settings-row-segment", "segment-2s", "settings-back-button"}
	if len(got) != len(want) {
		t.Fatalf("found ids %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d found %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSetSegmentLengthGuards(t *testing.T) {
	l := NewAppiumLauncher()
	// Unsupported platform → error (checked before session).
	l.sessions = map[string]string{"a": "s"}
	if err := l.SetSegmentLength(context.Background(), Device{Platform: PlatformAndroidTV, UDID: "a"}, "6s"); err == nil {
		t.Error("expected error for unsupported platform")
	}
	// Supported platform but no active session → error.
	if err := l.SetSegmentLength(context.Background(), Device{Platform: PlatformIPhone, UDID: "none"}, "6s"); err == nil {
		t.Error("expected error with no active session")
	}
}
