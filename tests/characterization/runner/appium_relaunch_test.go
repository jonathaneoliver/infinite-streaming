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

// RelaunchApp (#946) must reuse the existing session and cold-launch the app via
// mobile: terminateApp → mobile: launchApp, folding the new launch args into
// NSArgumentDomain (so -is.player_id / -is.server_url bind the new session on a
// WARM appium session — no createSession). This pins that command sequence.
func TestRelaunchApp(t *testing.T) {
	var mu sync.Mutex
	var scripts []string
	var launchArgs []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/execute/sync") {
			var req struct {
				Script string `json:"script"`
				Args   []struct {
					BundleID  string `json:"bundleId"`
					Arguments []any  `json:"arguments"`
				} `json:"args"`
			}
			_ = json.Unmarshal(body, &req)
			mu.Lock()
			scripts = append(scripts, req.Script)
			if req.Script == "mobile: launchApp" && len(req.Args) == 1 {
				launchArgs = req.Args[0].Arguments
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"value":null}`))
			return
		}
		// Every find resolves + every click/keys succeeds, so the picker/home
		// navigation runs through without the 4s not-found poll.
		if strings.HasSuffix(r.URL.Path, "/element") {
			_, _ = w.Write([]byte(`{"value":{"element-6066-11e4-a52e-4f735466cecf":"e1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"value":null}`))
	}))
	defer srv.Close()

	l := NewAppiumLauncher()
	l.URL = srv.URL
	l.hc = srv.Client()
	l.sessions = map[string]string{"udid-1": "sess-1"}
	l.BundleIDs = map[Platform]string{PlatformIPadSim: "com.jeoliver.InfiniteStreamPlayer"}

	d := Device{Platform: PlatformIPadSim, UDID: "udid-1"}
	err := l.RelaunchApp(context.Background(), d,
		[]string{"-is.player_id", "P123", "-is.server_url", "https://dev:21000"})
	if err != nil {
		t.Fatalf("RelaunchApp: %v", err)
	}

	mu.Lock()
	gotScripts := append([]string(nil), scripts...)
	gotArgs := launchArgs
	mu.Unlock()

	// terminate THEN launch, in that order.
	if len(gotScripts) < 2 || gotScripts[0] != "mobile: terminateApp" || gotScripts[1] != "mobile: launchApp" {
		t.Fatalf("want [terminateApp, launchApp], got %v", gotScripts)
	}
	// The new launch args reached NSArgumentDomain, incl. the per-experiment binding.
	as := make([]string, 0, len(gotArgs))
	for _, a := range gotArgs {
		if s, ok := a.(string); ok {
			as = append(as, s)
		}
	}
	joined := strings.Join(as, " ")
	for _, want := range []string{"-is.player_id", "P123", "-is.server_url", "https://dev:21000"} {
		if !strings.Contains(joined, want) {
			t.Errorf("launch args missing %q; got %q", want, joined)
		}
	}
	// Baseline test flags are folded in too (same as a cold LaunchToHome).
	if !strings.Contains(joined, "-is.flag.4k") {
		t.Errorf("baseline flags not folded into warm relaunch; got %q", joined)
	}
}

func TestRelaunchApp_NoSession(t *testing.T) {
	l := NewAppiumLauncher()
	l.BundleIDs = map[Platform]string{PlatformIPadSim: "com.x"}
	err := l.RelaunchApp(context.Background(), Device{Platform: PlatformIPadSim, UDID: "nope"}, nil)
	if err == nil {
		t.Fatal("expected an error when no session exists for the device")
	}
}

func TestRelaunchApp_UnsupportedPlatform(t *testing.T) {
	l := NewAppiumLauncher()
	l.sessions = map[string]string{"tv-1": "sess-1"}
	l.BundleIDs = map[Platform]string{PlatformAndroidTV: "com.x"}
	err := l.RelaunchApp(context.Background(), Device{Platform: PlatformAndroidTV, UDID: "tv-1"}, nil)
	if err == nil {
		t.Fatal("expected warm relaunch to be unsupported on androidtv (cold only)")
	}
}
