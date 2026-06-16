package charmatrix

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// leverProxy / leverApp are the two live_offset routing targets. Empty defaults
// to proxy (the #793 manifest hold-back) when a live_offset is present.
const (
	leverProxy = "proxy"
	leverApp   = "app"
)

// axisKeys is the set of scalar arm fields an `axes:` block may sweep. Object
// fields (shape/fault/content_manipulation/transfer_timeouts) and the
// Expand-assigned id are deliberately excluded — those are set on defaults or
// the explicit-arm escape hatch, not cartesian-swept. Unknown axis names are a
// validation error so a typo fails fast instead of silently expanding nothing.
var axisKeys = map[string]bool{
	"platform":          true,
	"protocol":          true,
	"content":           true,
	"segment":           true,
	"mode":              true,
	"class":             true,
	"lever":             true,
	"live_offset":       true,
	"peak_bitrate_mbps": true,
	"duration_s":        true,
	"reps":              true,
}

// Expand turns a spec into its ordered list of arms: the cartesian product of
// the axes (an odometer over axis names in sorted order, so ids are
// reproducible run-to-run) followed by any explicit arms. Each arm is the spec
// defaults with the combination's values layered on top. Validation runs up
// front (unknown axes, bad lever, out-of-window live_offset, …) so a malformed
// matrix fails before any session is touched.
func Expand(spec *Spec) ([]*Arm, error) {
	if spec == nil {
		return nil, fmt.Errorf("nil spec")
	}
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	baseMap, err := armToMap(spec.Defaults)
	if err != nil {
		return nil, err
	}
	// Spec-level class/reps/duration_s seed the defaults so every arm inherits
	// them unless an axis or explicit arm overrides.
	applySpecDefaults(baseMap, spec)

	var arms []*Arm

	// --- cartesian product over the axes ---
	names := sortedAxisNames(spec.Axes)
	if len(names) > 0 {
		// Odometer: indices[i] selects the current value of axis names[i].
		indices := make([]int, len(names))
		for {
			combo := cloneMap(baseMap)
			for i, name := range names {
				combo[name] = spec.Axes[name][indices[i]]
			}
			arm, err := mapToArm(combo)
			if err != nil {
				return nil, err
			}
			arm.ID = comboID(spec.Name, names, indices, spec.Axes)
			if err := validateArm(arm); err != nil {
				return nil, fmt.Errorf("arm %s: %w", arm.ID, err)
			}
			arms = append(arms, arm)

			// advance the odometer (rightmost axis fastest)
			pos := len(names) - 1
			for pos >= 0 {
				indices[pos]++
				if indices[pos] < len(spec.Axes[names[pos]]) {
					break
				}
				indices[pos] = 0
				pos--
			}
			if pos < 0 {
				break
			}
		}
	}

	// --- explicit-arm escape hatch (layered over the same defaults) ---
	for i, ex := range spec.Arms {
		exMap, err := armToMap(ex)
		if err != nil {
			return nil, err
		}
		merged := mergeMaps(cloneMap(baseMap), exMap)
		arm, err := mapToArm(merged)
		if err != nil {
			return nil, err
		}
		if arm.ID == "" {
			arm.ID = fmt.Sprintf("%s/arm%d", spec.Name, i)
		}
		if err := validateArm(arm); err != nil {
			return nil, fmt.Errorf("arm %s: %w", arm.ID, err)
		}
		arms = append(arms, arm)
	}

	if len(arms) == 0 {
		return nil, fmt.Errorf("spec %q expanded to zero arms: set `axes:` or `arms:`", spec.Name)
	}
	return arms, nil
}

