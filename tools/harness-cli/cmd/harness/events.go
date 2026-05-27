package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
)

const eventsUsage = `harness events <target|all> [flags]

Streams lifecycle events from /api/v2/events (proxy SSE).

Flags:
  --type T,...    comma-separated event types to filter (default: all)
                  player.created, player.updated, player.deleted,
                  play.started, play.updated, play.ended,
                  play.network.entry, heartbeat
  --raw           print raw frame JSON (default: one-liner)

Examples:
  harness events ipad
  harness events all --type play.started,play.ended
  harness events ipad --raw
`

func cmdEvents(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(eventsUsage)
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	typeFilter := fs.String("type", "", "comma-separated event types")
	raw := fs.Bool("raw", false, "print raw frame JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var playerID string
	if target := args[0]; target != "all" && target != "*" {
		pid, err := client.Resolve(ctx, target)
		if err != nil {
			return err
		}
		playerID = pid
	}

	params := api.EventsParams{
		PlayerID: playerID,
		Types:    splitCSV(*typeFilter),
	}

	fmt.Fprintf(os.Stderr, "tailing events player=%s — Ctrl-C to stop\n", labelOrAll(playerID))

	err := client.Events(ctx, params, func(f api.SSEFrame) error {
		if *raw {
			fmt.Printf("event:%s id:%s data:%s\n", f.Event, f.ID, f.Data)
			return nil
		}
		if f.Event == "heartbeat" || f.Event == "" {
			return nil
		}
		if asJSON {
			fmt.Println(f.Data)
			return nil
		}
		// Default human view: one-liner with event type + truncated
		// data preview. The data is event-type-shaped (PlayerRecord,
		// PlayRecord, NetworkLogEntry, …) so a typed projection per
		// type would be Phase 7 polish; for now show the operator
		// enough to see "something interesting is happening".
		preview := strings.ReplaceAll(f.Data, "\n", " ")
		if len(preview) > 160 {
			preview = preview[:157] + "..."
		}
		fmt.Printf("%-20s %s\n", f.Event, preview)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
