package main

// sweep_pool.go is the STREAMING POOL executor (issue #950): a barrierless
// concurrent dispatcher that packs independent sweep experiments across the free
// Appium-farm devices (availableDevices, #948), one worker per device, each
// claiming serviceable work (the #949 platform gate) and running it until its
// platform's backlog is dry. This is the counterpart to the SYNCHRONIZED fleet
// mode (driveFleet): there is NO shared start barrier — a device grabs the next
// item the instant it frees, so N sims run N independent 1-sim tests at once
// (acceptance scenario 2), and reps (#951) ride the same executor.
//
// The engine below (runStreamingPool) is deliberately decoupled from the actual
// bootstrap→probe→analyze work via the poolRunner func, so the concurrency +
// claim-until-dry + per-device serviceable logic is unit-tested against a fake
// runner without a live farm.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

// poolClaimer is the slice of the sweep Store the pool needs: an atomic,
// serviceable-gated claim, plus a requeue for the rare post-claim device
// mismatch. The real *sweep.Store satisfies it; tests inject a fake.
type poolClaimer interface {
	ClaimNext(owner string, serviceable ...string) (*sweep.Experiment, error)
	Move(from, to sweep.Status, e *sweep.Experiment) error
}

// poolOutcome is one experiment's disposition in a streaming-pool run, with the
// per-phase timing (#946) that makes the cold-vs-warm / fresh-vs-warm-session
// delta measurable. All durations are milliseconds; StartedAt is UTC RFC3339.
type poolOutcome struct {
	ExpID   string
	Device  string // UDID the worker ran it on
	Verdict string // sweep verdict, when the run produced one
	Skipped bool   // claimed but the device didn't satisfy a finer requirement → requeued
	Err     error

	StartedAt   string // UTC, when this experiment's work began (wire = UTC)
	BootstrapMs int64  // config-on-connect session setup
	BringupMs   int64  // session-create + app launch + resume-to-play (the probe's PROBE_TIMING) — what warm shrinks
	ProbeMs     int64  // full probe subprocess wall-time (bring-up + play window)
	AnalyzeMs   int64  // ingest wait + oracle analyze
	TotalMs     int64  // whole experiment, bootstrap → verdict

	// Rep-batch (#946) — set only under a rep-batch. StartMode is cold|warm;
	// Reps carries each iteration's teardown (stop the previous) + startup
	// (launch+resume the next), kept separate so the warm saving is visible.
	StartMode string
	Reps      []repResult
}

// poolRunner does the real per-experiment work on a specific device: bootstrap
// the config-on-connect session, drive the probe pinned to dev, wait for
// ingest, analyze. Returns the verdict (or an error). Injected so the pool's
// scheduling is testable in isolation.
type poolRunner func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) poolOutcome

// sweepPlatformsForDevice maps a farm device capability to the sweep platform
// token(s) it can service. The farm reports coarse platforms (ios/tvos/android)
// + realDevice + a human name; sweep experiments carry fine tokens (ipad-sim /
// iphone / ipad / appletv / androidtv). The -sim suffix on the emitted tokens is
// what keeps a simulator worker from claiming real-hardware work (and vice
// versa) through the platform-level claim gate (#949).
func sweepPlatformsForDevice(d DeviceCapability) []string {
	switch strings.ToLower(d.Platform) {
	case "tvos":
		return []string{"appletv"}
	case "android":
		return []string{"androidtv"}
	case "ios":
		isPad := strings.Contains(strings.ToLower(d.Name), "ipad")
		if d.Real {
			if isPad {
				return []string{"ipad"}
			}
			return []string{"iphone"}
		}
		if isPad {
			return []string{"ipad-sim"}
		}
		// An iPhone simulator services both the explicit `iphone-sim` token and
		// the bare `iphone` the seed uses (qe-offhours maps iphone→a sim UDID).
		return []string{"iphone-sim", "iphone"}
	}
	return nil
}