// ToExperiment compiles the arm into the server-side recipe of record. The
// live_offset axis is routed to the manifest hold-back (ContentManipulation)
// only when the lever is proxy; an app-lever offset stays a client launch arg
// (see ClientLiveOffsetS) and never touches the server config. Segment /
// protocol live on the Experiment itself.
func (a *Arm) ToExperiment() *sweep.Experiment {
	e := &sweep.Experiment{
		ID:                  a.ID,
		Class:               sweep.Class(a.Class),
		Platform:            a.Platform,
		LaunchMode:          sweep.LaunchModeAppium,
		Protocol:            a.Protocol,
		Content:             a.Content,
		Segment:             a.Segment,
		Mode:                a.Mode,
		DurationS:           a.DurationS,
		Reps:                a.Reps,
		Kind:                sweep.KindHypothesis, // a matrix is a planned A/B sweep, not a seed/isolation
		Shape:               cloneShape(a.Shape),
		Fault:               cloneFault(a.Fault),
		ContentManipulation: cloneCM(a.ContentManipulation),
		TransferTimeouts:    cloneXfer(a.TransferTimeouts),
	}
	if a.LiveOffset != nil && a.serverLever() {
		if e.ContentManipulation == nil {
			e.ContentManipulation = &sweep.ContentManipulation{}
		}
		// An explicit content_manipulation.live_offset on the arm wins over the
		// axis value (the escape hatch is intentional).
		if e.ContentManipulation.LiveOffset == nil {
			v := *a.LiveOffset
			e.ContentManipulation.LiveOffset = &v
		}
	}
	return e
}

// serverLever reports whether this arm's live_offset lands server-side. Empty
// lever defaults to proxy (the #793 manifest hold-back).
func (a *Arm) serverLever() bool {
	return a.Lever == "" || a.Lever == leverProxy
}

// ClientLiveOffsetS is the value for the client's -is.flag.live_offset_s launch
// arg (CHAR_SWEEP_LIVE_OFFSET). It is the offset only when the lever is app;
// otherwise "0" — the probe always pins the flag so a run never inherits the
// app's persisted stepper value.
func (a *Arm) ClientLiveOffsetS() string {
	if a.LiveOffset != nil && a.Lever == leverApp {
		return formatNum(*a.LiveOffset)
	}
	return "0"
}

// IntendedLiveOffset is the offset the arm means to impose regardless of lever,
// for the post-run manipulation check (AchievedOffsetFromEvents /
// ManipulationLanded). ok is false when this arm has no live_offset.
func (a *Arm) IntendedLiveOffset() (float64, bool) {
	if a.LiveOffset == nil || *a.LiveOffset <= 0 {
		return 0, false
	}
	return *a.LiveOffset, true
}

// --- validation ----------------------------------------------------------

func validateSpec(spec *Spec) error {
	for name, vals := range spec.Axes {
		if !axisKeys[name] {
			return fmt.Errorf("unknown axis %q (known: %s)", name, knownAxisList())
		}
		if len(vals) == 0 {
			return fmt.Errorf("axis %q has no values", name)
		}
	}
	if spec.Class != "" {
		if err := validateClass(spec.Class); err != nil {
			return err
		}
	}
	return nil
}

func validateArm(a *Arm) error {
	if a.Platform != "" && !validPlatform(a.Platform) {
		return fmt.Errorf("platform %q invalid (ipad-sim|iphone|appletv|androidtv|web)", a.Platform)
	}
	if a.Protocol != "" && a.Protocol != "hls" && a.Protocol != "dash" {
		return fmt.Errorf("protocol %q invalid (hls|dash)", a.Protocol)
	}
	if a.Segment != "" && a.Segment != "s2" && a.Segment != "s6" && a.Segment != "ll" {
		return fmt.Errorf("segment %q invalid (s2|s6|ll)", a.Segment)
	}
	if a.Lever != "" && a.Lever != leverProxy && a.Lever != leverApp {
		return fmt.Errorf("lever %q invalid (proxy|app)", a.Lever)
	}
	if a.Class != "" {
		if err := validateClass(a.Class); err != nil {
			return err
		}
	}
	// live_offset window: validate against the proxy's supported enum up front so
	// an unsupported window fails before any session is configured. The app lever
	// reads the same product window, so the check applies to both.
	if a.LiveOffset != nil {
		off := proxy.ContentManipulationLiveOffset(int(*a.LiveOffset))
		if float64(int(*a.LiveOffset)) != *a.LiveOffset || !off.Valid() {
			return fmt.Errorf("live_offset %g is not a supported window (0|2|4|6|12|18|24|30|36|42)", *a.LiveOffset)
		}
	}
	return nil
}

