package sweep

import "fmt"

// A seedRecipe is one starter experiment shape: a mode (the playback motion)
// plus the class-appropriate overlay. config-class recipes set content/shape/
// transfer knobs; fault-class recipes set a Fault. Content is fixed to
// insane_new for now (§9). The characterization mode drives the bandwidth
// motion itself (pyramid shapes a pyramid, etc.).
type seedRecipe struct {
	family   string // recipe-family slug, for the id
	mode     string
	cm       *ContentManipulation
	shape    *Shape
	transfer *TransferTimeouts
	fault    *Fault
}

func floatPtr(v float64) *float64 { return &v }

// configRecipes — the realistic stream-config + benign-network set (§5, §10).
// No injected errors, no delay/loss, no sub-floor caps: patterns are
// ladder-derived (sustainable by construction) and the content knobs are
// legitimate manifest variations. This is the default depth-first set, built to
// surface ABR decision-quality issues (the over-downshift class) and
// manifest-config robustness.
var configRecipes = []seedRecipe{
	{family: "abr-pyramid", mode: "pyramid", shape: &Shape{Pattern: "pyramid", StepSeconds: 30, MarginPct: 5}},
	{family: "downshift", mode: "downshift_severity"}, // where over-downshift `notable` surfaces
	{family: "strip-avgbw", mode: "steps", cm: &ContentManipulation{StripAvgBandwidth: true}},
	{family: "sparse-ladder", mode: "steps", cm: &ContentManipulation{AllowedVariants: "drop-top-rung"}},
	{family: "live-offset", mode: "startup", cm: &ContentManipulation{LiveOffset: floatPtr(6)}},
	{family: "xfer-timeout", mode: "steps", transfer: &TransferTimeouts{ActiveSeconds: 10, AppliesSegments: true}},
}

// faultRecipes — the explicit-error recovery set (separate class). Each injects
// one HTTP/connection fault the player is expected to recover from; the oracle
// judges against the recovery-expected envelope (a fault within envelope is not
// an aberration). Seeded + run only when `--class fault` is selected.
var faultRecipes = []seedRecipe{
	{family: "seg5xx", mode: "steps", fault: &Fault{Type: "500", RequestKind: "segment", Frequency: 1, Mode: "requests"}},
	{family: "manifest-timeout", mode: "startup", fault: &Fault{Type: "timeout", RequestKind: "manifest", Frequency: 1, Mode: "requests"}},
	{family: "seg-corrupt", mode: "steps", fault: &Fault{Type: "corrupted", RequestKind: "segment", Frequency: 1, Mode: "requests"}},
}

// narrowPlatforms / fullPlatforms — the active sweep is **iOS-sim only** for
// now; --full is reserved for when the other platforms come online.
var (
	narrowPlatforms = []string{"ipad-sim"}
	fullPlatforms   = []string{"ipad-sim", "iphone", "appletv", "androidtv"}
	// Protocol is **HLS only** for now — the probe plays the app's default
	// (HLS) and protocol selection isn't wired in yet. DASH returns when it is.
	seedProtocols = []string{"hls"}
)

// SeedContent is the single content item the sweep runs against for now: the
// H264 build of the "insane new" clip (the catalogue `name`).
const SeedContent = "insane_new_p200_h264"

// recipesFor returns the recipe set for a class (config is the default).
func recipesFor(class Class) []seedRecipe {
	if class == ClassFault {
		return faultRecipes
	}
	return configRecipes
}

// Seed builds the starter backlog for one class. full=false is the narrow
// depth-first set (iPad-sim only); full=true widens across the physical-device
// platforms. `now` is the RFC3339 UTC stamp for created_at (passed in so
// callers control the clock). Scores are stamped with the default weights.
// Seed builds the starter experiment set. platformsOverride (optional) targets a
// specific platform (e.g. "androidtv") instead of the narrow/full defaults.
func Seed(class Class, full bool, now string, platformsOverride ...string) []*Experiment {
	if class == "" {
		class = ClassConfig
	}
	platforms := narrowPlatforms
	if full {
		platforms = fullPlatforms
	}
	if len(platformsOverride) > 0 {
		platforms = platformsOverride
	}
	recipes := recipesFor(class)
	w := DefaultWeights()
	var out []*Experiment
	for _, p := range platforms {
		for _, proto := range seedProtocols {
			for _, r := range recipes {
				e := &Experiment{
					ID:                  fmt.Sprintf("seed-%s-%s-%s-%s-%s", class, p, proto, r.family, r.mode),
					CreatedAt:           now,
					Class:               class,
					Platform:            p,
					LaunchMode:          LaunchModeAppium, // every item is driven by appium (the only mode the probe supports), incl. the physical Android TV
					Protocol:            proto,
					Content:             SeedContent,
					Mode:                r.mode,
					ContentManipulation: cloneCM(r.cm),
					Shape:               cloneShape(r.shape),
					TransferTimeouts:    cloneTransfer(r.transfer),
					Fault:               cloneFault(r.fault),
					Kind:                KindSeed,
					Reps:                1,
					Depth:               0,
					Why:                 "starter_seed",
					WhyText:             fmt.Sprintf("starter %s-class seed: %s recipe (%s) on %s/%s", class, r.family, r.mode, p, proto),
				}
				e.Score = w.Score(e)
				out = append(out, e)
			}
		}
	}
	return out
}

func cloneFault(f *Fault) *Fault {
	if f == nil {
		return nil
	}
	c := *f
	return &c
}

func cloneShape(s *Shape) *Shape {
	if s == nil {
		return nil
	}
	c := *s
	if s.RateMbps != nil {
		c.RateMbps = floatPtr(*s.RateMbps)
	}
	return &c
}

func cloneCM(cm *ContentManipulation) *ContentManipulation {
	if cm == nil {
		return nil
	}
	c := *cm
	if cm.LiveOffset != nil {
		c.LiveOffset = floatPtr(*cm.LiveOffset)
	}
	if cm.OverstateBandwidth != nil {
		c.OverstateBandwidth = floatPtr(*cm.OverstateBandwidth)
	}
	return &c
}

func cloneTransfer(t *TransferTimeouts) *TransferTimeouts {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
