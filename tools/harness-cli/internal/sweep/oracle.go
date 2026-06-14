package sweep

import "strings"

// The oracle turns a run's QoE labels into a trichotomy verdict (§3). It does
// NOT decide infra failures — `inconclusive` is set by the runner when there's
// no play_id / the probe errored, never from labels.
//
// A forwarder label is `"<severity>=<event>"` (synthesized events may carry a
// leading `*` on the event, e.g. `error=*qoe_vsf`). The verdict is the worst
// severity present: error/critical → aberration, warning → notable, otherwise
// clean. This mirrors the forwarder's own worstSeverity (analytics/go-forwarder
// /labels.go) so the sweep reuses production semantics rather than inventing
// thresholds.

const (
	sevError    = "error"
	sevCritical = "critical"
	sevWarning  = "warning"
	sevInfo     = "info"
	sevTesting  = "testing" // operator/test KV metadata — never tints a verdict
)

func severityRank(sev string) int {
	switch sev {
	case sevError:
		return 4
	case sevCritical:
		return 3
	case sevWarning:
		return 2
	case sevInfo:
		return 1
	default: // testing, unknown
		return 0
	}
}

// splitLabel returns (severity, event) for a "<sev>=<event>" label. The event
// keeps any leading `*` stripped (synthesized marker) for a clean signature.
func splitLabel(label string) (sev, event string) {
	i := strings.IndexByte(label, '=')
	if i < 0 {
		return "", label
	}
	sev = label[:i]
	event = strings.TrimPrefix(label[i+1:], "*")
	return sev, event
}

// worstLabel returns the highest-severity label and its rank. Ties resolve to
// the first seen (caller passes a stable order).
func worstLabel(labels []string) (label string, rank int) {
	for _, l := range labels {
		sev, _ := splitLabel(l)
		if r := severityRank(sev); r > rank {
			rank, label = r, l
		}
	}
	return label, rank
}

// Classify maps a run's labels to the trichotomy verdict. `testing=` and
// `info=` labels (incl. *qoe_tier_premium) are clean.
func Classify(labels []string) Verdict {
	_, rank := worstLabel(labels)
	switch {
	case rank >= severityRank(sevCritical): // error or critical
		return VerdictAberration
	case rank == severityRank(sevWarning):
		return VerdictNotable
	default:
		return VerdictClean
	}
}

// PrimaryKind returns the event of the worst label — the aberration/notable
// "kind" slug that seeds a finding signature (§4), e.g. `vsf`, `frozen`,
// `downshift_overshoot`. Empty when the run is clean.
func PrimaryKind(labels []string) string {
	label, rank := worstLabel(labels)
	if rank <= severityRank(sevInfo) {
		return ""
	}
	_, event := splitLabel(label)
	return event
}
