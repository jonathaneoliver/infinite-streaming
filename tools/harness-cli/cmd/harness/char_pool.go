package main

// char_pool.go — the CHARACTERIZATION half of the streaming pool (Q1/Q2). The
// pool engine (runStreamingPool) is already generic: it takes a poolClaimer
// (work source) + a poolRunner (per-item execution). Sweep supplies a CH-backed
// claimer + a probe runner scored by the label oracle; this file supplies a char
// claimer (mode×platform work list) + a runner that dispatches the named Go mode
// test (Test<Mode><Platform>) scored by ITS OWN assertions.
//
//   Q1 (char on the pool)  = charClaimer alone (--char-only).
//   Q2 (mixed char+sweep)  = combinedClaimer: char items first, then the sweep
//                            backlog, on ONE pool. The router in
//                            makeSweepPoolRunner picks char-vs-sweep per item by
//                            Experiment.Job, so both share the device pool,
//                            barrierless work-stealing, and the leak-hardening.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

// charPlatformToken maps a sweep platform token to the Go test-name suffix the
// characterization modes use (TestRampupIPadSim, TestPyramidIPhone, …).
var charPlatformToken = map[string]string{
	"ipad-sim":  "IPadSim",
	"iphone":    "IPhone",
	"ipad":      "IPad",
	"appletv":   "AppleTV",
	"androidtv": "AndroidTV",
	"web":       "Web",
}

