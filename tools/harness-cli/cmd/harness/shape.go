package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const shapeUsage = `harness shape <target> [flags]

Flags (any subset; omitted fields are not modified, --clear wipes all):
  --rate FLOAT     rate cap in Mbps (e.g. 1.5)
  --delay FLOAT    one-way delay in ms (e.g. 200)
  --loss FLOAT     packet loss %% (e.g. 0.5, range 0–100)
  --clear          send {"shape": null} — wipes rate/delay/loss/pattern/transport
  --show           print current shape without modifying

Examples:
  harness shape ipad --rate 1.5 --delay 100
  harness shape ipad --clear
  harness shape ipad --show
`

func cmdShape(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(shapeUsage)
	}
	fs := flag.NewFlagSet("shape", flag.ContinueOnError)
	rate := fs.Float64("rate", -1, "rate cap Mbps")
	delay := fs.Float64("delay", -1, "delay ms")
	loss := fs.Float64("loss", -1, "loss %")
	clear := fs.Bool("clear", false, "send {shape:null}")
	show := fs.Bool("show", false, "print current shape, don't modify")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	target := args[0]
	ctx := context.Background()
	pid, err := client.Resolve(ctx, target)
	if err != nil {
		return err
	}

	if *show {
		rec, etag, err := client.Player(ctx, pid)
		if err != nil {
			return err
		}
		if asJSON {
			return format.JSON(os.Stdout, map[string]any{
				"player_id": pid,
				"shape":     rec.Shape,
				"etag":      etag,
			})
		}
		if rec.Shape == nil {
			fmt.Printf("%s: no shaping\n", pid)
		} else {
			return format.JSON(os.Stdout, rec.Shape)
		}
		return nil
	}

	if *clear {
		if *rate >= 0 || *delay >= 0 || *loss >= 0 {
			return errors.New("--clear is mutually exclusive with --rate/--delay/--loss")
		}
		newETag, err := client.ClearShape(ctx, pid, "shape clear")
		if err != nil {
			return err
		}
		if asJSON {
			return format.JSON(os.Stdout, map[string]any{
				"player_id": pid,
				"cleared":   true,
				"etag":      newETag,
			})
		}
		fmt.Printf("cleared shape on %s (etag %s)\n", pid, shortRev(newETag))
		return nil
	}

	if *rate < 0 && *delay < 0 && *loss < 0 {
		return errors.New("nothing to do — pass --rate / --delay / --loss, or --clear, or --show")
	}

	shape := proxy.Shape{}
	if *rate >= 0 {
		v := float32(*rate)
		shape.RateMbps = &v
	}
	if *delay >= 0 {
		v := float32(*delay)
		shape.DelayMs = &v
	}
	if *loss >= 0 {
		v := float32(*loss)
		shape.LossPct = &v
	}
	action := fmt.Sprintf("shape rate=%v delay=%v loss=%v", *rate, *delay, *loss)
	newETag, err := client.PatchShape(ctx, pid, action, &shape)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"player_id": pid,
			"shape":     shape,
			"etag":      newETag,
		})
	}
	fmt.Printf("patched shape on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}
