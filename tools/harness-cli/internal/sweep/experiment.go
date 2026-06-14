// Package sweep is the local working store + experiment model for the
// automated fault-injection sweep (issue #772, docs/sweep-design.md).
//
// An *experiment* is one runnable recipe (one
// {platform × protocol × fault × shape × content × HLS-config × mode}
// combination) — ephemeral working state. A confirmed failure becomes a
// durable *finding* (a GitHub Issue) elsewhere; this package only owns the
// experiment queue. Experiments live as one JSON file each under a
// status-named subdirectory of `.sweep/`; the file's directory IS its status,
// and an atomic rename between directories is the parallel-safe claim lock
// (§4, §7 of the design).
package sweep

// Class is the sweep tier an experiment belongs to. The two tiers never mix —
// they have different purposes, recipe vocabularies, and oracle semantics, and
// are seeded + run one class at a time:
//
//   - config: realistic stream-config + benign-network variation (content
//     manipulation, rate shaping never below the sustainable floor, pattern
//     ladders, server transfer-timeouts). Asks "does the player make GOOD
//     decisions?" — ABR rung choice, over-downshift, manifest-config robustness.
//     ANY bad QoE label is the signal; there is no injected error to recover from.
//   - fault: explicit HTTP/connection error injection (4xx/5xx, corrupted,
//     connection_refused, dns_failure, rate_limiting, transport drop/reject,
//     request hangs). Asks "does the player RECOVER from errors, and are there
//     bugs in recovery?" — judged against the per-fault recovery-expected
//     envelope, so a fault the player should survive isn't a false positive.
type Class string

const (
	ClassConfig Class = "config" // realistic stream-config + network variation (default)
	ClassFault  Class = "fault"  // explicit error injection → recovery testing
)

// ClassOrDefault treats an empty Class as config (the default tier).
func (e *Experiment) ClassOrDefault() Class {
	if e.Class == "" {
		return ClassConfig
	}
	return e.Class
}

// Kind is what produced an experiment and how aggressively the scheduler
// should prioritise it (§5). isolation/bisect/hypothesis come from a hit;
// seed is the broad starter set.
type Kind string

const (
	KindSeed       Kind = "seed"       // broad starter cell
	KindIsolation  Kind = "isolation"  // one-factor-at-a-time probe off a confirmed hit (non-recursive)
	KindHypothesis Kind = "hypothesis" // proactive A/B comparison (non-recursive)
	KindBisect     Kind = "bisect"     // recursive narrowing of a continuous axis (depth-bounded)
)

// Arm tags one side of an A/B pair (§6). Empty for standalone experiments.
type Arm string

const (
	ArmControl Arm = "control"
	ArmVariant Arm = "variant"
)

// Verdict is the trichotomy outcome of analysing a run, plus the infra
// escape hatch `inconclusive` (§3, §11). Empty until the run is analysed.
type Verdict string

const (
	VerdictClean        Verdict = "clean"        // only info / *qoe_tier_premium
	VerdictNotable      Verdict = "notable"      // warning-tier label or high surprise, no error
	VerdictAberration   Verdict = "aberration"   // error / critical envelope breach
	VerdictInconclusive Verdict = "inconclusive" // probe/infra failure — NOT the player's fault
)

// Status is the lifecycle bucket, and also the subdirectory name under
// `.sweep/` that holds the experiment file (§4).
type Status string

const (
	StatusBacklog  Status = "backlog"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFound    Status = "found"
	StatusReview   Status = "review"
	StatusFeedback Status = "feedback"
)

// AllStatuses is the canonical ordered list — used to create the dir layout
// and to iterate buckets in `sweep status`.
var AllStatuses = []Status{
	StatusBacklog, StatusRunning, StatusDone, StatusFound, StatusReview, StatusFeedback,
}

// Fault mirrors the proxy FaultRule knobs the harness `fault add` exposes
// (§1). Only ever set on a `fault`-class experiment; nil otherwise (a
// config-class experiment never injects errors).
type Fault struct {
	Type        string `json:"type"`                   // 500, timeout, corrupted, connection_refused, …
	RequestKind string `json:"request_kind,omitempty"` // segment, manifest, master_manifest, init, audio_segment, …
	URLSubstr   string `json:"url_substr,omitempty"`   // optional URL scope
	Frequency   int    `json:"frequency,omitempty"`
	Mode        string `json:"mode,omitempty"`        // requests | seconds | failures_per_seconds | failures_per_packets
	Consecutive int    `json:"consecutive,omitempty"` // failure run length
}

