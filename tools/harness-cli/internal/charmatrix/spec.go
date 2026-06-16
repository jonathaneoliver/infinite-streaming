// Package charmatrix turns a declarative YAML matrix spec into runnable
// characterization arms (issue #811). It replaces the flat CHAR_* / per-arm
// CHAR_ARM_%d_* env-var surface with axes → cartesian expansion plus an
// explicit-arm escape hatch, reusing the typed recipe model in internal/sweep
// (Experiment + Shape/Fault/ContentManipulation/TransferTimeouts) and its
// server-side config path (experimentPlayerPatch → shaperBootstrapURL) rather
// than forking it.
//
// The two-layer constraint the matrix must respect (#793):
//
//   - Server-side knobs (manifest live_offset, faults, shape, content
//     manipulation, transfer timeouts) land via config-on-connect — no app
//     restart.
//   - Client-side knobs (segment, app-side live_offset override, protocol, peak
//     bitrate) are launch args read once at app launch — a cold launch per arm
//     whenever one of them changes (the per-play push is #800).
//
// The `lever` axis is what routes a live_offset value to the server (manifest
// hold-back, lever=proxy) versus the client (app override, lever=app).
package charmatrix

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"gopkg.in/yaml.v3"
)

// Spec is a whole matrix: shared defaults, the axes to expand, and/or an
// explicit list of arms. JSON tags drive decoding — the YAML loader funnels
// through a JSON shim so the reused sweep types' existing `json:` tags work with
// no dual-tagging.
type Spec struct {
	Name      string           `json:"name"`
	Class     string           `json:"class,omitempty"`      // config (default) | fault — applied to every arm that doesn't set its own
	Parallel  bool             `json:"parallel,omitempty"`   // true ⇒ run arms simultaneously on the fleet backend; false ⇒ sequential
	Reps      int              `json:"reps,omitempty"`       // confirmation reps per arm (arm-level wins)
	DurationS int              `json:"duration_s,omitempty"` // play window per arm (arm-level wins)
	Defaults  *Arm             `json:"defaults,omitempty"`   // base arm every expanded/explicit arm is layered over
	Axes      map[string][]any `json:"axes,omitempty"`       // axis-name → values; cartesian-expanded
	Arms      []*Arm           `json:"arms,omitempty"`       // explicit-arm escape hatch (appended after the cartesian product)
}

// Arm is the flat YAML projection of one runnable cell. It compiles to a
// sweep.Experiment via ToExperiment (so experimentPlayerPatch / toProxyContent /
// toProxyFaultRule are reused as-is) and exposes the client-side knobs the probe
// reads as launch args. The reused sweep recipe types are embedded by pointer so
// the YAML's nested `shape:` / `fault:` / `content_manipulation:` /
// `transfer_timeouts:` blocks decode straight into them.
type Arm struct {
	ID string `json:"id,omitempty"` // assigned by Expand from the axis values (reproducible); explicit-arm id wins if set

	// --- recipe axes (both server- and client-side) ---
	Platform  string `json:"platform,omitempty"`   // ipad-sim | iphone | appletv | androidtv | web
	Protocol  string `json:"protocol,omitempty"`   // hls | dash (client launch arg)
	Content   string `json:"content,omitempty"`    // catalogue name to resume
	Segment   string `json:"segment,omitempty"`    // s2 | s6 | ll (client launch arg; empty = app default s6)
	Mode      string `json:"mode,omitempty"`       // steps | pyramid | … (recorded on the experiment)
	Class     string `json:"class,omitempty"`      // config | fault (overrides the spec default)
	DurationS int    `json:"duration_s,omitempty"` // play window (overrides the spec default)
	Reps      int    `json:"reps,omitempty"`       // confirmation reps (overrides the spec default)

	// --- live-offset matrix (#793) ---
	Lever      string   `json:"lever,omitempty"`       // proxy (server manifest hold-back) | app (client override). Default proxy when a live_offset is set.
	LiveOffset *float64 `json:"live_offset,omitempty"` // the offset value; Lever decides whether it lands server- or client-side

	// --- client-side knob ---
	PeakBitrateMbps int `json:"peak_bitrate_mbps,omitempty"` // -is.flag.peak_bitrate_mbps startup clamp; 0 = omit (#683)

	// --- server-side knobs (reused sweep recipe types) ---
	Shape               *sweep.Shape               `json:"shape,omitempty"`
	Fault               *sweep.Fault               `json:"fault,omitempty"`
	ContentManipulation *sweep.ContentManipulation `json:"content_manipulation,omitempty"`
	TransferTimeouts    *sweep.TransferTimeouts    `json:"transfer_timeouts,omitempty"`
}

// Load parses a YAML matrix spec via the JSON shim: yaml.v3 → generic map →
// json.Unmarshal, so the reused sweep types' `json:` tags drive decoding and we
// never dual-tag. Returns the spec ready for Expand.
func Load(data []byte) (*Spec, error) {
	var generic any
	if err := yaml.Unmarshal(data, &generic); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	// yaml.v3 decodes maps as map[string]interface{} for string keys at the top
	// level, so the JSON re-encode is direct. (yaml.v3 ≥ v3 no longer produces
	// map[interface{}]interface{} for string-keyed maps.)
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
