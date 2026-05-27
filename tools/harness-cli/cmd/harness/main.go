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
// Subsequent phases add: fault, shape, tail, ts, query, groups,
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

Commands (live mutations are checkpoint-protected; see 'undo'):
  players list|show|create|rm|prune
  fault   list|add|edit|rm|clear
  shape   <target> --rate --delay --loss [--clear|--show]
  labels  show|set|rm|clear
  timeouts <target> --active --idle [--applies-*|--show|--clear]
  content  <target> --strip-* --overstate-* --live-offset [...]
  play    show|patch
  network <target>            live HAR from /players/{id}/network
  groups  list|show|create|patch|add|remove|rm

Streaming (live SSE):
  tail    <target|all>         network rows over /api/v2/timeseries
  ts      <target|all>         combined events+network+control rows
                               (--streams events,network,control)
  events  <target|all>         lifecycle SSE (/api/v2/events)

Query  (read-only forwarder /analytics/api/v2/*):  alias 'q'
  query   plays|play|aggregate|events|network|control|heatmap|bundle
                               server-side label filters via
                               --label-has X --label-not Y
                               'query plays' carries label_histogram

Operator/CLI:
  checkpoint list|show         pre-mutation checkpoints in ~/.claude/state/...
                               (alias: 'ck'; legacy: 'snapshot' / 'snap')
  undo [<target>|<id>]         replay the most recent checkpoint
  finding add <target>         capture state+note into .claude/findings/
  procedure soak|abr-sweep|fault-soak <target>
                               multi-step composed test procedures
  post characterization <file> upload a characterization-test report
                               JSON to the forwarder (test framework
                               calls this from WriteReport)
  info [--bundles]             healthz + info across both services
  raw <METHOD> <PATH>          escape hatch (no resolver, no checkpoint)

Targets are resolved against the live player list. A target may be a
full UUID, a >=6-char hex prefix, a label value (device/name), a
player IP, or a substring of the User-Agent.

Mutations are checkpointed to ~/.claude/state/harness/<repo>/ so
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
	case "checkpoint", "ck", "snapshot", "snap":
		// `checkpoint` is the canonical name as of v2.0.0 — the
		// pre-mutation state-save pattern overlapped naming-wise
		// with the player-events table that retired in #474. `ck`
		// is the short alias; `snapshot` / `snap` stay as legacy
		// aliases for scripts that haven't migrated yet.
		exit(cmdCheckpoint(client, args[1:], g.asJSON))
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
	case "query", "q":
		// `query` is the canonical name as of v2.0.0 — the
		// underlying CH tables hold both live and historical rows,
		// so "archive" was misleading. `q` is a short alias for
		// repeat use at the prompt.
		exit(cmdQuery(client, args[1:], g.asJSON))
	case "groups":
		exit(cmdGroups(client, args[1:], g.asJSON))
	case "info":
		exit(cmdInfo(client, args[1:], g.asJSON))
	case "raw":
		exit(cmdRaw(client, args[1:], g.asJSON))
	case "finding":
		exit(cmdFinding(client, args[1:], g.asJSON))
	case "procedure":
		exit(cmdProcedure(client, args[1:], g.asJSON))
	case "post":
		exit(cmdPost(client, args[1:], g.asJSON))
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
