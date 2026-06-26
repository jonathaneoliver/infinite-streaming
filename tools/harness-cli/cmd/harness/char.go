package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/api"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/charmatrix"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/forwarder"
)

// cmdChar is the `harness char …` dispatcher. The matrix runner (issue #811) is
// the only subcommand today: it expands a declarative YAML matrix spec into arms
// and drives them sequentially (or, parallel:true, on the fleet backend),
// reusing the sweep's config-on-connect bootstrap and measurement helpers.
func cmdChar(client *api.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New("usage: harness char matrix <spec.yaml> [--dry-run] [--char-dir DIR] [--duration-s N]")
	}
	switch args[0] {
	case "matrix":
		return cmdCharMatrix(client, args[1:], asJSON)
	default:
		return fmt.Errorf("unknown char subcommand %q (have: matrix)", args[0])
	}
}

// cmdCharMatrix loads + expands a matrix spec and runs it. Sequential by
// default: per arm it bootstraps a config-on-connect session (server-side
// recipe), cold-launches the appium probe bound to that session (client-side
// knobs via go test TestSweepProbe), then reads the events archive back to
// measure the achieved live-offset and render a results table. A parallel:true
// spec delegates to the existing fleet go-test backend.
func cmdCharMatrix(client *api.Client, args []string, asJSON bool) error {
	if len(args) < 1 || args[0] == "" {
		return errors.New("usage: harness char matrix <spec.yaml> [--dry-run] [--char-dir DIR] [--duration-s N]")
	}
	specPath := args[0]
	fs := flag.NewFlagSet("char matrix", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "expand + print the planned arms; touch no sessions")
	charDir := fs.String("char-dir", envOrDefault("CHAR_DIR", "tests/characterization"), "path to the characterization Go module (drives the probe via `go test`)")
	durationOverride := fs.Int("duration-s", 0, "override every arm's play window (seconds)")
	group := fs.String("group", "", "group_id to born-group every arm's session (dashboard A/B compare)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	data, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("read spec: %w", err)
	}
	spec, err := charmatrix.Load(data)
	if err != nil {
		return err
	}
	// A per-run id (UTC timestamp) is appended to every auto-generated group id so
	// two runs of the same spec never share a group_id — the dashboard won't join a
	// prior run's grouped sessions with this one. The spec name stays in the id for
	// readability; the timestamp makes it unique and records when the run started.
	runID := time.Now().UTC().Format("20060102T150405Z")
	arms, err := charmatrix.ExpandWithRunID(spec, runID)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "char matrix run id: %s\n", runID)

	// Dry run: show the plan (one row per arm, no measurements) and stop. Pure —
	// no network, so it's the fast way to sanity-check a spec's expansion.
	if *dryRun {
		results := make([]charmatrix.ArmResult, len(arms))
		for i, a := range arms {
			results[i] = charmatrix.ArmResult{Arm: a, IntendedOff: intendedOf(a)}
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{
				"spec": spec.Name, "parallel": spec.Parallel, "arms": planSummaries(arms),
			})
		}
		fmt.Printf("DRY RUN — %d arm(s) planned (parallel=%v)\n\n", len(arms), spec.Parallel)
		fmt.Print(charmatrix.RenderTable(spec.Name, results))
		return nil
	}

	var results []charmatrix.ArmResult
	if spec.Parallel {
		// parallel:true fans every arm out simultaneously: the CLI bootstraps each
		// arm's server recipe up front, then one fleet go-test run drives them all
		// at once (one device per arm), and we measure each afterward.
		results, err = runMatrixParallel(client, arms, *charDir, *durationOverride, *group)
		if err != nil {
			return err
		}
	} else {
		results = make([]charmatrix.ArmResult, 0, len(arms))
		for i, a := range arms {
			fmt.Fprintf(os.Stderr, "── arm %d/%d: %s ──\n", i+1, len(arms), a.ID)
			res := runArmSequential(client, a, *charDir, *durationOverride, *group)
			results = append(results, res)
		}
	}

	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"spec": spec.Name, "results": results,
		})
	}
	fmt.Print(charmatrix.RenderTable(spec.Name, results))
	return nil
}

