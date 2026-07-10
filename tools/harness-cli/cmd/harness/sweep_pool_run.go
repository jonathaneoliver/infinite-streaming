package main

// sweep_pool_run.go wires the streaming-pool ENGINE (sweep_pool.go) to the real
// per-experiment work and exposes it as `harness sweep run --concurrent`. The
// engine owns concurrency + device assignment + serviceable claiming; this file
// owns the boot→bootstrap→probe→analyze cycle for one experiment on one device
// — the same steps qe-offhours.sh runs serially, now driven in parallel.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/charmatrix"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

var playIDRe = regexp.MustCompile(`play_id:\s*([0-9a-fA-F-]{36})`)

func cmdSweepRun(client *api.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("sweep run", flag.ContinueOnError)
	concurrent := fs.Bool("concurrent", false, "pack independent experiments across all free farm devices (barrierless streaming pool, #950); without it this command is a no-op stub")
	owner := fs.String("owner", "", "owner id stamped on every claim (default: pool-<pid>)")
	charDir := fs.String("char-dir", envOrDefault("CHAR_DIR", "tests/characterization"), "path to the characterization Go module (drives the probe)")
	durationS := fs.Int("duration-s", envIntOr("QE_DURATION_S", 90), "play window per experiment (seconds)")
	content := fs.String("content", envOrDefault("CHAR_CONTENT", ""), "default catalogue clip when an experiment sets none")
	serviceable := fs.String("serviceable", "", "override the auto-derived serviceable platform set (comma-separated sweep tokens); default = derived from the free farm roster")
	ingestWaitS := fs.Int("ingest-wait-s", 30, "seconds to wait for label ingest before analyze")
	confirmReps := fs.Int("confirm-reps", 1, "confirmation reps to enqueue on a first-pass hit (n=1 guard)")
	maxDevices := fs.Int("max-devices", 0, "cap the worker pool below the free-device count (0 = one worker per free device)")
	maxExperiments := fs.Int("max-experiments", 0, "stop after claiming this many experiments across the pool (0 = drain the serviceable backlog); use a small value for a bounded smoke test")
	dryRun := fs.Bool("dry-run", false, "print the free roster + serviceable set + planned worker count; claim nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*concurrent {
		return errors.New("harness sweep run currently implements only --concurrent (the streaming pool, #950); pass --concurrent")
	}

	free := FreeDevices(availableDevices(deviceFarmBaseURL()))
	if len(free) == 0 {
		return errors.New("no free farm devices — boot the pool (tools/appium-device-farm/boot-pool.sh) or check the farm")
	}
	if *maxDevices > 0 && len(free) > *maxDevices {
		free = free[:*maxDevices]
	}

	// The pool claims per-device by that device's serviceable platform(s). An
	// explicit --serviceable narrows the whole pool to a token subset (e.g. only
	// ipad-sim work tonight); the per-device gate still applies within it.
	override := splitCSV(*serviceable)

	if *dryRun {
		workers := 0
		for _, d := range free {
			toks := sweepPlatformsForDevice(d)
			if len(override) > 0 {
				toks = intersectTokens(toks, override)
			}
			if len(toks) > 0 {
				workers++
			}
		}
		fmt.Printf("streaming pool DRY RUN — %d free device(s), %d worker(s)\n", len(free), workers)
		fmt.Printf("serviceable (auto): %s\n", strings.Join(serviceableTokens(free), ", "))
		if len(override) > 0 {
			fmt.Printf("serviceable (override): %s\n", strings.Join(override, ", "))
		}
		for _, d := range free {
			toks := sweepPlatformsForDevice(d)
			if len(override) > 0 {
				toks = intersectTokens(toks, override)
			}
			marker := ""
			if len(toks) == 0 {
				marker = " (no worker)"
			}
			fmt.Printf("  %-38s %-6s → %s%s\n", d.UDID, d.Platform, strings.Join(toks, ","), marker)
		}
		return nil
	}

	own := *owner
	if own == "" {
		own = fmt.Sprintf("pool-%d", os.Getpid())
	}
	s, err := openStore(client)
	if err != nil {
		return err
	}

	// If --serviceable narrows the pool, drop devices that can service none of the
	// allowed tokens so they don't spin up an idle worker.
	if len(override) > 0 {
		var kept []DeviceCapability
		for _, d := range free {
			if len(intersectTokens(sweepPlatformsForDevice(d), override)) > 0 {
				kept = append(kept, d)
			}
		}
		free = kept
		if len(free) == 0 {
			return fmt.Errorf("no free device can service the --serviceable set %q", *serviceable)
		}
	}

	runner := makeSweepPoolRunner(client, s, *charDir, strings.TrimSpace(*content),
		*durationS, *ingestWaitS, *confirmReps, override)

	fmt.Fprintf(os.Stderr, "streaming pool: %d worker(s) over %s\n", len(free), client.BaseURL)
	ctx := context.Background()
	outcomes := runStreamingPool(ctx, s, free, own, override, *maxExperiments, runner)
	fmt.Print(summarizePool(outcomes))
	return nil
}

