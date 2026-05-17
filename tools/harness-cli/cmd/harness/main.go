// harness — CLI driver for the InfiniteStream test harness.
//
// Wraps the v2 forwarder + proxy API surfaces (api/openapi/v2/*.yaml).
// Generated typed clients live under internal/v2gen/; this binary just
// translates prose-friendly subcommands to API calls and formats the
// result.
//
// Usage:
//
//	harness [global flags] <command> [args]
//
// Phase 1 commands (greenfield scaffold): players list / players show.
// Subsequent phases add: fault, shape, tail, ts, archive, groups,
// snapshot/undo, findings, procedure. Each command commits separately
// so the scaffold stays reviewable.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/snapshot"
)

const usage = `harness — drive the InfiniteStream test harness.

Usage:
  harness [global flags] <command> [args]

Global flags:
  --base URL       harness base URL (default $HARNESS_BASE_URL or
                   https://jonathanoliver-ubuntu.local:21000)
  --insecure       skip TLS verification (test-dev self-signed cert)
  --basic USER:PW  HTTP Basic auth (default $HARNESS_BASIC_AUTH)
  --json           emit JSON instead of human-readable output

Commands:
  players list                  list current v2 players (live)
  players show <player_id>      print full player record + ETag
  fault list|add|rm|clear       per-rule fault_rules CRUD (ETag-aware)
  shape <target>                PATCH player.shape (rate/delay/loss/clear)
  tail <target|all>             network stream SSE (/api/v2/timeseries)
  ts <target>                   combined samples+network stream
  events <target|all>           lifecycle SSE (/api/v2/events)
  snapshot list|show            show prior mutation snapshots
  undo [<target>]               replay the most recent snapshot

Coming in subsequent phases:
  players create|rm|prune       create/delete players
  fault edit <target> <rule>    per-rule PATCH
  labels|timeouts|content       player-record PATCH for remaining fields
  play <subcommand>             play-scoped GET/PATCH + play.fault.*
  network <target>              live HAR from /players/{id}/network
  archive <subcommand>          forwarder reads (plays, snapshots, network, events, heatmap, bundle)
  groups <subcommand>           player groups
  info / raw / bundles          escape hatches + introspection
  procedure / finding           multi-step ops + finding capture

Targets are resolved against the live player list. A target may be a
full UUID, a >=6-char hex prefix, a label value (device/name), a
player IP, or a substring of the User-Agent.

Mutations are snapshotted to ~/.claude/state/harness/<repo>/ so
'harness undo' can replay them.
`

type globalFlags struct {
	base      string
	insecure  bool
	basicAuth string
	asJSON    bool
}

func main() {
	g := parseGlobals(os.Args[1:])
	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	snap, err := openSnapshotStore()
	if err != nil {
		// Snapshot store failures shouldn't abort read-only commands;
		// warn and continue without undo coverage. Mutation commands
		// will then save no snapshot but otherwise work.
		fmt.Fprintln(os.Stderr, "warn: snapshot store unavailable:", err)
	}
	client, err := api.New(api.Options{
		BaseURL:   g.base,
		Insecure:  g.insecure,
		BasicAuth: g.basicAuth,
		Snap:      snap,
	})
	if err != nil {
		fail(err)
	}

	switch args[0] {
	case "players":
		exit(cmdPlayers(client, args[1:], g.asJSON))
	case "fault":
		exit(cmdFault(client, args[1:], g.asJSON))
	case "shape":
		exit(cmdShape(client, args[1:], g.asJSON))
	case "tail":
		exit(cmdTail(client, args[1:], g.asJSON))
	case "ts":
		exit(cmdTs(client, args[1:], g.asJSON))
	case "events":
		exit(cmdEvents(client, args[1:], g.asJSON))
	case "snapshot", "snap":
		exit(cmdSnapshot(client, args[1:], g.asJSON))
	case "undo":
		exit(cmdUndo(client, args[1:], g.asJSON))
	case "labels":
		exit(cmdLabels(client, args[1:], g.asJSON))
	case "timeouts":
		exit(cmdTimeouts(client, args[1:], g.asJSON))
	case "content":
		exit(cmdContent(client, args[1:], g.asJSON))
	case "play":
		exit(cmdPlay(client, args[1:], g.asJSON))
	case "network":
		exit(cmdNetwork(client, args[1:], g.asJSON))
	case "archive":
		exit(cmdArchive(client, args[1:], g.asJSON))
	case "groups":
		exit(cmdGroups(client, args[1:], g.asJSON))
	case "info":
		exit(cmdInfo(client, args[1:], g.asJSON))
	case "raw":
		exit(cmdRaw(client, args[1:], g.asJSON))
	case "help", "--help", "-h":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", args[0], usage)
		os.Exit(2)
	}
}

func parseGlobals(argv []string) globalFlags {
	g := globalFlags{}
	flag.StringVar(&g.base, "base", "", "harness base URL")
	flag.BoolVar(&g.insecure, "insecure", false, "skip TLS verification")
	flag.StringVar(&g.basicAuth, "basic", "", "HTTP Basic auth (user:password)")
	flag.BoolVar(&g.asJSON, "json", false, "emit JSON instead of human-readable output")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := flag.CommandLine.Parse(argv); err != nil {
		os.Exit(2)
	}
	return g
}

func exit(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

// openSnapshotStore picks a per-repo snapshot directory keyed by the
// working tree's basename. Worktrees of the same repo share state
// (e.g. `timeseries-441` and `harness-cli-greenfield` both write to
// `~/.claude/state/harness/<basename>/`). Override with
// $HARNESS_REPO_NAME when running outside a checkout (CI, scripts).
func openSnapshotStore() (*snapshot.Store, error) {
	repo := os.Getenv("HARNESS_REPO_NAME")
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		repo = filepath.Base(wd)
	}
	return snapshot.Open(repo)
}