// runMatrixParallel runs every arm simultaneously via the fleet backend. It
// bootstraps each arm's server-side recipe onto its own config-on-connect
// session up front (so the fleet probe only has to reattach + drive), runs
// TestCharMatrixFleet once with CHAR_FLEET_COUNT=N and the per-arm knobs in
// CHAR_ARM_<i>_* env, then measures each player. An arm whose bootstrap fails is
// recorded with its error and left out of the fleet env (that fleet index then
// skips), so one bad arm doesn't sink the run.
func runMatrixParallel(client *api.Client, arms []*charmatrix.Arm, charDir string, durationOverride int, group string) ([]charmatrix.ArmResult, error) {
	// The fleet allocates one device PER ARM from the device-farm by that arm's
	// platform capability, so a parallel matrix MAY mix platforms — e.g. an
	// ipad-sim master + a real iPhone slave sharing one group/pattern. Each arm's
	// platform rides in CHAR_ARM_<i>_PLATFORM (emitted below); arms[0]'s is the
	// primary (CHAR_SWEEP_PLATFORM) the fleet resolver falls back to.
	platform := arms[0].Platform

	// For a grouped pattern run, ONE master arms the pattern; the proxy propagates
	// it to the group's slaves (NETSHAPE group pattern propagation), so every arm
	// shares ONE bandwidth timeline instead of each running an independent,
	// possibly out-of-phase pyramid. The master must have ALL variants — a thinned
	// ladder would build a pyramid that only spans its reduced range. Prefer the
	// first full-ladder arm carrying the pattern; fall back to the first pattern
	// arm (with a warning) if none is full.
	patternMaster, firstPatternArm := -1, -1
	for i, a := range arms {
		if shapePattern(a) == "" {
			continue
		}
		if firstPatternArm < 0 {
			firstPatternArm = i
		}
		e := a.ToExperiment()
		if e.ContentManipulation == nil || e.ContentManipulation.AllowedVariants == "" {
			patternMaster = i
			break
		}
	}
	if patternMaster < 0 {
		patternMaster = firstPatternArm
		if patternMaster >= 0 {
			fmt.Fprintf(os.Stderr, "warn: no full-ladder arm to master the pattern — arm %d masters with a thinned ladder\n", patternMaster)
		}
	}

	results := make([]charmatrix.ArmResult, len(arms))
	var armEnv []string
	window := 0
	for i, a := range arms {
		res := charmatrix.ArmResult{Arm: a, IntendedOff: intendedOf(a)}
		e := a.ToExperiment()
		clip := sweep.ContentOrDefault(e.Content) // spec content: → CHAR_CONTENT (.env) → built-in default
		if clip == "" {
			res.Err = "no content (set arm/defaults content or CHAR_CONTENT)"
			results[i] = res
			continue
		}
		d := e.DurationS
		if durationOverride > 0 {
			d = durationOverride
		}
		if d <= 0 {
			d = 60
		}
		if d > window {
			window = d // the fleet shares one play window — take the longest arm's
		}

		playerID := uuid.NewString()
		res.PlayerID = playerID

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, berr := bootstrapMatrixSession(ctx, client, clip, playerID, group, e)
		cancel()
		if berr != nil {
			res.Err = "bootstrap: " + berr.Error()
			results[i] = res
			continue // no CHAR_ARM_i_PLAYER_ID emitted → that fleet index skips
		}
		results[i] = res
		fmt.Fprintf(os.Stderr, "bootstrapped arm %d/%d: %s (player_id=%s)\n", i+1, len(arms), a.ID, playerID)
		armEnv = append(armEnv,
			fmt.Sprintf("CHAR_ARM_%d_PLAYER_ID=%s", i, playerID),
			fmt.Sprintf("CHAR_ARM_%d_PLATFORM=%s", i, a.Platform),
			fmt.Sprintf("CHAR_ARM_%d_SEGMENT=%s", i, a.Segment),
			fmt.Sprintf("CHAR_ARM_%d_LIVE_OFFSET=%s", i, a.ClientLiveOffsetS()),
			fmt.Sprintf("CHAR_ARM_%d_PROTOCOL=%s", i, a.Protocol),
			fmt.Sprintf("CHAR_ARM_%d_CODEC=%s", i, a.Codec),
			fmt.Sprintf("CHAR_ARM_%d_PEAK_BITRATE=%d", i, a.PeakBitrateMbps),
			fmt.Sprintf("CHAR_ARM_%d_FIRST_VARIANT=%s", i, a.StartsFirstVariantS()),
			fmt.Sprintf("CHAR_ARM_%d_MUTED=%s", i, a.MutedS()),
			fmt.Sprintf("CHAR_ARM_%d_PATTERN=%s", i, shapePattern(a)),
			fmt.Sprintf("CHAR_ARM_%d_STEP_S=%d", i, shapeStepS(a)),
			fmt.Sprintf("CHAR_ARM_%d_MARGIN=%d", i, shapeMargin(a)),
			fmt.Sprintf("CHAR_ARM_%d_PATTERN_MASTER=%t", i, i == patternMaster),
			fmt.Sprintf("CHAR_ARM_%d_CONTENT=%s", i, clip),
		)
	}
	if window <= 0 {
		window = 60
	}

	// One fleet run drives every bootstrapped arm at once.
	if err := driveFleet(client, platform, len(arms), window, charDir, armEnv); err != nil {
		// Non-fatal: the plays may still have registered. Surface it and measure
		// what landed rather than dropping the whole table.
		fmt.Fprintf(os.Stderr, "fleet run reported an error (measuring anyway): %v\n", err)
	}

	for i := range arms {
		if results[i].Err != "" || results[i].PlayerID == "" {
			continue
		}
		measureArm(client, arms[i], results[i].PlayerID, &results[i])
	}
	return results, nil
}

