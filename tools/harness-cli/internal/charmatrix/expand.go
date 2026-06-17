package charmatrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// maxArmsPerGroup caps a comparison group: one control + ≤3 variants. A larger
// group is almost always a mistake (an over-wide compare axis, or a pairing too
// big to read), so Expand rejects it up front.
const maxArmsPerGroup = 4

// objectAxisKeys are the object-valued knobs an `axes:` block may sweep — each
// value is a whole recipe block (a sweep.Shape / Fault / ContentManipulation /
// TransferTimeouts), not a scalar. An object axis value replaces the defaults'
// block wholesale (shallow overlay, same as an explicit arm) and gets a
// `label`-or-hash id slug (see axisSlugAndValue) so a {pattern:…} block produces
// a clean id, not a stringified map. The Expand-assigned id/group/role are never
// axis-swept.
var objectAxisKeys = map[string]bool{
	"proxy.shape":                true,
	"proxy.fault":                true,
	"proxy.content_manipulation": true,
	"proxy.transfer_timeouts":    true,
}

// axisKeys is the set of scalar arm fields an `axes:` block may sweep, keyed by
// the dotted namespace the YAML uses. Unknown axis names (not in axisKeys or
// objectAxisKeys) are a validation error so a typo fails fast instead of silently
// expanding nothing.
var axisKeys = map[string]bool{
	"platform":                  true,
	"content":                   true,
	"mode":                      true,
	"class":                     true,
	"duration_s":                true,
	"reps":                      true,
	"is.segment":                true,
	"is.protocol":               true,
	"is.codec":                  true,
	"is.live_offset":            true,
	"is.peak_bitrate_mbps":      true,
	"is.starts_first_variant":   true,
	"proxy.live_offset":         true,
	"proxy.strip_codecs":        true,
	"proxy.strip_avg_bandwidth": true,
	"proxy.strip_resolution":    true,
	"proxy.allowed_variants":    true,
	"proxy.variant_order":       true,
	"proxy.overstate_bandwidth": true,
}

// Expand turns a spec into its ordered list of arms: the cartesian product of
// the axes (an odometer over axis names in sorted order, so ids are
// reproducible run-to-run), then any explicit arms, then any comparison groups.
// Each arm is the spec defaults with the combination's values layered on top.
// Validation runs up front (unknown axes, bad compare, out-of-window live_offset,
// over-large groups, …) so a malformed matrix fails before any session is touched.
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
		// Precompute each axis value's id-slug + decode-value once: object axis
		// values get a `label`-or-hash slug (so a {pattern:…} block yields a clean
		// id, not a stringified map); scalars keep their plain slug.
		axisVals := buildAxisVals(spec.Axes)
		// Odometer: indices[i] selects the current value of axis names[i].
		indices := make([]int, len(names))
		for {
			combo := cloneMap(baseMap)
			for i, name := range names {
				combo[name] = axisVals[name][indices[i]].val
			}
			arm, err := mapToArm(combo)
			if err != nil {
				return nil, err
			}
			arm.ID = comboID(spec.Name, names, indices, axisVals)
			// compare: groups arms that agree on every OTHER axis, with the
			// first value of the compare axis as control and the rest variants.
			if spec.Compare != "" {
				arm.Group = comboGroupID(spec.Name, names, indices, axisVals, spec.Compare, arm.Platform)
				arm.Role = comboRole(spec.Compare, names, indices, spec.Axes)
			}
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
		arm, err := overlayArm(baseMap, ex)
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

	// --- groups block: control + variants, pre-paired ---
	for gi, g := range spec.Groups {
		gid := g.ID
		if gid == "" {
			gid = fmt.Sprintf("g%d", gi)
		}
		groupKey := fmt.Sprintf("%s/%s", spec.Name, gid)

		ctrl, err := overlayArm(baseMap, g.Control)
		if err != nil {
			return nil, err
		}
		ctrl.Group = groupKey
		ctrl.Role = string(sweep.ArmControl)
		ctrl.ID = groupKey + "/control"
		if err := validateArm(ctrl); err != nil {
			return nil, fmt.Errorf("group %s control: %w", gid, err)
		}
		arms = append(arms, ctrl)

		for vi, v := range g.Variants {
			va, err := overlayArm(baseMap, v)
			if err != nil {
				return nil, err
			}
			va.Group = groupKey
			va.Role = string(sweep.ArmVariant)
			va.ID = fmt.Sprintf("%s/var%d", groupKey, vi)
			if err := validateArm(va); err != nil {
				return nil, fmt.Errorf("group %s var%d: %w", gid, vi, err)
			}
			arms = append(arms, va)
		}
	}

	if len(arms) == 0 {
		return nil, fmt.Errorf("spec %q expanded to zero arms: set `axes:`, `arms:`, or `groups:`", spec.Name)
	}
	return arms, nil
}

