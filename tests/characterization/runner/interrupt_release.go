package runner

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Session-leak hardening — every characterization runner must free BOTH the
// go-proxy session (the config-on-connect pool slot) AND the appium/WDA session
// (the device-farm slot) on EVERY exit, clean or killed. Otherwise a leaked WDA
// session keeps the sim busy=true and the next run hangs on "create session:
// context deadline exceeded" (see the leaked-appium-session finding), and a
// leaked proxy session exhausts the 4-slot pool ("session limit reached").
//
// A test's own defer/t.Cleanup covers the CLEAN exit, but not a kill: timeout(1),
// pkill, or Ctrl-C deliver SIGTERM/SIGINT, and Go's default handler terminates
// the process before any deferred cleanup runs. This file is the safety net that
// makes hardening automatic for every test that creates an appium session — no
// per-test wiring, so a future test can't forget:
//
//   - registerLauncher/registerSession are called from the AppiumLauncher session
//     path, so every open slot is tracked centrally.
//   - armInterruptBackstop (once, on first session) catches SIGINT/SIGTERM and,
//     after a short grace for any self-hardened test to unwind on its own, force-
//     releases whatever is still registered and exits — so a killed run frees its
//     slots instead of leaking them.
//   - ReleaseAllRegistered is also called from the modes TestMain after m.Run(),
//     so even a clean exit whose test forgot its own cleanup frees the slots.
//
// The runner package is imported only by the characterization test binaries (no
// production/CLI code opens a WDA session), so installing a process signal
// handler here has no blast radius outside tests.

var (
	hardenMu        sync.Mutex
	activeLaunchers = map[*AppiumLauncher]struct{}{}
	activeSessions  = map[*Session]struct{}{}
	armOnce         sync.Once
)

// interruptGrace is how long the backstop waits after a signal before force-
// releasing. A self-hardened test (one that catches the signal via the modes
// interruptContext) unwinds, releases, and lets the binary exit within this
// window — so the backstop's os.Exit never fires for it. An un-hardened test
// stays blocked in its play window, so after the grace the backstop releases its
// slots and exits on its behalf.
var interruptGrace = 6 * time.Second

func armInterruptBackstop() {
	armOnce.Do(func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-ch
			fmt.Fprintln(os.Stderr, "[interrupt-backstop] signal received; releasing sessions after grace…")
			time.Sleep(interruptGrace)
			ReleaseAllRegistered()
			os.Exit(130)
		}()
	})
}

func registerLauncher(a *AppiumLauncher) {
	if a == nil {
		return
	}
	armInterruptBackstop()
	hardenMu.Lock()
	activeLaunchers[a] = struct{}{}
	hardenMu.Unlock()
}

func unregisterLauncher(a *AppiumLauncher) {
	hardenMu.Lock()
	delete(activeLaunchers, a)
	hardenMu.Unlock()
}

func registerSession(s *Session) {
	if s == nil {
		return
	}
	hardenMu.Lock()
	activeSessions[s] = struct{}{}
	hardenMu.Unlock()
}

func unregisterSession(s *Session) {
	hardenMu.Lock()
	delete(activeSessions, s)
	hardenMu.Unlock()
}

// ReleaseAllRegistered frees every still-open proxy session (CloseViaUI +
// Release) and appium/WDA session (Launcher.Close → DELETE /session, which frees
// the device-farm slot). Best-effort and idempotent — safe to call more than
// once and from both the signal backstop and the modes TestMain. Ordering
// mirrors the documented teardown contract: proxy session first, then the WDA
// session.
func ReleaseAllRegistered() {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	hardenMu.Lock()
	sessions := make([]*Session, 0, len(activeSessions))
	for s := range activeSessions {
		sessions = append(sessions, s)
	}
	launchers := make([]*AppiumLauncher, 0, len(activeLaunchers))
	for a := range activeLaunchers {
		launchers = append(launchers, a)
	}
	activeSessions = map[*Session]struct{}{}
	activeLaunchers = map[*AppiumLauncher]struct{}{}
	hardenMu.Unlock()

	if len(sessions) > 0 || len(launchers) > 0 {
		fmt.Fprintf(os.Stderr, "[interrupt-backstop] releasing %d proxy session(s) + %d appium session(s)\n",
			len(sessions), len(launchers))
	}
	for _, s := range sessions {
		_ = s.CloseViaUI(ctx)
		_ = s.Release(ctx)
	}
	for _, a := range launchers {
		if err := a.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[interrupt-backstop] appium Close error: %v\n", err)
		}
	}
}
