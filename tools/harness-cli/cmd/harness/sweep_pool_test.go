package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

func TestSweepPlatformsForDevice(t *testing.T) {
	cases := []struct {
		name string
		dev  DeviceCapability
		want []string
	}{
		{"ipad sim", DeviceCapability{Platform: "ios", Name: "iPad Pro"}, []string{"ipad-sim"}},
		{"iphone sim", DeviceCapability{Platform: "ios", Name: "Fleet iPhone 15 #1"}, []string{"iphone-sim", "iphone"}},
		{"real iphone", DeviceCapability{Platform: "ios", Name: "Jonathans iPhone", Real: true}, []string{"iphone"}},
		{"real ipad", DeviceCapability{Platform: "ios", Name: "iPad Air", Real: true}, []string{"ipad"}},
		{"appletv", DeviceCapability{Platform: "tvos", Name: "Apple TV"}, []string{"appletv"}},
		{"android", DeviceCapability{Platform: "android", Name: "Android TV"}, []string{"androidtv"}},
		{"unknown", DeviceCapability{Platform: "windows", Name: "?"}, nil},
	}
	for _, c := range cases {
		got := sweepPlatformsForDevice(c.dev)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
				break
			}
		}
	}
}

func TestServiceableTokens_UnionDedup(t *testing.T) {
	devs := []DeviceCapability{
		{Platform: "ios", Name: "Fleet iPhone 15 #1"},
		{Platform: "ios", Name: "Fleet iPhone 15 #2"}, // dup tokens
		{Platform: "tvos", Name: "Apple TV"},
	}
	got := serviceableTokens(devs)
	// iphone-sim, iphone (from the two sims, deduped) + appletv.
	want := map[string]bool{"iphone-sim": true, "iphone": true, "appletv": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want keys %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Errorf("unexpected token %q", tok)
		}
	}
}

