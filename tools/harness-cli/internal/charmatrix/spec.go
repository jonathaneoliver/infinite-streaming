// Package charmatrix turns a declarative YAML matrix spec into runnable
// characterization arms (issue #811). It replaces the flat CHAR_* / per-arm
// CHAR_ARM_%d_* env-var surface with axes → cartesian expansion plus an
// explicit-arm / groups escape hatch, reusing the typed recipe model in
// internal/sweep (Experiment + Shape/Fault/ContentManipulation/TransferTimeouts)
// and its server-side config path (experimentPlayerPatch → shaperBootstrapURL)
// rather than forking it.
//
// Knob namespaces — the prefix is the layer (feat/811-namespaced-knobs):
//
//   - is.*    → client launch arg, read once at launch → cold relaunch on change
//     (is.segment, is.protocol, is.codec, is.live_offset, is.peak_bitrate_mbps,
//     is.starts_first_variant).
//   - proxy.* → server config-on-connect, no relaunch (proxy.live_offset,
//     proxy.shape, proxy.fault, proxy.content_manipulation, proxy.transfer_timeouts).
//
// live_offset is the knob that exists on BOTH surfaces: proxy.live_offset rewrites
// the manifest hold-back server-side; is.live_offset is the client's own
// target-latency override. They are orthogonal — set one, the other, both (the
// precedence case), or neither. This replaces the old single live_offset + a
// `lever` router, whose mutual-exclusivity could never express the both-set cell.
package charmatrix

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"gopkg.in/yaml.v3"
)

// Spec is a whole matrix: shared defaults, the axes to expand, and/or explicit
// arms or comparison groups. JSON tags drive decoding — the YAML loader funnels
// through a JSON shim so the reused sweep types' existing `json:` tags work with
// no dual-tagging.
type Spec struct {
	Name      string           `json:"name"`
	Class     string           `json:"class,omitempty"`      // config (default) | fault — applied to every arm that doesn't set its own
	Parallel  bool             `json:"parallel,omitempty"`   // true ⇒ run arms simultaneously on the fleet backend; false ⇒ sequential
	Reps      int              `json:"reps,omitempty"`       // confirmation reps per arm (arm-level wins)
	DurationS int              `json:"duration_s,omitempty"` // play window per arm (arm-level wins)
	Compare   string           `json:"compare,omitempty"`    // axis name whose values form the control/variant arms WITHIN each group (≤4 values; requires parallel)
	Defaults  *Arm             `json:"defaults,omitempty"`   // base arm every expanded/explicit/group arm is layered over
	Axes      map[string][]any `json:"axes,omitempty"`       // axis-name → values; cartesian-expanded
	Arms      []*Arm           `json:"arms,omitempty"`       // explicit-arm escape hatch (appended after the cartesian product)
	Groups    []*Group         `json:"groups,omitempty"`     // control + variants comparison groups (appended after explicit arms)
}

// Group is the ergonomic control+variants form (the common ≤4-arm pairing). Every
// member is layered over the spec defaults; Expand stamps a shared Group id and
// the control/variant Role, so the dashboard receives them pre-paired.
type Group struct {
	ID       string `json:"id,omitempty"`       // group slug; defaults to gN by position
	Control  *Arm   `json:"control,omitempty"`  // the baseline arm (an empty `{}` means defaults unchanged)
	Variants []*Arm `json:"variants,omitempty"` // 1–3 variant arms, each flipping one knob off the control
}

