package runner

import (
	"context"
	"errors"
	"fmt"
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