func TestDeviceMatchesRequirement(t *testing.T) {
	tr, fa := true, false
	dev := DeviceCapability{UDID: "ABC", Real: true}
	cases := []struct {
		name string
		e    *sweep.Experiment
		want bool
	}{
		{"no requirement", &sweep.Experiment{}, true},
		{"udid match", &sweep.Experiment{DeviceUDID: "abc"}, true}, // case-insensitive
		{"udid mismatch", &sweep.Experiment{DeviceUDID: "XYZ"}, false},
		{"require real ok", &sweep.Experiment{RequireReal: &tr}, true},
		{"require sim on real dev", &sweep.Experiment{RequireReal: &fa}, false},
	}
	for _, c := range cases {
		if got := deviceMatchesRequirement(dev, c.e); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeClaimer hands out a fixed backlog atomically, gated by serviceable
// platform tokens, and returns nil when its (serviceable) backlog is dry —
// modelling the server-side claim. Records requeues.
type fakeClaimer struct {
	mu       sync.Mutex
	backlog  []*sweep.Experiment
	requeued []string
	claims   int32
}

func (f *fakeClaimer) ClaimNext(owner string, serviceable ...string) (*sweep.Experiment, error) {
	atomic.AddInt32(&f.claims, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	allow := map[string]bool{}
	for _, s := range serviceable {
		allow[s] = true
	}
	for i, e := range f.backlog {
		if e == nil {
			continue
		}
		if len(allow) == 0 || allow[e.Platform] {
			f.backlog[i] = nil // atomic take
			return e, nil
		}
	}
	return nil, nil
}

func (f *fakeClaimer) Move(from, to sweep.Status, e *sweep.Experiment) error {
	f.mu.Lock()
	f.requeued = append(f.requeued, e.ID)
	f.mu.Unlock()
	return nil
}

func TestRunStreamingPool_PacksUntilDry(t *testing.T) {
	// 6 iphone-sim experiments, 3 iphone-sim devices → every item runs exactly
	// once, distributed across devices, no double-run, no leftover.
	backlog := make([]*sweep.Experiment, 6)
	for i := range backlog {
		backlog[i] = &sweep.Experiment{ID: idOf(i), Platform: "iphone-sim"}
	}
	claimer := &fakeClaimer{backlog: backlog}
	devices := []DeviceCapability{
		{UDID: "D1", Platform: "ios", Name: "Fleet iPhone 15 #1"},
		{UDID: "D2", Platform: "ios", Name: "Fleet iPhone 15 #2"},
		{UDID: "D3", Platform: "ios", Name: "Fleet iPhone 15 #3"},
	}

	var mu sync.Mutex
	ranBy := map[string]string{} // expID → device
	// Deterministic concurrency proof: the first 3 runners (one per device) each
	// signal entry, then block on `release`. Once all 3 are simultaneously inside
	// (entered drains), we KNOW the pool ran them in parallel; then we release.
	entered := make(chan struct{}, len(devices))
	release := make(chan struct{})
	var releasedOnce sync.Once
	run := func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) poolOutcome {
		select {
		case entered <- struct{}{}:
			<-release // among the first wave: hold until the test confirms overlap
		default:
			// later waves (devices freed after release) don't block
		}
		mu.Lock()
		if _, dup := ranBy[e.ID]; dup {
			t.Errorf("experiment %s ran twice", e.ID)
		}
		ranBy[e.ID] = dev.UDID
		mu.Unlock()
		return poolOutcome{ExpID: e.ID, Device: dev.UDID, Verdict: "clean"}
	}

	// Watcher: once len(devices) runners are concurrently parked, release them.
	go func() {
		for i := 0; i < len(devices); i++ {
			<-entered
		}
		releasedOnce.Do(func() { close(release) })
	}()

	outcomes := runStreamingPool(context.Background(), claimer, devices, "owner", nil, 0, run)
	releasedOnce.Do(func() { close(release) }) // safety: unblock if the wave never filled
	if len(ranBy) != 6 {
		t.Fatalf("ran %d experiments, want 6 (leftover or double-run)", len(ranBy))
	}
	if len(outcomes) != 6 {
		t.Fatalf("got %d outcomes, want 6", len(outcomes))
	}
}

func TestRunStreamingPool_PlatformGating(t *testing.T) {
	// A tvos-only backlog with only iphone-sim devices → nothing runs (the device
	// can't service the work; claim returns nil for its tokens). Scenario 3.
	backlog := []*sweep.Experiment{{ID: "tv0", Platform: "appletv"}}
	claimer := &fakeClaimer{backlog: backlog}
	devices := []DeviceCapability{{UDID: "D1", Platform: "ios", Name: "Fleet iPhone 15 #1"}}
	ran := 0
	run := func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) poolOutcome {
		ran++
		return poolOutcome{ExpID: e.ID}
	}
	outcomes := runStreamingPool(context.Background(), claimer, devices, "o", nil, 0, run)
	if ran != 0 {
		t.Errorf("ran %d appletv items on an iphone device, want 0 (availability gate)", ran)
	}
	if len(outcomes) != 0 {
		t.Errorf("got %d outcomes, want 0", len(outcomes))
	}
}

func TestRunStreamingPool_RequeuesOnDeviceMismatch(t *testing.T) {
	// An experiment pinned to a specific UDID, claimed by a different same-platform
	// device, must be requeued (Move → backlog), not run.
	backlog := []*sweep.Experiment{{ID: "pinned", Platform: "iphone-sim", DeviceUDID: "OTHER"}}
	claimer := &fakeClaimer{backlog: backlog}
	devices := []DeviceCapability{{UDID: "D1", Platform: "ios", Name: "Fleet iPhone 15 #1"}}
	ran := 0
	run := func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) poolOutcome {
		ran++
		return poolOutcome{ExpID: e.ID}
	}
	outcomes := runStreamingPool(context.Background(), claimer, devices, "o", nil, 0, run)
	if ran != 0 {
		t.Errorf("ran a UDID-pinned experiment on the wrong device")
	}
	if len(claimer.requeued) != 1 || claimer.requeued[0] != "pinned" {
		t.Errorf("expected 'pinned' requeued, got %v", claimer.requeued)
	}
	if len(outcomes) != 1 || !outcomes[0].Skipped {
		t.Errorf("expected one skipped outcome, got %+v", outcomes)
	}
}

