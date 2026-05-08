package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// Phase L: play-scope mutation handlers via snapshot+restore.
//
// v2 distinguishes player-scope (persistent) from play-scope
// (auto-clears when the play ends or rotates). v1's session map is
// single-scope, so play-scope is implemented as snapshot+overlay+
// restore:
//
//   1. PATCH /plays/{play_id} arrives; resolve play_id → player_id
//      via EventSource.PlayerForPlay (which tracks the most recent
//      play_id per player from the network event stream).
//   2. For each touched leaf path, snapshot the player's pre-patch
//      value into `_v2_play_overrides[play_id][<v1-storage-key>]`.
//      The snapshot lives on the session map.
//   3. Apply the patch like player-scope (same translators).
//   4. When the play_id rotates (Phase J detection in
//      EventSource.detectPlayRotation), the snapshot is replayed:
//      every saved (key, original_value) is written back, then the
//      kernel apply re-runs with the rolled-back state. The
//      override entry is deleted.
//
// Per-rule fault sub-resources for plays use the same machinery.
// Conflict detection runs against a per-play FieldRevisions tracker
// (Server.playRevs), distinct from the per-player one.

// resolvePlayForPATCH resolves a v2 play_id to its (player_id,
// v1_session) tuple. Writes a 404 problem and returns false if no
// active play matches.
func (s *Server) resolvePlayForPATCH(w http.ResponseWriter, playID string) (string, map[string]any, bool) {
	if s.events == nil {
		writePlayNotFound(w, playID)
		return "", nil, false
	}
	playerID, ok := s.events.PlayerForPlay(playID)
	if !ok {
		writePlayNotFound(w, playID)
		return "", nil, false
	}
	sess, ok := s.v1.SessionByPlayerID(playerID)
	if !ok {
		writePlayNotFound(w, playID)
		return "", nil, false
	}
	return playerID, sess, true
}

// playRevs returns the per-play FieldRevisions tracker. Lazily
// created; freed when the play rotates.
func (s *Server) playRevs(playID string) *FieldRevisions {
	s.playRevsMu.Lock()
	defer s.playRevsMu.Unlock()
	if s.playRevsMp == nil {
		s.playRevsMp = map[string]*FieldRevisions{}
	}
	fr, ok := s.playRevsMp[playID]
	if !ok {
		fr = NewFieldRevisions()
		s.playRevsMp[playID] = fr
	}
	return fr
}

func (s *Server) dropPlayRevs(playID string) {
	s.playRevsMu.Lock()
	delete(s.playRevsMp, playID)
	s.playRevsMu.Unlock()
}

// snapshotV1FieldsForPaths records the current v1 storage values for
// every v1 key that the supplied v2 leaf paths would write — so the
// restore on rotation can roll them back.
//
// If a key didn't exist pre-patch (the snapshot value is "absent"),
// we record `nil` and the restore re-deletes it. Empty-snapshot
// paths (e.g. paths that don't map to any v1 key) are skipped.
func snapshotV1FieldsForPaths(sess map[string]any, paths []string) map[string]any {
	snapshot := map[string]any{}
	record := func(k string) {
		if _, already := snapshot[k]; already {
			return
		}
		if v, has := sess[k]; has {
			snapshot[k] = v
		} else {
			snapshot[k] = nil
		}
	}
	for _, p := range paths {
		switch {
		case p == "labels", strings.HasPrefix(p, "labels."):
			record("_v2_labels")
		case p == "shape.rate_mbps":
			record("nftables_bandwidth_mbps")
		case p == "shape.delay_ms":
			record("nftables_delay_ms")
		case p == "shape.loss_pct":
			record("nftables_packet_loss")
		case p == "shape.transport_fault", strings.HasPrefix(p, "shape.transport_fault."):
			for _, k := range []string{
				"transport_failure_type", "transport_fault_type",
				"transport_failure_frequency", "transport_consecutive_failures",
				"transport_failure_mode",
			} {
				record(k)
			}
		case p == "shape.pattern", strings.HasPrefix(p, "shape.pattern."):
			record("_v2_shape_pattern")
		case p == "shape":
			for _, k := range []string{
				"_v2_shape_pattern",
				"nftables_bandwidth_mbps", "nftables_delay_ms", "nftables_packet_loss",
				"transport_failure_type", "transport_fault_type",
				"transport_failure_frequency", "transport_consecutive_failures",
				"transport_failure_mode",
			} {
				record(k)
			}
		case p == "fault_rules", strings.HasPrefix(p, "fault_rules."):
			for _, surface := range faultSurfaces {
				for _, suffix := range []string{
					"_failure_type", "_failure_frequency", "_consecutive_failures",
					"_failure_mode", "_failure_units", "_consecutive_units", "_frequency_units",
				} {
					record(surface + suffix)
				}
			}
			record("_v2_fault_rules")
		}
	}
	return snapshot
}

