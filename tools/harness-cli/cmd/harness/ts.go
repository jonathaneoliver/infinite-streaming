package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
)

const tsUsage = `harness ts <target|all> [flags]

Combined timeseries — samples + network — from /api/v2/timeseries.
Network rows render like 'tail'; sample rows render the headline
ABR/buffer/RTT/FPS values on a single line.

Flags:
  --streams S      override default 'samples,network' (CSV)
  --bundles B      override default per-stream bundles (CSV)
  --max-hz N       rate-limit live deltas (default 0 = uncapped)
  --raw            print raw frame JSON

Examples:
  harness ts ipad
  harness ts ipad --max-hz 10
  harness ts all --streams samples
`

// tailSampleRow is the projection used when ts renders a sample
// frame. Like tailNetworkRow, every variable-shape field is `any`
// because CH JSONEachRow returns inconsistent JSON types per column.
type tailSampleRow struct {
	Ts                  string `json:"ts"`
	PlayerID            string `json:"player_id"`
	PlayID              string `json:"play_id"`
	State               any    `json:"state,omitempty"`
	BandwidthEstMbps    any    `json:"bandwidth_estimate_mbps,omitempty"`
	BufferSeconds       any    `json:"buffer_seconds,omitempty"`
	RttMs               any    `json:"rtt_ms,omitempty"`
	RenditionMbps       any    `json:"rendition_mbps,omitempty"`
	ShaperLimitMbps     any    `json:"shaper_limit_mbps,omitempty"`
	FpsRunning          any    `json:"fps_running,omitempty"`
	DroppedFramesDelta  any    `json:"dropped_frames_delta,omitempty"`
	Downshifts          any    `json:"downshifts,omitempty"`
}

func cmdTs(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 {
		return errors.New(tsUsage)
	}
	fs := flag.NewFlagSet("ts", flag.ContinueOnError)
	streams := fs.String("streams", "samples,network", "comma-separated streams")
	bundles := fs.String("bundles", "", "comma-separated bundles (defaults per-stream)")
	maxHz := fs.Int("max-hz", 0, "rate cap")
	raw := fs.Bool("raw", false, "raw frame JSON")
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

	fmt.Fprintf(os.Stderr, "ts streams=%s player=%s — Ctrl-C to stop\n",
		strings.Join(params.Streams, ","), labelOrAll(playerID))

	return client.Timeseries(ctx, params, func(f api.SSEFrame) error {
		if *raw {
			fmt.Printf("event:%s id:%s data:%s\n", f.Event, f.ID, f.Data)
			return nil
		}
		if f.Event == "heartbeat" || f.Event == "" {
			return nil
		}
		if asJSON {
			fmt.Println(f.Data)
			return nil
		}
		switch f.Event {
		case "network", "network.row":
			var row tailNetworkRow
			if err := json.Unmarshal([]byte(f.Data), &row); err == nil {
				fmt.Println("N " + formatNetworkRow(row))
			}
		case "sample", "sample.row":
			var row tailSampleRow
			if err := json.Unmarshal([]byte(f.Data), &row); err == nil {
				fmt.Println("S " + formatSampleRow(row))
			}
		default:
			preview := f.Data
			if len(preview) > 140 {
				preview = preview[:137] + "..."
			}
			fmt.Printf("%-12s %s\n", f.Event, preview)
		}
		return nil
	})
}

// formatSampleRow renders one ABR/buffer/RTT/FPS sample. Empty cells
// render as "—" so a missing column doesn't lie as zero.
func formatSampleRow(r tailSampleRow) string {
	state := anyStr(r.State, "—")
	bw := "—"
	if f, ok := anyFloat(r.BandwidthEstMbps); ok {
		bw = fmt.Sprintf("%5.2fMbps", f)
	}
	buf := "—"
	if f, ok := anyFloat(r.BufferSeconds); ok {
		buf = fmt.Sprintf("%4.1fs", f)
	}
	rtt := "—"
	if f, ok := anyFloat(r.RttMs); ok {
		rtt = fmt.Sprintf("%5.1fms", f)
	}
	rend := "—"
	if f, ok := anyFloat(r.RenditionMbps); ok {
		rend = fmt.Sprintf("%5.2fMbps", f)
	}
	fps := "—"
	if f, ok := anyFloat(r.FpsRunning); ok {
		fps = fmt.Sprintf("%4.1f", f)
	}
	drops := "—"
	if n, ok := anyInt(r.DroppedFramesDelta); ok && n > 0 {
		drops = fmt.Sprintf("Δdrop=%d", n)
	}
	return fmt.Sprintf("%s  %-10s buf=%-6s rtt=%-7s bw=%-11s rend=%-11s fps=%-4s %s",
		formatTs(r.Ts), state, buf, rtt, bw, rend, fps, drops)
}
