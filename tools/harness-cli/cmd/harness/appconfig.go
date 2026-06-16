package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
)

const appConfigUsage = `harness app-config <target> [flags]

PATCH the player's client-side app_config (#800). These are behaviour knobs
the PLAYER applies at its next play boundary — the per-play, no-restart
counterpart to the cold-start launch args (is.segment etc., #797). The proxy
stores them on the session and surfaces them on GET /api/sessions; the player
overlays any set field onto its own state when it opens the next play.

Use this to reconfigure a RUNNING app between plays without a cold relaunch.
(The first play of a fresh launch has no session yet — use config-on-connect
'app.<field>' bootstrap args for that; this command targets subsequent plays.)

Flags:
  --segment LADDER     ll | s2 | s6
  --protocol PROTO     hls | dash
  --live-offset SEC    seconds behind live edge (>=0; 0 = manifest/Go-Live decides)
  --peak-bitrate MBPS  ABR ceiling in Mbps (>=0; 0 = no cap)
  --clear              send {"app_config": null} (wipe all client config)

Only the flags you pass are written (JSON Merge Patch); omit a field to leave
the player's current value untouched.
`

func cmdAppConfig(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(appConfigUsage)
	}
	fs := flag.NewFlagSet("app-config", flag.ContinueOnError)
	segment := fs.String("segment", "", "")
	protocol := fs.String("protocol", "", "")
	liveOffset := fs.Float64("live-offset", 0, "")
	peakBitrate := fs.Int("peak-bitrate", 0, "")
	clear := fs.Bool("clear", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// fs.Visit reports only flags actually passed, so a 0 the user typed is
	// distinguishable from an unset field (which must stay absent from the
	// merge patch, per JSON Merge Patch semantics).
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	body, err := buildAppConfigBody(*segment, *protocol, *liveOffset, *peakBitrate, set, *clear)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	newETag, err := client.PatchRawWithSnapshot(ctx, pid, "app-config", body)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid, "app_config": json.RawMessage(body), "etag": newETag,
		})
	}
	if *clear {
		fmt.Printf("cleared app_config on %s (etag %s)\n", pid, shortRev(newETag))
	} else {
		fmt.Printf("patched app_config on %s: %s (etag %s)\n", pid, string(body), shortRev(newETag))
	}
	return nil
}

// buildAppConfigBody turns the parsed flags into the PATCH body. Pure (no
// network) so it's unit-testable. `set` marks which numeric/string flags were
// actually passed (via flag.FlagSet.Visit) so an explicit 0 is written while an
// omitted field stays absent. --clear wins and emits {"app_config": null}.
// Enum values are validated here so a bad arg fails fast rather than 400ing.
func buildAppConfigBody(segment, protocol string, liveOffset float64, peakBitrate int, set map[string]bool, clear bool) ([]byte, error) {
	if clear {
		return []byte(`{"app_config": null}`), nil
	}
	ac := map[string]any{}
	if set["segment"] {
		switch segment {
		case "ll", "s2", "s6":
			ac["segment"] = segment
		default:
			return nil, fmt.Errorf("invalid --segment %q (want ll|s2|s6)", segment)
		}
	}
	if set["protocol"] {
		switch protocol {
		case "hls", "dash":
			ac["protocol"] = protocol
		default:
			return nil, fmt.Errorf("invalid --protocol %q (want hls|dash)", protocol)
		}
	}
	if set["live-offset"] {
		if liveOffset < 0 {
			return nil, fmt.Errorf("--live-offset must be >= 0 (got %v)", liveOffset)
		}
		ac["live_offset_s"] = liveOffset
	}
	if set["peak-bitrate"] {
		if peakBitrate < 0 {
			return nil, fmt.Errorf("--peak-bitrate must be >= 0 (got %d)", peakBitrate)
		}
		ac["peak_bitrate_mbps"] = peakBitrate
	}
	if len(ac) == 0 {
		return nil, errors.New("nothing to do — pass --segment / --protocol / --live-offset / --peak-bitrate, or --clear")
	}
	return json.Marshal(map[string]any{"app_config": ac})
}
