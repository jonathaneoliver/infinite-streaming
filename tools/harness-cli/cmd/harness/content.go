package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const contentUsage = `harness content <target> [flags]

PATCH the player's master-playlist content manipulations (applied at
manifest-serve time on the next master fetch).

Flags:
  --strip-codecs               remove CODECS from EXT-X-STREAM-INF
  --strip-average-bandwidth    remove AVERAGE-BANDWIDTH
  --overstate-bandwidth        inflate BANDWIDTH by 10%
  --live-offset SEC            live-edge offset window
  --allowed-variants url[,url] whitelist variant URIs (others stripped)
  --show                       print current and exit
  --clear                      send {"content": null}
`

func cmdContent(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(contentUsage)
	}
	fs := flag.NewFlagSet("content", flag.ContinueOnError)
	stripCodecs := fs.Bool("strip-codecs", false, "")
	stripAvgBw := fs.Bool("strip-average-bandwidth", false, "")
	overstateBw := fs.Bool("overstate-bandwidth", false, "")
	liveOffset := fs.Int("live-offset", -1, "")
	allowedCSV := fs.String("allowed-variants", "", "")
	show := fs.Bool("show", false, "")
	clear := fs.Bool("clear", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, args[0])
	if err != nil {
		return err
	}
	if *show {
		rec, _, err := client.Player(ctx, pid)
		if err != nil {
			return err
		}
		if rec.Content == nil {
			fmt.Println("no content manipulations")
			return nil
		}
		return format.JSON(os.Stdout, rec.Content)
	}
	if *clear {
		newETag, err := client.PatchRawWithSnapshot(ctx, pid, "content clear", []byte(`{"content": null}`))
		if err != nil {
			return err
		}
		fmt.Printf("cleared content on %s (etag %s)\n", pid, shortRev(newETag))
		return nil
	}
	cm := proxy.ContentManipulation{}
	touched := false
	if *stripCodecs {
		cm.StripCodecs = stripCodecs
		touched = true
	}
	if *stripAvgBw {
		cm.StripAverageBandwidth = stripAvgBw
		touched = true
	}
	if *overstateBw {
		cm.OverstateBandwidth = overstateBw
		touched = true
	}
	if *liveOffset >= 0 {
		lo := proxy.ContentManipulationLiveOffset(*liveOffset)
		cm.LiveOffset = &lo
		touched = true
	}
	if *allowedCSV != "" {
		variants := strings.Split(*allowedCSV, ",")
		cm.AllowedVariants = &variants
		touched = true
	}
	if !touched {
		return errors.New("nothing to do — pass a content flag, or --show / --clear")
	}
	patch := proxy.PlayerPatch{Content: &cm}
	newETag, err := client.PatchPlayer(ctx, pid, "content", patch)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "content": cm, "etag": newETag})
	}
	fmt.Printf("patched content on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}
