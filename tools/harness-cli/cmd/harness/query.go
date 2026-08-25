package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/format"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/forwarder"
)

// parsePlayID maps the operator-supplied string to a typed UUID for
// the generated forwarder client. Surfaces parse errors as part of
// the command output rather than panicking.
func parsePlayID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid play_id %q: %w", s, err)
	}
	return id, nil
}

const queryUsage = `harness query <subcommand>     (alias: harness q)

Read-only queries against the forwarder /analytics/api/v2/* surface.
The underlying ClickHouse tables (session_events, network_requests,
control_events) hold both live and historical rows up to the 30-day
TTL — so these subcommands work for "what just happened" as well as
"what happened last week". For a live SSE multiplex, use
'harness ts <player> --streams events,network,control'.

Subcommands:
  plays    [--limit N] [--from ISO] [--to ISO] [--classification C]
           [--player-id UUID] [--play-id UUID] [--attempt-id N]
           [--label-has L ...] [--label-not L ...]
                                  list plays (one row per archived
                                  playback; includes labels_total +
                                  label_histogram)
  play     <play_id>              one play + _links
  aggregate [--from --to --classification]
                                  aggregate stats across plays
  events   <play_id> [--limit N] [--label-has L ...] [--label-not L ...]
                                  player events (session_events rows)
  network  <play_id> [--limit N] [--faulted-only] [--fault-category C] [--label-has L ...] [--label-not L ...]
                                  per-request HAR rows
  control  <play_id> [--source S] [--event E] [--mode M]
           [--label-has L ...] [--label-not L ...] [--limit N]
                                  proxy / harness action log
  avmetrics [<play_id>] [--event-type T ...] [--from ISO] [--to ISO]
           [--label-has L ...] [--label-not L ...] [--limit N]
                                  iOS AVMetrics events (highest-resolution
                                  failure-timing feed: CoreMedia error
                                  codes, variant-switch start/complete).
                                  Bounded read — closes (no SSE hack).
  heatmap  <play_id>
  bundle   <play_id> --out PATH   download play bundle ZIP

Label filters (issue #474 follow-up):
  --label-has X    row must contain label X (repeatable; AND).
  --label-not X    row must NOT contain label X (repeatable; AND).
  Combine for tristate queries, e.g.
    --label-has warning=http_4xx --label-not warning=*fault_rule_enabled
  --mode M         (control only) shortcut → --label-has info=*pattern_step_M

Pass --json globally for raw responses suitable for piping.
`

func cmdQuery(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New(queryUsage)
	}
	switch args[0] {
	case "plays":
		return cmdQueryPlays(client, args[1:], asJSON)
	case "play":
		return cmdQueryPlay(client, args[1:], asJSON)
	case "aggregate":
		return cmdQueryAggregate(client, args[1:], asJSON)
	case "events":
		return cmdQueryEvents(client, args[1:], asJSON)
	case "network":
		return cmdQueryNetwork(client, args[1:], asJSON)
	// `markers` subcommand retired in issue #474 Milestone C. The
	// session_markers table is gone; severity-tagged labels now ride on
	// each session_events / network_requests row directly and discrete
	// proxy/harness actions live on control_events.
	case "control":
		return cmdQueryControl(client, args[1:], asJSON)
	case "avmetrics":
		return cmdQueryAVMetrics(client, args[1:], asJSON)
	case "heatmap":
		return cmdQueryHeatmap(client, args[1:], asJSON)
	case "bundle":
		return cmdQueryBundle(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown query subcommand: %s\n\n%s", args[0], queryUsage)
	}
}

// printOrJSON writes raw bytes to stdout when --json, otherwise
// pretty-prints by re-indenting JSON. Falls back to raw if the body
// isn't valid JSON.
func printOrJSON(body []byte, asJSON bool) error {
	if asJSON {
		_, err := os.Stdout.Write(body)
		return err
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		_, err := os.Stdout.Write(body)
		return err
	}
	return format.JSON(os.Stdout, v)
}

func parseLimit(fs *flag.FlagSet, args []string) (*int, error) {
	limit := fs.Int("limit", 0, "max items returned")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *limit <= 0 {
		return nil, nil
	}
	v := *limit
	return &v, nil
}