// driveFleet runs the fleet probe (TestCharMatrixFleet) once: N devices of the
// given platform, one arm each (CHAR_FLEET_COUNT=N + the per-arm env), playing
// simultaneously. Streams the test output through so the operator sees live
// progress + per-arm viewer links.
func driveFleet(client *api.Client, platform string, n, windowS int, charDir string, armEnv []string) error {
	// Fleet bring-up (N parallel WDA builds + the shared home barrier) is slower
	// than a single launch, so budget more headroom than the sequential probe.
	timeout := time.Duration(windowS+600) * time.Second

	// Pre-compile the fleet test to a standalone binary and run THAT directly,
	// instead of `go test ./modes`. The process we Start() (and signal on a
	// graceful stop) must BE the test binary: `go test` runs the binary as a
	// grandchild in its own process group, so neither a direct signal to go test
	// nor a group-signal reaches the binary where interruptContext lives — the
	// binary (and its appium session) get orphaned on interrupt (#853, verified
	// across a 5-cycle stress test). Running the compiled binary directly makes
	// cmd.Process the test binary, so SIGTERM lands straight on it →
	// interruptContext → t.Cleanup → appium session release.
	binF, err := os.CreateTemp("", "char-fleet-*.test")
	if err != nil {
		return fmt.Errorf("fleet test tempfile: %w", err)
	}
	bin := binF.Name()
	binF.Close()
	defer os.Remove(bin)
	build := exec.Command("go", "test", "-c", "-o", bin, "./modes")
	build.Dir = charDir
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("compile TestCharMatrixFleet (dir=%s): %w", charDir, err)
	}

	// Each arm appends the device-farm UDID it acquired to this manifest, so the
	// post-run reap releases EXACTLY the devices THIS run used — concurrent-run
	// safe, never another run's. The farm has no release-by-session/tag API
	// (unblock is per-UDID), and the test is what knows its own UDIDs.
	manifest := ""
	if mf, err := os.CreateTemp("", "char-fleet-devices-*.txt"); err == nil {
		manifest = mf.Name()
		mf.Close()
		defer os.Remove(manifest)
	}
	cmd := exec.Command(bin, "-test.run=TestCharMatrixFleet", "-test.count=1",
		fmt.Sprintf("-test.timeout=%ds", int(timeout.Seconds())), "-test.v=true")
	cmd.Dir = charDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"LAUNCH_MODE=appium",
		"CHAR_MATRIX_FLEET=1",
		"HARNESS_BASE_URL="+client.BaseURL,
		"CHAR_SWEEP_PLATFORM="+platform,
		"CHAR_FLEET_COUNT="+strconv.Itoa(n),
		"CHAR_SWEEP_DURATION_S="+strconv.Itoa(windowS),
		"CHAR_DEVICE_MANIFEST="+manifest,
	)
	cmd.Env = append(cmd.Env, armEnv...)

	// Graceful interrupt (#853): forward SIGINT/SIGTERM straight to the test
	// binary (now our direct child) and WAIT for it to drain rather than letting
	// the Go runtime kill the CLI first. interruptContext cancels the play window
	// and t.Cleanup releases the appium session; without this the CLI would die
	// first and orphan the session, blocking the next run. SIGKILL is uncatchable,
	// so a hard kill still needs the appium-restart / startup-sweep backstop.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run TestCharMatrixFleet (dir=%s): start: %w", charDir, err)
	}
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM) // straight to the test binary
			}
		}
	}()
	werr := cmd.Wait()
	// Release any device the appium device-farm still holds "busy" now that the
	// test process has exited. On a forced stop the farm doesn't clear the device
	// when we delete the session, AND it won't accept the release while the test
	// is alive — so reap here, post-exit, where its unblock API takes effect.
	// Without this, devices leak "busy" across runs until create-session hangs
	// to its 180s timeout (#853). Scoped to THIS run's devices via the manifest.
	reapDeviceFarm(deviceFarmBaseURL(), manifestUDIDs(manifest))
	if werr != nil {
		return fmt.Errorf("go test TestCharMatrixFleet (dir=%s): %w", charDir, werr)
	}
	return nil
}

