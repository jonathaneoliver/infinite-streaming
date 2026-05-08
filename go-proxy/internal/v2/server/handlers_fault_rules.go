package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// Per-rule fault sub-resource handlers (player scope only — play-scope
// handlers stay 501 until plays are first-class).
//
// Path: /api/v2/players/{player_id}/fault_rules/{rule_id}
//
// Each per-rule mutation is a separate "field" for concurrency
// purposes: editing rule `r1` doesn't contend with editing rule `r2`,
// but a whole-array PATCH on `fault_rules` contends with both
// (DESIGN.md § Per-rule fault sub-resources). The FieldRevisions
// `Conflicts` method already handles this hierarchically — we just
// enumerate the right paths per call.

// PostApiV2PlayersPlayerIdFaultRules appends a rule to the player's
// fault_rules array.
func (s *Server) PostApiV2PlayersPlayerIdFaultRules(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, params oapigen.PostApiV2PlayersPlayerIdFaultRulesParams) {
	if s.v1 == nil {
		notImplemented(w, "PostApiV2PlayersPlayerIdFaultRules")
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required",
			"per-rule POST requires a strong-tag If-Match header",
			nil,
		)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "read body failed", err.Error(), nil)
		return
	}
	var newRule map[string]any
	if err := json.Unmarshal(body, &newRule); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "invalid JSON body", err.Error(), nil)
		return
	}

	pidStr := playerId.String()
	if _, exists := s.v1.SessionByPlayerID(pidStr); !exists {
		writePlayerNotFound(w, pidStr)
		return
	}
	fr := s.fieldRevs(pidStr)

	// Conflict scope: any path under `fault_rules` (whole-array writes
	// AND any sibling per-rule mutation invalidate this append).
	if conflicts := fr.Conflicts(ifMatch, []string{"fault_rules"}); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}

	rev := newRevision()
	var written map[string]any
	_, found, mErr := s.v1.MutatePlayer(pidStr, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, []string{"fault_rules"}); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		current := faultRulesFromSession(sess)
		if current == nil {
			current = []any{}
		}
		// Server-fill rule id if omitted.
		if id, _ := newRule["id"].(string); id == "" {
			newRule["id"] = newRevision() // good-enough monotonic id
		}
		current = append(current, newRule)
		if err := translateFaultRules(sess, current); err != nil {
			return err
		}
		sess["control_revision"] = rev
		fr.TouchWith([]string{"fault_rules"}, rev)
		written = newRule
		return nil
	})
	if mErr != nil {
		var ce conflictErr
		if errors.As(mErr, &ce) {
			writePreconditionFailed(w, fr.Top(), ce.paths)
			return
		}
		var ufre *unsupportedFaultRuleError
		if errors.As(mErr, &ufre) {
			writeProblem(w, http.StatusNotImplemented,
				"https://harness/errors/fault-rule-not-supported",
				"fault_rule cannot be translated to v1", ufre.Error(),
				map[string]any{"rule_id": ufre.RuleID, "reason": ufre.Reason},
			)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "mutation failed", mErr.Error(), nil)
		return
	}
	if !found {
		writePlayerNotFound(w, pidStr)
		return
	}
	w.Header().Set("ETag", formatETag(rev))
	if id, _ := written["id"].(string); id != "" {
		w.Header().Set("Location", "/api/v2/players/"+pidStr+"/fault_rules/"+id)
	}
	writeJSON(w, http.StatusCreated, written)
}

