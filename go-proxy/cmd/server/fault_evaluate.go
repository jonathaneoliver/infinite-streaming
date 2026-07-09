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
	return &FailureHandler{
		failureType:      normalizeRequestFailureType(mf.Type),
		consecutiveUnits: consecutiveUnits,
		frequencyUnits:   frequencyUnits,
		failureFrequency: mf.Frequency,
		consecutive:      consecutive,
		failureAt:        st.failureAt,
		failureRecoverAt: st.failureRecoverAt,
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

// faultRulesForSession returns the ordered fault_rules to evaluate. It prefers
// the v2 array (`_v2_fault_rules`, written by every v2 PATCH). When absent it
// synthesises rules from the v1 surface fields so callers that still write v1
// directly (the server_behavior suite) keep working under the native engine —
// the reverse bridge that lives until epic step 3 repoints those writers.
func faultRulesForSession(session SessionData) []any {
	if raw, ok := session["_v2_fault_rules"].([]any); ok && len(raw) > 0 {
		return raw
	}
	return synthRulesFromV1(session)
}

// synthRulesFromV1 builds an ordered fault_rules array from the v1 surface
// fields, for sessions configured through the legacy v1 write path (the
// server_behavior suite PATCHes `<surface>_failure_type` etc. directly, never
// `fault_rules`). Order matches v1 evaluation precedence: the `all` override
// first (it short-circuited every request in v1's HandleRequest), then
// segment / manifest / master_manifest.
//
// This is the reverse bridge — temporary. Epic step 3 repoints the v1 writers
// onto v2 fault_rules, after which this and the v1 surface fields are deleted.
// The url-scope mapping (`*_failure_urls` → url_match substring) is a best-
// effort approximation of v1's shouldApplyFailure substring branch; exact
// variant/base scoring is not reproduced (and isn't needed once step 3 lands).
func synthRulesFromV1(session SessionData) []any {
	var rules []any
	if r := synthSurfaceRule(session, "all", nil); r != nil {
		rules = append(rules, r)
	}
	if r := synthSurfaceRule(session, "segment", []any{"segment", "partial"}); r != nil {
		rules = append(rules, r)
	}
	if r := synthSurfaceRule(session, "manifest", []any{"manifest"}); r != nil {
		rules = append(rules, r)
	}
	if r := synthSurfaceRule(session, "master_manifest", []any{"master_manifest"}); r != nil {
		rules = append(rules, r)
	}
	return rules
}

// synthSurfaceRule builds one fault_rule from a v1 surface's fields, or nil when
// that surface has no active fault. `kinds` is the request_kind filter (nil for
// the `all` override, which matches every kind).
func synthSurfaceRule(session SessionData, surface string, kinds []any) map[string]any {
	ft := normalizeRequestFailureType(getString(session, surface+"_failure_type"))
	if ft == "" || ft == "none" {
		return nil
	}
	filter := map[string]any{}
	if kinds != nil {
		filter["request_kind"] = kinds
	}
	if urls := getStringSlice(session, surface+"_failure_urls"); len(urls) > 0 && !containsAllToken(urls) {
		patterns := make([]any, 0, len(urls))
		for _, u := range urls {
			patterns = append(patterns, u)
		}
		filter["url_match"] = map[string]any{"mode": "substring", "patterns": patterns}
	}
	// Derive the v2 mode from the ACTUAL v1 cadence units — NOT from
	// `_failure_mode`, which the v1 write API (server_behavior) never sets. The
	// unit resolution here mirrors NewFailureHandler exactly (fall back to
	// `_failure_units`, then the visible-default requests/seconds), so a
	// count-mode config (units=requests/requests) round-trips as `requests`
	// rather than defaulting to time-based `failures_per_seconds`.
	mode := modeFromUnits(resolveV1Units(session, surface))
	rule := map[string]any{
		"id":          "v1:" + surface,
		"type":        ft,
		"frequency":   getInt(session, surface+"_failure_frequency"),
		"consecutive": getInt(session, surface+"_consecutive_failures"),
		"mode":        mode,
	}
	if len(filter) > 0 {
		rule["filter"] = filter
	}
	return rule
}

// resolveV1Units reproduces NewFailureHandler's (consecutiveUnits,
// frequencyUnits) resolution for a v1 surface: prefer the explicit per-axis
// units, fall back to the shared `_failure_units`, then the dashboard's visible
// default (requests on-window, seconds gap).
func resolveV1Units(session SessionData, surface string) (consecUnits, freqUnits string) {
	failureUnits := getString(session, surface+"_failure_units")
	consecUnits = getString(session, surface+"_consecutive_units")
	freqUnits = getString(session, surface+"_frequency_units")
	if consecUnits == "" {
		consecUnits = failureUnits
	}
	if freqUnits == "" {
		freqUnits = failureUnits
	}
	if consecUnits == "" {
		consecUnits = "requests"
	}
	if freqUnits == "" {
		freqUnits = "seconds"
	}
	return consecUnits, freqUnits
}

// modeFromUnits inverts unitsForMode: it maps a resolved (consecutiveUnits,
// frequencyUnits) pair back to the v2 FaultRule.mode that reproduces it. The
// three clean combos translate_faults emits are covered; the mixed
// (seconds, requests) case isn't representable as a single v2 mode and falls to
// failures_per_seconds (no real config produces it — the v1 API and the
// dashboard only write the three clean combos).
func modeFromUnits(consecUnits, freqUnits string) string {
	switch {
	case consecUnits == "requests" && freqUnits == "requests":
		return "requests"
	case consecUnits == "seconds" && freqUnits == "seconds":
		return "seconds"
	default:
		return "failures_per_seconds"
	}
}

// containsAllToken reports whether a v1 *_failure_urls list carries the "All"
// sentinel (case-insensitive), which means "match every request" — the
// synthesised rule then gets no url filter.
func containsAllToken(urls []string) bool {
	for _, u := range urls {
		if u == "All" || u == "all" {
			return true
		}
	}
	return false
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
