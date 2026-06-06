package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// #627: ClosePlaybackViaUI must drive the app's OWN UI close so the app
// emits its real client play_end — iOS taps the playback-back-button
// element (find + click), Android presses system Back. This pins the
// WebDriver call sequence per platform against a mock Appium server.
func TestClosePlaybackViaUI(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		// find-element returns a W3C element id; everything else returns null.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/element") {
			_, _ = w.Write([]byte(`{"value":{"element-6066-11e4-a52e-4f735466cecf":"elem-1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":null}`))
	}))
	defer srv.Close()

	newL := func() *AppiumLauncher {
		l := NewAppiumLauncher()
		l.URL = srv.URL
		l.hc = srv.Client()
		l.sessions = map[string]string{"udid-1": "sess-1"}
		return l
	}
	reset := func() { mu.Lock(); paths = nil; mu.Unlock() }
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }

	t.Run("iOS taps back button", func(t *testing.T) {
		reset()
		l := newL()
		d := Device{Platform: PlatformIPhone, UDID: "udid-1"}
		if err := l.ClosePlaybackViaUI(context.Background(), d); err != nil {
			t.Fatalf("ClosePlaybackViaUI: %v", err)
		}
		want := []string{
			"POST /session/sess-1/element",              // find playback-back-button
			"POST /session/sess-1/element/elem-1/click", // tap it
		}
		if g := got(); len(g) != 2 || g[0] != want[0] || g[1] != want[1] {
			t.Errorf("iOS close hit %v, want %v", g, want)
		}
	})

	t.Run("Android presses system back", func(t *testing.T) {
		reset()
		l := newL()
		d := Device{Platform: PlatformAndroidTV, UDID: "udid-1"}
		if err := l.ClosePlaybackViaUI(context.Background(), d); err != nil {
			t.Fatalf("ClosePlaybackViaUI: %v", err)
		}
		if g := got(); len(g) != 1 || g[0] != "POST /session/sess-1/back" {
			t.Errorf("Android close hit %v, want [POST /session/sess-1/back]", g)
		}
	})

	t.Run("no session is a no-op", func(t *testing.T) {
		reset()
		l := newL()
		l.sessions = map[string]string{} // never launched
		d := Device{Platform: PlatformIPhone, UDID: "udid-1"}
		if err := l.ClosePlaybackViaUI(context.Background(), d); err != nil {
			t.Fatalf("expected nil for no-session, got %v", err)
		}
		if g := got(); len(g) != 0 {
			t.Errorf("no-session close hit server %v, want none", g)
		}
	})
}

// Session.CloseViaUI on a launcher that can't drive the UI (Manual / CLI,
// which don't implement UICloser) must be a silent no-op (#627).
func TestSessionCloseViaUINonUICloser(t *testing.T) {
	s := &Session{Launcher: NewManualLauncher()}
	if err := s.CloseViaUI(context.Background()); err != nil {
		t.Errorf("CloseViaUI on non-UICloser launcher = %v, want nil", err)
	}
	// And the Appium launcher MUST satisfy the interface.
	if _, ok := any(NewAppiumLauncher()).(UICloser); !ok {
		t.Error("AppiumLauncher should implement UICloser")
	}
}