// deviceFarmBaseURL is the appium server hosting the device-farm plugin.
func deviceFarmBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("CHAR_APPIUM_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:4723"
}

type farmDevice struct {
	UDID string `json:"udid"`
	Host string `json:"host"`
	Busy bool   `json:"busy"`
}

// reapDeviceFarm poll-unblocks the device-farm devices THIS run acquired (udids,
// from the per-run manifest the test wrote) until they read free or a bound
// elapses. Must be called AFTER the test process exits: the farm only releases
// once the session's connection is gone. Scoped to the run's own devices, so
// concurrent runs never release each other's. Best-effort.
func reapDeviceFarm(base string, udids []string) {
	if len(udids) == 0 {
		return
	}
	want := make(map[string]bool, len(udids))
	for _, u := range udids {
		want[strings.ToUpper(strings.TrimSpace(u))] = true
	}
	client := &http.Client{Timeout: 6 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var mine []farmDevice
		for _, d := range busyFarmDevices(client, base) {
			if want[strings.ToUpper(d.UDID)] {
				mine = append(mine, d)
			}
		}
		if len(mine) == 0 {
			return
		}
		for _, d := range mine {
			body, _ := json.Marshal(map[string]string{"udid": d.UDID, "host": d.Host})
			req, err := http.NewRequest(http.MethodPost, base+"/device-farm/api/unblock", bytes.NewReader(body))
			if err != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			if resp, derr := client.Do(req); derr == nil {
				resp.Body.Close()
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// manifestUDIDs reads the device-farm UDIDs the test recorded for this run (one
// per line). Empty/missing → nil, so the reap is a no-op.
func manifestUDIDs(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if u := strings.TrimSpace(line); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func busyFarmDevices(client *http.Client, base string) []farmDevice {
	resp, err := client.Get(base + "/device-farm/api/device")
	if err != nil {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var all []farmDevice
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	var busy []farmDevice
	for _, d := range all {
		if d.Busy {
			busy = append(busy, d)
		}
	}
	return busy
}

// runArmSequential bootstraps, drives, and measures one arm. Every failure is
// captured onto the result's Err (never aborts the whole matrix) so a flaky arm
// doesn't lose the rest of the table.
func runArmSequential(client *api.Client, a *charmatrix.Arm, charDir string, durationOverride int, group string) charmatrix.ArmResult {
	res := charmatrix.ArmResult{Arm: a, IntendedOff: intendedOf(a)}

	e := a.ToExperiment()
	clip := sweep.ContentOrDefault(e.Content) // spec content: → CHAR_CONTENT (.env) → built-in default
	if clip == "" {
		res.Err = "no content (set arm/defaults content or CHAR_CONTENT)"
		return res
	}

	playerID := uuid.NewString()
	res.PlayerID = playerID

	// 1. Bootstrap the server-side recipe onto a fresh config-on-connect session.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	if _, err := bootstrapMatrixSession(ctx, client, clip, playerID, group, e); err != nil {
		cancel()
		res.Err = "bootstrap: " + err.Error()
		return res
	}
	cancel()

	// 2. Drive the probe: cold-launch the appium app bound to this session with
	//    the arm's client-side knobs. Reuses TestSweepProbe (which feeds the same
	//    runner.ProbeLaunchArgs the matrix shares) via env.
	dur := e.DurationS
	if durationOverride > 0 {
		dur = durationOverride
	}
	if dur <= 0 {
		dur = 60
	}
	if err := driveProbe(client, a, playerID, clip, dur, charDir); err != nil {
		res.Err = "probe: " + err.Error()
		// fall through to measure — the play may still have registered.
	}

	// 3. Measure: read the events archive for this player and check whether the
	//    achieved offset reflects the intended one (the #793 manipulation gate).
	measureArm(client, a, playerID, &res)
	return res
}

// bootstrapMatrixSession materialises an in-memory experiment's recipe onto a
// proxy session via config-on-connect (no store round-trip): build the combined
// PlayerPatch, base64 it onto the shaper master URL, and GET it. A 3xx means the
// session is configured (#712 applies before the redirect); 4xx/5xx is a reject.
// Mirrors cmdSweepBootstrap's HTTP handling.
func bootstrapMatrixSession(ctx context.Context, client *api.Client, clip, playerID, group string, e *sweep.Experiment) ([]string, error) {
	patch, summary, err := experimentPlayerPatch(ctx, client, clip, e)
	if err != nil {
		return nil, err
	}
	cfgJSON, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	cfgB64 := base64.RawURLEncoding.EncodeToString(cfgJSON)
	// The arm's own group (stamped by compare:/groups: in Expand) wins so paired
	// arms arrive born-grouped on the dashboard; the --group CLI flag is the
	// fallback for an ungrouped matrix the operator wants to tag by hand.
	groupID := e.Group
	if groupID == "" {
		groupID = group
	}
	bootURL, err := shaperBootstrapURL(client.BaseURL, clip, playerID, groupID, cfgB64)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     client.HTTP.Transport,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bootURL, nil)
	if err != nil {
		return nil, err
	}
	if client.BasicAuth != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(client.BasicAuth)))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bootstrap GET: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("proxy returned %d: %s", resp.StatusCode, string(body))
	}
	return summary, nil
}

// driveProbe runs the appium probe for one arm by shelling out to the
// characterization module's TestSweepProbe with the arm's knobs as env. The
// client-side knobs (segment / app live_offset / protocol / content) flow
// through CHAR_SWEEP_* → runner.ProbeLaunchArgs; the server recipe is already
// live on the session from the bootstrap. stdout/stderr stream through so the
// operator sees the probe's progress + viewer link live.
func driveProbe(client *api.Client, a *charmatrix.Arm, playerID, clip string, durationS int, charDir string) error {
	// Generous timeout: launch + bring-up + the play window + cleanup.
	timeout := time.Duration(durationS+240) * time.Second
	cmd := exec.Command("go", "test", "./modes", "-run", "TestSweepProbe", "-count=1",
		"-timeout", fmt.Sprintf("%ds", int(timeout.Seconds())), "-v")
	cmd.Dir = charDir
	cmd.Stdout = os.Stderr // probe logs are progress, not the command's data output
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"LAUNCH_MODE=appium",
		"HARNESS_BASE_URL="+client.BaseURL,
		"CHAR_PLAYER_ID="+playerID,
		"CHAR_SWEEP_PLATFORM="+a.Platform,
		"CHAR_SWEEP_DURATION_S="+strconv.Itoa(durationS),
		"CHAR_SWEEP_SEGMENT="+a.Segment,
		"CHAR_SWEEP_LIVE_OFFSET="+a.ClientLiveOffsetS(),
		"CHAR_SWEEP_PROTOCOL="+a.Protocol,
		"CHAR_SWEEP_CODEC="+a.Codec,
		"CHAR_SWEEP_PEAK_BITRATE="+strconv.Itoa(a.PeakBitrateMbps),
		"CHAR_SWEEP_FIRST_VARIANT="+a.StartsFirstVariantS(),
		"CHAR_SWEEP_MUTED="+a.MutedS(),
		"CHAR_SWEEP_PATTERN="+shapePattern(a),
		"CHAR_SWEEP_STEP_S="+strconv.Itoa(shapeStepS(a)),
		"CHAR_SWEEP_MARGIN="+strconv.Itoa(shapeMargin(a)),
		"CHAR_CONTENT="+clip,
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go test TestSweepProbe (dir=%s): %w", charDir, err)
	}
	return nil
}