// ToExperiment compiles the arm into the server-side recipe of record. The
// server live offset (proxy.live_offset) routes to the manifest hold-back
// (ContentManipulation); the client live offset (is.live_offset) stays a client
// launch arg (see ClientLiveOffsetS) and never touches the server config — so the
// both-set arm imposes both surfaces at once (the precedence cell). Segment /
// protocol live on the Experiment itself; Group / Arm carry the pairing.
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
		Group:               a.Group,
		Arm:                 sweep.Arm(a.Role),
		Shape:               cloneShape(a.Shape),
		Fault:               cloneFault(a.Fault),
		ContentManipulation: a.contentManipulation(),
		TransferTimeouts:    cloneXfer(a.TransferTimeouts),
	}
	return e
}

// contentManipulation folds the flat proxy.* conveniences (proxy.live_offset,
// proxy.strip_*, proxy.allowed_variants, proxy.variant_order,
// proxy.overstate_bandwidth) onto the nested proxy.content_manipulation block.
// The nested block wins per-field (it is the explicit full form); a flat
// convenience only fills a field the block left unset. Returns nil when nothing
// manipulates the manifest.
func (a *Arm) contentManipulation() *sweep.ContentManipulation {
	cm := cloneCM(a.ContentManipulation) // nested block, or nil
	ensure := func() {
		if cm == nil {
			cm = &sweep.ContentManipulation{}
		}
	}
	if a.ProxyLiveOffset != nil && *a.ProxyLiveOffset > 0 {
		ensure()
		if cm.LiveOffset == nil {
			v := *a.ProxyLiveOffset
			cm.LiveOffset = &v
		}
	}
	// Bool strips OR in (a nested-true stays true; a flat-true sets it).
	if a.StripCodecs != nil && *a.StripCodecs {
		ensure()
		cm.StripCodecs = true
	}
	if a.StripAvgBandwidth != nil && *a.StripAvgBandwidth {
		ensure()
		cm.StripAvgBandwidth = true
	}
	if a.StripResolution != nil && *a.StripResolution {
		ensure()
		cm.StripResolution = true
	}
	if a.AllowedVariants != "" {
		ensure()
		if cm.AllowedVariants == "" {
			cm.AllowedVariants = a.AllowedVariants
		}
	}
	if a.VariantOrder != "" {
		ensure()
		if cm.VariantOrder == "" {
			cm.VariantOrder = a.VariantOrder
		}
	}
	if a.OverstateBandwidth != nil {
		ensure()
		if cm.OverstateBandwidth == nil {
			v := *a.OverstateBandwidth
			cm.OverstateBandwidth = &v
		}
	}
	return cm
}

// ClientLiveOffsetS is the value for the client's -is.flag.live_offset_s launch
// arg (CHAR_SWEEP_LIVE_OFFSET): the is.live_offset value, or "0" — the probe
// always pins the flag so a run never inherits the app's persisted stepper value.
func (a *Arm) ClientLiveOffsetS() string {
	if a.AppLiveOffset != nil && *a.AppLiveOffset > 0 {
		return formatNum(*a.AppLiveOffset)
	}
	return "0"
}