// recordPlayOverrideSnapshot stashes the supplied snapshot under
// `_v2_play_overrides[playID]`. If a snapshot already exists (a
// previous play-scope patch on the same play), the FIRST values
// win — that way successive PATCHes during one play accumulate
// overlays but the rollback returns the player to its pre-play
// state, not its pre-second-PATCH state.
func recordPlayOverrideSnapshot(sess map[string]any, playID string, snapshot map[string]any) {
	if len(snapshot) == 0 {
		return
	}
	overrides, _ := sess["_v2_play_overrides"].(map[string]any)
	if overrides == nil {
		overrides = map[string]any{}
	}
	existing, _ := overrides[playID].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range snapshot {
		if _, already := existing[k]; !already {
			existing[k] = v
		}
	}
	overrides[playID] = existing
	sess["_v2_play_overrides"] = overrides
}

// playRecordFromSession projects the v1 session + EventSource play
// state into a v2 PlayRecord. Used by GET /plays/{id} and PATCH
// responses.
func playRecordFromSession(sess map[string]any, playID string) (oapigen.PlayRecord, bool) {
	if sess == nil {
		return oapigen.PlayRecord{}, false
	}
	playUUID, err := uuid.Parse(playID)
	if err != nil {
		return oapigen.PlayRecord{}, false
	}
	playerUUID, err := uuid.Parse(getString(sess, "player_id"))
	if err != nil {
		return oapigen.PlayRecord{}, false
	}
	rec := oapigen.PlayRecord{
		Id:              playUUID,
		PlayerId:        playerUUID,
		ControlRevision: getString(sess, "control_revision"),
		StartedAt:       time.Now().UTC(), // best-effort; v1 doesn't track per-play start
	}
	if labels, ok := sess["_v2_labels"].(map[string]any); ok && len(labels) > 0 {
		out := oapigen.Labels{}
		for k, v := range labels {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		rec.Labels = &out
	}
	return rec, true
}

// ----- GET /api/v2/plays/{play_id} -----------------------------------------

// GetApiV2PlaysPlayId returns the live play record. Replaces the
// Phase B 404 placeholder.
func (s *Server) GetApiV2PlaysPlayId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId) {
	if s.v1 == nil || s.events == nil {
		writePlayNotFound(w, playId.String())
		return
	}
	playerID, ok := s.events.PlayerForPlay(playId.String())
	if !ok {
		writePlayNotFound(w, playId.String())
		return
	}
	sess, ok := s.v1.SessionByPlayerID(playerID)
	if !ok {
		writePlayNotFound(w, playId.String())
		return
	}
	rec, ok := playRecordFromSession(sess, playId.String())
	if !ok {
		writePlayNotFound(w, playId.String())
		return
	}
	if rec.ControlRevision != "" {
		w.Header().Set("ETag", formatETag(rec.ControlRevision))
	}
	writeJSON(w, http.StatusOK, rec)
}

// ----- PATCH /api/v2/plays/{play_id} ---------------------------------------

