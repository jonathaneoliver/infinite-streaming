package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/snapshot"
)

const snapshotUsage = `harness snapshot <subcommand>

Subcommands:
  list [<target>]               show recent snapshots (newest first)
  show <snapshot_id>            print one snapshot in full (id prefix ok)

Snapshots live under ~/.claude/state/harness/<repo>/ and are written
by every mutation command (fault, shape, etc.). 'harness undo'
consumes them.
`

func cmdSnapshot(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(snapshotUsage)
	}
	if client.Snap == nil {
		return errors.New("snapshot store unavailable (see startup warning)")
	}
	switch args[0] {
	case "list", "ls":
		return cmdSnapshotList(client, args[1:], asJSON)
	case "show", "cat":
		return cmdSnapshotShow(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown snapshot subcommand: %s\n\n%s", args[0], snapshotUsage)
	}
}

func cmdSnapshotList(client *api.Client, args []string, asJSON bool) error {
	var playerID string
	if len(args) == 1 {
		pid, err := client.Resolve(context.Background(), args[0])
		if err != nil {
			// Allow listing by partial UUID even if not currently in
			// the live player list (player may have disconnected).
			playerID = args[0]
		} else {
			playerID = pid
		}
	} else if len(args) > 1 {
		return errors.New("usage: harness snapshot list [<target>]")
	}
	snaps, err := client.Snap.List(playerID, 50)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, snaps)
	}
	if len(snaps) == 0 {
		fmt.Println("no snapshots")
		return nil
	}
	for _, s := range snaps {
		id := fmt.Sprintf("%013d", s.Ts.UnixMilli())
		fmt.Printf("%s  %s  %s  %s\n",
			id[len(id)-8:],
			s.Ts.Local().Format("15:04:05"),
			shortPlayer(s.PlayerID),
			s.Action,
		)
	}
	return nil
}

func cmdSnapshotShow(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness snapshot show <id-prefix>")
	}
	snap, path, err := resolveSnapshot(client, args[0])
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, snap)
	}
	fmt.Printf("file:        %s\n", path)
	fmt.Printf("ts:          %s\n", snap.Ts.Local().Format(time.RFC3339))
	fmt.Printf("player_id:   %s\n", snap.PlayerID)
	fmt.Printf("action:      %s\n", snap.Action)
	fmt.Printf("etag_before: %s\n", snap.EtagBefore)
	fmt.Printf("etag_after:  %s\n", snap.EtagAfter)
	fmt.Println("patch:")
	fmt.Println(indent(string(snap.Patch), "  "))
	fmt.Println("before (truncated):")
	beforeStr := string(snap.Before)
	if len(beforeStr) > 2000 {
		beforeStr = beforeStr[:2000] + "\n  ... (truncated; use --json for full)"
	}
	fmt.Println(indent(beforeStr, "  "))
	return nil
}

// resolveSnapshot translates an operator-friendly id (player UUID
// prefix, full filename prefix, or the 8-char trailing-ts shown by
// `snapshot list`) into a Snapshot + its file path.
func resolveSnapshot(client *api.Client, idArg string) (snapshot.Snapshot, string, error) {
	// First try as a filename prefix (player_id prefix; the form
	// FindByPrefix natively understands).
	if s, p, err := client.Snap.FindByPrefix(idArg); err == nil {
		return s, p, nil
	}
	// Fall back to the 8-char short-id shown by `list` (trailing
	// digits of the unix-ms timestamp).
	all, err := client.Snap.List("", 0)
	if err != nil {
		return snapshot.Snapshot{}, "", err
	}
	for _, s := range all {
		id := fmt.Sprintf("%013d", s.Ts.UnixMilli())
		if strings.HasSuffix(id, idArg) {
			p := fmt.Sprintf("%s/%s-%s.json", client.Snap.Dir, s.PlayerID, id)
			return s, p, nil
		}
	}
	return snapshot.Snapshot{}, "", fmt.Errorf("no snapshot matches %q", idArg)
}

func indent(s, prefix string) string {
	var sb strings.Builder
	for _, line := range strings.Split(s, "\n") {
		sb.WriteString(prefix)
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func shortPlayer(id string) string {
	c := strings.ReplaceAll(id, "-", "")
	if len(c) > 8 {
		return c[:8]
	}
	return c
}
