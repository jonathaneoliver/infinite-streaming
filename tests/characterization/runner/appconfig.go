package runner

import (
	"context"
	"fmt"
	"strconv"
)

// AppConfig is the client-side, per-play behaviour config the harness can push
// to a RUNNING player (#800): the player applies it at its NEXT play boundary
// with no cold relaunch — the per-play counterpart to the is.* launch args
// (#797). Empty strings / nil pointers are omitted from the patch (JSON Merge
// Patch), so a partial AppConfig leaves the player's other values untouched.
type AppConfig struct {
	Segment         string   // "" | ll | s2 | s6
	Protocol        string   // "" | hls | dash
	LiveOffsetS     *float64 // seconds behind live; nil = leave unchanged
	PeakBitrateMbps *int     // ABR ceiling Mbps; nil = leave unchanged
}

// ApplyAppConfig PATCHes the bound player's app_config via the harness CLI, so
// the next play this app opens picks up the new client-side config WITHOUT a
// cold relaunch. The session must already exist (player bound + first play
// made) — the proxy has no session to carry app_config before the player's
// first bootstrap request, so this targets reconfiguration of a running app
// between plays, not the very first play (use config-on-connect for that).
func (s *Session) ApplyAppConfig(ctx context.Context, cfg AppConfig) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("apply app-config: no player bound")
	}
	args := []string{"app-config", s.PlayerID}
	if cfg.Segment != "" {
		args = append(args, "--segment", cfg.Segment)
	}
	if cfg.Protocol != "" {
		args = append(args, "--protocol", cfg.Protocol)
	}
	if cfg.LiveOffsetS != nil {
		args = append(args, "--live-offset", strconv.FormatFloat(*cfg.LiveOffsetS, 'f', -1, 64))
	}
	if cfg.PeakBitrateMbps != nil {
		args = append(args, "--peak-bitrate", strconv.Itoa(*cfg.PeakBitrateMbps))
	}
	if len(args) == 2 { // only "app-config <pid>" — nothing to set
		return fmt.Errorf("apply app-config: empty AppConfig")
	}
	_, err := runHarness(ctx, args...)
	return err
}

// ClearAppConfig wipes all client-side app_config on the bound player, so the
// next play falls back to the app's own (launch-arg / Settings) values.
func (s *Session) ClearAppConfig(ctx context.Context) error {
	if s == nil || s.PlayerID == "" {
		return fmt.Errorf("clear app-config: no player bound")
	}
	_, err := runHarness(ctx, "app-config", s.PlayerID, "--clear")
	return err
}