// measureArm reads the events archive for the player and fills the result's
// offset + verdict. Keyed by player_id (not play_id) so it survives play_id
// rotation and cross-traffic — the player_id is the stable session identity the
// bootstrap minted. For a live-offset arm it runs the #793 manipulation check:
// did the achieved offset actually move to ~the intended value?
func measureArm(client *api.Client, a *charmatrix.Arm, playerID string, res *charmatrix.ArmResult) {
	pid, err := parsePlayID(playerID)
	if err != nil {
		res.Err = appendErr(res.Err, "player_id: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := client.ArchiveEvents(ctx, &forwarder.GetApiV2EventsParams{PlayerId: &pid})
	if err != nil {
		res.Err = appendErr(res.Err, "query events: "+err.Error())
		return
	}
	if intended, ok := a.IntendedLiveOffset(); ok {
		achieved := sweep.AchievedOffsetFromEvents(body)
		res.HasOffset = achieved.HasData
		got := achieved.RecommendedS
		if got <= 0 {
			got = achieved.TrueS
		}
		res.AchievedOff = got
		res.Landed = sweep.ManipulationLanded(intended, achieved, sweep.SegmentSlackS(a.Segment))
		if !res.Landed {
			res.Note = fmt.Sprintf("IV did not move (intended %.0fs, achieved ~%.0fs)", intended, got)
		}
	}
}

// --- small local helpers --------------------------------------------------

// shapePattern / shapeStepS / shapeMargin extract the post-launch bandwidth
// pattern from an arm's proxy.shape. The pattern is NOT applied by the
// config-on-connect bootstrap (experimentPlayerPatch defers it); the probe arms
// it after playback starts via ApplyPattern, so the runner passes it through env.
func shapePattern(a *charmatrix.Arm) string {
	if a.Shape != nil {
		return a.Shape.Pattern
	}
	return ""
}

func shapeStepS(a *charmatrix.Arm) int {
	if a.Shape != nil && a.Shape.StepSeconds > 0 {
		return a.Shape.StepSeconds
	}
	return 12
}

func shapeMargin(a *charmatrix.Arm) int {
	if a.Shape != nil && a.Shape.MarginPct > 0 {
		return a.Shape.MarginPct
	}
	return 5
}

func intendedOf(a *charmatrix.Arm) float64 {
	if off, ok := a.IntendedLiveOffset(); ok {
		return off
	}
	return 0
}

func planSummaries(arms []*charmatrix.Arm) []map[string]any {
	out := make([]map[string]any, len(arms))
	for i, a := range arms {
		e := a.ToExperiment()
		out[i] = map[string]any{
			"id":            a.ID,
			"platform":      a.Platform,
			"segment":       a.Segment,
			"protocol":      a.Protocol,
			"group":         a.Group,
			"role":          a.Role,
			"live_offset":   intendedOf(a),
			"client_offset": a.ClientLiveOffsetS(),
			"codec":         a.Codec,
			"peak_bitrate":  a.PeakBitrateMbps,
			"first_variant": a.StartsFirstVariantS(),
			"muted":         a.MutedS(),
			"class":         e.Class,
			"duration_s":    e.DurationS,
		}
	}
	return out
}

func appendErr(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

func envOrDefault(key, dflt string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return dflt
}
