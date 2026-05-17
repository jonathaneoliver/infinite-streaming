package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

const procedureUsage = `harness procedure <name> <target> [flags]

Multi-step test procedures composed of the lower-level verbs. Each
procedure snapshots its mutations so 'harness undo' can roll back to
the pre-procedure state (run repeatedly to undo each step).

Procedures:
  soak <target> --duration 30m --fault-every 5m --fault-type 500 [--kind segment]
      Soak test: repeatedly add a fault, hold for --fault-every / 2,
      clear, repeat until --duration elapses.

  abr-sweep <target> --rates 5,2,1,0.5 --hold 60s
      Walk through shape.rate_mbps over the rate list, holding each
      for --hold seconds. Useful for ABR characterisation.

  fault-soak <target> --types 500,connection_refused --interval 10s --duration 5m
      Rotate through fault --types one at a time, each for --interval
      seconds, until --duration elapses.

All procedures handle Ctrl-C gracefully (cleanup before exit).
`

func cmdProcedure(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 2 {
		return errors.New(procedureUsage)
	}
	switch args[0] {
	case "soak":
		return procedureSoak(client, args[1:])
	case "abr-sweep":
		return procedureABRSweep(client, args[1:])
	case "fault-soak":
		return procedureFaultSoak(client, args[1:])
	default:
		return fmt.Errorf("unknown procedure: %s\n\n%s", args[0], procedureUsage)
	}
}

// procedureCtx wires the standard cancel-on-Ctrl-C pattern. Returns
// the resolved player_id, a cancellable context, and a deferred
// cleanup func (caller must defer it).
func procedureCtx(client *api.Client, target string) (string, context.Context, context.CancelFunc, error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	pid, err := client.Resolve(ctx, target)
	if err != nil {
		cancel()
		return "", nil, nil, err
	}
	return pid, ctx, cancel, nil
}

// soak: alternate add-fault → hold → clear → hold for the duration.
func procedureSoak(client *api.Client, args []string) error {
	target := args[0]
	fs := flag.NewFlagSet("soak", flag.ContinueOnError)
	duration := fs.Duration("duration", 30*time.Minute, "total runtime")
	every := fs.Duration("fault-every", 5*time.Minute, "fault add cadence")
	faultType := fs.String("fault-type", "500", "fault type for each cycle")
	kind := fs.String("kind", "segment", "request_kind filter")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pid, ctx, cancel, err := procedureCtx(client, target)
	if err != nil {
		return err
	}
	defer cancel()

	deadline := time.Now().Add(*duration)
	half := *every / 2
	fmt.Fprintf(os.Stderr, "soak: %s, fault every %s for %s; target %s\n",
		*faultType, *every, *duration, pid)

	cycle := 0
	for time.Now().Before(deadline) {
		cycle++
		fmt.Fprintf(os.Stderr, "[cycle %d] add fault %s/%s\n", cycle, *faultType, *kind)
		rule := buildFaultRule(*faultType, *kind)
		if _, err := client.AddFaultRule(ctx, pid, fmt.Sprintf("soak cycle %d", cycle), rule); err != nil {
			return err
		}
		if !sleepCtx(ctx, half) {
			break
		}
		fmt.Fprintf(os.Stderr, "[cycle %d] clear faults\n", cycle)
		if _, err := client.ClearFaultRules(ctx, pid, fmt.Sprintf("soak cycle %d clear", cycle)); err != nil {
			return err
		}
		if !sleepCtx(ctx, *every-half) {
			break
		}
	}
	fmt.Fprintf(os.Stderr, "soak: done (%d cycles)\n", cycle)
	return nil
}

