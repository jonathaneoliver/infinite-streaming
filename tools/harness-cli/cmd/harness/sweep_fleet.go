package main

// sweep_fleet.go runs a sweep ISOLATION FAN concurrently on the device farm —
// one device per arm, all sharing the isolation group/pattern (issue #874) —
// instead of serially on one operator-pinned sim. It reuses the char-matrix
// fleet machinery: a fan Experiment → charmatrix.Arm (ArmFromExperiment, #873) →
// charplan.ArmConfig (ToArmConfig) → a charplan.RunPlan driven through driveFleet
// → TestCharMatrixFleet → resolveFleetDeviceFarm. The single-device path stays
// the default for singletons (the normal sweep loop) — only ≥2-arm fans fan out.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/charplan"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/charmatrix"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

// runLevelFromEnv resolves the run-level startup/recovery knobs once, the same
// way runMatrixParallel does (shared by every arm in the fleet).
func runLevelFromEnv() charmatrix.RunLevel {
	return charmatrix.RunLevel{
		StartupFwdBufferS:  strings.TrimSpace(os.Getenv("CHAR_FWD_BUFFER_S")),
		StartupFwdRelease:  strings.TrimSpace(os.Getenv("CHAR_FWD_RELEASE")),
		PersistentPeakMbps: strings.TrimSpace(os.Getenv("CHAR_PERSIST_PEAK")),
		LocalProxy:         charplan.ParseBool(os.Getenv("CHAR_LOCAL_PROXY")),
		AutoRecovery:       charplan.ParseBool(os.Getenv("CHAR_AUTO_RECOVERY")),
	}
}

// buildFanRunPlan converts a sweep isolation fan into a charplan.RunPlan: each
// experiment → ArmFromExperiment (#873) → ToArmConfig with its bootstrapped
// player_id; the first full-ladder pattern arm masters (slaves bind via the
// shared isolation group). playerIDs is index-aligned with fan ("" =
// bootstrap-failed → zero-value ArmConfig → that fleet index skips). Pure (no
// I/O), so it is unit-testable as the deterministic core of #874.
func buildFanRunPlan(fan []*sweep.Experiment, playerIDs []string, rl charmatrix.RunLevel, baseURL, manifest string, durationS int) *charplan.RunPlan {
	arms := fanArms(fan)
	master, _ := charmatrix.PatternMasterIndex(arms)
	planArms := make([]charplan.ArmConfig, len(fan))
	for i, a := range arms {
		if i >= len(playerIDs) || playerIDs[i] == "" {
			continue // zero-value ArmConfig → the probe skips this fleet index
		}
		clip := sweep.ContentOrDefault(fan[i].Content)
		planArms[i] = a.ToArmConfig(playerIDs[i], clip, rl, i == master)
	}
	if durationS <= 0 {
		durationS = 60
	}
	platform := ""
	if len(fan) > 0 {
		platform = fan[0].Platform
	}
	return &charplan.RunPlan{
		BaseURL:        baseURL,
		Platform:       platform,
		FleetCount:     len(fan),
		DurationS:      durationS,
		DeviceManifest: manifest,
		Arms:           planArms,
	}
}

// fanArms projects a fan of experiments into char-matrix arms (the #873 bridge),
// preserving order so FleetIndex == position.
func fanArms(fan []*sweep.Experiment) []*charmatrix.Arm {
	arms := make([]*charmatrix.Arm, len(fan))
	for i, e := range fan {
		arms[i] = charmatrix.ArmFromExperiment(e)
	}
	return arms
}

// fanWindow is the shared play window: the longest arm's duration, or 60.
func fanWindow(fan []*sweep.Experiment, override int) int {
	if override > 0 {
		return override
	}
	w := 0
	for _, e := range fan {
		if e.DurationS > w {
			w = e.DurationS
		}
	}
	if w <= 0 {
		w = 60
	}
	return w
}