// StartsFirstVariantS is the value for the -is.flag.starts_first_variant launch
// arg: "true"/"false" when the knob is set, or "" (omit) when unset — false is
// meaningful (let ABR pick the join rung), so it can't collapse to the zero value.
func (a *Arm) StartsFirstVariantS() string {
	if a.StartsFirstVariant == nil {
		return ""
	}
	return strconv.FormatBool(*a.StartsFirstVariant)
}

// IntendedLiveOffset is the offset the arm means to impose, for the post-run
// manipulation check (AchievedOffsetFromEvents / ManipulationLanded). The server
// offset (proxy.live_offset) is the one that lands as a manifest change, so it
// wins; an app-only arm reports its client override. ok is false when neither is
// set.
func (a *Arm) IntendedLiveOffset() (float64, bool) {
	if a.ProxyLiveOffset != nil && *a.ProxyLiveOffset > 0 {
		return *a.ProxyLiveOffset, true
	}
	if a.AppLiveOffset != nil && *a.AppLiveOffset > 0 {
		return *a.AppLiveOffset, true
	}
	return 0, false
}

// --- validation ----------------------------------------------------------

func validateSpec(spec *Spec) error {
	for name, vals := range spec.Axes {
		if !axisKeys[name] && !objectAxisKeys[name] {
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
	if spec.Compare != "" {
		vals, ok := spec.Axes[spec.Compare]
		if !ok {
			return fmt.Errorf("compare axis %q is not one of the axes", spec.Compare)
		}
		if len(vals) < 2 {
			return fmt.Errorf("compare axis %q needs ≥2 values to form a comparison (has %d)", spec.Compare, len(vals))
		}
		if len(vals) > maxArmsPerGroup {
			return fmt.Errorf("compare axis %q has %d values; a group holds at most %d arms", spec.Compare, len(vals), maxArmsPerGroup)
		}
		if !spec.Parallel {
			return fmt.Errorf("compare axis %q requires parallel: true — sequential arms defeat the pairing (temporal confounds won't cancel)", spec.Compare)
		}
	}
	for gi, g := range spec.Groups {
		if len(g.Variants) == 0 {
			return fmt.Errorf("group %d (%q) needs at least one variant", gi, g.ID)
		}
		if n := 1 + len(g.Variants); n > maxArmsPerGroup {
			return fmt.Errorf("group %d (%q) has %d arms; at most %d", gi, g.ID, n, maxArmsPerGroup)
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
	if a.Role != "" && a.Role != string(sweep.ArmControl) && a.Role != string(sweep.ArmVariant) {
		return fmt.Errorf("role %q invalid (control|variant)", a.Role)
	}
	switch a.VariantOrder {
	case "", "default", "ascending", "descending":
	default:
		return fmt.Errorf("variant_order %q invalid (default|ascending|descending)", a.VariantOrder)
	}
	if a.Class != "" {
		if err := validateClass(a.Class); err != nil {
			return err
		}
	}
	// live_offset window: validate both surfaces against the proxy's supported
	// enum up front so an unsupported window fails before any session is
	// configured. The app surface reads the same product window.
	for _, off := range []*float64{a.ProxyLiveOffset, a.AppLiveOffset} {
		if off == nil {
			continue
		}
		o := proxy.ContentManipulationLiveOffset(int(*off))
		if float64(int(*off)) != *off || !o.Valid() {
			return fmt.Errorf("live_offset %g is not a supported window (0|2|4|6|12|18|24|30|36|42)", *off)
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
// and '.' (never '=' or ',', which the forwarder label vocab forbids). The slug
// is precomputed per axis value (scalar slug, or object label/hash).
func comboID(specName string, names []string, indices []int, av map[string][]axisVal) string {
	parts := make([]string, 0, len(names))
	for i, name := range names {
		parts = append(parts, name+"-"+av[name][indices[i]].slug)
	}
	return specName + "/" + strings.Join(parts, ".")
}

// comboGroupID is comboID with the compare axis (and platform) OMITTED, suffixed
// by platform — so every arm differing only on the compare axis lands in one
// group, and arms on different devices never cross-compare (mirrors seed.go's
// grp-<slug>-<platform> convention).
func comboGroupID(specName string, names []string, indices []int, av map[string][]axisVal, compare, platform string) string {
	parts := make([]string, 0, len(names))
	for i, name := range names {
		if name == compare || name == "platform" {
			continue
		}
		parts = append(parts, name+"-"+av[name][indices[i]].slug)
	}
	base := specName
	if len(parts) > 0 {
		base += "/" + strings.Join(parts, ".")
	}
	if platform != "" {
		return fmt.Sprintf("grp-%s-%s", base, platform)
	}
	return "grp-" + base
}

// comboRole tags the arm's side of the comparison: the FIRST value of the compare
// axis is the control, every other value a variant (Experiment.Arm is a free
// string, so a >2-way compare axis yields one control + N variants).
func comboRole(compare string, names []string, indices []int, axes map[string][]any) string {
	for i, name := range names {
		if name == compare {
			if indices[i] == 0 {
				return string(sweep.ArmControl)
			}
			return string(sweep.ArmVariant)
		}
	}
	return ""
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
	names := make([]string, 0, len(axisKeys)+len(objectAxisKeys))
	for n := range axisKeys {
		names = append(names, n)
	}
	for n := range objectAxisKeys {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// axisVal pairs an axis value's id-slug with the value to merge into the arm map.
type axisVal struct {
	slug string
	val  any
}

// buildAxisVals precomputes the (slug, decode-value) for every axis value so the
// combo build and comboID stay consistent.
func buildAxisVals(axes map[string][]any) map[string][]axisVal {
	out := make(map[string][]axisVal, len(axes))
	for name, vals := range axes {
		av := make([]axisVal, len(vals))
		for i, v := range vals {
			s, decoded := axisSlugAndValue(v)
			av[i] = axisVal{slug: s, val: decoded}
		}
		out[name] = av
	}
	return out
}

// axisSlugAndValue derives an id-slug for one axis value and the value to merge.
// Scalars keep their plain slug. An object value (a recipe block) uses an optional
// reserved `label:` key for a readable id — stripped before the block is decoded
// — or, absent a label, a short stable hash of the block's canonical JSON.
func axisSlugAndValue(v any) (string, any) {
	m, ok := v.(map[string]any)
	if !ok {
		return slug(v), v
	}
	if lbl, ok := m["label"].(string); ok && lbl != "" {
		clean := make(map[string]any, len(m))
		for k, vv := range m {
			if k == "label" {
				continue
			}
			clean[k] = vv
		}
		return slug(lbl), clean
	}
	return hashObject(m), m
}

// hashObject is a short, label-safe, stable id for an unlabelled object axis
// value. json.Marshal sorts map keys, so the same block always hashes the same.
func hashObject(m map[string]any) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "obj"
	}
	h := fnv.New32a()
	_, _ = h.Write(b)
	return fmt.Sprintf("obj-%08x", h.Sum32())
}

// overlayArm layers an explicit/group arm over the defaults map (arm wins) and
// returns the merged Arm. Shared by the explicit-arm and groups paths.
func overlayArm(base map[string]any, a *Arm) (*Arm, error) {
	am, err := armToMap(a)
	if err != nil {
		return nil, err
	}
	return mapToArm(mergeMaps(cloneMap(base), am))
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

// mapToArm decodes a merged arm map strictly: DisallowUnknownFields applies
// recursively, so a typo'd key inside an object axis value (e.g. proxy.shape:
// {patern: pyramid}) is rejected rather than silently dropped. Combo keys are
// always valid Arm fields (axis names are validated; defaults come from a valid
// Arm), so the only thing strict mode adds is catching object-internal typos.
func mapToArm(m map[string]any) (*Arm, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var a Arm
	if err := dec.Decode(&a); err != nil {
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
// are one level of scalars plus whole object blocks, and an explicit arm replaces
// a block wholesale rather than deep-merging into it.
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