func idOf(i int) string { return "exp-" + string(rune('a'+i)) }

func TestRunStreamingPool_MaxExperimentsCap(t *testing.T) {
	// 10 items, 3 devices, cap 2 → exactly 2 run (bounded smoke test).
	backlog := make([]*sweep.Experiment, 10)
	for i := range backlog {
		backlog[i] = &sweep.Experiment{ID: "cap-" + string(rune('a'+i)), Platform: "iphone-sim"}
	}
	claimer := &fakeClaimer{backlog: backlog}
	devices := []DeviceCapability{
		{UDID: "D1", Platform: "ios", Name: "Fleet iPhone 15 #1"},
		{UDID: "D2", Platform: "ios", Name: "Fleet iPhone 15 #2"},
		{UDID: "D3", Platform: "ios", Name: "Fleet iPhone 15 #3"},
	}
	var ran int32
	run := func(ctx context.Context, e *sweep.Experiment, dev DeviceCapability) poolOutcome {
		atomic.AddInt32(&ran, 1)
		return poolOutcome{ExpID: e.ID}
	}
	outcomes := runStreamingPool(context.Background(), claimer, devices, "o", nil, 2, run)
	if ran != 2 {
		t.Errorf("ran %d experiments, want exactly 2 (the cap)", ran)
	}
	if len(outcomes) != 2 {
		t.Errorf("got %d outcomes, want 2", len(outcomes))
	}
}

func TestRunnerPlatformForDevice(t *testing.T) {
	cases := []struct {
		name string
		dev  DeviceCapability
		want string
	}{
		// Every iOS sim → ipad-sim (the runner's mapSimRuntime), even an iPhone model.
		{"iphone sim", DeviceCapability{Platform: "ios", Name: "Fleet iPhone 15 #1"}, "ipad-sim"},
		{"ipad sim", DeviceCapability{Platform: "ios", Name: "iPad Pro"}, "ipad-sim"},
		{"real iphone", DeviceCapability{Platform: "ios", Name: "Jonathans iPhone", Real: true}, "iphone"},
		{"real ipad", DeviceCapability{Platform: "ios", Name: "iPad Air", Real: true}, "ipad"},
		{"appletv", DeviceCapability{Platform: "tvos", Name: "Apple TV"}, "appletv"},
		{"android", DeviceCapability{Platform: "android", Name: "Android TV"}, "androidtv"},
	}
	for _, c := range cases {
		if got := runnerPlatformForDevice(c.dev); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSummarizePool_Timing(t *testing.T) {
	outcomes := []poolOutcome{
		{ExpID: "exp-a", Device: "D1AAAAAA", Verdict: "clean", BringupMs: 22000, ProbeMs: 68000, TotalMs: 100000},
		{ExpID: "exp-b", Device: "D2BBBBBB", Verdict: "notable", BringupMs: 24000, ProbeMs: 70000, TotalMs: 102000},
		{ExpID: "exp-c", Device: "D1AAAAAA", Err: errThing, BringupMs: 0, ProbeMs: 180000, TotalMs: 181000},
	}
	// Two workers, ~283s of work; pretend it took 190s wall-clock → ~1.5× parallel.
	got := summarizePool(outcomes, 190000)
	for _, want := range []string{"bringup", "probe", "total", "CLEAN", "NOTABLE", "ERR", "streaming pool: 2 ran, 0 skipped, 1 errored", "wall-clock 190s", "parallel"} {
		if !contains(got, want) {
			t.Errorf("summary missing %q\n---\n%s", want, got)
		}
	}
}

var errThing = fmtErr("boom")

func fmtErr(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (indexOf(hay, needle) >= 0)
}
func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
