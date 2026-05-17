package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
)

const tailUsage = `harness tail <target|all> [flags]

Streams /api/v2/timeseries SSE frames as they happen.

Flags:
  --streams S      comma-separated streams (default network)
                   one of: samples, network, events
  --bundles B      comma-separated bundle names (default empty)
  --max-hz N       cap delta rate (default 0 = uncapped)
  --raw            print the raw JSON data of each frame
                   (default: one-line per network row)

Examples:
  harness tail ipad                       # network rows for one player
  harness tail all                        # network rows across every player
  harness tail ipad --streams samples     # ABR/buffer/RTT sample rows
  harness tail ipad --raw                 # full JSON per frame
`

// tailNetworkRow is the projection we render in the default
// (non-raw) tail mode. CH's JSONEachRow output is inconsistent
// about numeric types (UInt64 → JSON string, UInt8 → JSON number,
// Float → JSON number, Nullable → null or omitted), so every
// not-clearly-text field is `any` and coerced on display. `Ts` is a
// string because CH emits naive `2026-05-17 13:33:56.050` (no T,
// no zone).
type tailNetworkRow struct {
	Ts          string `json:"ts"`
	PlayerID    string `json:"player_id"`
	PlayID      string `json:"play_id"`
	Method      any    `json:"method,omitempty"`
	Status      any    `json:"status,omitempty"`
	RequestKind any    `json:"request_kind,omitempty"`
	TotalMs     any    `json:"total_ms,omitempty"`
	BytesIn     any    `json:"bytes_in,omitempty"`
	Path        any    `json:"path,omitempty"`
	URL         any    `json:"url,omitempty"`
	Faulted     any    `json:"faulted,omitempty"`
	FaultType   any    `json:"fault_type,omitempty"`
}

func cmdTail(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(tailUsage)
	}
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	streams := fs.String("streams", "network", "comma-separated streams")
	bundles := fs.String("bundles", "", "comma-separated bundles")
	maxHz := fs.Int("max-hz", 0, "rate cap (events/sec/stream)")
	raw := fs.Bool("raw", false, "print raw frame JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	target := args[0]

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var playerID string
	if target != "all" && target != "*" {
		pid, err := client.Resolve(ctx, target)
		if err != nil {
			return err
		}
		playerID = pid
	}

	streamsList := splitCSV(*streams)
	bundlesList := splitCSV(*bundles)
	// Default bundles to the per-stream "full" bundle when the user
	// hasn't picked one — otherwise the server returns only identity
	// columns (ts/player_id/session_id/play_id) and the formatter
	// renders an unhelpful sea of dashes. Mirrors the dashboard's
	// per-stream defaults in useSessionTimeSeries.
	if len(bundlesList) == 0 {
		for _, s := range streamsList {
			switch s {
			case "samples":
				bundlesList = append(bundlesList, "charts_minimal", "lanes_v1")
			case "network":
				bundlesList = append(bundlesList, "network")
			case "events":
				bundlesList = append(bundlesList, "events")
			}
		}
	}
	params := api.TimeseriesParams{
		PlayerID: playerID,
		Streams:  streamsList,
		Bundles:  bundlesList,
		MaxHz:    *maxHz,
	}

	fmt.Fprintf(os.Stderr, "tailing streams=%s player=%s — Ctrl-C to stop\n",
		strings.Join(params.Streams, ","), labelOrAll(playerID))

	err := client.Timeseries(ctx, params, func(f api.SSEFrame) error {
		if *raw {
			fmt.Printf("event:%s id:%s data:%s\n", f.Event, f.ID, f.Data)
			return nil
		}
		// Heartbeats are silent in the default view (they'd just be
		// noise per second). Pass --raw to see them.
		if f.Event == "heartbeat" || f.Event == "" {
			return nil
		}
		if asJSON {
			fmt.Println(f.Data)
			return nil
		}
		// Default human view focuses on network rows since that's the
		// most common tail use case (watch HAR-shaped traffic). Other
		// stream events fall through to a one-line "event: data..." render.
		if f.Event == "network" || f.Event == "network.row" {
			var row tailNetworkRow
			if err := json.Unmarshal([]byte(f.Data), &row); err != nil {
				fmt.Printf("%s  [decode err: %v]\n", f.Event, err)
				return nil
			}
			fmt.Println(formatNetworkRow(row))
			return nil
		}
		// Unknown event types — print the type and a truncated payload
		// so the operator can see something is flowing.
		preview := f.Data
		if len(preview) > 140 {
			preview = preview[:137] + "..."
		}
		fmt.Printf("%-12s %s\n", f.Event, preview)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func formatNetworkRow(r tailNetworkRow) string {
	status := anyStr(r.Status, "—")
	method := anyStr(r.Method, "—")
	kind := anyStr(r.RequestKind, "—")
	dur := "—"
	if f, ok := anyFloat(r.TotalMs); ok {
		dur = fmt.Sprintf("%6.1fms", f)
	}
	bytesIn := "—"
	if n, ok := anyInt(r.BytesIn); ok {
		bytesIn = humanBytes(n)
	}
	path := "—"
	switch {
	case anyStr(r.Path, "") != "":
		path = anyStr(r.Path, "")
	case anyStr(r.URL, "") != "":
		path = anyStr(r.URL, "")
	}
	if len(path) > 70 {
		path = "…" + path[len(path)-69:]
	}
	fault := ""
	if n, ok := anyInt(r.Faulted); ok && n == 1 {
		fault = "  ⚠ " + anyStr(r.FaultType, "?")
	}
	return fmt.Sprintf("%s  %-7s %-3s %-15s %9s %8s  %s%s",
		formatTs(r.Ts),
		method,
		status,
		kind,
		dur,
		bytesIn,
		path,
		fault,
	)
}

// formatTs trims the ClickHouse `YYYY-MM-DD HH:MM:SS.mmm` form down
// to just the time-of-day portion (charts care about wall-clock; the
// date is implicit in the operator's session).
func formatTs(ts string) string {
	if ts == "" {
		return "—"
	}
	if i := strings.IndexByte(ts, ' '); i > 0 && i+1 < len(ts) {
		ts = ts[i+1:]
	}
	if i := strings.IndexByte(ts, 'T'); i > 0 && i+1 < len(ts) {
		ts = ts[i+1:]
	}
	if len(ts) > 12 {
		ts = ts[:12]
	}
	return ts
}

// anyStr coerces a CH-serialised JSON value to its string form. Used
// because /api/v2/timeseries leaks ClickHouse type quirks: some
// numeric columns arrive as JSON strings (UInt64), some as numbers
// (UInt8, Float). Returns dflt if v is nil or formats to an empty
// string.
func anyStr(v any, dflt string) string {
	switch x := v.(type) {
	case nil:
		return dflt
	case string:
		if x == "" {
			return dflt
		}
		return x
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func anyFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func anyInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case string:
		n, err := strconv.Atoi(x)
		return n, err == nil
	default:
		return 0, false
	}
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func labelOrAll(pid string) string {
	if pid == "" {
		return "all"
	}
	return pid
}
