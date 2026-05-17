package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
)

const playersUsage = `harness players <subcommand>

Subcommands:
  list                          GET /api/v2/players (typed)
  show <player_id>              GET /api/v2/players/<id>?include=raw — full
                                record + the current ETag (for the next PATCH)
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
		return errors.New("usage: harness players show <player_id>")
	}
	ctx := context.Background()
	player, etag, err := client.Player(ctx, args[0])
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
