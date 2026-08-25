package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// #627/#660: ClosePlaybackViaUI must drive the app's OWN UI close so the
// app emits its real client play_end — iOS taps the playback-back-button
// element and VERIFIES the screen closed (the chevron stays findable while
// the controls overlay is auto-hidden, so a click can "succeed" yet only
// reveal the overlay; find-fails is the closed-screen probe). Android
// presses system Back. Pins the WebDriver call sequence per platform
// against a mock Appium server whose find-element responses are scripted.
func TestClosePlaybackViaUI(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	// findFound scripts the find-element responses, one bool per call:
	// true → element id returned; false → W3C no-such-element error.
	// Finds past the end of the script report not-found.
	var findFound []bool
	var findCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/element") {
			found := findCalls < len(findFound) && findFound[findCalls]
			findCalls++
			mu.Unlock()
			if found {
				_, _ = w.Write([]byte(`{"value":{"element-6066-11e4-a52e-4f735466cecf":"elem-1"}}`))
			} else {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"value":{"error":"no such element","message":"not located"}}`))
			}
			return
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"value":null}`))
	}))
	defer srv.Close()

	newL := func() *AppiumLauncher {
		l := NewAppiumLauncher()
		l.URL = srv.URL
		l.hc = srv.Client()
		l.closeBeat = time.Millisecond
		l.sessions = map[string]string{"udid-1": "sess-1"}
		return l
	}
	reset := func(script ...bool) {
		mu.Lock()
		paths = nil
		findFound = script
		findCalls = 0
		mu.Unlock()
	}
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), paths...) }
	wantSeq := func(t *testing.T, want []string) {
		t.Helper()
		g := got()
		if len(g) != len(want) {
			t.Fatalf("close hit %v, want %v", g, want)
		}
		for i := range want {
			if g[i] != want[i] {
				t.Fatalf("close hit %v, want %v", g, want)
			}
		}
	}

	t.Run("iOS taps back button and verifies closed", func(t *testing.T) {
		reset(true, false) // chevron present; gone after one tap
		l := newL()
		d := Device{Platform: PlatformIPhone, UDID: "udid-1"}
		if err := l.ClosePlaybackViaUI(context.Background(), d); err != nil {
			t.Fatalf("ClosePlaybackViaUI: %v", err)
		}
		wantSeq(t, []string{
			"POST /session/sess-1/element",              // find playback-back-button
			"POST /session/sess-1/element/elem-1/click", // tap it
			"POST /session/sess-1/element",              // probe: gone → closed
		})
	})

	t.Run("iOS hidden-controls overlay needs a second tap (#660)", func(t *testing.T) {
		// First tap lands on the hidden-overlay surface and only reveals
		// the controls: chevron still findable. Second tap closes.
		reset(true, true, false)
		l := newL()
		d := Device{Platform: PlatformIPhone, UDID: "udid-1"}
		if err := l.ClosePlaybackViaUI(context.Background(), d); err != nil {
			t.Fatalf("ClosePlaybackViaUI: %v", err)
		}
		wantSeq(t, []string{
			"POST /session/sess-1/element",              // find
			"POST /session/sess-1/element/elem-1/click", // tap (only reveals overlay)
			"POST /session/sess-1/element",              // probe: still open
			"POST /session/sess-1/element/elem-1/click", // tap again (hits)
			"POST /session/sess-1/element",              // probe: gone → closed
		})
	})

	t.Run("iOS already on home is a no-op", func(t *testing.T) {
		reset(false) // chevron never present
		l := newL()
		d := Device{Platform: PlatformIPhone, UDID: "udid-1"}
		if err := l.ClosePlaybackViaUI(context.Background(), d); err != nil {
			t.Fatalf("expected nil when already on home, got %v", err)
		}
		wantSeq(t, []string{"POST /session/sess-1/element"}) // single probe, no click
	})

	t.Run("iOS errors loudly when the screen never closes (#660)", func(t *testing.T) {
		reset(true, true, true, true) // chevron survives every tap
		l := newL()
		d := Device{Platform: PlatformIPhone, UDID: "udid-1"}
		err := l.ClosePlaybackViaUI(context.Background(), d)
		if err == nil {
			t.Fatal("expected an error when the playback screen never closes")
		}
		if !strings.Contains(err.Error(), "still open") {
			t.Fatalf("error %q should name the still-open screen", err)
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

// Session.ReleaseDevice is opt-in (#627): without CHAR_RELEASE_DEVICE=1 it
// must not touch the device, even for an Appium launcher — so it never
// kills WDA mid-suite. AppiumLauncher must implement DeviceReleaser, and a
// non-releaser launcher (Manual) is a silent no-op.
func TestSessionReleaseDeviceGating(t *testing.T) {
	if _, ok := any(NewAppiumLauncher()).(DeviceReleaser); !ok {
		t.Error("AppiumLauncher should implement DeviceReleaser")
	}

	// Env unset → no-op even with a real Appium launcher bound to an iOS
	// device. (If the gate leaked, this would shell out to devicectl.)
	t.Setenv("CHAR_RELEASE_DEVICE", "")
	s := &Session{Launcher: NewAppiumLauncher(), Device: Device{Platform: PlatformIPhone, UDID: "udid-x"}}
	if err := s.ReleaseDevice(context.Background()); err != nil {
		t.Errorf("ReleaseDevice with gate unset = %v, want nil (no-op)", err)
	}

	// Opt-in set, but a non-releaser launcher (Manual) is still a no-op.
	t.Setenv("CHAR_RELEASE_DEVICE", "1")
	sm := &Session{Launcher: NewManualLauncher(), Device: Device{Platform: PlatformIPhone}}
	if err := sm.ReleaseDevice(context.Background()); err != nil {
		t.Errorf("ReleaseDevice on non-DeviceReleaser launcher = %v, want nil", err)
	}

	// Opt-in set, Appium launcher, but a non-iOS platform → no devicectl,
	// returns nil.
	sa := &Session{Launcher: NewAppiumLauncher(), Device: Device{Platform: PlatformAndroidTV, UDID: "android-1"}}
	if err := sa.ReleaseDevice(context.Background()); err != nil {
		t.Errorf("ReleaseDevice on non-iOS platform = %v, want nil", err)
	}
}