// runnerPlatformForDevice returns the platform the characterization RUNNER will
// classify this device as during discovery — which the probe filters on. It
// differs from the sweep tokens: the runner's mapSimRuntime labels EVERY iOS
// simulator `ipad-sim` (never iphone-sim), so a Fleet iPhone 15 sim is
// discovered as ipad-sim. Passing the experiment's own token (e.g. iphone) as
// CHAR_SWEEP_PLATFORM would make the probe find no matching device and skip.
func runnerPlatformForDevice(d DeviceCapability) string {
	switch strings.ToLower(d.Platform) {
	case "tvos":
		return "appletv"
	case "android":
		return "androidtv"
	case "ios":
		if d.Real {
			if strings.Contains(strings.ToLower(d.Name), "ipad") {
				return "ipad"
			}
			return "iphone"
		}
		return "ipad-sim" // the runner classifies every iOS simulator as ipad-sim
	}
	return d.Platform
}

// serviceableTokens is the union of the sweep platform tokens the given free
// devices can service — the allow-list a caller passes to `sweep next --claim
// --serviceable` so unserviceable work is never returned (scenario 3's gate).
func serviceableTokens(devices []DeviceCapability) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range devices {
		for _, tok := range sweepPlatformsForDevice(d) {
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	return out
}

// deviceMatchesRequirement checks the finer device requirement (#949) the
// platform-level claim gate can't express: a specific device_udid pin, and the
// require_real sim-vs-hardware constraint. Platform is already matched by the
// claim; this catches the case where an experiment pinned to THE iphone was
// claimed by a different same-platform device.
func deviceMatchesRequirement(dev DeviceCapability, e *sweep.Experiment) bool {
	if e.DeviceUDID != "" && !strings.EqualFold(e.DeviceUDID, dev.UDID) {
		return false
	}
	if e.RequireReal != nil && *e.RequireReal != dev.Real {
		return false
	}
	return true
}

// runStreamingPool runs one worker per device concurrently. Each worker loops:
// claim the top serviceable experiment for ITS platform(s) → run it on its
// device → repeat, until the claim returns nil (that platform's backlog is dry)
// or ctx is cancelled. No shared barrier: a worker never waits on another, so a
// fast device keeps pulling work while a slow one is mid-play. Returns every
// outcome (order non-deterministic). The server-side claim is atomic, so two
// workers never get the same experiment.
// allow, when non-empty, narrows every worker's claim to that sweep-token subset
// (the `--serviceable` override) — a device still only claims the intersection of
// what it can physically run and what the operator allowed tonight. Empty ⇒ each
// device claims its full platform set.
// maxExperiments, when >0, caps the total experiments the pool will claim across
// all workers (a bounded run — e.g. a smoke test); 0 = unbounded (drain the
// serviceable backlog).
func runStreamingPool(ctx context.Context, claimer poolClaimer, devices []DeviceCapability, owner string, allow []string, maxExperiments int, run poolRunner) []poolOutcome {
	var (
		mu       sync.Mutex
		outcomes []poolOutcome
		wg       sync.WaitGroup
		claimed  int32 // total claims dispatched, for the maxExperiments cap
	)
	record := func(o poolOutcome) {
		mu.Lock()
		outcomes = append(outcomes, o)
		mu.Unlock()
	}
	for _, dev := range devices {
		dev := dev
		tokens := sweepPlatformsForDevice(dev)
		// Also let this worker claim work keyed on its RUNNER platform — what the
		// device runs AS (every iOS sim runs as ipad-sim, per runnerPlatformForDevice).
		// This is what matches char items (keyed on the runner platform, e.g.
		// TestRampupIPadSim) to a Fleet iPhone 15 sim whose raw sweep tokens are
		// iphone-sim/iphone; it also lets a sim claim ipad-sim sweep work it can run.
		if rp := runnerPlatformForDevice(dev); rp != "" && !containsToken(tokens, rp) {
			tokens = append(tokens, rp)
		}
		if len(allow) > 0 {
			tokens = intersectTokens(tokens, allow)
		}
		if len(tokens) == 0 {
			continue // unmappable, or nothing this device can run is in the allow-list
		}
		// Each worker claims under a UNIQUE owner (base + device). The server-side
		// claim arbitrates by owner (argMin(owner,…) → winner==owner); if two
		// workers shared an owner, both would see winner==owner and both promote the
		// SAME experiment — a double-claim (observed live: 3 sims ran one exp id).
		// A per-device owner makes the arbitration pick exactly one winner.
		workerOwner := owner + "-" + shortUDID(dev.UDID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				// Bounded-run cap: reserve a slot atomically before claiming, so at
				// most maxExperiments claims proceed across all workers.
				if maxExperiments > 0 && atomic.AddInt32(&claimed, 1) > int32(maxExperiments) {
					return
				}
				e, err := claimer.ClaimNext(workerOwner, tokens...)
				if err != nil {
					record(poolOutcome{Device: dev.UDID, Err: err})
					return // a claim transport error stops this worker (don't hot-loop)
				}
				if e == nil {
					return // dry for this device's platform(s)
				}
				// Finer requirement the platform gate can't express: if THIS device
				// doesn't satisfy it, requeue and let a matching runner take it.
				if !deviceMatchesRequirement(dev, e) {
					_ = claimer.Move(sweep.StatusRunning, sweep.StatusBacklog, e)
					record(poolOutcome{ExpID: e.ID, Device: dev.UDID, Skipped: true})
					continue
				}
				record(run(ctx, e, dev))
			}
		}()
	}
	wg.Wait()
	return outcomes
}

