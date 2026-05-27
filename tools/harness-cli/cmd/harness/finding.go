package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
)

const findingUsage = `harness finding add <target> --tag T --note "..."

Capture a finding about a current session into .claude/findings/.
The file embeds the player's current state, the recent snapshot
history for that player, and the operator-supplied note + tags.
Designed so Claude can read the findings later to understand what
the operator noticed during the session.

Flags:
  --tag T         single tag (repeatable)
  --note STRING   free-form note (required)
  --out DIR       output directory (default .claude/findings)

Examples:
  harness finding add ipad --tag stall --tag abr --note "buffer hit 0 at 22:30"
`

type findingFile struct {
	V         int               `json:"v"`
	Ts        time.Time         `json:"ts"`
	PlayerID  string            `json:"player_id"`
	Target    string            `json:"target_arg"`
	Tags      []string          `json:"tags,omitempty"`
	Note      string            `json:"note"`
	Player    json.RawMessage   `json:"player_snapshot,omitempty"`
	Snapshots []json.RawMessage `json:"recent_snapshots,omitempty"`
}

func cmdFinding(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New(findingUsage)
	}
	if len(args) < 2 {
		return errors.New("usage: harness finding add <target> --note '...' [--tag T...]")
	}
	target := args[1]
	fs := flag.NewFlagSet("finding add", flag.ContinueOnError)
	note := fs.String("note", "", "free-form note (required)")
	out := fs.String("out", ".claude/findings", "output directory")
	tags := stringSliceFlag{}
	fs.Var(&tags, "tag", "tag (repeatable)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if *note == "" {
		return errors.New("--note is required")
	}
	ctx := context.Background()
	pid, err := client.Resolve(ctx, target)
	if err != nil {
		return err
	}
	rec, _, err := client.Player(ctx, pid)
	if err != nil {
		return err
	}
	playerJSON, _ := json.Marshal(rec)

	var snaps []json.RawMessage
	if client.Snap != nil {
		all, _ := client.Snap.List(pid, 5)
		for _, s := range all {
			b, _ := json.Marshal(s)
			snaps = append(snaps, b)
		}
	}

	finding := findingFile{
		V:         1,
		Ts:        time.Now().UTC(),
		PlayerID:  pid,
		Target:    target,
		Tags:      []string(tags),
		Note:      *note,
		Player:    playerJSON,
		Snapshots: snaps,
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%013d.json", shortPlayer(pid), finding.Ts.UnixMilli())
	path := filepath.Join(*out, name)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(finding); err != nil {
		return err
	}

	if asJSON {
		return format.JSON(os.Stdout, map[string]any{
			"file":      path,
			"player_id": pid,
			"tags":      finding.Tags,
		})
	}
	fmt.Printf("wrote finding → %s\n", path)
	if len(finding.Tags) > 0 {
		fmt.Printf("tags: %s\n", strings.Join(finding.Tags, ", "))
	}
	fmt.Printf("note: %s\n", *note)
	return nil
}
