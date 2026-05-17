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

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
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
  tail <target|all>             /api/v2/timeseries network stream (SSE)

Coming in subsequent commits:
  ts <target>                   full timeseries subscription (samples + network)
  archive <subcommand>          /api/v2/snapshots, /session_events, /network_requests
  groups <subcommand>           player groups
  snapshot / undo / history     CLI-side undo stack
  finding add ...               write to .claude/findings/
  procedure <name> ...          multi-step (soak, ABR sweep)

Targets are resolved against the live player list. A target may be a
full UUID, a >=6-char hex prefix, a label value (device/name), a
player IP, or a substring of the User-Agent.
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

	client, err := api.New(api.Options{
		BaseURL:   g.base,
		Insecure:  g.insecure,
		BasicAuth: g.basicAuth,
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