// Arm is the flat YAML projection of one runnable cell. It compiles to a
// sweep.Experiment via ToExperiment (so experimentPlayerPatch / toProxyContent /
// toProxyFaultRule are reused as-is) and exposes the client-side knobs the probe
// reads as launch args. JSON tags carry the dotted namespace (is.* / proxy.*) —
// Go's encoding/json treats a dotted tag as a literal flat key, so no explosion
// machinery is needed.
type Arm struct {
	ID string `json:"id,omitempty"` // assigned by Expand from the axis values (reproducible); explicit-arm id wins if set

	// --- comparison grouping (set by Expand from compare:/groups:; explicit arms may hand-set) ---
	Group string `json:"group,omitempty"` // A/B pairing id; arms sharing it differ only on the compare axis
	Role  string `json:"role,omitempty"`  // control | variant — maps to sweep.Experiment.Arm

	// --- run-level (un-namespaced: not a client/server knob) ---
	Platform  string `json:"platform,omitempty"`   // ipad-sim | iphone | appletv | androidtv | web
	Content   string `json:"content,omitempty"`    // catalogue name to resume
	Mode      string `json:"mode,omitempty"`       // steps | pyramid | … (recorded on the experiment)
	Class     string `json:"class,omitempty"`      // config | fault (overrides the spec default)
	DurationS int    `json:"duration_s,omitempty"` // play window (overrides the spec default)
	Reps      int    `json:"reps,omitempty"`       // confirmation reps (overrides the spec default)

	// --- client knobs (is.* — launch arg, cold relaunch on change) ---
	Segment            string   `json:"is.segment,omitempty"`              // s2 | s6 | ll (empty = app default s6)
	Protocol           string   `json:"is.protocol,omitempty"`             // hls | dash
	Codec              string   `json:"is.codec,omitempty"`                // h264 | hevc | av1
	AppLiveOffset      *float64 `json:"is.live_offset,omitempty"`          // app-side target-latency override
	PeakBitrateMbps    int      `json:"is.peak_bitrate_mbps,omitempty"`    // startup peak-bitrate clamp (Mbps; app truncates to Int) → low start rung; 0 = off (#683)
	StartsFirstVariant *bool    `json:"is.starts_first_variant,omitempty"` // join on first manifest rung vs let ABR pick
	Muted              *bool    `json:"is.muted,omitempty"`                // mute audio (#838); nil = app default-mutes, set false to force audible

	// --- server knobs (proxy.* — config-on-connect, no relaunch) ---
	ProxyLiveOffset     *float64                   `json:"proxy.live_offset,omitempty"` // manifest hold-back (server live edge)
	Shape               *sweep.Shape               `json:"proxy.shape,omitempty"`
	Fault               *sweep.Fault               `json:"proxy.fault,omitempty"`
	ContentManipulation *sweep.ContentManipulation `json:"proxy.content_manipulation,omitempty"` // full nested form (per-field wins over the flat conveniences below)
	TransferTimeouts    *sweep.TransferTimeouts    `json:"proxy.transfer_timeouts,omitempty"`

	// --- flat content-manipulation conveniences (proxy.* → folded onto
	// ContentManipulation in ToExperiment; the nested block above wins per-field) ---
	StripCodecs        *bool    `json:"proxy.strip_codecs,omitempty"`
	StripAvgBandwidth  *bool    `json:"proxy.strip_avg_bandwidth,omitempty"`
	StripResolution    *bool    `json:"proxy.strip_resolution,omitempty"`
	AllowedVariants    string   `json:"proxy.allowed_variants,omitempty"` // ladder spec: drop-top-rung | drop-top-<N> | keep-bottom-<N>
	VariantOrder       string   `json:"proxy.variant_order,omitempty"`    // default | ascending | descending
	OverstateBandwidth *float64 `json:"proxy.overstate_bandwidth,omitempty"`
}

// Load parses a YAML matrix spec via the JSON shim: yaml.v3 → generic map →
// json.Unmarshal, so the reused sweep types' `json:` tags drive decoding and we
// never dual-tag. Returns the spec ready for Expand.
func Load(data []byte) (*Spec, error) {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	jsonBytes, err := json.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("yaml→json shim: %w", err)
	}
	var spec Spec
	dec := json.NewDecoder(bytes.NewReader(jsonBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}
	if spec.Name == "" {
		return nil, fmt.Errorf("spec.name is required")
	}
	return &spec, nil
}