// makeSweepPoolRunner builds the poolRunner that runs ONE experiment on its
// assigned device: boot the sim if needed, bootstrap the config-on-connect
// session, drive the probe pinned to the device, wait for ingest, analyze.
func makeSweepPoolRunner(client *api.Client, s *sweep.Store, charDir, contentDefault string, durationS, ingestWaitS, confirmReps int, override []string) poolRunner {
	return func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) poolOutcome {
		out := poolOutcome{ExpID: e.ID, Device: dev.UDID}
		clip := sweep.ContentOrDefault(e.Content)
		if clip == "" {
			clip = contentDefault
		}
		if clip == "" {
			out.Err = errors.New("no content (set --content, CHAR_CONTENT, or the experiment's content)")
			_ = s.Move(sweep.StatusRunning, sweep.StatusBacklog, e)
			return out
		}

		// A shutdown sim must be booted before the probe's Discover can find it.
		// Best-effort; a real device / already-booted sim is a fast no-op.
		if !dev.Real {
			bootSimBestEffort(ctx, dev.UDID)
		}

		// Bootstrap the recipe onto a fresh config-on-connect session.
		pid := uuid.NewString()
		bctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, berr := bootstrapMatrixSession(bctx, client, clip, pid, e.Group, e)
		cancel()
		if berr != nil {
			out.Err = fmt.Errorf("bootstrap: %w", berr)
			_ = s.Move(sweep.StatusRunning, sweep.StatusBacklog, e) // requeue for a retry
			return out
		}

		// Drive the probe pinned to THIS device and capture its play_id. The probe
		// filters discovered devices by CHAR_SWEEP_PLATFORM, which must be the
		// RUNNER's platform for the assigned device (e.g. every iOS sim is
		// discovered as ipad-sim) — NOT the experiment's sweep token (iphone), or
		// the probe finds no matching device and skips.
		playID, perr := runSweepProbeCapture(ctx, client.BaseURL, charDir, e, runnerPlatformForDevice(dev), pid, dev.UDID, clip, durationS)
		if perr != nil || playID == "" {
			if perr == nil {
				perr = errors.New("probe produced no play_id (crash/inconclusive)")
			}
			out.Err = fmt.Errorf("probe: %w", perr)
			_ = s.Move(sweep.StatusRunning, sweep.StatusBacklog, e) // requeue
			return out
		}

		// Let the forwarder ingest the play's labels before the oracle reads them;
		// analyzing too early reads 0 labels → false inconclusive.
		if !sleepCtx(ctx, time.Duration(ingestWaitS)*time.Second) {
			out.Err = errors.New("cancelled before analyze")
			return out
		}
		bucket, _, _, aerr := analyzeExperiment(client, s, e, sweep.StatusRunning, playID, confirmReps)
		if aerr != nil {
			out.Err = fmt.Errorf("analyze: %w", aerr)
			return out
		}
		if e.Result != nil {
			out.Verdict = string(e.Result.Verdict)
		} else {
			out.Verdict = string(bucket)
		}
		return out
	}
}

