package runner

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ApplyRate sets the network rate cap on the bound player, in Mbps. A rate
// of zero means "uncap" — the proxy clears the tc rule for that session.
// Always sends pattern: null implicitly via the harness `--rate` slider
// mode, which is the same path the dashboard slider uses.
func (s *Session) ApplyRate(ctx context.Context, rateMbps float64) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("apply rate: no player bound")
	}
	if rateMbps < 0 || math.IsNaN(rateMbps) || math.IsInf(rateMbps, 0) {
		return fmt.Errorf("apply rate: invalid rate %v", rateMbps)
	}
	args := []string{"shape", s.PlayerID, "--rate", strconv.FormatFloat(rateMbps, 'f', -1, 64)}
	if _, err := runHarness(ctx, args...); err != nil {
		return err
	}
	return nil
}

// ClearShape removes all shaping (rate cap, delay, loss, pattern). Used
// at test teardown to leave the proxy in a clean state.
func (s *Session) ClearShape(ctx context.Context) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("clear shape: no player bound")
	}
	_, err := runHarness(ctx, "shape", s.PlayerID, "--clear")
	return err
}

// SetSegmentTimeout arms the proxy's server-side response timeout
// on segment fetches for this player. Setting active=0 disables.
// Used by the abort characterization test to force a server-driven
// abort BEFORE iOS's own ~30s segment timeout fires.
//
// The proxy applies the timeout to active in-flight transfers
// (any segment fetch taking longer than `active` is closed by the
// server, surfacing as a transfer_abandoned / fault_action row
// on network_requests).
func (s *Session) SetSegmentTimeout(ctx context.Context, active time.Duration) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("set segment timeout: no player bound")
	}
	args := []string{"timeouts", s.PlayerID}
	if active <= 0 {
		args = append(args, "--clear")
	} else {
		args = append(args,
			"--active", strconv.Itoa(int(active.Seconds())),
			"--applies-segments")
	}
	_, err := runHarness(ctx, args...)
	return err
}

// ArmFault posts a once-only fault rule on the bound player. The
// next request matching `kind` (e.g. "segment") triggers the named
// `shape` (e.g. "request_first_byte_hang"). Frequency defaults to 1
// (every matching request — the rule is then cleared via ClearFaults).
//
// Used by the abort characterization test to inject a single HTTP-
// layer fault and observe the player's recovery.
func (s *Session) ArmFault(ctx context.Context, shape, kind string, frequency int) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("arm fault: no player bound")
	}
	if shape == "" || kind == "" {
		return fmt.Errorf("arm fault: shape and kind required")
	}
	if frequency <= 0 {
		frequency = 1
	}
	args := []string{
		"fault", "add", s.PlayerID,
		"--type", shape,
		"--kind", kind,
		"--frequency", strconv.Itoa(frequency),
	}
	_, err := runHarness(ctx, args...)
	return err
}

// ClearFaults wipes all fault rules from the bound player. The
// abort test calls this between cycles so each cycle starts from
// a known clean state.
func (s *Session) ClearFaults(ctx context.Context) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("clear faults: no player bound")
	}
	_, err := runHarness(ctx, "fault", "clear", s.PlayerID)
	return err
}

// LabelPlay merges k=v labels onto the bound PLAYER (not the play).
// Player labels work via PATCH /api/v2/players/{id} which exists from
// the moment the player registers — unlike /api/v2/plays/{id} which
// 404s briefly during a launch/relaunch transition.
//
// The forwarder side: when the player's labels change, the proxy emits
// a `label_changed` control_event carrying the labels payload as JSON
// in Info. The forwarder parses it and stamps each `<key>_<value>` as
// an `info=<…>` entry into the row's labels[] column. That row is
// keyed by the current play_id, so end-to-end the labels end up on
// THIS play in the archive — even though we patched the player.
//
// Idempotent (additive merge). No-op when labels is empty.
func (s *Session) LabelPlay(ctx context.Context, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("label: no player bound")
	}
	// `harness labels set` is the player-scope merge — additive, exists
	// for the lifetime of the player record.
	args := []string{"labels", "set", s.PlayerID}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := labels[k]
		if strings.ContainsAny(k, ",=") || strings.ContainsAny(v, ",=") {
			return fmt.Errorf("label: key/value %q=%q contains forbidden , or =", k, v)
		}
		args = append(args, k+"="+v)
	}
	_, err := runHarness(ctx, args...)
	return err
}
