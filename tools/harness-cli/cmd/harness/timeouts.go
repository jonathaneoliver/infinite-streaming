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

const timeoutsUsage = `harness timeouts <target> [flags]

PATCH the player's transfer_timeouts (server-side response transfer
timeouts). 0 = disabled.

Flags:
  --active SEC         active_timeout_seconds (total response cap)
  --idle SEC           idle_timeout_seconds (time-since-last-write cap)
  --applies-segments   default-on; pass --applies-segments=false to scope off
  --applies-manifests
  --applies-master
  --show               print current and exit
  --clear              clear all transfer_timeouts (sends null)
`

func cmdTimeouts(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(timeoutsUsage)
	}
	fs := flag.NewFlagSet("timeouts", flag.ContinueOnError)
	active := fs.Int("active", -1, "active_timeout_seconds")
	idle := fs.Int("idle", -1, "idle_timeout_seconds")
	asegs := fs.Bool("applies-segments", true, "scope to segment requests")
	amans := fs.Bool("applies-manifests", false, "scope to manifest requests")
	amast := fs.Bool("applies-master", false, "scope to master manifest")
	show := fs.Bool("show", false, "print current and exit")
	clear := fs.Bool("clear", false, "clear timeouts (send null)")
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
		if asJSON {
			return format.JSON(os.Stdout, rec.TransferTimeouts)
		}
		if rec.TransferTimeouts == nil {
			fmt.Println("no transfer_timeouts")
			return nil
		}
		return format.JSON(os.Stdout, rec.TransferTimeouts)
	}
	if *clear {
		// PlayerPatch.TransferTimeouts is `*TransferTimeouts`, so nil
		// would omit the key. Send raw null instead.
		newETag, err := client.PatchRaw(ctx, pid, "", []byte(`{"transfer_timeouts": null}`))
		if err != nil {
			return err
		}
		fmt.Printf("cleared transfer_timeouts on %s (etag %s)\n", pid, shortRev(newETag))
		return nil
	}
	if *active < 0 && *idle < 0 {
		return errors.New("nothing to do — pass --active / --idle / --show / --clear")
	}
	t := proxy.TransferTimeouts{}
	if *active >= 0 {
		t.ActiveTimeoutSeconds = active
	}
	if *idle >= 0 {
		t.IdleTimeoutSeconds = idle
	}
	t.AppliesSegments = asegs
	t.AppliesManifests = amans
	t.AppliesMaster = amast
	patch := proxy.PlayerPatch{TransferTimeouts: &t}
	action := fmt.Sprintf("timeouts active=%d idle=%d", *active, *idle)
	newETag, err := client.PatchPlayer(ctx, pid, action, patch)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"player_id": pid, "transfer_timeouts": t, "etag": newETag})
	}
	fmt.Printf("patched transfer_timeouts on %s (etag %s)\n", pid, shortRev(newETag))
	return nil
}
