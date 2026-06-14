package sweep

import (
	"fmt"
	"hash/fnv"
	"strconv"
)

// Axis names a single dimension the isolation fan can flip to attribute a
// confirmed hit (§6). The LLM picks which axes (and values) are most
// informative for a given failure; this package materialises them as valid
// one-axis-flip experiments. The content axis is deferred (single content).
type Axis string

const (
	AxisPlatform          Axis = "platform"
	AxisProtocol          Axis = "protocol"
	AxisLiveOffset        Axis = "liveoffset"
	AxisLadder            Axis = "ladder"
	AxisVariantOrder      Axis = "variant_order"
	AxisStripAvgBandwidth Axis = "strip_avg_bandwidth"
	AxisStripCodecs       Axis = "strip_codecs"
	AxisStripResolution   Axis = "strip_resolution"
	AxisOverstate         Axis = "overstate_bandwidth"
)

// AxisTier returns the escalation tier (§6): Tier 1 = platform/protocol (cheap,
// high-signal, different devices → simultaneous); Tier 2 = the manifest knobs.
func AxisTier(a Axis) int {
	switch a {
	case AxisPlatform, AxisProtocol:
		return 1
	default:
		return 2
	}
}

// Flip is one axis change the LLM proposes for a variant.
type Flip struct {
	Axis  Axis   `json:"axis"`
	Value string `json:"value"` // parsed per-axis (platform name, "drop-top-rung", a float for offsets, "true" for strips)
}

// MaxIsolationAxes caps a single failure's fan so it can't monopolise the pool (§6).
const MaxIsolationAxes = 8

func shortHash(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%06x", h.Sum32()&0xffffff)
}

func cloneExperimentRecipe(p *Experiment) *Experiment {
	e := *p // shallow copy of scalars
	e.Fault = cloneFault(p.Fault)
	if p.Shape != nil {
		sh := *p.Shape
		e.Shape = &sh
	}
	if p.ContentManipulation != nil {
		cm := *p.ContentManipulation
		e.ContentManipulation = &cm
	}
	if p.TransferTimeouts != nil {
		tt := *p.TransferTimeouts
		e.TransferTimeouts = &tt
	}
	// clear per-experiment state that must not be inherited
	e.Owner, e.ClaimedAt, e.PlayerID, e.PlayID, e.Result = "", "", "", "", nil
	return &e
}

func (e *Experiment) ensureCM() *ContentManipulation {
	if e.ContentManipulation == nil {
		e.ContentManipulation = &ContentManipulation{}
	}
	return e.ContentManipulation
}

// applyFlip mutates exactly one axis on e. Returns an error for an unknown
// axis or unparseable value, so a bad LLM proposal fails loudly.
func applyFlip(e *Experiment, f Flip) error {
	switch f.Axis {
	case AxisPlatform:
		e.Platform = f.Value
	case AxisProtocol:
		e.Protocol = f.Value
	case AxisLiveOffset:
		v, err := strconv.ParseFloat(f.Value, 64)
		if err != nil {
			return fmt.Errorf("liveoffset value %q: %w", f.Value, err)
		}
		e.ensureCM().LiveOffset = &v
	case AxisOverstate:
		v, err := strconv.ParseFloat(f.Value, 64)
		if err != nil {
			return fmt.Errorf("overstate value %q: %w", f.Value, err)
		}
		e.ensureCM().OverstateBandwidth = &v
	case AxisLadder:
		e.ensureCM().AllowedVariants = f.Value
	case AxisVariantOrder:
		e.ensureCM().VariantOrder = f.Value
	case AxisStripAvgBandwidth:
		e.ensureCM().StripAvgBandwidth = true
	case AxisStripCodecs:
		e.ensureCM().StripCodecs = true
	case AxisStripResolution:
		e.ensureCM().StripResolution = true
	default:
		return fmt.Errorf("unknown isolation axis %q", f.Axis)
	}
	return nil
}

// IsolationFan materialises an OFAT ablation off a confirmed-aberrant parent
// (§6): a `control` arm (the reproducing config, unchanged) plus one `variant`
// arm per flip, each differing from the control in EXACTLY one axis. All arms
// share one isolation `group` so compare-mode overlays them and the
// differential oracle reads control-vs-variant. Caller supplies `now`
// (RFC3339). Errors if a flip is invalid or the fan exceeds MaxIsolationAxes.
func IsolationFan(parent *Experiment, flips []Flip, now string) ([]*Experiment, error) {
	if len(flips) == 0 {
		return nil, fmt.Errorf("isolation fan needs at least one axis")
	}
	if len(flips) > MaxIsolationAxes {
		return nil, fmt.Errorf("isolation fan %d exceeds MaxIsolationAxes %d", len(flips), MaxIsolationAxes)
	}
	group := "iso-" + shortHash(parent.ID)

	control := cloneExperimentRecipe(parent)
	control.ID = group + "-control"
	control.Kind = KindIsolation
	control.Arm = ArmControl
	control.Group = group
	control.Parent = parent.ID
	control.CreatedAt = now
	control.Reps = 1
	control.Score = 0

	out := []*Experiment{control}
	for _, f := range flips {
		v := cloneExperimentRecipe(parent)
		if err := applyFlip(v, f); err != nil {
			return nil, err
		}
		v.ID = fmt.Sprintf("%s-%s", group, f.Axis)
		v.Kind = KindIsolation
		v.Arm = ArmVariant
		v.Group = group
		v.Parent = parent.ID
		v.CreatedAt = now
		v.Reps = 1
		v.Why = fmt.Sprintf("isolate_%s", f.Axis)
		// invariant: a variant differs from the control in exactly one axis
		if diff, ok := OneAxisDiff(control, v); !ok {
			return nil, fmt.Errorf("variant %s is not a single-axis flip (diff=%v)", v.ID, diff)
		}
		v.Score = 0
		out = append(out, v)
	}
	return out, nil
}