// PatchApiV2PlaysPlayId applies a play-scope merge patch with
// snapshot/restore semantics. Supported fields match the player
// scope (labels / shape / fault_rules); unrecognised paths return
// 501. Concurrency is checked against a per-play FieldRevisions
// tracker so two PATCHes on different plays don't contend.
func (s *Server) PatchApiV2PlaysPlayId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, params oapigen.PatchApiV2PlaysPlayIdParams) {
	if s.v1 == nil || s.events == nil {
		writePlayNotFound(w, playId.String())
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required",
			"PATCH /plays requires a strong-tag If-Match header (RFC 7232)",
			nil,
		)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "read body failed", err.Error(), nil)
		return
	}
	patch, paths, perr := DecodePatch(body)
	if perr != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "invalid merge patch body", perr.Error(), nil)
		return
	}
	if unsupported := unsupportedPaths(paths); len(unsupported) > 0 {
		writeProblem(w,
			http.StatusNotImplemented,
			"https://harness/errors/field-translation-pending",
			"v2-to-v1 translation not yet wired for these paths",
			"Phase L supports the same field set as player-scope (labels, shape, fault_rules)",
			map[string]any{"unsupported_paths": unsupported},
		)
		return
	}

	playIDStr := playId.String()
	playerID, _, ok := s.resolvePlayForPATCH(w, playIDStr)
	if !ok {
		return
	}
	fr := s.playRevs(playIDStr)
	if conflicts := fr.Conflicts(ifMatch, paths); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}
	rev := newRevision()
	post, found, mErr := s.v1.MutatePlayer(playerID, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, paths); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		// Snapshot the v1 storage values BEFORE applying the patch.
		// EventSource.detectPlayRotation reads this on rotation and
		// rolls back. Per-play first-write-wins so successive PATCHes
		// during one play don't bury the original.
		snap := snapshotV1FieldsForPaths(sess, paths)
		recordPlayOverrideSnapshot(sess, playIDStr, snap)

		if err := applyPatchToSession(sess, patch); err != nil {
			return err
		}
		sess["control_revision"] = rev
		fr.TouchWith(paths, rev)
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
		writePlayNotFound(w, playIDStr)
		return
	}

	// Drive kernel apply for the touched fields.
	if patternTouched(paths) {
		applyPatternFromSession(s, post, playerID)
	} else if shapeFieldsTouched(paths) {
		_ = s.v1.ApplyShapeToPlayer(playerID)
	}
	if transportFaultTouched(paths) {
		applyTransportFaultFromSession(s, post, playerID)
	}

	rec, ok := playRecordFromSession(post, playIDStr)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "post-patch translation failed", "", nil)
		return
	}
	rec.ControlRevision = rev
	w.Header().Set("ETag", formatETag(rev))
	writeJSON(w, http.StatusOK, rec)
}

// ----- /api/v2/plays/{play_id}/fault_rules{,/{rule_id}} -------------------

// PostApiV2PlaysPlayIdFaultRules appends a play-scope fault rule.
// Snapshots fault state on first write, then translates+applies
// like the player-scope per-rule handler.
func (s *Server) PostApiV2PlaysPlayIdFaultRules(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, params oapigen.PostApiV2PlaysPlayIdFaultRulesParams) {
	if s.v1 == nil || s.events == nil {
		writePlayNotFound(w, playId.String())
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required", "per-rule POST requires a strong-tag If-Match header", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "read body failed", err.Error(), nil)
		return
	}
	var newRule map[string]any
	if jerr := unmarshalRule(body, &newRule); jerr != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "invalid JSON body", jerr.Error(), nil)
		return
	}

	playIDStr := playId.String()
	playerID, _, ok := s.resolvePlayForPATCH(w, playIDStr)
	if !ok {
		return
	}
	fr := s.playRevs(playIDStr)
	if conflicts := fr.Conflicts(ifMatch, []string{"fault_rules"}); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}
	rev := newRevision()
	var written map[string]any
	_, found, mErr := s.v1.MutatePlayer(playerID, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, []string{"fault_rules"}); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		snap := snapshotV1FieldsForPaths(sess, []string{"fault_rules"})
		recordPlayOverrideSnapshot(sess, playIDStr, snap)

		current := faultRulesFromSession(sess)
		if current == nil {
			current = []any{}
		}
		if id, _ := newRule["id"].(string); id == "" {
			newRule["id"] = newRevision()
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
		writePlayMutationError(w, fr, playIDStr, mErr)
		return
	}
	if !found {
		writePlayNotFound(w, playIDStr)
		return
	}
	w.Header().Set("ETag", formatETag(rev))
	if id, _ := written["id"].(string); id != "" {
		w.Header().Set("Location", "/api/v2/plays/"+playIDStr+"/fault_rules/"+id)
	}
	writeJSON(w, http.StatusCreated, written)
}