func cmdQueryPlays(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("query plays", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max plays")
	classification := fs.String("classification", "", "interesting|other|favourite")
	playerID := fs.String("player-id", "", "filter to one player_id (UUID)")
	playID := fs.String("play-id", "", "filter to one play_id (UUID)")
	attemptID := fs.Int("attempt-id", 0, "filter to one attempt_id (1-based int) — plays containing this recovery attempt")
	from := fs.String("from", "", "ISO 8601 lower bound (e.g. 2026-05-17T00:00:00Z)")
	to := fs.String("to", "", "ISO 8601 upper bound (exclusive)")
	var labelHas, labelNot arrayFlag
	fs.Var(&labelHas, "label-has", "row must have this label (repeatable; AND semantics)")
	fs.Var(&labelNot, "label-not", "row must NOT have this label (repeatable; AND semantics)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := &forwarder.GetApiV2PlaysParams{}
	if len(labelHas) > 0 {
		v := forwarder.LabelHasFilter(labelHas)
		params.LabelHas = &v
	}
	if len(labelNot) > 0 {
		v := forwarder.LabelNotFilter(labelNot)
		params.LabelNot = &v
	}
	if *limit > 0 {
		v := *limit
		params.Limit = &v
	}
	if *classification != "" {
		c := forwarder.GetApiV2PlaysParamsClassification(*classification)
		params.Classification = &c
	}
	if *playerID != "" {
		pid, err := uuid.Parse(*playerID)
		if err != nil {
			return fmt.Errorf("invalid --player-id %q: %w", *playerID, err)
		}
		params.PlayerId = &pid
	}
	if *playID != "" {
		pid, err := uuid.Parse(*playID)
		if err != nil {
			return fmt.Errorf("invalid --play-id %q: %w", *playID, err)
		}
		params.PlayId = &pid
	}
	if *attemptID > 0 {
		v := *attemptID
		params.AttemptId = &v
	}
	if *from != "" {
		t, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return fmt.Errorf("invalid --from %q (need RFC3339, e.g. 2026-05-17T00:00:00Z): %w", *from, err)
		}
		params.From = &t
	}
	if *to != "" {
		t, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return fmt.Errorf("invalid --to %q (need RFC3339): %w", *to, err)
		}
		params.To = &t
	}
	body, err := client.ArchivePlays(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdQueryPlay(client *api.Client, args []string, asJSON bool) error {
	if len(args) != 1 {
		return errors.New("usage: harness query play <play_id>")
	}
	body, err := client.ArchivePlay(context.Background(), args[0])
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdQueryAggregate(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("query aggregate", flag.ContinueOnError)
	classification := fs.String("classification", "", "interesting|other|favourite")
	if err := fs.Parse(args); err != nil {
		return err
	}
	params := &forwarder.GetApiV2PlaysAggregateParams{}
	if *classification != "" {
		c := forwarder.GetApiV2PlaysAggregateParamsClassification(*classification)
		params.Classification = &c
	}
	body, err := client.ArchivePlaysAggregate(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdQueryEvents(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness query events <play_id> [--limit N] [--label-has L ...] [--label-not L ...]")
	}
	playID := args[0]
	fs := flag.NewFlagSet("query events", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max rows")
	var labelHas, labelNot arrayFlag
	fs.Var(&labelHas, "label-has", "row must have this label (repeatable; AND semantics)")
	fs.Var(&labelNot, "label-not", "row must NOT have this label (repeatable; AND semantics)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	params := &forwarder.GetApiV2EventsParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	if *limit > 0 {
		l := *limit
		params.Limit = &l
	}
	if len(labelHas) > 0 {
		v := forwarder.LabelHasFilter(labelHas)
		params.LabelHas = &v
	}
	if len(labelNot) > 0 {
		v := forwarder.LabelNotFilter(labelNot)
		params.LabelNot = &v
	}
	body, err := client.ArchiveEvents(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

// cmdQueryControl wires the harness CLI to the new
// /api/v2/control_events endpoint (issue #474 Milestone B). Mirrors
// the existing `network` / `events` subcommands. `--mode` is a
// shortcut that expands to `--label-has info=*pattern_step_<mode>`
// (the densest pattern-locality signal — one row per step advance).
func cmdQueryControl(client *api.Client, args []string, asJSON bool) error {
	// play_id is an OPTIONAL leading positional — omit it to query global /
	// session-less events (e.g. server_start) by --event or --label-has. A
	// leading token starting with '-' is a flag, not a play_id.
	var playID string
	flagArgs := args
	if len(args) >= 1 && !strings.HasPrefix(args[0], "-") {
		playID = args[0]
		flagArgs = args[1:]
	}
	fs := flag.NewFlagSet("query control", flag.ContinueOnError)
	source := fs.String("source", "", "filter to one source (harness|proxy|auto)")
	var events arrayFlag
	fs.Var(&events, "event", "filter to event name (repeatable)")
	mode := fs.String("mode", "", "shortcut: --label-has info=*pattern_step_<mode>")
	var labelHas, labelNot arrayFlag
	fs.Var(&labelHas, "label-has", "row must have this label (repeatable; AND semantics)")
	fs.Var(&labelNot, "label-not", "row must NOT have this label (repeatable; AND semantics)")
	limit := fs.Int("limit", 0, "max rows")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if playID == "" && len(events) == 0 && len(labelHas) == 0 && *mode == "" {
		return errors.New("usage: harness query control [<play_id>] [--event E] [--label-has L] [--source S] [--mode M] [--label-not L] [--limit N]\n  a play_id, --event, --label-has, or --mode is required")
	}
	params := &forwarder.GetApiV2ControlEventsParams{}
	if playID != "" {
		pid, err := parsePlayID(playID)
		if err != nil {
			return err
		}
		params.PlayId = &pid
	}
	if *source != "" {
		s := forwarder.GetApiV2ControlEventsParamsSource(*source)
		if !s.Valid() {
			return fmt.Errorf("invalid --source %q: expected harness|proxy|auto", *source)
		}
		params.Source = &s
	}
	if len(events) > 0 {
		ev := []string(events)
		params.Event = &ev
	}
	if *mode != "" {
		labelHas = append(labelHas, "info=*pattern_step_"+*mode)
	}
	if *limit > 0 {
		l := *limit
		params.Limit = &l
	}
	if len(labelHas) > 0 {
		v := forwarder.LabelHasFilter(labelHas)
		params.LabelHas = &v
	}
	if len(labelNot) > 0 {
		v := forwarder.LabelNotFilter(labelNot)
		params.LabelNot = &v
	}
	body, err := client.ArchiveControlEvents(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

// cmdQueryAVMetrics queries the iOS AVMetrics event log (#693) — the
// highest-resolution failure-timing feed. Mirrors cmdQueryControl: the
// play_id positional is optional, so an operator can pull by --event-type
// / --label-has alone (e.g. every error-bearing AVMetric in a window).
// Bounded read, so it closes — no curl --max-time hack.
func cmdQueryAVMetrics(client *api.Client, args []string, asJSON bool) error {
	var playID string
	flagArgs := args
	if len(args) >= 1 && !strings.HasPrefix(args[0], "-") {
		playID = args[0]
		flagArgs = args[1:]
	}
	fs := flag.NewFlagSet("query avmetrics", flag.ContinueOnError)
	var eventTypes arrayFlag
	fs.Var(&eventTypes, "event-type", "filter to AVMetric subclass name, e.g. HLSPlaylistRequestEvent (repeatable; OR)")
	from := fs.String("from", "", "ISO lower bound on ts (inclusive)")
	to := fs.String("to", "", "ISO upper bound on ts (exclusive)")
	var labelHas, labelNot arrayFlag
	fs.Var(&labelHas, "label-has", "row must have this label (repeatable; AND semantics)")
	fs.Var(&labelNot, "label-not", "row must NOT have this label (repeatable; AND semantics)")
	limit := fs.Int("limit", 0, "max rows")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if playID == "" && len(eventTypes) == 0 && len(labelHas) == 0 {
		return errors.New("usage: harness query avmetrics [<play_id>] [--event-type T] [--label-has L] [--from ISO] [--to ISO] [--label-not L] [--limit N]\n  a play_id, --event-type, or --label-has is required")
	}
	params := &forwarder.GetApiV2AvmetricEventsParams{}
	if playID != "" {
		pid, err := parsePlayID(playID)
		if err != nil {
			return err
		}
		params.PlayId = &pid
	}
	if len(eventTypes) > 0 {
		et := []string(eventTypes)
		params.EventType = &et
	}
	if *from != "" {
		t, err := time.Parse(time.RFC3339, *from)
		if err != nil {
			return fmt.Errorf("invalid --from %q (need RFC3339, e.g. 2026-05-17T00:00:00Z): %w", *from, err)
		}
		params.From = &t
	}
	if *to != "" {
		t, err := time.Parse(time.RFC3339, *to)
		if err != nil {
			return fmt.Errorf("invalid --to %q (need RFC3339): %w", *to, err)
		}
		params.To = &t
	}
	if *limit > 0 {
		l := *limit
		params.Limit = &l
	}
	if len(labelHas) > 0 {
		v := forwarder.LabelHasFilter(labelHas)
		params.LabelHas = &v
	}
	if len(labelNot) > 0 {
		v := forwarder.LabelNotFilter(labelNot)
		params.LabelNot = &v
	}
	body, err := client.ArchiveAVMetricEvents(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

// arrayFlag is a repeatable string flag (Cobra's StringSlice without
// pulling in Cobra). Used for `--event foo --event bar` style CLI.
type arrayFlag []string

func (a *arrayFlag) String() string {
	if a == nil {
		return ""
	}
	return strings.Join(*a, ",")
}
func (a *arrayFlag) Set(v string) error {
	*a = append(*a, v)
	return nil
}

func cmdQueryNetwork(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness query network <play_id> [--limit N] [--faulted-only] [--fault-category C] [--label-has L ...] [--label-not L ...]")
	}
	playID := args[0]
	fs := flag.NewFlagSet("query network", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max rows")
	faultedOnly := fs.Bool("faulted-only", false, "only faulted/aborted rows (filters client-side after --limit; raise --limit to scan more)")
	faultCategory := fs.String("fault-category", "", "only rows with this fault_category (client_disconnect|http|transfer_timeout|socket|transport|corruption); implies --faulted-only")
	var labelHas, labelNot arrayFlag
	fs.Var(&labelHas, "label-has", "row must have this label (repeatable; AND semantics)")
	fs.Var(&labelNot, "label-not", "row must NOT have this label (repeatable; AND semantics)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	params := &forwarder.GetApiV2NetworkRequestsParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	if *limit > 0 {
		l := *limit
		params.Limit = &l
	}
	if len(labelHas) > 0 {
		v := forwarder.LabelHasFilter(labelHas)
		params.LabelHas = &v
	}
	if len(labelNot) > 0 {
		v := forwarder.LabelNotFilter(labelNot)
		params.LabelNot = &v
	}
	body, err := client.ArchiveNetworkRequests(context.Background(), params)
	if err != nil {
		return err
	}
	if *faultedOnly || *faultCategory != "" {
		body, err = filterNetworkFaults(body, *faultCategory)
		if err != nil {
			return err
		}
	}
	return printOrJSON(body, asJSON)
}

// filterNetworkFaults keeps only faulted rows (optionally a single
// fault_category) in a /api/v2/network_requests envelope. Client-side
// because the read API's `faulted_only` query param isn't in the generated
// client spec. Preserves non-`items` envelope fields (e.g. next_cursor).
func filterNetworkFaults(body []byte, category string) ([]byte, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return body, nil // shape changed — leave untouched
	}
	rawItems, ok := env["items"]
	if !ok {
		return body, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(rawItems, &items); err != nil {
		return body, nil
	}
	kept := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if !rowFaulted(it) {
			continue
		}
		if category != "" && anyStr(it["fault_category"], "") != category {
			continue
		}
		kept = append(kept, it)
	}
	newItems, err := json.Marshal(kept)
	if err != nil {
		return nil, err
	}
	env["items"] = newItems
	return json.Marshal(env)
}

// rowFaulted reports whether a network row carries any fault signal. The
// read API returns fault_type/fault_category but not always a `faulted`
// flag, so check all three.
func rowFaulted(it map[string]any) bool {
	if anyStr(it["fault_type"], "") != "" {
		return true
	}
	if anyStr(it["fault_category"], "") != "" {
		return true
	}
	if n, ok := anyInt(it["faulted"]); ok && n == 1 {
		return true
	}
	return false
}

func cmdQueryHeatmap(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New("usage: harness query heatmap <play_id>")
	}
	playID := args[0]
	params := &forwarder.GetApiV2SessionHeatmapParams{}
	pid, err := parsePlayID(playID)
	if err != nil {
		return err
	}
	params.PlayId = &pid
	body, err := client.ArchiveSessionHeatmap(context.Background(), params)
	if err != nil {
		return err
	}
	return printOrJSON(body, asJSON)
}

func cmdQueryBundle(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("query bundle", flag.ContinueOnError)
	out := fs.String("out", "", "output path (required; bundle is a ZIP)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 || *out == "" {
		return errors.New("usage: harness query bundle <play_id> --out PATH")
	}
	playID := rest[0]
	body, err := client.ArchivePlayBundle(context.Background(), playID)
	if err != nil {
		return err
	}
	defer body.Close()
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(f, body)
	if err != nil {
		return err
	}
	if asJSON {
		return format.JSON(os.Stdout, map[string]any{"out": *out, "bytes": n})
	}
	fmt.Printf("wrote %d bytes → %s\n", n, *out)
	return nil
}
