package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/forwarder"
)

// parsePlayID maps the operator-supplied string to a typed UUID for
// the generated forwarder client. Surfaces parse errors as part of
// the command output rather than panicking.
func parsePlayID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid play_id %q: %w", s, err)
	}
	return id, nil
}

const archiveUsage = `harness archive <subcommand>

Subcommands (all read-only — forwarder /analytics/api/v2/*):
  plays [--limit N] [--from ISO] [--to ISO] [--classification C]
        [--player-id UUID] [--play-id UUID]
                                  list plays (one row per archived playback)
  play <play_id>                  one play + _links
  aggregate [--from --to --classification]
                                  aggregate stats across plays
  snapshots <play_id> [--limit]
  network <play_id> [--limit]
  events <play_id> [--limit]
  heatmap <play_id>
  bundle <play_id> --out PATH     download play bundle ZIP

Pass --json globally for raw responses suitable for piping.
`

func cmdArchive(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(archiveUsage)
	}
	switch args[0] {
	case "plays":
		return cmdArchivePlays(client, args[1:], asJSON)
	case "play":
		return cmdArchivePlay(client, args[1:], asJSON)
	case "aggregate":
		return cmdArchiveAggregate(client, args[1:], asJSON)
	case "snapshots":
		return cmdArchiveSnapshots(client, args[1:], asJSON)
	case "network":
		return cmdArchiveNetwork(client, args[1:], asJSON)
	case "events":
		return cmdArchiveEvents(client, args[1:], asJSON)
	case "heatmap":
		return cmdArchiveHeatmap(client, args[1:], asJSON)
	case "bundle":
		return cmdArchiveBundle(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown archive subcommand: %s\n\n%s", args[0], archiveUsage)
	}
}

// printOrJSON writes raw bytes to stdout when --json, otherwise
// pretty-prints by re-indenting JSON. Falls back to raw if the body
// isn't valid JSON.
func printOrJSON(body []byte, asJSON bool) error {
	if asJSON {
		_, err := os.Stdout.Write(body)
		return err
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		_, err := os.Stdout.Write(body)
		return err
	}
	return format.JSON(os.Stdout, v)
}

func parseLimit(fs *flag.FlagSet, args []string) (*int, error) {
	limit := fs.Int("limit", 0, "max items returned")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *limit <= 0 {
		return nil, nil
	}
	v := *limit
	return &v, nil
}

func cmdArchivePlays(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("archive plays", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max plays")
	classification := fs.String("classification", "", "interesting|other|favourite")
	playerID := fs.String("player-id", "", "filter to one player_id (UUID)")
	playID := fs.String("play-id", "", "filter to one play_id (UUID)")
	from := fs.String("from", "", "ISO 8601 lower bound (e.g. 2026-05-17T00:00:00Z)")
	to := fs.String("to", "", "ISO 8601 upper bound (exclusive)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := &forwarder.GetApiV2PlaysParams{}
	if *limit > 0 {
		v := *limit
		params.Limit = &v
	}
	if *classification != "" {
		c := forwarder.GetApiV2PlaysParamsClassification(*classification)
		params.Classification = &c
	}
	if *playerID != "" {
		pid, err := uuid.Parse(*playerID)
		if err != nil {
			return fmt.Errorf("invalid --player-id %q: %w", *playerID, err)
		}
		params.PlayerId = &pid
	}
	if *playID != "" {
		pid, err := uuid.Parse(*playID)
		if err != nil {
			return fmt.Errorf("invalid --play-id %q: %w", *playID, err)
		}
		params.PlayId = &pid
	}
	if *from != "" {
		t, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return fmt.Errorf("invalid --from %q (need RFC3339, e.g. 2026-05-17T00:00:00Z): %w", *from, err)
		}
		params.From = &t
	}
	if *to != "" {
		t, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return fmt.Errorf("invalid --to %q (need RFC3339): %w", *to, err)
		}
		params.To = &t
	}
	body, err := client.ArchivePlays(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchivePlay(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness archive play <play_id>")
	}
	body, err := client.ArchivePlay(context.Background(), args[0])
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchiveAggregate(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("archive aggregate", flag.ContinueOnError)
	classification := fs.String("classification", "", "interesting|other|favourite")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := &forwarder.GetApiV2PlaysAggregateParams{}
	if *classification != "" {
		c := forwarder.GetApiV2PlaysAggregateParamsClassification(*classification)
		params.Classification = &c
	}
	body, err := client.ArchivePlaysAggregate(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchiveSnapshots(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness archive snapshots <play_id> [--limit N]")
	}
	playID := args[0]
	fs := flag.NewFlagSet("archive snapshots", flag.ContinueOnError)
	limit, err := parseLimit(fs, args[1:])
	if err != nil {
		return err
	}
	params := &forwarder.GetApiV2SnapshotsParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	if limit != nil {
		params.Limit = limit
	}
	body, err := client.ArchiveSnapshots(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchiveNetwork(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness archive network <play_id> [--limit N]")
	}
	playID := args[0]
	fs := flag.NewFlagSet("archive network", flag.ContinueOnError)
	limit, err := parseLimit(fs, args[1:])
	if err != nil {
		return err
	}
	params := &forwarder.GetApiV2NetworkRequestsParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	if limit != nil {
		params.Limit = limit
	}
	body, err := client.ArchiveNetworkRequests(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchiveEvents(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness archive events <play_id> [--limit N]")
	}
	playID := args[0]
	fs := flag.NewFlagSet("archive events", flag.ContinueOnError)
	limit, err := parseLimit(fs, args[1:])
	if err != nil {
		return err
	}
	params := &forwarder.GetApiV2SessionEventsParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	if limit != nil {
		params.Limit = limit
	}
	body, err := client.ArchiveSessionEvents(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchiveHeatmap(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness archive heatmap <play_id>")
	}
	playID := args[0]
	params := &forwarder.GetApiV2SessionHeatmapParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	body, err := client.ArchiveSessionHeatmap(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdArchiveBundle(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("archive bundle", flag.ContinueOnError)
	out := fs.String("out", "", "output path (required; bundle is a ZIP)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 || *out == "" {
		return errors.New("usage: harness archive bundle <play_id> --out PATH")
	}
	playID := rest[0]
	body, err := client.ArchivePlayBundle(context.Background(), playID)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, body)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"out": *out, "bytes": n})
	}
	fmt.Printf("wrote %d bytes → %s\n", n, *out)
	return nil
}