func cmdSweepRunFan(client *api.Client, args []string, asJSON bool) error {
	var parent string
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		parent, rest = args[0], args[1:]
	}
	fs := flag.NewFlagSet("sweep run-fan", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "build + print the fleet RunPlan; bootstrap nothing, launch no devices")
	durationS := fs.Int("duration-s", 0, "shared play window in seconds (0 = longest arm's, default 60)")
	charDir := fs.String("char-dir", envOrDefault("CHAR_DIR", "tests/characterization"), "path to the characterization Go module (drives the fleet probe)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if parent == "" {
		return errors.New("usage: harness sweep run-fan <parent-experiment-id> [--dry-run] [--duration-s N]")
	}

	s, err := openStore(client)
	if err != nil {
		return err
	}
	all, err := s.List("") // a fan can span buckets (control in found, variants in backlog)
	if err != nil {
		return err
	}
	// The isolation fan is the control + single-axis-flip variants IsolationFan
	// stamped with Parent==parent AND Kind==isolation. Filtering on Kind excludes
	// confirmation reps (same Parent, but same-recipe re-runs, not flip variants)
	// so a rep-batch never gets folded into the fleet fan.
	var fan []*sweep.Experiment
	for _, e := range all {
		if e.Parent == parent && e.Kind == sweep.KindIsolation {
			fan = append(fan, e)
		}
	}
	sort.Slice(fan, func(i, j int) bool { return fan[i].ID < fan[j].ID })

	// Gate: only ≥2-arm fans fan out. A singleton stays on the single-device
	// probe (the normal sweep loop) — the loop's cost-per-tick discipline.
	if len(fan) < 2 {
		return fmt.Errorf("run-fan needs a ≥2-arm isolation fan; parent %q has %d member(s). A singleton runs on the single-device probe (claim it via the normal sweep loop)", parent, len(fan))
	}

	rl := runLevelFromEnv()
	window := fanWindow(fan, *durationS)

	if *dryRun {
		// Placeholder player_ids so the plan is fully populated for inspection;
		// no session is bootstrapped, no device is touched.
		ph := make([]string, len(fan))
		for i := range ph {
			ph[i] = fmt.Sprintf("dry-run-arm-%d", i)
		}
		plan := buildFanRunPlan(fan, ph, rl, client.BaseURL, "", window)
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(plan)
		}
		arms := fanArms(fan)
		master, thinned := charmatrix.PatternMasterIndex(arms)
		fmt.Printf("DRY RUN — fleet RunPlan for fan parent %q: %d arms, group=%q, shared window=%ds\n",
			parent, len(fan), fan[0].Group, window)
		for i, e := range fan {
			pattern := arms[i].ShapePattern()
			if pattern == "" {
				pattern = "-"
			}
			fmt.Printf("  arm %d  %-44s platform=%-9s seg=%-3s pattern=%-8s master=%v\n",
				i, e.ID, e.Platform, e.Segment, pattern, i == master)
		}
		if master < 0 {
			fmt.Println("note: no pattern arm — nothing masters a shared bandwidth timeline")
		} else if thinned {
			fmt.Printf("note: master arm %d has a thinned ladder (no full-ladder pattern arm)\n", master)
		}
		return nil
	}

	// Real run: bootstrap each arm into the SHARED isolation group (so the proxy
	// propagates the master's pattern to all), then drive the fleet — one device
	// per arm — via the existing TestCharMatrixFleet path (device-farm allocation,
	// #853 release-on-interrupt + reap all live in driveFleet).
	playerIDs := make([]string, len(fan))
	var armEnv []string
	for i, e := range fan {
		clip := sweep.ContentOrDefault(e.Content)
		pid := uuid.NewString()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, berr := bootstrapMatrixSession(ctx, client, clip, pid, e.Group, e)
		cancel()
		if berr != nil {
			fmt.Fprintf(os.Stderr, "arm %d bootstrap failed (%s): %v — fleet index %d will skip\n", i, e.ID, berr, i)
			continue
		}
		playerIDs[i] = pid
		armEnv = append(armEnv, fmt.Sprintf("CHAR_ARM_%d_PLATFORM=%s", i, e.Platform))
		fmt.Fprintf(os.Stderr, "bootstrapped arm %d/%d: %s (player_id=%s)\n", i+1, len(fan), e.ID, pid)
	}

	plan := buildFanRunPlan(fan, playerIDs, rl, client.BaseURL, "", window)
	if err := driveFleet(client, fan[0].Platform, len(fan), window, *charDir, armEnv, plan.Arms); err != nil {
		return fmt.Errorf("fleet run: %w", err)
	}
	fmt.Printf("fan %q drove %d arms concurrently on the fleet; analyze each via 'harness sweep analyze <id> --play <play_id>'\n", parent, len(fan))
	return nil
}