// Shape is the realistic-bandwidth knob (§1). RateMbps is a static cap that
// must never sit below the lowest variant's sustainable rate (the floor guard —
// we test ABR decision quality, never forced starvation); Pattern is a
// ladder-derived sweep (pyramid/ramp/…) which stays within sustainable rungs by
// construction. delay_ms / loss_pct are deliberately ABSENT — steady network
// degradation is out of scope for both classes. Nil means "no shaping".
type Shape struct {
	RateMbps    *float64 `json:"rate_mbps,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`      // pyramid | ramp_up | ramp_down | square_wave | transient_shock
	StepSeconds int      `json:"step_seconds,omitempty"` // 6|12|18|24|60|120
	MarginPct   int      `json:"margin_pct,omitempty"`   // 0|5|10|25|50
}

// TransferTimeouts mirrors the proxy server-side transfer-timeout knob — a
// genuinely slow/stalled origin (config-class, §1). Distinct from the
// request_*_hang fault types (those are fault-class injected errors). 0 ⇒
// disabled. AppliesSegments defaults true server-side; set the Applies* flags
// to scope which request kinds the timeout governs.
type TransferTimeouts struct {
	ActiveSeconds    int  `json:"active_seconds,omitempty"`
	IdleSeconds      int  `json:"idle_seconds,omitempty"`
	AppliesSegments  bool `json:"applies_segments,omitempty"`
	AppliesManifests bool `json:"applies_manifests,omitempty"`
	AppliesMaster    bool `json:"applies_master,omitempty"`
}

// ContentManipulation mirrors the per-session master-manifest rewrite knobs
// (§1, §6) — the levers for HLS-aspect A/B comparison. Nil means "unmodified
// master". Pointers/empties distinguish "not set" from "set to zero".
type ContentManipulation struct {
	LiveOffset         *float64 `json:"live_offset,omitempty"`
	AllowedVariants    string   `json:"allowed_variants,omitempty"` // e.g. "drop-top-rung", a rung-set spec
	VariantOrder       string   `json:"variant_order,omitempty"`    // HLS-only
	StripCodecs        bool     `json:"strip_codecs,omitempty"`
	StripAvgBandwidth  bool     `json:"strip_avg_bandwidth,omitempty"`
	StripResolution    bool     `json:"strip_resolution,omitempty"`
	OverstateBandwidth *float64 `json:"overstate_bandwidth,omitempty"`
}

// Result is filled after a run is analysed.
type Result struct {
	Verdict Verdict  `json:"verdict"`
	Labels  []string `json:"labels,omitempty"` // the QoE labels the oracle saw
	Note    string   `json:"note,omitempty"`   // one-line human/oracle summary
}

// Experiment is one runnable recipe + its bookkeeping. JSON-serialised one
// per file under `.sweep/<status>/<id>.json`.
type Experiment struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"` // RFC3339 UTC; stamped by the caller (Date.now is unavailable in some contexts)

	// --- the recipe (matrix axes) ---
	Class               Class                `json:"class,omitempty"` // config (default) | fault — the sweep tier
	Platform            string               `json:"platform"`        // ipad-sim | iphone | appletv | androidtv | web
	Protocol            string               `json:"protocol"`        // hls | dash
	Content             string               `json:"content"`         // fixed to insane_new for now
	Mode                string               `json:"mode"`            // steps | pyramid | downshift_severity | …
	DurationS           int                  `json:"duration_s,omitempty"`
	Fault               *Fault               `json:"fault,omitempty"` // fault-class only
	Shape               *Shape               `json:"shape,omitempty"`
	ContentManipulation *ContentManipulation `json:"content_manipulation,omitempty"`
	TransferTimeouts    *TransferTimeouts    `json:"transfer_timeouts,omitempty"`

	// --- provenance / scheduling ---
	Kind     Kind    `json:"kind"`
	Arm      Arm     `json:"arm,omitempty"`
	Group    string  `json:"group,omitempty"`     // A/B pairing id
	Reps     int     `json:"reps,omitempty"`      // confirmation reps requested (1 for seed; ≥3 to confirm)
	RepGroup string  `json:"rep_group,omitempty"` // ties a rep-batch together
	Depth    int     `json:"depth"`               // recursive bisection depth (0–3 bound)
	Parent   string  `json:"parent,omitempty"`    // origin experiment id
	Score    float64 `json:"score"`               // scheduler sort key (§5)
	Why      string  `json:"why,omitempty"`       // LLM rationale slug (the why= label seed)
	WhyText  string  `json:"why_text,omitempty"`  // full prose rationale (also emitted as a control_event)

	// --- runtime / outcome ---
	Owner     string  `json:"owner,omitempty"`      // runner/worktree id, stamped at claim
	ClaimedAt string  `json:"claimed_at,omitempty"` // RFC3339 UTC stamped at claim; drives the stale-claim reaper (§11)
	PlayerID  string  `json:"player_id,omitempty"`  // the proxy session the probe played (stamped at bootstrap)
	PlayID    string  `json:"play_id,omitempty"`    // the play the run produced
	Result    *Result `json:"result,omitempty"`     // filled after analysis
}