// PatchApiV2PlaysPlayIdFaultRulesRuleId mutates one play-scope rule.
func (s *Server) PatchApiV2PlaysPlayIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, ruleId oapigen.RuleId, params oapigen.PatchApiV2PlaysPlayIdFaultRulesRuleIdParams) {
	if s.v1 == nil || s.events == nil {
		writePlayNotFound(w, playId.String())
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required", "per-rule PATCH requires a strong-tag If-Match header", nil)
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
	playIDStr := playId.String()
	rulePath := "fault_rules." + string(ruleId)

	playerID, _, ok := s.resolvePlayForPATCH(w, playIDStr)
	if !ok {
		return
	}
	fr := s.playRevs(playIDStr)
	if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}
	rev := newRevision()
	var written map[string]any
	_, found, mErr := s.v1.MutatePlayer(playerID, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		snap := snapshotV1FieldsForPaths(sess, []string{rulePath})
		recordPlayOverrideSnapshot(sess, playIDStr, snap)

		current := faultRulesFromSession(sess)
		idx := findFaultRuleIndex(current, string(ruleId))
		if idx < 0 {
			return errFaultRuleNotFound
		}
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
			writeFaultRuleNotFound(w, playerID, string(ruleId))
			return
		}
		writePlayMutationError(w, fr, playIDStr, mErr)
		return
	}
	if !found {
		writePlayNotFound(w, playIDStr)
		return
	}
	w.Header().Set("ETag", formatETag(rev))
	writeJSON(w, http.StatusOK, written)
}

// DeleteApiV2PlaysPlayIdFaultRulesRuleId removes one play-scope rule.
func (s *Server) DeleteApiV2PlaysPlayIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, ruleId oapigen.RuleId, params oapigen.DeleteApiV2PlaysPlayIdFaultRulesRuleIdParams) {
	if s.v1 == nil || s.events == nil {
		writePlayNotFound(w, playId.String())
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required", "per-rule DELETE requires a strong-tag If-Match header", nil)
		return
	}
	playIDStr := playId.String()
	rulePath := "fault_rules." + string(ruleId)
	playerID, _, ok := s.resolvePlayForPATCH(w, playIDStr)
	if !ok {
		return
	}
	fr := s.playRevs(playIDStr)
	if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}
	rev := newRevision()
	_, found, mErr := s.v1.MutatePlayer(playerID, func(sess map[string]any) error {
		if conflicts := fr.Conflicts(ifMatch, []string{rulePath}); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		snap := snapshotV1FieldsForPaths(sess, []string{rulePath})
		recordPlayOverrideSnapshot(sess, playIDStr, snap)

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
			writeFaultRuleNotFound(w, playerID, string(ruleId))
			return
		}
		writePlayMutationError(w, fr, playIDStr, mErr)
		return
	}
	if !found {
		writePlayNotFound(w, playIDStr)
		return
	}
	w.Header().Set("ETag", formatETag(rev))
	w.WriteHeader(http.StatusNoContent)
}

// ----- helpers -------------------------------------------------------------

func writePlayNotFound(w http.ResponseWriter, playID string) {
	writeProblem(w, http.StatusNotFound,
		"https://harness/errors/play-not-found",
		"play not found",
		"no live play with that id is currently active; for archived plays query /analytics/api/v2/plays/{play_id}",
		map[string]any{"play_id": playID},
	)
}

func writePlayMutationError(w http.ResponseWriter, fr *FieldRevisions, playID string, err error) {
	var ce conflictErr
	if errors.As(err, &ce) {
		writePreconditionFailed(w, fr.Top(), ce.paths)
		return
	}
	var ufre *unsupportedFaultRuleError
	if errors.As(err, &ufre) {
		writeProblem(w, http.StatusNotImplemented,
			"https://harness/errors/fault-rule-not-supported",
			"fault_rule cannot be translated to v1", ufre.Error(),
			map[string]any{"rule_id": ufre.RuleID, "reason": ufre.Reason})
		return
	}
	writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "play mutation failed", err.Error(), nil)
}

// unmarshalRule unmarshals a JSON body into a single FaultRule-shaped
// map. Mirrors the player-side handler's body parsing.
func unmarshalRule(body []byte, out *map[string]any) error {
	if len(body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(body, out)
}
