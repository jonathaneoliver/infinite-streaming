package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/snapshot"
)

const undoUsage = `harness undo [<target>|<snapshot-id>]

Replays the most recent snapshot for the named target (default: most
recent across any player). Reconstructs a PATCH body from the
snapshotted PlayerRecord — fault_rules, shape, content, labels,
transfer_timeouts — and sends it with the player's CURRENT etag so
concurrent mutations fail-fast on 412 rather than getting stomped.

Flags:
  --dry         print the PATCH body without sending
  --yes         skip confirmation prompt
`

func cmdUndo(client *api.Client, args []string, asJSON bool) error {
	if client.Snap == nil {
		return errors.New("snapshot store unavailable")
	}
	dry := false
	yes := false
	rest := args[:0]
	for _, a := range args {
		switch a {
		case "--dry":
			dry = true
		case "--yes", "-y":
			yes = true
		case "-h", "--help":
			return errors.New(undoUsage)
		default:
			rest = append(rest, a)
		}
	}
	args = rest

	ctx := context.Background()
	snap, path, err := selectUndoTarget(ctx, client, args)
	if err != nil {
		return err
	}

	patch, err := buildUndoPatch(snap)
	if err != nil {
		return err
	}

	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"snapshot_file": path,
			"player_id":     snap.PlayerID,
			"action":        snap.Action,
			"patch":         json.RawMessage(patch),
			"dry":           dry,
		})
	}

	fmt.Printf("undo target: %s\n", path)
	fmt.Printf("action:      %s\n", snap.Action)
	fmt.Printf("player_id:   %s\n", snap.PlayerID)
	fmt.Println("patch:")
	fmt.Println(indent(string(patch), "  "))

	if dry {
		return nil
	}
	if !yes {
		fmt.Print("\napply? [y/N] ")
		var resp string
		fmt.Scanln(&resp)
		if resp != "y" && resp != "Y" {
			return errors.New("aborted")
		}
	}

	newETag, err := client.PatchRaw(ctx, snap.PlayerID, "", patch)
	if err != nil {
		return err
	}
	fmt.Printf("\nrestored %s (etag %s)\n", snap.PlayerID, shortRev(newETag))
	return nil
}

func selectUndoTarget(ctx context.Context, client *api.Client, args []string) (snapshot.Snapshot, string, error) {
	if len(args) == 0 {
		all, err := client.Snap.List("", 1)
		if err != nil {
			return snapshot.Snapshot{}, "", err
		}
		if len(all) == 0 {
			return snapshot.Snapshot{}, "", errors.New("no snapshots to undo")
		}
		s := all[0]
		p := fmt.Sprintf("%s/%s-%013d.json", client.Snap.Dir, s.PlayerID, s.Ts.UnixMilli())
		return s, p, nil
	}
	if len(args) > 1 {
		return snapshot.Snapshot{}, "", errors.New("usage: harness undo [<target>|<snapshot-id>]")
	}
	arg := args[0]
	// Try as a target (player resolution) first; fall back to snapshot id.
	if pid, err := client.Resolve(ctx, arg); err == nil {
		s, p, err := client.Snap.Latest(pid)
		if err != nil {
			return snapshot.Snapshot{}, "", fmt.Errorf("no snapshot for %s", pid)
		}
		return s, p, nil
	}
	return resolveSnapshot(client, arg)
}

// buildUndoPatch reconstructs a merge-patch body from the snapshotted
// PlayerRecord. We only restore the *broadcast-eligible* fields that
// PATCH /api/v2/players accepts: fault_rules, shape, content, labels,
// transfer_timeouts. Runtime fields (player_metrics, server_metrics,
// fault_counters) are not patchable and so are skipped — they'll
// re-derive themselves as the player keeps running.
//
// Shape is sent literally — if the snapshot has shape:null then the
// patch sends shape:null. Same for fault_rules:[] (clear) vs
// fault_rules:[…] (restore set).
func buildUndoPatch(snap snapshot.Snapshot) ([]byte, error) {
	var before map[string]json.RawMessage
	if err := json.Unmarshal(snap.Before, &before); err != nil {
		return nil, fmt.Errorf("undo: parse snapshot before: %w", err)
	}
	patchable := []string{"fault_rules", "shape", "content", "labels", "transfer_timeouts"}
	out := make(map[string]json.RawMessage, len(patchable))
	for _, key := range patchable {
		if v, ok := before[key]; ok {
			out[key] = v
		} else {
			// Field absent in snapshot → was unset → patch to null.
			// Without this, undoing an `add` of a field never removes
			// it because the patch wouldn't mention the key.
			out[key] = json.RawMessage("null")
		}
	}
	return json.Marshal(out)
}