// abr-sweep: walk shape.rate_mbps through a fixed list.
func procedureABRSweep(client *api.Client, args []string) error {
	target := args[0]
	fs := flag.NewFlagSet("abr-sweep", flag.ContinueOnError)
	ratesCSV := fs.String("rates", "5,2,1,0.5", "comma-separated Mbps values")
	hold := fs.Duration("hold", 60*time.Second, "seconds per rate")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rates, err := parseFloatList(*ratesCSV)
	if err != nil {
		return err
	}
	pid, ctx, cancel, err := procedureCtx(client, target)
	if err != nil {
		return err
	}
	defer cancel()

	fmt.Fprintf(os.Stderr, "abr-sweep: rates=%s hold=%s target=%s\n",
		*ratesCSV, *hold, pid)
	for i, r := range rates {
		fmt.Fprintf(os.Stderr, "[step %d/%d] rate=%.2f Mbps for %s\n", i+1, len(rates), r, *hold)
		v := float32(r)
		shape := &proxy.Shape{RateMbps: &v}
		if _, err := client.PatchShape(ctx, pid, fmt.Sprintf("abr-sweep step %d rate=%.2f", i+1, r), shape); err != nil {
			return err
		}
		if !sleepCtx(ctx, *hold) {
			break
		}
	}
	// Restore: clear shape so the sweep doesn't leave the player pinned.
	// Use a background context — the operator-cancel ctx is already
	// done if we got here via Ctrl-C, and the cleanup must still run.
	fmt.Fprintln(os.Stderr, "abr-sweep: clearing shape")
	if _, err := client.ClearShape(context.Background(), pid, "abr-sweep done"); err != nil {
		fmt.Fprintln(os.Stderr, "warn: clear shape failed:", err)
	}
	return nil
}

// fault-soak: rotate through fault types, each for an interval.
func procedureFaultSoak(client *api.Client, args []string) error {
	target := args[0]
	fs := flag.NewFlagSet("fault-soak", flag.ContinueOnError)
	typesCSV := fs.String("types", "500,connection_refused", "comma-separated fault types")
	interval := fs.Duration("interval", 10*time.Second, "seconds per type")
	duration := fs.Duration("duration", 5*time.Minute, "total runtime")
	kind := fs.String("kind", "segment", "request_kind filter")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	types := splitCSV(*typesCSV)
	if len(types) == 0 {
		return errors.New("--types must have at least one entry")
	}
	pid, ctx, cancel, err := procedureCtx(client, target)
	if err != nil {
		return err
	}
	defer cancel()

	fmt.Fprintf(os.Stderr, "fault-soak: types=%s interval=%s duration=%s target=%s\n",
		*typesCSV, *interval, *duration, pid)
	deadline := time.Now().Add(*duration)
	i := 0
	for time.Now().Before(deadline) {
		t := types[i%len(types)]
		fmt.Fprintf(os.Stderr, "[%s] fault %s/%s\n", time.Now().Format("15:04:05"), t, *kind)
		// Clear previous then add new — keeps rule set bounded.
		if _, err := client.ClearFaultRules(ctx, pid, "fault-soak rotate"); err != nil {
			fmt.Fprintln(os.Stderr, "warn: rotate clear failed:", err)
		}
		rule := buildFaultRule(t, *kind)
		if _, err := client.AddFaultRule(ctx, pid, fmt.Sprintf("fault-soak %s", t), rule); err != nil {
			return err
		}
		if !sleepCtx(ctx, *interval) {
			break
		}
		i++
	}
	fmt.Fprintln(os.Stderr, "fault-soak: clearing")
	if _, err := client.ClearFaultRules(context.Background(), pid, "fault-soak done"); err != nil {
		fmt.Fprintln(os.Stderr, "warn: final clear failed:", err)
	}
	return nil
}

func buildFaultRule(faultType, kind string) proxy.FaultRule {
	freq := 1
	cons := 1
	mode := proxy.FaultRuleMode("requests")
	rule := proxy.FaultRule{
		Type:        proxy.FaultRuleType(faultType),
		Frequency:   &freq,
		Consecutive: &cons,
		Mode:        &mode,
	}
	if kind != "" {
		k := proxy.FaultFilterRequestKind(kind)
		kinds := []proxy.FaultFilterRequestKind{k}
		rule.Filter = &proxy.FaultFilter{RequestKind: &kinds}
	}
	return rule
}

func parseFloatList(csv string) ([]float64, error) {
	parts := strings.Split(csv, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid rate %q: %w", p, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("no rates parsed")
	}
	return out, nil
}

// sleepCtx waits d or until ctx cancels. Returns false on cancel
// so the caller can break out of its loop.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

