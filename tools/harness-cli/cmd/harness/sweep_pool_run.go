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
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/charmatrix"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

var playIDRe = regexp.MustCompile(`play_id:\s*([0-9a-fA-F-]{36})`)
var bringupRe = regexp.MustCompile(`PROBE_TIMING bringup_ms=(\d+)`)

var (
	modesBinOnce sync.Once
	modesBinPath string
	modesBinErr  error
)

// ensureModesBinary compiles the characterization `modes` test package into a
// standalone binary ONCE, and returns its path. The pool then runs that binary
// directly instead of via `go test ./modes` — critical for leak-hardening: the
// `go test` wrapper does NOT propagate SIGTERM to the test binary it spawns (it
// kills the child), so on ctx-cancel neither the modes interruptContext nor the
// interrupt backstop in the binary ever runs, and the appium/proxy session
// leaks. Running the binary directly means hardenSubprocessCancel's SIGTERM
// reaches the process that actually holds the session, which then frees both
// slots before exiting (proven: a direct SIGTERM releases the sim in ~3s, a
// `go test`-wrapped one leaks). Bonus: no per-probe recompile.
func ensureModesBinary(charDir string) (string, error) {
	modesBinOnce.Do(func() {
		out := filepath.Join(os.TempDir(), "sweep-modes.test")
		cmd := exec.Command("go", "test", "-c", "-o", out, "./modes")
		cmd.Dir = charDir
		if b, err := cmd.CombinedOutput(); err != nil {
			modesBinErr = fmt.Errorf("compile modes test binary: %w\n%s", err, b)
			return
		}
		modesBinPath = out
	})
	return modesBinPath, modesBinErr
}

// hardenSubprocessCancel makes ctx-cancel deliver a CATCHABLE SIGTERM to the
// test binary's whole process group, instead of the default SIGKILL to just the
// `go test` wrapper (which orphans the child modes.test binary — the one holding
// the appium/WDA session — leaving the sim busy). With Setpgid the binary shares
// the wrapper's group, so signalling -pid reaches it; its interrupt backstop
// then frees the appium + proxy slots and exits within the grace window.
// WaitDelay force-kills the group if it doesn't exit in time. macOS/Linux only,
// which is where the harness runs.
func hardenSubprocessCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) // -pid = whole group
	}
	cmd.WaitDelay = 20 * time.Second
}

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
	repBatch := fs.Int("rep-batch", 0, "run each claimed experiment as an N-rep batch in ONE warm appium session (#946), instead of a single cold probe; 0/1 = single play")
	startMode := fs.String("start-mode", "cold", "rep-batch start mode: cold (relaunch the app per rep) | warm (resume-in-place, no relaunch — warm buffers/ABR)")
	dryRun := fs.Bool("dry-run", false, "print the free roster + serviceable set + planned worker count; claim nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*concurrent {
		return errors.New("harness sweep run currently implements only --concurrent (the streaming pool, #950); pass --concurrent")
	}
	if *startMode != "cold" && *startMode != "warm" {
		return fmt.Errorf("--start-mode %q invalid (cold|warm)", *startMode)
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
		*durationS, *ingestWaitS, *confirmReps, override, *repBatch, *startMode)

	fmt.Fprintf(os.Stderr, "streaming pool: %d worker(s) over %s\n", len(free), client.BaseURL)
	// Cancel the whole pool (and each in-flight probe subprocess) on Ctrl-C /
	// SIGTERM. This is REQUIRED for the leak-hardening: the probe subprocesses run
	// in their own process group (Setpgid, see hardenSubprocessCancel), so the
	// terminal's Ctrl-C no longer reaches them — cancelling this ctx is the only
	// path that fires cmd.Cancel → SIGTERM → the binary's backstop frees its
	// appium + proxy slots. Without it a Ctrl-C would orphan the subprocesses.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runStart := time.Now()
	outcomes := runStreamingPool(ctx, s, free, own, override, *maxExperiments, runner)
	wallMs := time.Since(runStart).Milliseconds()
	fmt.Print(summarizePool(outcomes, wallMs))
	return nil
}

