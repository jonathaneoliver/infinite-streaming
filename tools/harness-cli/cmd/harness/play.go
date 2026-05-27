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

const playUsage = `harness play <subcommand>

Subcommands:
  show <play_id>                       GET /api/v2/plays/<play_id>
  patch <play_id> --shape ... etc      PATCH play-scoped overrides
                                       (shape/fault_rules/content/labels)
                                       Auto-cleared on play end.

Convenience: the play_id can be derived from a target via 'players
show <target>' → current_play.id, or 'archive plays' for ended plays.
`

func cmdPlay(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(playUsage)
	}
	switch args[0] {
	case "show":
		return cmdPlayShow(client, args[1:], asJSON)
	case "patch":
		return cmdPlayPatch(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown play subcommand: %s\n\n%s", args[0], playUsage)
	}
}

func cmdPlayShow(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness play show <play_id>")
	}
	rec, etag, err := client.Play(context.Background(), args[0])
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"play": rec, "etag": etag})
	}
	if err := format.JSON(os.Stdout, rec); err != nil {
		return err
	}
	fmt.Printf("\nETag: %q\n", etag)
	return nil
}

func cmdPlayPatch(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness play patch <play_id> [--rate N] [--delay N] [--loss N] [--clear-shape] [--labels k=v,...]")
	}
	fs := flag.NewFlagSet("play patch", flag.ContinueOnError)
	rate := fs.Float64("rate", -1, "shape.rate_mbps")
	delay := fs.Float64("delay", -1, "shape.delay_ms")
	loss := fs.Float64("loss", -1, "shape.loss_pct")
	clearShape := fs.Bool("clear-shape", false, "send shape:null on the play scope")
	labelsCSV := fs.String("labels", "", "k=v,k=v")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	playID := args[0]
	patch := map[string]any{}
	if *clearShape {
		patch["shape"] = nil
	} else if *rate >= 0 || *delay >= 0 || *loss >= 0 {
		shape := map[string]any{}
		if *rate >= 0 {
			shape["rate_mbps"] = *rate
		}
		if *delay >= 0 {
			shape["delay_ms"] = *delay
		}
		if *loss >= 0 {
			shape["loss_pct"] = *loss
		}
		patch["shape"] = shape
	}
	if *labelsCSV != "" {
		labels, err := parseLabels(*labelsCSV)
		if err != nil {
			return err
		}
		patch["labels"] = labels
	}
	if len(patch) == 0 {
		return errors.New("nothing to patch — pass --rate/--delay/--loss/--clear-shape/--labels")
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	newETag, err := client.PatchPlay(context.Background(), playID, "play patch", body)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"play_id": playID, "patch": patch, "etag": newETag})
	}
	fmt.Printf("patched play %s (etag %s)\n", playID, shortRev(newETag))
	return nil
}
