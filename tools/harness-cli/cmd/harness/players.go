package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const playersUsage = `harness players <subcommand>

Subcommands:
  list                          GET /api/v2/players (typed)
  show <target>                 GET /api/v2/players/{id} — full record + ETag
  create [flags]                POST a new player; --manifest URL, --labels k=v,...
  rm <target> [--yes]           DELETE one player
  prune [--idle-for DUR] [--yes] DELETE every player (or only those
                                with last_seen older than --idle-for)
`

func cmdPlayers(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(playersUsage)
	}
	switch args[0] {
	case "list":
		return cmdPlayersList(client, args[1:], asJSON)
	case "show":
		return cmdPlayersShow(client, args[1:], asJSON)
	case "create":
		return cmdPlayersCreate(client, args[1:], asJSON)
	case "rm", "remove", "delete":
		return cmdPlayersRm(client, args[1:], asJSON)
	case "prune":
		return cmdPlayersPrune(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown players subcommand: %s\n\n%s", args[0], playersUsage)
	}
}

func cmdPlayersList(client *api.Client, args []string, asJSON bool) error {
	if len(args) > 0 {
		return errors.New("usage: harness players list")
	}
	ctx := context.Background()
	players, err := client.Players(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, players)
	}
	format.PlayersTable(os.Stdout, players)
	return nil
}

func cmdPlayersShow(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness players show <target>")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		// Allow bypassing resolve only when the input parses as a
		// UUID — used for players that have just disconnected from
		// the live list. Anything else is a target typo and should
		// surface the resolver's "no match" / "ambiguous" message.
		if _, perr := uuid.Parse(args[0]); perr != nil {
			return err
		}
		pid = args[0]
	}
	player, etag, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	if asJSON {
		// Surface the ETag as a sibling so JSON consumers can capture it
		// alongside the record body. Wrap in an envelope rather than
		// mutating the typed PlayerRecord.
		return format.JSON(os.Stdout, map[string]any{
			"player": player,
			"etag":   etag,
		})
	}
	// Human-readable: pretty-print the record + an ETag line at the
	// bottom (since etag isn't part of the record JSON).
	if err := format.JSON(os.Stdout, player); err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "\nETag (for next PATCH): %q\n", etag)
	return nil
}

func cmdPlayersCreate(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("players create", flag.ContinueOnError)
	manifest := fs.String("manifest", "", "manifest_url to seed onto the player")
	labelsCSV := fs.String("labels", "", "k=v,k=v label pairs")
	synthetic := fs.Bool("synthetic", false, "mark player as synthetic (test-only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	req := proxy.PlayerCreateRequest{}
	if *manifest != "" {
		req.ManifestUrl = manifest
	}
	if *synthetic {
		req.Synthetic = synthetic
	}
	if *labelsCSV != "" {
		labels, err := parseLabels(*labelsCSV)
		if err != nil {
			return err
		}
		l := proxy.Labels(labels)
		req.Labels = &l
	}
	rec, etag, err := client.CreatePlayer(context.Background(), req)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player": rec, "etag": etag})
	}
	fmt.Printf("created %s (etag %s)\n", rec.Id, shortRev(etag))
	return nil
}

func cmdPlayersRm(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("players rm", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return errors.New("usage: harness players rm <target> [--yes]")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, rest[0])
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Printf("delete player %s? [y/N] ", pid)
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			return errors.New("aborted")
		}
	}
	if err := client.DeletePlayer(ctx, pid, "players rm"); err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "deleted": true})
	}
	fmt.Printf("deleted %s\n", pid)
	return nil
}

func cmdPlayersPrune(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("players prune", flag.ContinueOnError)
	idleFor := fs.Duration("idle-for", 0, "only delete players idle at least this long (default 0 = nuke all)")
	yes := fs.Bool("yes", false, "skip confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	players, err := client.Players(ctx)
	if err != nil {
		return err
	}
	var victims []string
	now := time.Now()
	for _, p := range players {
		if *idleFor > 0 {
			if p.LastSeenAt == nil || now.Sub(*p.LastSeenAt) < *idleFor {
				continue
			}
		}
		victims = append(victims, p.Id.String())
	}
	if len(victims) == 0 {
		fmt.Println("nothing to prune")
		return nil
	}
	if !*yes {
		fmt.Printf("delete %d player(s)? [y/N] ", len(victims))
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			return errors.New("aborted")
		}
	}
	// If --idle-for unset and victim count == total, use the bulk
	// DELETE which is atomic and one round-trip. Otherwise iterate.
	if *idleFor == 0 && len(victims) == len(players) {
		if err := client.DeleteAllPlayers(ctx, fmt.Sprintf("players prune (all %d)", len(victims))); err != nil {
			return err
		}
	} else {
		for _, pid := range victims {
			if err := client.DeletePlayer(ctx, pid, "players prune"); err != nil {
				return fmt.Errorf("delete %s: %w", pid, err)
			}
		}
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"deleted": victims})
	}
	fmt.Printf("pruned %d player(s)\n", len(victims))
	return nil
}

// parseLabels turns "k1=v1,k2=v2" into a map. Empty values are
// allowed (`--labels test=` is valid CH "label exists with empty
// value"); empty keys are rejected.
func parseLabels(csv string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(csv, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("bad label %q (want k=v)", pair)
		}
		out[k] = v
	}
	return out, nil
}