// charModeCamel converts a mode slug (downshift_severity) to its Go CamelCase
// (DownshiftSeverity), matching the Test<Mode><Platform> convention.
func charModeCamel(mode string) string {
	parts := strings.Split(mode, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// charTestName builds the Go test name for a (mode, platform) char run, or an
// error if the platform has no known suffix.
func charTestName(mode, platform string) (string, error) {
	tok, ok := charPlatformToken[platform]
	if !ok {
		return "", fmt.Errorf("char platform %q has no test-name suffix (known: ipad-sim, iphone, ipad, appletv, androidtv, web)", platform)
	}
	m := charModeCamel(strings.TrimSpace(mode))
	if m == "" {
		return "", fmt.Errorf("empty char mode")
	}
	return "Test" + m + tok, nil
}

// charWorkItems expands modes × platforms × reps into in-memory Experiments the
// pool runs as CHARACTERIZATION tests (Job="char"). Validated up front so a bad
// mode/platform fails before any device is touched. NOT persisted to ClickHouse.
func charWorkItems(modes, platforms []string, reps int, content string) ([]*sweep.Experiment, error) {
	if reps < 1 {
		reps = 1
	}
	// Platform is an OPTIONAL claim filter; no filter = run on any free device
	// (the test variant is derived from each device at run time). An empty list
	// means "any".
	if len(platforms) == 0 {
		platforms = []string{""}
	}
	var items []*sweep.Experiment
	for _, m := range modes {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if charModeCamel(m) == "" {
			return nil, fmt.Errorf("invalid char mode %q", m)
		}
		for _, p := range platforms {
			p = strings.TrimSpace(p)
			id := "char-" + m
			if p != "" {
				id += "-" + p
			}
			for rep := 0; rep < reps; rep++ {
				repID := id
				if reps > 1 {
					repID += "-rep" + strconv.Itoa(rep)
				}
				items = append(items, &sweep.Experiment{
					ID:         repID,
					Job:        "char",
					Mode:       m,
					Platform:   p, // claim filter (farm vocab), NOT the test suffix
					Content:    content,
					LaunchMode: "appium",
					Reps:       1,
				})
			}
		}
	}
	return items, nil
}

// charClaimer is an in-memory poolClaimer over a fixed char work list. ClaimNext
// hands each worker the next item its platform can service (so a sim worker
// won't grab an androidtv mode); Move is a no-op (nothing to persist). Safe for
// concurrent workers.
type charClaimer struct {
	mu    sync.Mutex
	items []*sweep.Experiment
}

func (c *charClaimer) ClaimNext(owner string, serviceable ...string) (*sweep.Experiment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, e := range c.items {
		if e == nil {
			continue
		}
		// e.Platform is an OPTIONAL claim filter in farm vocabulary (iphone,
		// iphone-sim, ipad-sim, appletv, androidtv). Empty = run on any free
		// device (the variant is chosen per-device at run time).
		if e.Platform == "" || containsToken(serviceable, e.Platform) {
			c.items[i] = nil // consumed
			e.Owner = owner
			return e, nil
		}
	}
	return nil, nil // nothing left this worker's platform(s) can run
}

func (c *charClaimer) Move(sweep.Status, sweep.Status, *sweep.Experiment) error { return nil }

// combinedClaimer runs char items FIRST, then delegates to the sweep store — so
// one pool drains a mixed backlog (Q2). Move routes char items to a no-op and
// sweep items to the store.
type combinedClaimer struct {
	char *charClaimer
	base poolClaimer
}

func (c *combinedClaimer) ClaimNext(owner string, serviceable ...string) (*sweep.Experiment, error) {
	if e, err := c.char.ClaimNext(owner, serviceable...); err != nil || e != nil {
		return e, err
	}
	return c.base.ClaimNext(owner, serviceable...)
}

func (c *combinedClaimer) Move(from, to sweep.Status, e *sweep.Experiment) error {
	if e != nil && e.Job == "char" {
		return nil
	}
	return c.base.Move(from, to, e)
}

func containsToken(set []string, tok string) bool {
	for _, s := range set {
		if s == tok {
			return true
		}
	}
	return false
}

// runCharModeCapture runs ONE characterization mode as a pool item: dispatch
// go test -run Test<Mode><Platform> pinned to the worker's device, scored by the
// mode's own pass/fail (its ABR assertions) — NOT the sweep label oracle. Same
// device orchestration + hardened subprocess launch (ensureModesBinary /
// hardenSubprocessCancel) as the sweep probe, so the leak-hardening applies
// unchanged. Verdict "pass"/"fail" from the exit code; Err is reserved for
// couldn't-even-run (bad name / boot / compile), which reads as ERR not fail.
func runCharModeCapture(ctx context.Context, base, charDir string, e *sweep.Experiment, dev DeviceCapability, durationS int) (out poolOutcome) {
	start := time.Now()
	out = poolOutcome{ExpID: e.ID, Device: dev.UDID, StartedAt: start.UTC().Format(time.RFC3339)}
	defer func() { out.TotalMs = time.Since(start).Milliseconds() }()

	// The TEST VARIANT is decided by the DEVICE, not a user string: the runner
	// classifies every iOS sim as ipad-sim (→ TestRampupIPadSim), a real iPhone
	// as iphone (→ TestRampupIPhone), etc. — same mapping the sweep probe uses.
	// e.Platform is only the claim filter (farm vocabulary), which differs from
	// the test suffix (a Fleet iPhone 15 sim is serviceable as iphone-sim but its
	// test variant is ipad-sim).
	testName, err := charTestName(e.Mode, runnerPlatformForDevice(dev))
	if err != nil {
		out.Err = err
		return out
	}
	if !dev.Real {
		bootSimBestEffort(ctx, dev.UDID)
	}
	bin, berr := ensureModesBinary(charDir)
	if berr != nil {
		out.Err = berr
		return out
	}

	// Generous outer bound; the mode's own -test.timeout still governs internally.
	timeout := time.Duration(durationS+600) * time.Second
	cmd := exec.CommandContext(ctx, bin, "-test.run", testName+"$", "-test.count=1",
		"-test.timeout", fmt.Sprintf("%ds", int(timeout.Seconds())), "-test.v")
	cmd.Dir = charDir
	hardenSubprocessCancel(cmd)
	var buf bytes.Buffer
	pw := &prefixWriter{w: os.Stderr, prefix: []byte("[" + shortUDID(dev.UDID) + " char:" + e.Mode + "] ")}
	cmd.Stdout = io.MultiWriter(pw, &buf)
	cmd.Stderr = io.MultiWriter(pw, &buf)
	// The char modes use their OWN env (not the sweep-probe CHAR_SWEEP_* set):
	// pin the assigned device + point at this deploy; the runner's server-picker
	// navigation handles an unseeded sim.
	cmd.Env = append(os.Environ(),
		"LAUNCH_MODE=appium",
		"HARNESS_BASE_URL="+base,
		"CHARACTERIZATION_DEVICE_UDID="+dev.UDID,
		"CHAR_CONTENT="+sweep.ContentOrDefault(e.Content),
	)
	probeStart := time.Now()
	runErr := cmd.Run()
	out.ProbeMs = time.Since(probeStart).Milliseconds()
	if d := parseProbeDevice(buf.String()); d != "" { // DF reassigned — report the device it ACTUALLY ran on
		out.Device = d
	}
	// `go test -run` matching nothing PASSES with exit 0 ("no tests to run") — a
	// silent false pass. Treat it as a can't-run error (bad mode, or this device's
	// platform has no such mode variant), which renders as ERR not a green pass.
	if strings.Contains(buf.String(), "no tests to run") {
		out.Err = fmt.Errorf("no test %q for %s (unknown mode, or no variant for this device)", testName, e.Mode)
		return out
	}
	if runErr == nil {
		out.Verdict = "pass"
	} else {
		// The test ran and its assertions failed (or it was cancelled): a RESULT,
		// not an infra error — leave Err nil so it renders as `fail`, not `ERR`.
		out.Verdict = "fail"
	}
	return out
}