// BisectRate emits up to two depth+1 `bisect` children that narrow the rate
// axis (§6 bound: ≤2 follow-ups, stop at depth 3). Returns nil when the parent
// is already at the depth cap. Each child sets shape.rate_mbps to one of the
// provided probe rates.
func BisectRate(parent *Experiment, rates []float64, now string) []*Experiment {
	if parent.Depth >= 3 {
		return nil
	}
	if len(rates) > 2 {
		rates = rates[:2]
	}
	var out []*Experiment
	for _, r := range rates {
		c := cloneExperimentRecipe(parent)
		rr := r
		if c.Shape == nil {
			c.Shape = &Shape{}
		}
		c.Shape.RateMbps = &rr
		c.ID = fmt.Sprintf("bisect-%s-r%s", shortHash(parent.ID), strconv.FormatFloat(r, 'f', -1, 64))
		c.Kind = KindBisect
		c.Arm = ""
		c.Group = ""
		c.Depth = parent.Depth + 1
		c.Parent = parent.ID
		c.CreatedAt = now
		c.Reps = 1
		c.Why = fmt.Sprintf("bisect_rate_%.3g", r)
		c.Score = 0
		out = append(out, c)
	}
	return out
}

// OneAxisDiff reports the recipe axis on which a and b differ, and whether they
// differ in EXACTLY one. Used to enforce the OFAT invariant (verification #3)
// and is the gate IsolationFan runs on every variant it produces.
func OneAxisDiff(a, b *Experiment) (axis string, ok bool) {
	var diffs []string
	if a.Platform != b.Platform {
		diffs = append(diffs, "platform")
	}
	if a.Protocol != b.Protocol {
		diffs = append(diffs, "protocol")
	}
	if a.Mode != b.Mode {
		diffs = append(diffs, "mode")
	}
	if a.Content != b.Content {
		diffs = append(diffs, "content")
	}
	if faultKey(a.Fault) != faultKey(b.Fault) {
		diffs = append(diffs, "fault")
	}
	if cmKey(a.ContentManipulation) != cmKey(b.ContentManipulation) {
		diffs = append(diffs, "content_manipulation")
	}
	if shapeKey(a.Shape) != shapeKey(b.Shape) {
		diffs = append(diffs, "shape")
	}
	if transferKey(a.TransferTimeouts) != transferKey(b.TransferTimeouts) {
		diffs = append(diffs, "transfer_timeouts")
	}
	if len(diffs) == 1 {
		return diffs[0], true
	}
	return fmt.Sprintf("%v", diffs), false
}

func faultKey(f *Fault) string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s|%d|%s|%d", f.Type, f.RequestKind, f.URLSubstr, f.Frequency, f.Mode, f.Consecutive)
}

func cmKey(c *ContentManipulation) string {
	if c == nil {
		return ""
	}
	off, over := "nil", "nil"
	if c.LiveOffset != nil {
		off = strconv.FormatFloat(*c.LiveOffset, 'f', -1, 64)
	}
	if c.OverstateBandwidth != nil {
		over = strconv.FormatFloat(*c.OverstateBandwidth, 'f', -1, 64)
	}
	return fmt.Sprintf("%s|%s|%s|%t|%t|%t|%s", off, c.AllowedVariants, c.VariantOrder,
		c.StripCodecs, c.StripAvgBandwidth, c.StripResolution, over)
}

func shapeKey(s *Shape) string {
	if s == nil {
		return ""
	}
	rate := "nil"
	if s.RateMbps != nil {
		rate = strconv.FormatFloat(*s.RateMbps, 'f', -1, 64)
	}
	return fmt.Sprintf("%s|%s|%d|%d", rate, s.Pattern, s.StepSeconds, s.MarginPct)
}

func transferKey(t *TransferTimeouts) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("%d|%d|%t|%t|%t", t.ActiveSeconds, t.IdleSeconds, t.AppliesSegments, t.AppliesManifests, t.AppliesMaster)
}
