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

// ApplyPattern arms a throughput pattern (pyramid / ramp_up / ramp_down /
// square_wave / transient_shock) on the bound player. The harness builds the
// step ladder from the player's CURRENT manifest variants, so the master must
// already be fetched — call WaitForManifest first. This is the same path the
// dashboard pattern panel and the characterization modes use; it's how the
// sweep drives a config-class pattern recipe's bandwidth motion.
func (s *Session) ApplyPattern(ctx context.Context, pattern string, stepSeconds, marginPct int) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("apply pattern: no player bound")
	}
	if pattern == "" {
		return fmt.Errorf("apply pattern: empty pattern")
	}
	args := []string{
		"shape", s.PlayerID,
		"--pattern", pattern,
		"--step-seconds", strconv.Itoa(stepSeconds),
		"--margin", strconv.Itoa(marginPct),
	}
	_, err := runHarness(ctx, args...)
	return err
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

// ResetProxy clears shape + fault rules + content to a clean baseline (the
// comprehensive `harness reset`). Called at test START to drop any carry-over from
// a prior run that reused this player_id. Manual sessions never call this, so the
// proxy's reattach carry-over default is untouched.
func (s *Session) ResetProxy(ctx context.Context) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("reset proxy: no player bound")
	}
	_, err := runHarness(ctx, "reset", s.PlayerID)
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

// ArmFault posts a one-shot fault rule on the bound player. The
// next request matching `kind` (e.g. "segment") triggers the named
// `shape` (e.g. "request_first_byte_hang"); the rule does not
// repeat (frequency=0 means "no next cycle"). Caller clears the
// rule via ClearFaults after the observation window.
//
// Cadence semantics for one-shot:
//   - consecutive=1: fault is "on" for one matching request.
//   - frequency=0:   no next cycle; the fault fires once and stays off.
//
// urlPatterns optionally scopes the fault to URLs whose pathBase
// or pathParent matches any of the supplied substrings. Empty
// patterns ⇒ match every URL on the surface. The abort test
// passes the list of video variant directory names so audio
// segments aren't affected.
//
// Used by the abort characterization test to inject a single HTTP-
// layer fault and observe the player's recovery.
func (s *Session) ArmFault(ctx context.Context, shape, kind string, urlPatterns ...string) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("arm fault: no player bound")
	}
	if shape == "" || kind == "" {
		return fmt.Errorf("arm fault: shape and kind required")
	}
	args := []string{
		"fault", "add", s.PlayerID,
		"--type", shape,
		"--kind", kind,
		"--frequency", "0",
		"--consecutive", "1",
		"--mode", "requests",
	}
	if len(urlPatterns) > 0 {
		args = append(args, "--url-substr", strings.Join(urlPatterns, ","))
	}
	_, err := runHarness(ctx, args...)
	return err
}

// ArmFaultRepeating posts a SUSTAINED fault rule on the bound player: the
// `shape` (e.g. "404", "corrupted", "request_first_byte_reset") fires on
// `consecutive` matching `kind` requests in a row and re-arms for `frequency`
// cycles, so every request in the window is affected — unlike ArmFault's
// one-shot. Used by the fault-recovery probe to keep a fault on long enough to
// drive AVPlayer toward a .failed state, then ClearFaults.
func (s *Session) ArmFaultRepeating(ctx context.Context, shape, kind string, consecutive, frequency int, urlPatterns ...string) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("arm fault: no player bound")
	}
	if shape == "" || kind == "" {
		return fmt.Errorf("arm fault: shape and kind required")
	}
	args := []string{
		"fault", "add", s.PlayerID,
		"--type", shape,
		"--kind", kind,
		"--frequency", strconv.Itoa(frequency),
		"--consecutive", strconv.Itoa(consecutive),
		"--mode", "requests",
	}
	if len(urlPatterns) > 0 {
		args = append(args, "--url-substr", strings.Join(urlPatterns, ","))
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
