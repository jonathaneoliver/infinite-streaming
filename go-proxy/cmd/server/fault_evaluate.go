package main

import "time"

// ruleCadenceState is one fault_rule's scheduler state, persisted per-rule on
// the session under `_faultrule_state[ruleID]`. It mirrors v1's per-surface
// `<prefix>_failure_at` / `_recover_at` / count fields — keyed by rule id
// instead of surface so each rule in the ordered array cycles independently
// (#919 step 2). failureAt / failureRecoverAt are interface-typed because, like
// v1, they hold either an int (count-space schedule) or an RFC3339-ish string
// (time-space schedule) depending on the rule's mode.
type ruleCadenceState struct {
	count            int
	failureAt        any
	failureRecoverAt any
	done             bool // a one-shot rule (frequency 0) that has fired and permanently reverted to none
}

// evaluateFaultRules is the native v2 fault decision path: classify → first-
// match over fault_rules → run the reused cadence engine with per-rule state.
// Returns the failure type to apply ("none" when no fault fires). This is the
// #919 replacement for the v1 surface-prefix RequestHandler/FailureHandler
// dispatch — a rule's FaultFilter (request_kind / variant / url_match) decides
// what it applies to, so audio/init/variant scope work (#917/#918) with no
// translation to v1's 6 fixed surfaces.
func evaluateFaultRules(session SessionData, rc RequestClass, now time.Time) string {
	rules := faultRulesForSession(session)
	if len(rules) == 0 {
		return "none"
	}
	mf, ok := matchFaultRule(rules, rc)
	if !ok {
		return "none"
	}
	if t := normalizeRequestFailureType(mf.Type); t == "" || t == "none" {
		return "none"
	}

	st := getRuleState(session, mf.RuleID)
	if st.done {
		return "none"
	}
	st.count++

	fh := failureHandlerFromMatch(mf, st)
	ft := fh.HandleFailure(st.count, now)
	st.failureAt = fh.failureAt
	st.failureRecoverAt = fh.failureRecoverAt
	if fh.resetFailureType != nil {
		// One-shot (frequency 0) completed its on-window → this rule
		// permanently reverts to none, matching v1's `_reset_failure_type`
		// self-clear (which set `<surface>_failure_type=none`).
		st.done = true
	}
	setRuleState(session, mf.RuleID, st)
	return ft
}

// failureHandlerFromMatch builds a cadence engine for a matched rule from its
// (type, frequency, mode, consecutive) plus the rule's persisted schedule
// state. Reuses the exact v1 FailureHandler math; only the source of the
// parameters/state differs (per-rule vs per-surface).
func failureHandlerFromMatch(mf MatchedFault, st *ruleCadenceState) *FailureHandler {
	consecutiveUnits, frequencyUnits := unitsForMode(mf.Mode)
	consecutive := mf.Consecutive
	if consecutive <= 0 {
		consecutive = 1 // schema default; also what makes the cadence engine act
	}
	// consecutive=0 AND frequency=0 → continuous: fault every matching request
	// until the rule is cleared, instead of the one-shot single failure the
	// cadence math would otherwise produce. This is the intuitive meaning of
	// the UI's default (0,0) — "on until I cancel it".
	continuous := mf.Consecutive == 0 && mf.Frequency == 0
	return &FailureHandler{
		failureType:      normalizeRequestFailureType(mf.Type),
		consecutiveUnits: consecutiveUnits,
		frequencyUnits:   frequencyUnits,
		failureFrequency: mf.Frequency,
		consecutive:      consecutive,
		failureAt:        st.failureAt,
		failureRecoverAt: st.failureRecoverAt,
		continuous:       continuous,
	}
}

// unitsForMode maps the v2 FaultRule.mode enum to v1's (consecutiveUnits,
// frequencyUnits) pair. Must stay identical to translate_faults.go's mapping
// so the native path and the (still-live) v1 path schedule faults the same way.
func unitsForMode(mode string) (consecutiveUnits, frequencyUnits string) {
	switch mode {
	case "requests":
		return "requests", "requests"
	case "seconds":
		return "seconds", "seconds"
	default: // "failures_per_seconds" (v2 default) and anything unrecognised
		return "requests", "seconds"
	}
}

// faultRulesForSession returns the ordered v2 fault_rules to evaluate, written
// by every v2 PATCH onto `_v2_fault_rules`. This is now the sole fault input —
// the v1 surface model and its reverse bridge were retired in #925.
func faultRulesForSession(session SessionData) []any {
	raw, _ := session["_v2_fault_rules"].([]any)
	return raw
}

// getRuleState reads a rule's persisted cadence state, or a zero state when the
// rule has not fired yet.
func getRuleState(session SessionData, ruleID string) *ruleCadenceState {
	st := &ruleCadenceState{}
	all, _ := session["_faultrule_state"].(map[string]any)
	if all == nil {
		return st
	}
	raw, _ := all[ruleStateKey(ruleID)].(map[string]any)
	if raw == nil {
		return st
	}
	st.count = intFromInterface(raw["count"])
	st.failureAt = raw["failure_at"]
	st.failureRecoverAt = raw["failure_recover_at"]
	if d, ok := raw["done"].(bool); ok {
		st.done = d
	}
	return st
}

// setRuleState persists a rule's cadence state back onto the session.
func setRuleState(session SessionData, ruleID string, st *ruleCadenceState) {
	all, _ := session["_faultrule_state"].(map[string]any)
	if all == nil {
		all = map[string]any{}
		session["_faultrule_state"] = all
	}
	all[ruleStateKey(ruleID)] = map[string]any{
		"count":              st.count,
		"failure_at":         st.failureAt,
		"failure_recover_at": st.failureRecoverAt,
		"done":               st.done,
	}
}

// ruleStateKey keys per-rule state. Rules normally carry a server-generated
// UUIDv7 id; an id-less rule falls back to a fixed key (its state is shared,
// which is acceptable because id-less rules can't be individually targeted
// anyway).
func ruleStateKey(ruleID string) string {
	if ruleID == "" {
		return "_anon"
	}
	return ruleID
}
