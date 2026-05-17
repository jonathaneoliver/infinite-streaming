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
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const networkUsage = `harness network <target> [flags]

GET /api/v2/players/<id>/network — live HAR-shaped network log
(proxy-side ring buffer).

Flags:
  --limit N      cap entries returned (default server-side)
  --raw          print the raw JSON envelope (paging info + items)

For historical HAR (after a play ends), use 'harness archive network <play_id>'.
`

func cmdNetwork(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(networkUsage)
	}
	fs := flag.NewFlagSet("network", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max entries")
	raw := fs.Bool("raw", false, "raw JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	params := &proxy.GetApiV2PlayersPlayerIdNetworkParams{}
	if *limit > 0 {
		l := *limit
		params.Limit = &l
	}
	body, err := client.PlayerNetwork(ctx, pid, params)
	if err != nil {
		return err
	}
	if asJSON || *raw {
		_, err := os.Stdout.Write(body)
		return err
	}
	// Pretty: decode the envelope and render each item with the
	// network-row formatter (same as tail's network row, conveniently).
	var page struct {
		Items []tailNetworkRow `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		// Fall back to raw if shape changed.
		fmt.Println(string(body))
		return nil
	}
	if len(page.Items) == 0 {
		fmt.Println("no entries")
		return nil
	}
	for _, row := range page.Items {
		fmt.Println(formatNetworkRow(row))
	}
	_ = format.JSON // keep unused import lint quiet if formatNetworkRow path skips JSON
	return nil
}