func validateClass(c string) error {
	if c != string(sweep.ClassConfig) && c != string(sweep.ClassFault) {
		return fmt.Errorf("class %q invalid (config|fault)", c)
	}
	return nil
}

func validPlatform(p string) bool {
	switch p {
	case "ipad-sim", "iphone", "appletv", "androidtv", "web":
		return true
	}
	return false
}

// --- helpers -------------------------------------------------------------

func sortedAxisNames(axes map[string][]any) []string {
	names := make([]string, 0, len(axes))
	for n := range axes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// comboID builds a reproducible, label-safe id from the current odometer
// position: name/axis-value pairs in sorted-axis order, joined by '.'. Uses '-'
// and '.' (never '=' or ',', which the forwarder label vocab forbids).
func comboID(specName string, names []string, indices []int, axes map[string][]any) string {
	parts := make([]string, 0, len(names))
	for i, name := range names {
		parts = append(parts, name+"-"+slug(axes[name][indices[i]]))
	}
	return specName + "/" + strings.Join(parts, ".")
}

func slug(v any) string {
	s := formatAny(v)
	s = strings.ReplaceAll(s, "=", "_")
	s = strings.ReplaceAll(s, ",", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// formatAny renders an axis value for an id; formatNum keeps integral floats
// integral (24, not 24.000000).
func formatAny(v any) string {
	switch t := v.(type) {
	case float64:
		return formatNum(t)
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func formatNum(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func knownAxisList() string {
	names := make([]string, 0, len(axisKeys))
	for n := range axisKeys {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// armToMap round-trips an arm through JSON to a generic map (nil → empty map),
// so axis overlay and defaults-merge happen on a uniform representation.
func armToMap(a *Arm) (map[string]any, error) {
	if a == nil {
		return map[string]any{}, nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func mapToArm(m map[string]any) (*Arm, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var a Arm
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeMaps overlays src onto dst (src wins) and returns dst. Shallow — arm maps
// are one level of scalars plus whole object blocks, and an explicit arm
// replaces a block wholesale rather than deep-merging into it.
func mergeMaps(dst, src map[string]any) map[string]any {
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func applySpecDefaults(base map[string]any, spec *Spec) {
	if _, ok := base["class"]; !ok && spec.Class != "" {
		base["class"] = spec.Class
	}
	if _, ok := base["reps"]; !ok && spec.Reps != 0 {
		base["reps"] = spec.Reps
	}
	if _, ok := base["duration_s"]; !ok && spec.DurationS != 0 {
		base["duration_s"] = spec.DurationS
	}
}

// --- deep clones of the reused sweep recipe types (so arms never share a
// pointer with the defaults they were layered over) ---

func cloneShape(s *sweep.Shape) *sweep.Shape {
	if s == nil {
		return nil
	}
	c := *s
	if s.RateMbps != nil {
		v := *s.RateMbps
		c.RateMbps = &v
	}
	return &c
}

func cloneFault(f *sweep.Fault) *sweep.Fault {
	if f == nil {
		return nil
	}
	c := *f
	return &c
}

func cloneCM(cm *sweep.ContentManipulation) *sweep.ContentManipulation {
	if cm == nil {
		return nil
	}
	c := *cm
	if cm.LiveOffset != nil {
		v := *cm.LiveOffset
		c.LiveOffset = &v
	}
	if cm.OverstateBandwidth != nil {
		v := *cm.OverstateBandwidth
		c.OverstateBandwidth = &v
	}
	return &c
}

func cloneXfer(t *sweep.TransferTimeouts) *sweep.TransferTimeouts {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
