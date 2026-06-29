package runner

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden regenerates testdata/probe_golden.txt instead of asserting
// against it. Run: go test ./runner -run TestProbeLaunchArgsGolden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite the probe launch-arg golden file")

// TestProbeLaunchArgsGolden freezes the FINAL launch-arg slice the app receives
// — ProbeLaunchArgs + the (currently test-body-inline) startup/local_proxy/
// auto_recovery knobs + withBaselineTestFlags — across the full input matrix.
//
// It is the parity oracle for the charplan refactor (the RunPlan typed-config
// migration): the producer/consumer rewrite must leave every scenario's bytes
// identical, EXCEPT the one deliberately-fixed line — `auto_recovery=off` today
// leaks through as the literal string "off" on the fleet path (the inline append
// pre-sets the arg so charAutoRecovery never normalises it), which the refactor
// corrects to "false". When that fix lands, ONLY the auto-recovery-off scenario's
// golden line changes; every other scenario must stay byte-identical, and that
// one-line diff IS the proof the fix is surgical.
func TestProbeLaunchArgsGolden(t *testing.T) {
	// Replicates today's two-step build: ProbeLaunchArgs (the "domain" knobs)
	// then the five knobs char_matrix_fleet_test.go appends inline from env, in
	// the same order, then the baseline fill. local_proxy/auto_recovery default
	// the way the fleet path does ("false"/"true", always appended).
	currentArgs := func(s scenario) []string {
		args := ProbeLaunchArgs(s.cfg)
		if s.fwdBuffer != "" {
			args = append(args, "-is.flag.startup_forward_buffer_s", s.fwdBuffer)
		}
		if s.fwdRelease != "" {
			args = append(args, "-is.flag.startup_fwd_release", s.fwdRelease)
		}
		if s.persistPeak != "" {
			args = append(args, "-is.flag.persistent_peak_bitrate_mbps", s.persistPeak)
		}
		lp := s.localProxy
		if lp == "" {
			lp = "false"
		}
		args = append(args, "-is.flag.local_proxy", lp)
		ar := s.autoRecovery
		if ar == "" {
			ar = "true"
		}
		args = append(args, "-is.flag.auto_recovery", ar)
		return withBaselineTestFlags(args)
	}

	var got strings.Builder
	for _, s := range goldenScenarios {
		// Determinism: withBaselineTestFlags reads CHAR_AUTO_RECOVERY (only used
		// when auto_recovery isn't already present — never, on this path) and
		// CHAR_CONTENT (the lastPlayed baseline fill when an arm pins no content).
		// Pin both so the golden never depends on the shell or the repo .env.
		func() {
			t.Setenv("CHAR_AUTO_RECOVERY", "")
			t.Setenv("CHAR_CONTENT", s.charContent)
			got.WriteString("### " + s.name + "\n")
			got.WriteString(strings.Join(currentArgs(s), "\n"))
			got.WriteString("\n\n")
		}()
	}

	goldenPath := filepath.Join("testdata", "probe_golden.txt")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote golden: %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update-golden to create): %v", err)
	}
	if got.String() != string(want) {
		t.Errorf("launch-arg golden mismatch — run with -update-golden to inspect the diff and confirm it's only the intended change.\n--- got ---\n%s", got.String())
	}
}

// scenario is one launch-arg input combination. The fwd*/persistPeak/localProxy/
// autoRecovery fields stand in for the five knobs the matrix test currently
// appends inline from CHAR_* env; they fold into ProbeConfig in the refactor.
type scenario struct {
	name         string
	cfg          ProbeConfig
	fwdBuffer    string
	fwdRelease   string
	persistPeak  string
	localProxy   string
	autoRecovery string
	charContent  string // CHAR_CONTENT for the lastPlayed baseline fill
}

var goldenScenarios = []scenario{
	{
		name:        "minimal-baseline-content",
		cfg:         ProbeConfig{PlayerID: "11111111-1111-1111-1111-111111111111"},
		charContent: "insane_newer_p200_h264",
	},
	{
		name: "full-client-knobs",
		cfg: ProbeConfig{
			PlayerID:           "22222222-2222-2222-2222-222222222222",
			Content:            "insane_newer_p200_hevc",
			Segment:            "s2",
			LiveOffsetS:        "24",
			Protocol:           "hls",
			Codec:              "hevc",
			PeakBitrateMbps:    8,
			StartsFirstVariant: "true",
			Muted:              "false",
		},
		charContent: "ignored-arm-pins-content",
	},
	{
		name:        "muted-true",
		cfg:         ProbeConfig{PlayerID: "33333333-3333-3333-3333-333333333333", Muted: "true"},
		charContent: "insane_newer_p200_h264",
	},
	{
		name:        "first-variant-false",
		cfg:         ProbeConfig{PlayerID: "44444444-4444-4444-4444-444444444444", StartsFirstVariant: "false"},
		charContent: "insane_newer_p200_h264",
	},
	{
		name:         "inline5-all-set",
		cfg:          ProbeConfig{PlayerID: "55555555-5555-5555-5555-555555555555", Content: "insane_newer_p200_av1"},
		fwdBuffer:    "6",
		fwdRelease:   "ttff",
		persistPeak:  "3",
		localProxy:   "true",
		autoRecovery: "false",
	},
	{
		// THE BUG: today this yields the literal "-is.flag.auto_recovery off".
		// After the fix it must become "false". This is the ONLY golden line
		// expected to change in the refactor.
		name:         "auto-recovery-off-bug",
		cfg:          ProbeConfig{PlayerID: "66666666-6666-6666-6666-666666666666", Content: "insane_newer_p200_h264"},
		autoRecovery: "off",
	},
	{
		name:        "peak-zero-baseline-fills",
		cfg:         ProbeConfig{PlayerID: "77777777-7777-7777-7777-777777777777", PeakBitrateMbps: 0, Content: "insane_newer_p200_h264"},
		charContent: "unused",
	},
}
