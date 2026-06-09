package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Session is the open-handle the runner hands to characterization tests.
// One Session = one (device, player_id, harness connection) triple. Tests
// drive Session.Apply(rate) and Session.Sample() rather than reaching back
// to the underlying launcher.
type Session struct {
	Device   Device
	PlayerID string
	Launcher Launcher
}

// CurrentPlayID returns the bound player's active play_id, fetched fresh
// from the harness. Used by mode tests to print + persist the play_id at
// sweep start so the run is findable in the archive later.
func (s *Session) CurrentPlayID(ctx context.Context) (string, error) {
	if s == nil || s.PlayerID == "" {
		return "", errors.New("session: no player bound")
	}
	rec, err := s.PlayerState(ctx)
	if err != nil {
		return "", err
	}
	if rec.CurrentPlay == nil || rec.CurrentPlay.ID == "" {
		return "", fmt.Errorf("session: player %s has no current play", s.PlayerID)
	}
	return rec.CurrentPlay.ID, nil
}

// PlayerState returns the latest player record. Lightweight wrapper over
// ShowPlayer so test code doesn't need to know about the harness binary.
func (s *Session) PlayerState(ctx context.Context) (*PlayerRecord, error) {
	if s == nil || s.PlayerID == "" {
		return nil, errors.New("session: no player bound")
	}
	return ShowPlayer(ctx, s.PlayerID)
}

// CloseViaUI closes the playback screen the way a user would — driving
// the app's own back navigation through the launcher — so the app emits
// its real client play_end and the play shows up cleanly ended in the
// sessions view (#627) rather than dangling in_progress after a hard
// terminate. Re-entering playback (the next test's launch) rotates the
// play_id, so back-to-back tests get distinct, bounded plays; multiple
// play_ids within one test are fine.
//
// No-op for launchers that can't drive the UI (CLI / Manual). Call this
// in a test's cleanup BEFORE Launcher.Close() tears the session down.
func (s *Session) CloseViaUI(ctx context.Context) error {
	if s == nil {
		return nil
	}
	c, ok := s.Launcher.(UICloser)
	if !ok {
		return nil
	}
	return c.ClosePlaybackViaUI(ctx, s.Device)
}

// Release deletes the proxy session for this player so its slot frees
// immediately instead of lingering until the 5-min idle reaper.
//
// CRITICAL under config-on-connect: every run mints a fresh player_id, so
// without an explicit release a handful of back-to-back runs exhausts the
// small session pool (4 slots) and the next run 503s "session limit reached".
// The delete also clears the port's transport-fault loop and records the
// session end (the ClickHouse archive is unaffected — it streams independently).
// Best-effort; call in a test's cleanup after CloseViaUI, before Launcher.Close().
func (s *Session) Release(ctx context.Context) error {
	if s == nil || s.PlayerID == "" {
		return nil
	}
	_, err := runHarness(ctx, "players", "rm", "--yes", s.PlayerID)
	return err
}

// ReleaseDevice fully releases the device after a run — e.g. terminating
// WebDriverAgent so iOS's "Automation Running" overlay clears. Opt-in:
// runs only when CHAR_RELEASE_DEVICE=1, because Appium keeps WDA resident
// between sessions by design for fast reuse across back-to-back tests, so
// killing it costs a WDA (re)launch on the next run. No-op for launchers
// that can't release the device (CLI / Manual). Call in a test's cleanup
// AFTER Launcher.Close().
func (s *Session) ReleaseDevice(ctx context.Context) error {
	if s == nil || os.Getenv("CHAR_RELEASE_DEVICE") != "1" {
		return nil
	}
	r, ok := s.Launcher.(DeviceReleaser)
	if !ok {
		return nil
	}
	return r.ReleaseDevice(ctx, s.Device)
}

// WaitForHeartbeat polls the harness until the bound player reports
// last_seen_at within 60s of now, or until ctx fires. Used by every
// Launch implementation right before it returns the Session.
func (s *Session) WaitForHeartbeat(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		rec, err := s.PlayerState(ctx)
		if err == nil && rec.IsHeartbeating(time.Now()) {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("wait for heartbeat: %w", err)
			}
			return fmt.Errorf("wait for heartbeat: player %s not seen within %s",
				s.PlayerID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