// summarizePool renders a one-line-per-outcome summary of a streaming-pool run.
// summarizePool renders one line per outcome with its phase timing (bring-up /
// probe / total), then an aggregate — including total work-time vs the run's
// wall-clock (wallMs), which shows the concurrency win and, once warm mode
// lands, the bring-up saving. wallMs ≤ 0 omits the concurrency line.
func summarizePool(outcomes []poolOutcome, wallMs int64) string {
	var b strings.Builder
	var ran, skipped, errs int
	var sumWork, sumBringup int64
	fmt.Fprintf(&b, "  %-6s %-40s %-9s %8s %8s %8s\n", "STATUS", "EXPERIMENT", "DEVICE", "bringup", "probe", "total")
	for _, o := range outcomes {
		status := strings.ToUpper(orDash(o.Verdict))
		switch {
		case o.Err != nil:
			errs++
			status = "ERR"
		case o.Skipped:
			skipped++
			status = "SKIP"
		default:
			ran++
		}
		sumWork += o.TotalMs
		sumBringup += o.BringupMs
		fmt.Fprintf(&b, "  %-6s %-40s %-9s %7ds %7ds %7ds\n",
			status, truncate(o.ExpID, 40), shortUDID(o.Device),
			o.BringupMs/1000, o.ProbeMs/1000, o.TotalMs/1000)
		// Rep-batch (#946): show per-rep teardown (stop the previous) + startup
		// (launch+resume) separately, so the warm saving is visible (warm startup
		// ~1s vs cold ~9s) AND the shutdown+relaunch cost is kept.
		if len(o.Reps) > 0 {
			parts := make([]string, len(o.Reps))
			for i, r := range o.Reps {
				parts[i] = fmt.Sprintf("r%d %s tear=%.1fs start=%.1fs", i, r.Mode,
					float64(r.TeardownMs)/1000, float64(r.StartupMs)/1000)
			}
			fmt.Fprintf(&b, "         rep-batch (%s): %s\n", orDash(o.StartMode), strings.Join(parts, " · "))
		}
	}
	fmt.Fprintf(&b, "streaming pool: %d ran, %d skipped, %d errored\n", ran, skipped, errs)
	if n := int64(len(outcomes)); n > 0 {
		fmt.Fprintf(&b, "timing: bring-up avg %ds · work-time %ds total", sumBringup/1000/max64(n, 1), sumWork/1000)
		if wallMs > 0 {
			// wall-clock < sum-of-work by the concurrency factor; the ratio is the
			// effective parallelism the pool achieved.
			fmt.Fprintf(&b, " · wall-clock %ds (%.1f× parallel)", wallMs/1000, float64(sumWork)/float64(max64(wallMs, 1)))
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