// PatchApiV2PlayersPlayerIdFaultRulesRuleId mutates one rule by id.
func (s *Server) PatchApiV2PlayersPlayerIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, ruleId oapigen.RuleId, params oapigen.PatchApiV2PlayersPlayerIdFaultRulesRuleIdParams) {
	if s.v1 == nil {
		notImplemented(w, "PatchApiV2PlayersPlayerIdFaultRulesRuleId")
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required",
			"per-rule PATCH requires a strong-tag If-Match header",
			nil,
		)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "read body failed", err.Error(), nil)
		return
	}
	patch, _, perr := DecodePatch(body)
	if perr != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "invalid merge patch body", perr.Error(), nil)
		return
	}
	pidStr := playerId.String()
	rulePath := "fault_rules." + string(ruleId)

	if _, exists := s.v1.SessionByPlayerID(pidStr); !exists {
		writePlayerNotFound(w, pidStr)
		return
	}
	fr := s.fieldRevs(pidStr)
	if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}
	rev := newRevision()
	var written map[string]any
	_, found, mErr := s.v1.MutatePlayer(pidStr, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		current := faultRulesFromSession(sess)
		idx := findFaultRuleIndex(current, string(ruleId))
		if idx < 0 {
			return errFaultRuleNotFound
		}
		// Apply Merge Patch onto the existing rule object.
		existing, _ := current[idx].(map[string]any)
		if existing == nil {
			existing = map[string]any{}
		}
		merged := ApplyMergePatch(existing, patch)
		current[idx] = merged
		if err := translateFaultRules(sess, current); err != nil {
			return err
		}
		sess["control_revision"] = rev
		fr.TouchWith([]string{rulePath}, rev)
		written = merged
		return nil
	})
	if mErr != nil {
		if errors.Is(mErr, errFaultRuleNotFound) {
			writeFaultRuleNotFound(w, pidStr, string(ruleId))
			return
		}
		var ce conflictErr
		if errors.As(mErr, &ce) {
			writePreconditionFailed(w, fr.Top(), ce.paths)
			return
		}
		var ufre *unsupportedFaultRuleError
		if errors.As(mErr, &ufre) {
			writeProblem(w, http.StatusNotImplemented,
				"https://harness/errors/fault-rule-not-supported",
				"fault_rule cannot be translated to v1", ufre.Error(),
				map[string]any{"rule_id": ufre.RuleID, "reason": ufre.Reason},
			)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "mutation failed", mErr.Error(), nil)
		return
	}
	if !found {
		writePlayerNotFound(w, pidStr)
		return
	}
	w.Header().Set("ETag", formatETag(rev))
	writeJSON(w, http.StatusOK, written)
}

// DeleteApiV2PlayersPlayerIdFaultRulesRuleId removes one rule.
func (s *Server) DeleteApiV2PlayersPlayerIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, ruleId oapigen.RuleId, params oapigen.DeleteApiV2PlayersPlayerIdFaultRulesRuleIdParams) {
	if s.v1 == nil {
		notImplemented(w, "DeleteApiV2PlayersPlayerIdFaultRulesRuleId")
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required",
			"per-rule DELETE requires a strong-tag If-Match header",
			nil,
		)
		return
	}
	pidStr := playerId.String()
	rulePath := "fault_rules." + string(ruleId)
	if _, exists := s.v1.SessionByPlayerID(pidStr); !exists {
		writePlayerNotFound(w, pidStr)
		return
	}
	fr := s.fieldRevs(pidStr)
	if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}
	rev := newRevision()
	_, found, mErr := s.v1.MutatePlayer(pidStr, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		current := faultRulesFromSession(sess)
		idx := findFaultRuleIndex(current, string(ruleId))
		if idx < 0 {
			return errFaultRuleNotFound
		}
		current = append(current[:idx], current[idx+1:]...)
		if err := translateFaultRules(sess, current); err != nil {
			return err
		}
		sess["control_revision"] = rev
		fr.TouchWith([]string{rulePath}, rev)
		return nil
	})
	if mErr != nil {
		if errors.Is(mErr, errFaultRuleNotFound) {
			writeFaultRuleNotFound(w, pidStr, string(ruleId))
			return
		}
		var ce conflictErr
		if errors.As(mErr, &ce) {
			writePreconditionFailed(w, fr.Top(), ce.paths)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "mutation failed", mErr.Error(), nil)
		return
	}
	if !found {
		writePlayerNotFound(w, pidStr)
		return
	}
	w.Header().Set("ETag", formatETag(rev))
	w.WriteHeader(http.StatusNoContent)
}

var errFaultRuleNotFound = errors.New("fault rule not found")

func writeFaultRuleNotFound(w http.ResponseWriter, playerID, ruleID string) {
	writeProblem(w, http.StatusNotFound,
		"https://harness/errors/fault-rule-not-found",
		"fault rule not found",
		"no rule with that id is currently configured for this player",
		map[string]any{"player_id": playerID, "rule_id": ruleID},
	)
}
