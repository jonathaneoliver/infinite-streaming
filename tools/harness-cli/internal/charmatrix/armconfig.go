package charmatrix

// armconfig.go projects a char-matrix Arm into the typed charplan.ArmConfig the
// fleet probe reads (issue #874). This mapping used to live inline in the CLI's
// runMatrixParallel (cmd/harness/char.go); it is extracted here so BOTH the
// char-matrix fleet path AND the sweep isolation-fan fleet path build ArmConfigs
// the same way, instead of duplicating the projection (the issue's "prefer
// reusing over duplicating allocation logic").

import (
	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/charplan"
)

// RunLevel carries the startup/recovery knobs that are the SAME for every arm in
// a run (the CLI resolves them once from CHAR_* env and stamps each arm).
type RunLevel struct {
	StartupFwdBufferS  string
	StartupFwdRelease  string
	PersistentPeakMbps string
	LocalProxy         *bool // nil = resolver default (false, forced off for characterization)
	AutoRecovery       *bool // nil = resolver default (true, the #865 default-ON)
}

// ShapePattern / ShapeStepS / ShapeMargin extract the post-launch bandwidth
// pattern from the arm's proxy.shape. The pattern is NOT applied by the
// config-on-connect bootstrap (it is deferred); the probe arms it after playback
// starts via ApplyPattern. Defaults mirror charplan.Default{StepS,MarginPct}.
func (a *Arm) ShapePattern() string {
	if a.Shape != nil {
		return a.Shape.Pattern
	}
	return ""
}

func (a *Arm) ShapeStepS() int {
	if a.Shape != nil && a.Shape.StepSeconds > 0 {
		return a.Shape.StepSeconds
	}
	return charplan.DefaultStepS
}

func (a *Arm) ShapeMargin() int {
	if a.Shape != nil && a.Shape.MarginPct > 0 {
		return a.Shape.MarginPct
	}
	return charplan.DefaultMarginPct
}

// ToArmConfig projects the arm into the typed handoff the probe reads. playerID
// is the already-bootstrapped config-on-connect session ("" => the probe skips
// that fleet index); content is the resolved clip (the caller applied its
// default); rl carries the run-level startup/recovery knobs; patternMaster marks
// the one arm that arms the shared bandwidth pattern (slaves bind via the proxy
// group). WithDefaults is applied so the returned config is fully resolved.
func (a *Arm) ToArmConfig(playerID, content string, rl RunLevel, patternMaster bool) charplan.ArmConfig {
	ac := charplan.ArmConfig{
		PlayerID:           playerID,
		Platform:           a.Platform,
		Content:            content,
		Segment:            a.Segment,
		LiveOffsetS:        a.ClientLiveOffsetS(),
		Protocol:           a.Protocol,
		Codec:              a.Codec,
		PeakBitrateMbps:    a.PeakBitrateMbps,
		StartsFirstVariant: charplan.ParseBool(a.StartsFirstVariantS()),
		Muted:              charplan.ParseBool(a.MutedS()),
		StartupFwdBufferS:  rl.StartupFwdBufferS,
		StartupFwdRelease:  rl.StartupFwdRelease,
		PersistentPeakMbps: rl.PersistentPeakMbps,
		LocalProxy:         rl.LocalProxy,
		AutoRecovery:       rl.AutoRecovery,
		Pattern:            a.ShapePattern(),
		StepS:              a.ShapeStepS(),
		MarginPct:          a.ShapeMargin(),
		PatternMaster:      patternMaster,
	}
	ac.WithDefaults()
	return ac
}

// PatternMasterIndex picks the arm that should master a grouped bandwidth
// pattern: the first FULL-ladder arm carrying a pattern (a thinned ladder would
// build a pattern that only spans its reduced range), else the first pattern arm,
// else -1 (no pattern in the set — nothing to master). thinnedFallback reports
// whether the chosen master had to fall back to a thinned ladder (the caller may
// warn), so the selection logic stays in one place.
func PatternMasterIndex(arms []*Arm) (master int, thinnedFallback bool) {
	master, firstPatternArm := -1, -1
	for i, a := range arms {
		if a.ShapePattern() == "" {
			continue
		}
		if firstPatternArm < 0 {
			firstPatternArm = i
		}
		e := a.ToExperiment()
		if e.ContentManipulation == nil || e.ContentManipulation.AllowedVariants == "" {
			return i, false // a full-ladder pattern arm — the ideal master
		}
	}
	// No full-ladder pattern arm: fall back to the first pattern arm (if any).
	if firstPatternArm >= 0 {
		return firstPatternArm, true
	}
	return -1, false
}