// makeSweepPoolRunner builds the poolRunner that runs ONE experiment on its
// assigned device: boot the sim if needed, bootstrap the config-on-connect
// session, drive the probe pinned to the device, wait for ingest, analyze.
func makeSweepPoolRunner(client *api.Client, s *sweep.Store, charDir, contentDefault string, durationS, ingestWaitS, confirmReps int, override []string, repBatch int, startMode string) poolRunner {
	return func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) (out poolOutcome) {
		expStart := time.Now()
		out = poolOutcome{ExpID: e.ID, Device: dev.UDID, StartedAt: expStart.UTC().Format(time.RFC3339)}
		defer func() { out.TotalMs = time.Since(expStart).Milliseconds() }()
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
		bootStart := time.Now()
		bctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, berr := bootstrapMatrixSession(bctx, client, clip, pid, e.Group, e)
		cancel()
		out.BootstrapMs = time.Since(bootStart).Milliseconds()
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
		probePlatform := runnerPlatformForDevice(dev)
		// Run-level --rep-batch overrides; otherwise the experiment's own reps +
		// start_mode drive it (spec-driven auto-routing, #946).
		effReps, effMode := resolveRepBatch(repBatch, startMode, e)
		var playID string
		if effReps > 1 {
			// Warm rep-loop (#946): run N reps of this config in ONE warm appium
			// session on this device. start_mode=warm resumes each play in place
			// (no relaunch — warm buffers); cold relaunches per rep. Produces N
			// play_ids; the first drives the verdict, the rest are recorded on the
			// outcome for confirmation + the per-rep timing shows the warm saving.
			reps, bringupMs, probeMs, rerr := runRepBatchCapture(ctx, client.BaseURL, charDir, e, probePlatform, pid, dev.UDID, clip, durationS, effReps, effMode)
			out.BringupMs, out.ProbeMs, out.Reps, out.StartMode = bringupMs, probeMs, reps, effMode
			if rerr != nil || len(reps) == 0 {
				if rerr == nil {
					rerr = errors.New("rep-batch produced no play_id")
				}
				out.Err = fmt.Errorf("rep-batch: %w", rerr)
				_ = s.Move(sweep.StatusRunning, sweep.StatusBacklog, e)
				return out
			}
			playID = reps[0].PlayID
		} else {
			pid2, bringupMs, probeMs, perr := runSweepProbeCapture(ctx, client.BaseURL, charDir, e, probePlatform, pid, dev.UDID, clip, durationS)
			out.BringupMs, out.ProbeMs = bringupMs, probeMs
			if perr != nil || pid2 == "" {
				if perr == nil {
					perr = errors.New("probe produced no play_id (crash/inconclusive)")
				}
				out.Err = fmt.Errorf("probe: %w", perr)
				_ = s.Move(sweep.StatusRunning, sweep.StatusBacklog, e) // requeue
				return out
			}
			playID = pid2
		}

		// Let the forwarder ingest the play's labels before the oracle reads them;
		// analyzing too early reads 0 labels → false inconclusive.
		analyzeStart := time.Now()
		if !sleepCtx(ctx, time.Duration(ingestWaitS)*time.Second) {
			out.Err = errors.New("cancelled before analyze")
			return out
		}
		bucket, _, _, aerr := analyzeExperiment(client, s, e, sweep.StatusRunning, playID, confirmReps)
		out.AnalyzeMs = time.Since(analyzeStart).Milliseconds()
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
// Returns the play_id, the probe's self-reported bring-up ms (session+launch+
// resume, from its PROBE_TIMING line), the full subprocess wall-time ms, and any
// error. bringupMs/probeMs are 0 when unparsed.
func runSweepProbeCapture(ctx context.Context, base, charDir string, e *sweep.Experiment, probePlatform, playerID, udid, clip string, durationS int) (playID string, bringupMs, probeMs int64, err error) {
	if durationS <= 0 {
		durationS = 60
	}
	a := charmatrix.ArmFromExperiment(e)
	probeStart := time.Now()
	timeout := time.Duration(durationS+240) * time.Second
	bin, berr := ensureModesBinary(charDir)
	if berr != nil {
		return "", 0, time.Since(probeStart).Milliseconds(), berr
	}
	cmd := exec.CommandContext(ctx, bin, "-test.run", "TestSweepProbe$", "-test.count=1",
		"-test.timeout", fmt.Sprintf("%ds", int(timeout.Seconds())), "-test.v")
	cmd.Dir = charDir
	hardenSubprocessCancel(cmd) // ctx-cancel → SIGTERM to the binary → backstop frees slots
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
	probeMs = time.Since(probeStart).Milliseconds()
	if m := bringupRe.FindStringSubmatch(buf.String()); len(m) == 2 {
		bringupMs, _ = strconv.ParseInt(m[1], 10, 64)
	}
	if m := playIDRe.FindStringSubmatch(buf.String()); len(m) == 2 {
		return m[1], bringupMs, probeMs, nil // a play_id means the probe streamed, even if `go test` later non-zeroed
	}
	return "", bringupMs, probeMs, runErr
}

// repResult is one iteration of a warm rep-batch: teardown (stop the previous
// play/app — kept separate) + startup (launch+resume the next) + its play_id.
type repResult struct {
	PlayID     string
	TeardownMs int64
	StartupMs  int64
	Mode       string // cold | warm (rep 0 is always cold)
}

var repBatchRe = regexp.MustCompile(`REPBATCH rep=(\d+) mode=(\w+) teardown_ms=(\d+) startup_ms=(\d+) play_id=([0-9a-fA-F-]{36})`)

// runRepBatchCapture invokes TestSweepRepBatch (the warm rep-loop, #946): N reps
// of one config in a single warm session on udid, with start_mode cold|warm.
// Parses the REPBATCH lines into per-rep results. Returns the reps, the first
// rep's bring-up (for the outcome's headline BringupMs), the subprocess wall-
// time, and any error.
func runRepBatchCapture(ctx context.Context, base, charDir string, e *sweep.Experiment, probePlatform, playerID, udid, clip string, durationS, reps int, startMode string) ([]repResult, int64, int64, error) {
	if durationS <= 0 {
		durationS = 60
	}
	a := charmatrix.ArmFromExperiment(e)
	start := time.Now()
	timeout := time.Duration((durationS+240)*reps) * time.Second
	bin, berr := ensureModesBinary(charDir)
	if berr != nil {
		return nil, 0, time.Since(start).Milliseconds(), berr
	}
	cmd := exec.CommandContext(ctx, bin, "-test.run", "TestSweepRepBatch$", "-test.count=1",
		"-test.timeout", fmt.Sprintf("%ds", int(timeout.Seconds())), "-test.v")
	cmd.Dir = charDir
	hardenSubprocessCancel(cmd) // ctx-cancel → SIGTERM to the binary → backstop frees slots
	var buf bytes.Buffer
	pw := &prefixWriter{w: os.Stderr, prefix: []byte("[" + shortUDID(udid) + "] ")}
	cmd.Stdout = io.MultiWriter(pw, &buf)
	cmd.Stderr = io.MultiWriter(pw, &buf)
	cmd.Env = append(os.Environ(),
		"LAUNCH_MODE=appium",
		"HARNESS_BASE_URL="+base,
		"CHAR_PLAYER_ID="+playerID,
		"CHARACTERIZATION_DEVICE_UDID="+udid,
		"CHAR_SWEEP_PLATFORM="+probePlatform,
		"CHAR_SWEEP_SERVER_URL="+base,
		"CHAR_SWEEP_DURATION_S="+strconv.Itoa(durationS),
		"CHAR_REP_COUNT="+strconv.Itoa(reps),
		"CHAR_START_MODE="+startMode,
		"CHAR_SWEEP_SEGMENT="+a.Segment,
		"CHAR_SWEEP_PROTOCOL="+a.Protocol,
		"CHAR_SWEEP_CODEC="+a.Codec,
		"CHAR_SWEEP_MUTED="+a.MutedS(),
		"CHAR_CONTENT="+clip,
	)
	runErr := cmd.Run()
	probeMs := time.Since(start).Milliseconds()
	var results []repResult
	for _, m := range repBatchRe.FindAllStringSubmatch(buf.String(), -1) {
		td, _ := strconv.ParseInt(m[3], 10, 64)
		su, _ := strconv.ParseInt(m[4], 10, 64)
		results = append(results, repResult{PlayID: m[5], TeardownMs: td, StartupMs: su, Mode: m[2]})
	}
	var firstBringup int64
	if len(results) > 0 {
		firstBringup = results[0].StartupMs
	}
	if len(results) == 0 && runErr == nil {
		runErr = errors.New("rep-batch produced no REPBATCH play_id lines")
	}
	return results, firstBringup, probeMs, runErr
}

// resolveRepBatch decides how many reps + which start mode to run a claimed
// experiment as (#946). A run-level --rep-batch (>1) is an explicit operator
// override and wins for every experiment. Otherwise the experiment's OWN spec
// drives it: an experiment carrying reps>1 runs as a rep-batch in its own
// start_mode (start_mode: warm → the warm rep-loop) — so `compare: start_mode`
// / a `reps: 3, start_mode: warm` arm auto-routes with no run-level flag. reps≤1
// ⇒ a single probe (start_mode warm has no effect on one play — rep 0 is cold).
func resolveRepBatch(runReps int, runMode string, e *sweep.Experiment) (reps int, mode string) {
	if runReps > 1 {
		return runReps, runMode
	}
	if e.Reps > 1 {
		return e.Reps, string(e.StartModeOrDefault())
	}
	return 1, string(e.StartModeOrDefault())
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
