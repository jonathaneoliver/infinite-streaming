// Package charplan is the wire contract between the `harness char matrix` CLI
// (tools/harness-cli) and the characterization probe (tests/characterization).
//
// It is the single source of truth for the per-arm client-side knobs plus the
// run-level orchestration values that cross the `go test` process boundary. The
// CLI writes exactly one RunPlan to a temp file (path passed in CHAR_RUN_PLAN_FILE);
// the probe reads exactly one. This replaces the flat CHAR_ARM_<i>_* / CHAR_SWEEP_*
// env surface that was hand-maintained across two emit sites and two readers.
//
// It lives in go-proxy because that is the ONLY module both consumers already
// import (pkg/ladder) — neither consumer module imports the other, and the CLI's
// own Arm/Experiment types are under internal/. charplan is a pure-data leaf:
// stdlib only, and it imports neither consumer (the ArmConfig→ProbeConfig adapter
// lives runner-side, since go-proxy cannot import tests/characterization).
package charplan

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	// SchemaVersion is bumped on any breaking change to RunPlan/ArmConfig. Load
	// rejects a plan whose version differs, so a stale test binary run against a
	// newer CLI fails loudly instead of silently mis-reading config.
	SchemaVersion = 1

	// DefaultStepS / DefaultMarginPct are the shape-pattern defaults, defined
	// once here instead of re-hardcoded in the CLI (shapeStepS/shapeMargin) and
	// the probe.
	DefaultStepS     = 12
	DefaultMarginPct = 5
)

// RunPlan is the whole handoff for one `harness char matrix` invocation: the
// run-level orchestration values plus one ArmConfig per fleet index. The CLI
// writes exactly one; the probe reads exactly one.
type RunPlan struct {
	SchemaVersion  int         `json:"v"`
	BaseURL        string      `json:"base_url,omitempty"`        // HARNESS_BASE_URL
	Platform       string      `json:"platform,omitempty"`        // fleet primary platform (arms[0]); resolver fallback
	FleetCount     int         `json:"fleet_count"`               // == len(Arms); explicit so the probe never derives it
	DurationS      int         `json:"duration_s"`                // shared play window (longest arm)
	DeviceManifest string      `json:"device_manifest,omitempty"` // path the probe appends acquired UDIDs to
	Arms           []ArmConfig `json:"arms"`
}

// ArmConfig is everything the probe needs to bind + cold-launch ONE arm. It is
// the merge of the old armProbeConfig (fleet path) and the CHAR_SWEEP_* twin
// (sequential path) into one struct serving both.
//
// Tri-state booleans are *bool: nil = "use the resolver default" (WithDefaults),
// &true/&false = force. This replaces the old ""-vs-"false" magic-string logic.
type ArmConfig struct {
	// binding
	PlayerID string `json:"player_id"`          // bootstrapped config-on-connect session id ("" => probe skips this index)
	Platform string `json:"platform,omitempty"` // per-arm platform capability (mixed fleets)
	Content  string `json:"content,omitempty"`  // resolved clip (the CLI already applied its default)

	// client launch-arg knobs (cold relaunch on change → ProbeLaunchArgs)
	Segment            string `json:"segment,omitempty"`
	LiveOffsetS        string `json:"live_offset_s,omitempty"` // "" => probe pins "0"
	Protocol           string `json:"protocol,omitempty"`
	Codec              string `json:"codec,omitempty"`
	PeakBitrateMbps    int    `json:"peak_bitrate_mbps,omitempty"` // 0 => omit (app's natural pick)
	StartsFirstVariant *bool  `json:"starts_first_variant,omitempty"`
	Muted              *bool  `json:"muted,omitempty"`

	// startup / recovery knobs (formerly appended inline from CHAR_* env)
	StartupFwdBufferS  string `json:"startup_fwd_buffer_s,omitempty"`
	StartupFwdRelease  string `json:"startup_fwd_release,omitempty"`
	PersistentPeakMbps string `json:"persistent_peak_mbps,omitempty"`
	LocalProxy         *bool  `json:"local_proxy,omitempty"`   // nil => WithDefaults sets false
	AutoRecovery       *bool  `json:"auto_recovery,omitempty"` // nil => WithDefaults sets true

	// post-launch bandwidth pattern (drives ApplyPattern, not a launch arg)
	Pattern       string `json:"pattern,omitempty"`
	StepS         int    `json:"step_s,omitempty"`
	MarginPct     int    `json:"margin_pct,omitempty"`
	PatternMaster bool   `json:"pattern_master,omitempty"`
}

// ParseBool maps the operator vocabulary to a tri-state: "" or anything
// unrecognised → nil (the caller's default applies); 1/true/on/yes → &true;
// 0/false/off/no → &false. This is the SINGLE bool parser — it replaces the
// three divergent ones (charAutoRecovery's 0/false/off/no, DeviceFarmEnabled's
// 0/false/off, patternMaster's == "true") and, by resolving the value before it
// reaches a launch arg, fixes the fleet-path bug where CHAR_AUTO_RECOVERY=off
// reached the app as the literal string "off".
func ParseBool(s string) *bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes":
		v := true
		return &v
	case "0", "false", "off", "no":
		v := false
		return &v
	default:
		return nil
	}
}

// Bool dereferences a tri-state with an explicit fallback.
func Bool(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// WithDefaults fills the resolver defaults the producer is responsible for, so
// the probe reads already-resolved values: step/margin (12/5), local_proxy
// (false — forced off for characterization), auto_recovery (true — the #865
// default-ON). LiveOffsetS's "0" default stays in ProbeLaunchArgs (a launch-arg
// concern).
func (a *ArmConfig) WithDefaults() {
	if a.StepS == 0 {
		a.StepS = DefaultStepS
	}
	if a.MarginPct == 0 {
		a.MarginPct = DefaultMarginPct
	}
	if a.LocalProxy == nil {
		f := false
		a.LocalProxy = &f
	}
	if a.AutoRecovery == nil {
		t := true
		a.AutoRecovery = &t
	}
}

// Save marshals a plan to path (indented, for `cat`-ability).
func Save(path string, p *RunPlan) error {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = SchemaVersion
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("charplan: marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// Load reads + validates a plan written by Save. It hard-fails on a missing path
// or a schema-version mismatch so a stale test binary against a newer CLI errors
// loudly rather than silently reading zero arms.
func Load(path string) (*RunPlan, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("charplan: empty plan path (CHAR_RUN_PLAN_FILE unset)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("charplan: read %s: %w", path, err)
	}
	var p RunPlan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("charplan: parse %s: %w", path, err)
	}
	if p.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("charplan: plan schema v%d != supported v%d (rebuild the harness binary)", p.SchemaVersion, SchemaVersion)
	}
	return &p, nil
}
