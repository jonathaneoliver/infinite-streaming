package charmatrix

// bridge.go is the char-matrix YAML ↔ sweep queue adapter (issue #873). It is the
// inverse of expand.go's ToExperiment: where ToExperiment compiles an authored
// Arm into a queue Experiment (the import direction), this file reconstructs an
// Arm — and a whole runnable Spec — from queue Experiments (the export
// direction), plus the YAML emitter and the silent-drop guard.
//
// Lossless intent: every recipe knob BOTH models carry (Class/Platform/Protocol/
// Segment/Mode/Shape/Fault/ContentManipulation/TransferTimeouts + Group/Role +
// Muted/Reps/DurationS) round-trips. The knobs that exist ONLY on the client Arm
// (is.codec / is.peak_bitrate_mbps / is.live_offset / is.starts_first_variant)
// have no field on sweep.Experiment, so ToExperiment necessarily drops them —
// DroppedClientKnobs surfaces that so `import` can refuse loudly rather than lose
// them silently (the LabelPlay silent-drop trap).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
	"gopkg.in/yaml.v3"
)

// DroppedClientKnobs lists the client-only is.* knobs this arm sets that
// ToExperiment cannot carry onto a sweep.Experiment. Non-empty ⇒ importing the
// arm into the queue would silently lose them; the caller must refuse or warn.
// (Muted is NOT here — ToExperiment carries it onto Experiment.Muted.)
func (a *Arm) DroppedClientKnobs() []string {
	var dropped []string
	if a.Codec != "" {
		dropped = append(dropped, "is.codec")
	}
	if a.AppLiveOffset != nil {
		dropped = append(dropped, "is.live_offset")
	}
	if a.PeakBitrateMbps != 0 {
		dropped = append(dropped, "is.peak_bitrate_mbps")
	}
	if a.StartsFirstVariant != nil {
		dropped = append(dropped, "is.starts_first_variant")
	}
	return dropped
}

// ArmFromExperiment is the export inverse of ToExperiment: it reconstructs the
// flat char-matrix Arm from a queue Experiment. The server-side recipe is
// emitted in its canonical nested form (proxy.shape / proxy.fault /
// proxy.content_manipulation / proxy.transfer_timeouts) — the flat proxy.*
// conveniences are an input-only ergonomic that ToExperiment folds INTO the
// nested block, so a re-import reads them back identically. Client-only knobs are
// not recoverable, but a queue Experiment never had them, so nothing is lost on
// this path (the loss, if any, was guarded at import via DroppedClientKnobs).
//
// Pointers are deep-cloned so the returned Arm shares no state with e.
func ArmFromExperiment(e *sweep.Experiment) *Arm {
	return &Arm{
		ID:                  e.ID,
		Group:               e.Group,
		Role:                string(e.Arm),
		Platform:            e.Platform,
		Content:             e.Content,
		Mode:                e.Mode,
		Class:               string(e.Class),
		DurationS:           e.DurationS,
		Reps:                e.Reps,
		Segment:             e.Segment,
		Protocol:            e.Protocol,
		Muted:               cloneBool(e.Muted),
		Shape:               cloneShape(e.Shape),
		Fault:               cloneFault(e.Fault),
		ContentManipulation: cloneCM(e.ContentManipulation),
		TransferTimeouts:    cloneXfer(e.TransferTimeouts),
	}
}

// SpecFromExperiments assembles a runnable Spec from queue experiments. Members
// that share a non-empty Group and carry control/variant roles become a Group{}
// (so `harness char matrix` re-pairs them on the dashboard, the way an isolation
// fan or A/B batch ran); everything else becomes a flat Arm. Output is
// deterministic: groups and arms are sorted by id. The result is byte-stable
// across calls so it can back a golden round-trip test.
func SpecFromExperiments(name string, exps []*sweep.Experiment) (*Spec, error) {
	if name == "" {
		return nil, fmt.Errorf("spec name is required")
	}
	if len(exps) == 0 {
		return nil, fmt.Errorf("no experiments to export")
	}

	// Partition by Group. A group with a control + ≥1 variant exports as a
	// Group{}; anything else (ungrouped, or a group with no clear control)
	// exports as standalone arms so nothing is silently merged.
	byGroup := map[string][]*sweep.Experiment{}
	var groupOrder []string
	for _, e := range exps {
		if _, seen := byGroup[e.Group]; !seen && e.Group != "" {
			groupOrder = append(groupOrder, e.Group)
		}
		byGroup[e.Group] = append(byGroup[e.Group], e)
	}
	sort.Strings(groupOrder)

	spec := &Spec{Name: name}

	for _, g := range groupOrder {
		members := byGroup[g]
		var control *sweep.Experiment
		var variants []*sweep.Experiment
		for _, e := range members {
			switch e.Arm {
			case sweep.ArmControl:
				if control == nil { // first control wins; extras fall through to flat arms
					control = e
					continue
				}
			case sweep.ArmVariant:
				variants = append(variants, e)
				continue
			}
			// no usable role → treat as a standalone arm
			spec.Arms = append(spec.Arms, ArmFromExperiment(e))
		}
		if control != nil && len(variants) > 0 {
			sort.Slice(variants, func(i, j int) bool { return variants[i].ID < variants[j].ID })
			grp := &Group{ID: g, Control: ArmFromExperiment(control)}
			for _, v := range variants {
				grp.Variants = append(grp.Variants, ArmFromExperiment(v))
			}
			spec.Groups = append(spec.Groups, grp)
		} else {
			// incomplete pairing: emit whatever members as flat arms
			if control != nil {
				spec.Arms = append(spec.Arms, ArmFromExperiment(control))
			}
			for _, v := range variants {
				spec.Arms = append(spec.Arms, ArmFromExperiment(v))
			}
		}
	}

	// Ungrouped experiments → flat arms.
	for _, e := range byGroup[""] {
		spec.Arms = append(spec.Arms, ArmFromExperiment(e))
	}
	sort.Slice(spec.Arms, func(i, j int) bool { return spec.Arms[i].ID < spec.Arms[j].ID })

	return spec, nil
}

// Marshal renders a Spec back to YAML, mirroring Load in reverse: the typed Spec
// is JSON-encoded (so the dotted is.*/proxy.* json tags become the keys), funneled
// through a generic map, then YAML-encoded. yaml.v3 emits map keys in sorted
// order, so the output is deterministic — the "modulo ordering" the round-trip
// allows. The emitted YAML is accepted by Load unchanged.
func Marshal(spec *Spec) ([]byte, error) {
	jsonBytes, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("spec→json: %w", err)
	}
	var generic any
	if err := json.Unmarshal(jsonBytes, &generic); err != nil {
		return nil, fmt.Errorf("json→generic: %w", err)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(generic); err != nil {
		return nil, fmt.Errorf("generic→yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yaml close: %w", err)
	}
	return buf.Bytes(), nil
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
	return &v
}