// runSweepProbeCapture runs TestSweepProbe pinned to udid, streaming its output
// to stderr (device-prefixed) while capturing it to parse the play_id. Mirrors
// driveProbe's env, sourced from the experiment via the #873 bridge so the
// client knobs (segment / offset / codec / pattern / …) match the recipe.
func runSweepProbeCapture(ctx context.Context, base, charDir string, e *sweep.Experiment, probePlatform, playerID, udid, clip string, durationS int) (string, error) {
	if durationS <= 0 {
		durationS = 60
	}
	a := charmatrix.ArmFromExperiment(e)
	timeout := time.Duration(durationS+240) * time.Second
	cmd := exec.CommandContext(ctx, "go", "test", "./modes", "-run", "TestSweepProbe", "-count=1",
		"-timeout", fmt.Sprintf("%ds", int(timeout.Seconds())), "-v")
	cmd.Dir = charDir
	var buf bytes.Buffer
	// Prefix each probe's stderr with its UDID so concurrent workers are legible.
	pw := &prefixWriter{w: os.Stderr, prefix: []byte("[" + shortUDID(udid) + "] ")}
	cmd.Stdout = io.MultiWriter(pw, &buf)
	cmd.Stderr = io.MultiWriter(pw, &buf)
	cmd.Env = append(os.Environ(),
		"LAUNCH_MODE=appium",
		"HARNESS_BASE_URL="+base,
		"CHAR_PLAYER_ID="+playerID,
		"CHARACTERIZATION_DEVICE_UDID="+udid,
		"CHAR_SWEEP_PLATFORM="+probePlatform,
		// Pin the app to THIS run's server on startup (-is.server_url, #942) so the
		// probe doesn't depend on the sim's saved server — an unseeded sim otherwise
		// hits the picker and never streams.
		"CHAR_SWEEP_SERVER_URL="+base,
		"CHAR_SWEEP_DURATION_S="+strconv.Itoa(durationS),
		"CHAR_SWEEP_SEGMENT="+a.Segment,
		"CHAR_SWEEP_LIVE_OFFSET="+a.ClientLiveOffsetS(),
		"CHAR_SWEEP_PROTOCOL="+a.Protocol,
		"CHAR_SWEEP_CODEC="+a.Codec,
		"CHAR_SWEEP_PEAK_BITRATE="+strconv.Itoa(a.PeakBitrateMbps),
		"CHAR_SWEEP_FIRST_VARIANT="+a.StartsFirstVariantS(),
		"CHAR_SWEEP_MUTED="+a.MutedS(),
		"CHAR_SWEEP_PATTERN="+a.ShapePattern(),
		"CHAR_SWEEP_STEP_S="+strconv.Itoa(a.ShapeStepS()),
		"CHAR_SWEEP_MARGIN="+strconv.Itoa(a.ShapeMargin()),
		"CHAR_CONTENT="+clip,
	)
	runErr := cmd.Run()
	if m := playIDRe.FindStringSubmatch(buf.String()); len(m) == 2 {
		return m[1], nil // a play_id means the probe streamed, even if `go test` later non-zeroed
	}
	return "", runErr
}

// bootSimBestEffort boots a simulator and waits for it, so the probe's Discover
// (booted-only) can find it. Best-effort: an error (non-darwin, already booted,
// a real device passed by mistake) is ignored — the probe's own Skip handles a
// still-absent device.
func bootSimBestEffort(ctx context.Context, udid string) {
	if udid == "" {
		return
	}
	bctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// bootstatus -b boots if needed and blocks until booted.
	_ = exec.CommandContext(bctx, "xcrun", "simctl", "bootstatus", udid, "-b").Run()
}

// envIntOr reads an integer env var, falling back to def when unset/unparseable.
func envIntOr(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// intersectTokens returns the elements of a that are also in b (order of a).
func intersectTokens(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range b {
		set[s] = true
	}
	var out []string
	for _, s := range a {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}

func shortUDID(u string) string {
	if len(u) <= 8 {
		return u
	}
	return u[:8]
}

// prefixWriter prefixes each line written to it — so interleaved concurrent
// probe output stays attributable to its device.
type prefixWriter struct {
	w      io.Writer
	prefix []byte
	atBOL  bool
	once   bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if !p.once {
		p.atBOL = true
		p.once = true
	}
	var out []byte
	for _, c := range b {
		if p.atBOL {
			out = append(out, p.prefix...)
			p.atBOL = false
		}
		out = append(out, c)
		if c == '\n' {
			p.atBOL = true
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}
