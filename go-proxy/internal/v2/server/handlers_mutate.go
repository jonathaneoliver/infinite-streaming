package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// Phase D mutation handlers. The mutation pipeline is uniform:
//
//  1. Parse If-Match (required for PATCH; absent for POST/DELETE).
//  2. Decode Merge Patch body (PATCH only) and enumerate leaf paths.
//  3. Lock the v1 store via V1Adapter.MutatePlayer.
//  4. Inside the lock, run conflict detection against the player's
//     FieldRevisions. If conflicts → 412 with conflicts list. Else
//     apply the patch + bump touched-path revisions.
//  5. Return the post-mutation player record + new ETag.
//
// Phase D scope: PATCH /players/{id} for `labels` only — the Merge
// Patch pipeline is field-agnostic, but `shape` and `fault_rules` need
// dedicated v1-storage translators that don't exist yet. PATCHes
// touching those fields are accepted into the Merge Patch flow but
// rejected with 501-style problem detail before write, so the
// concurrency model is uniformly proven.
//
// POST /players upsert is not yet implemented — adapter returns
// errSyntheticPlayerNotImplemented and we surface 501.
//
// Per-rule fault sub-resources stay 501 in handlers_stub.go until the
// fault_rules translator lands.

// ----- Players (mutations) -------------------------------------------------

// PostApiV2Players: synthetic-player upsert.
//
//   - 201 if newly created
//   - 200 if a player with the same player_id already exists with
//     v2-equivalent labels
//   - 400 on malformed body / invalid player_id
//   - 409 if the player_id exists with a different body
//   - 503 if the proxy's session limit is reached
func (s *Server) PostApiV2Players(w http.ResponseWriter, r *http.Request) {
	if s.v1 == nil {
		notImplemented(w, "PostApiV2Players")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "read body failed", err.Error(), nil)
		return
	}
	payload := map[string]any{}
	if len(strings.TrimSpace(string(body))) > 0 {
		patch, _, perr := DecodePatch(body)
		if perr != nil {
			writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "invalid request body", perr.Error(), nil)
			return
		}
		payload = patch
	}
	pid, _ := payload["player_id"].(string)

	status, record, cerr := s.v1.CreateSyntheticPlayer(pid, payload)
	if cerr != nil {
		writePlayerCreateError(w, cerr)
		return
	}
	if status == 0 {
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "adapter returned status=0 with no error", "", nil)
		return
	}
	if status == http.StatusConflict {
		writeProblem(w, http.StatusConflict,
			"https://harness/errors/player-exists-different-settings",
			"player exists with different settings",
			"a player with that player_id is already connected; mutate it via PATCH instead",
			map[string]any{"existing_player_id": pid, "hint": "use PATCH"},
		)
		return
	}
	rec, ok := playerFromSession(record)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "post-create translation failed", "", nil)
		return
	}
	if rec.ControlRevision != "" {
		w.Header().Set("ETag", formatETag(rec.ControlRevision))
	}
	w.Header().Set("Location", "/api/v2/players/"+rec.Id.String())
	writeJSON(w, status, rec)
}

// writePlayerCreateError maps adapter sentinel errors into RFC 7807
// problem responses. Lives in handlers_mutate.go because the adapter
// errors are package-private to cmd/server (we only see them via the
// V1Adapter return value, but the messages are stable enough to
// pattern-match on).
func writePlayerCreateError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "player_id must be a UUID"):
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "player_id must be a UUID", msg, nil)
	case strings.Contains(msg, "session limit reached"):
		writeProblem(w, http.StatusServiceUnavailable, "https://harness/errors/session-limit", "session limit reached", msg, nil)
	case strings.Contains(msg, "v1 *App backing"):
		// Test-mode adapter without a real *App. Surface as 501 so
		// callers know the feature isn't wired.
		writeProblem(w, http.StatusNotImplemented, "https://harness/errors/not-implemented", "synthetic player creation requires a v1 backing", msg, nil)
	default:
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "create failed", msg, nil)
	}
}

// DeleteApiV2Players clears every active player.
func (s *Server) DeleteApiV2Players(w http.ResponseWriter, r *http.Request) {
	if s.v1 == nil {
		notImplemented(w, "DeleteApiV2Players")
		return
	}
	s.v1.ClearAllPlayers()
	s.revsMu.Lock()
	s.revs = map[string]*FieldRevisions{}
	s.revsMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// DeleteApiV2PlayersPlayerId drops one player.
func (s *Server) DeleteApiV2PlayersPlayerId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId) {
	if s.v1 == nil {
		notImplemented(w, "DeleteApiV2PlayersPlayerId")
		return
	}
	if !s.v1.DeletePlayer(playerId.String()) {
		writePlayerNotFound(w, playerId.String())
		return
	}
	s.dropFieldRevs(playerId.String())
	w.WriteHeader(http.StatusNoContent)
}

// PatchApiV2PlayersPlayerId applies a JSON Merge Patch with field-level
// optimistic concurrency. Phase D supports `labels` end-to-end; other
// fields fail before write with 501.
func (s *Server) PatchApiV2PlayersPlayerId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, params oapigen.PatchApiV2PlayersPlayerIdParams) {
	if s.v1 == nil {
		notImplemented(w, "PatchApiV2PlayersPlayerId")
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required",
			"PATCH requires a strong-tag If-Match header (RFC 7232) — echo the most recent ETag verbatim",
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
			"Phase D supports `labels`; `shape` and `fault_rules` need dedicated v1-storage translators (Phase D follow-up). The Merge Patch and field-level concurrency pipeline is in place — only the persistence-side mapping is missing.",
			map[string]any{"unsupported_paths": unsupported},
		)
		return
	}

	pidStr := playerId.String()

	// Pre-check whether the player exists at all. Avoids creating a
	// per-player FieldRevisions tracker for a UUID that doesn't
	// resolve, which would otherwise leak under PATCH-floods.
	if _, exists := s.v1.SessionByPlayerID(pidStr); !exists {
		writePlayerNotFound(w, pidStr)
		return
	}
	fr := s.fieldRevs(pidStr)

	// First-pass conflict check (outside the v1 write lock — cheap
	// rejection for the common case).
	if conflicts := fr.Conflicts(ifMatch, paths); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}

	// Pre-allocate the new revision so we can stamp v1's
	// control_revision and v2's FieldRevisions atomically inside the
	// single MutatePlayer fn — closes the TOCTOU window between the
	// patch publish and the FieldRevisions Touch.
	rev := newRevision()

	// Capture the player's group_id under the same MutatePlayer call
	// so we can fan-out broadcast under one consistent view. groupID
	// is empty when the player isn't in any group — broadcast becomes
	// a no-op.
	var groupID string
	post, found, mErr := s.v1.MutatePlayer(pidStr, func(s map[string]any) error {
		// Re-check under sessionsMu. Another v2 PATCH that won the
		// outer race would have updated FieldRevisions before
		// publishing — the second check sees that.
		if conflicts := fr.Conflicts(ifMatch, paths); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		if err := applyPatchToSession(s, patch); err != nil {
			return err
		}
		// Stamp control_revision (RFC3339Nano) + FieldRevisions
		// inside the same lock so SSE subscribers never see the
		// post-patch payload paired with the prior revision.
		s["control_revision"] = rev
		fr.TouchWith(paths, rev)
		groupID = getString(s, "group_id")
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
				"fault_rule cannot be translated to the v1 surface model",
				ufre.Error(),
				map[string]any{"rule_id": ufre.RuleID, "reason": ufre.Reason},
			)
			return
		}
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "mutation failed", mErr.Error(), nil)
		return
	}
	if !found {
		// Race: the player vanished between the existence pre-check
		// and the lock acquisition. Treat as 404.
		writePlayerNotFound(w, pidStr)
		return
	}

	// Auto-broadcast to other group members (DESIGN.md § Player groups
	// — auto-broadcast preserved from v1). Each member gets the same
	// new control_revision and the same patch applied; their per-
	// player FieldRevisions tracker is bumped to the same `rev` so a
	// concurrent PATCHer reading from any member sees the latest
	// revision uniformly.
	var broadcastTouched []string
	if groupID != "" {
		touched, bErr := s.v1.BroadcastPatch(groupID, pidStr, rev, func(member map[string]any) error {
			return applyPatchToSession(member, patch)
		})
		if bErr != nil {
			// Broadcast failure on a sibling member shouldn't 500
			// the whole PATCH — the originating member already
			// landed cleanly. Log via a problem extension instead.
			writeProblem(w, http.StatusInternalServerError, "https://harness/errors/broadcast-failed", "primary patch succeeded but group broadcast failed", bErr.Error(), map[string]any{"primary_player_id": pidStr, "group_id": groupID})
			return
		}
		for _, p := range touched {
			s.fieldRevs(p).TouchWith(paths, rev)
		}
		broadcastTouched = touched
	}

	// Drive the kernel-side state for shape / transport_fault changes.
	// These run after the SessionData write has been published so v1
	// SSE consumers see the new field values before the kernel apply
	// fires its own log lines.
	// shape.pattern takes precedence over shape.rate_mbps when both
	// are set — v1's pattern loop owns the rate while enabled. Fire
	// pattern apply first so a "pattern + rate" body lands the
	// pattern path (not the static-rate path).
	if patternTouched(paths) {
		applyPatternFromSession(s, post, pidStr)
		for _, p := range broadcastTouched {
			if memberSess, ok := s.v1.SessionByPlayerID(p); ok {
				applyPatternFromSession(s, memberSess, p)
			}
		}
	} else if shapeFieldsTouched(paths) {
		_ = s.v1.ApplyShapeToPlayer(pidStr)
		for _, p := range broadcastTouched {
			_ = s.v1.ApplyShapeToPlayer(p)
		}
	}
	if transportFaultTouched(paths) {
		applyTransportFaultFromSession(s, post, pidStr)
		for _, p := range broadcastTouched {
			if memberSess, ok := s.v1.SessionByPlayerID(p); ok {
				applyTransportFaultFromSession(s, memberSess, p)
			}
		}
	}

	rec, ok := playerFromSession(post)
	if !ok {
		writeProblem(w, http.StatusInternalServerError, "https://harness/errors/internal", "post-patch translation failed", "", nil)
		return
	}
	rec.ControlRevision = rev
	w.Header().Set("ETag", formatETag(rev))
	writeJSON(w, http.StatusOK, rec)
}

// ----- helpers -------------------------------------------------------------

// unsupportedPaths returns the leaf paths whose v2→v1 translator hasn't
// been written yet.
//
// Phase D: labels.* only.
// Phase H: + shape.{rate_mbps,delay_ms,loss_pct,transport_fault.*}
// Phase I: + fault_rules (whole-array PATCH; the per-rule sub-resource
//          endpoints have their own paths and don't run through here).
// Phase K: + shape.pattern (drives v1's pattern step-engine).
func unsupportedPaths(paths []string) []string {
	var bad []string
	for _, p := range paths {
		switch {
		case p == "labels", strings.HasPrefix(p, "labels."):
		case p == "shape.rate_mbps":
		case p == "shape.delay_ms":
		case p == "shape.loss_pct":
		case p == "shape.transport_fault", strings.HasPrefix(p, "shape.transport_fault."):
		case p == "shape.pattern", strings.HasPrefix(p, "shape.pattern."):
		case p == "shape":
		case p == "fault_rules":
		default:
			bad = append(bad, p)
		}
	}
	return bad
}

// shapeFieldsTouched reports whether the patch touches any kernel-side
// shape state (rate/delay/loss). Used to decide whether to invoke
// ApplyShapeToPlayer after a successful PATCH.
func shapeFieldsTouched(paths []string) bool {
	for _, p := range paths {
		switch p {
		case "shape", "shape.rate_mbps", "shape.delay_ms", "shape.loss_pct":
			return true
		}
	}
	return false
}

// transportFaultTouched reports whether the patch touches transport
// fault state. Used to decide whether to invoke
// ApplyTransportFaultToPlayer after a successful PATCH.
func transportFaultTouched(paths []string) bool {
	for _, p := range paths {
		if p == "shape" || p == "shape.transport_fault" || strings.HasPrefix(p, "shape.transport_fault.") {
			return true
		}
	}
	return false
}

// patternTouched reports whether the patch touches shape.pattern.
// Used to decide whether to invoke ApplyPatternToPlayer after a
// successful PATCH.
func patternTouched(paths []string) bool {
	for _, p := range paths {
		if p == "shape" || p == "shape.pattern" || strings.HasPrefix(p, "shape.pattern.") {
			return true
		}
	}
	return false
}

// applyPatchToSession projects the v2 Merge Patch onto the v1
// SessionData map. Translates v2 field names to v1 storage keys:
//
//	labels.*          → s["_v2_labels"][...]                (Phase D)
//	shape.rate_mbps   → s["nftables_bandwidth_mbps"]        (Phase H)
//	shape.delay_ms    → s["nftables_delay_ms"]
//	shape.loss_pct    → s["nftables_packet_loss"]
//	shape.transport_fault.{type,frequency,consecutive,mode}
//	                  → s["transport_failure_*"] / s["transport_fault_*"]
//
// Other v2 paths are admitted via unsupportedPaths but stored as
// `_v2_unsupported.<path>` for Phase debugging visibility — they
// don't drive any kernel state.
func applyPatchToSession(s map[string]any, patch map[string]any) error {
	if labels, hasLabels := patch["labels"]; hasLabels {
		applyLabelsPatch(s, labels)
	}
	if shape, hasShape := patch["shape"]; hasShape {
		applyShapePatch(s, shape)
	}
	if rulesAny, hasRules := patch["fault_rules"]; hasRules {
		if rulesAny == nil {
			if err := translateFaultRules(s, nil); err != nil {
				return err
			}
		} else {
			rules, ok := rulesAny.([]any)
			if !ok {
				return &unsupportedFaultRuleError{Reason: "fault_rules must be an array"}
			}
			if err := translateFaultRules(s, rules); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyLabelsPatch(s map[string]any, labels any) {
	if labels == nil {
		delete(s, "_v2_labels")
		return
	}
	patchMap, ok := labels.(map[string]any)
	if !ok {
		return
	}
	current, _ := s["_v2_labels"].(map[string]any)
	if current == nil {
		current = map[string]any{}
	}
	for k, v := range patchMap {
		if v == nil {
			delete(current, k)
			continue
		}
		current[k] = v
	}
	if len(current) == 0 {
		delete(s, "_v2_labels")
		return
	}
	s["_v2_labels"] = current
}

func applyShapePatch(s map[string]any, shape any) {
	if shape == nil {
		// Wholesale wipe — clear every translated v1 field.
		s["nftables_bandwidth_mbps"] = float64(0)
		s["nftables_delay_ms"] = 0
		s["nftables_packet_loss"] = float64(0)
		s["transport_failure_type"] = "none"
		s["transport_fault_type"] = "none"
		s["transport_failure_frequency"] = 0
		s["transport_consecutive_failures"] = 1
		s["transport_failure_mode"] = "failures_per_seconds"
		return
	}
	shapeMap, ok := shape.(map[string]any)
	if !ok {
		return
	}
	if v, present := shapeMap["rate_mbps"]; present {
		if v == nil {
			s["nftables_bandwidth_mbps"] = float64(0)
		} else if f, ok := numericFloat(v); ok {
			s["nftables_bandwidth_mbps"] = f
		}
	}
	if v, present := shapeMap["delay_ms"]; present {
		if v == nil {
			s["nftables_delay_ms"] = 0
		} else if f, ok := numericFloat(v); ok {
			s["nftables_delay_ms"] = int(f)
		}
	}
	if v, present := shapeMap["loss_pct"]; present {
		if v == nil {
			s["nftables_packet_loss"] = float64(0)
		} else if f, ok := numericFloat(v); ok {
			s["nftables_packet_loss"] = f
		}
	}
	if tf, present := shapeMap["transport_fault"]; present {
		applyTransportFaultPatch(s, tf)
	}
	if pat, present := shapeMap["pattern"]; present {
		applyPatternPatch(s, pat)
	}
}

// applyPatternPatch stashes the v2 pattern shape on `_v2_shape_pattern`
// for round-trip. The kernel-apply path (handlers_mutate's PATCH flow
// → ApplyPatternToPlayer) reads this back, translates to []NftShapeStep,
// and drives v1's applyShapePattern. Setting shape.pattern: null
// disarms the v1 step engine.
func applyPatternPatch(s map[string]any, pat any) {
	if pat == nil {
		delete(s, "_v2_shape_pattern")
		// Setting nftables_pattern_enabled=false here is harmless;
		// applyShapePattern with empty steps will write the same
		// keys via updateSessionsByPortWithControl. Keeping the
		// translator field-only avoids stale state if the kernel
		// apply fails after the SessionData publish.
		return
	}
	m, ok := pat.(map[string]any)
	if !ok {
		return
	}
	s["_v2_shape_pattern"] = m
}

// extractPatternSteps reads the v2 pattern stash from the session map
// and returns the v1-shaped step slice. Returns nil when no pattern
// is set or the stash is malformed.
func extractPatternSteps(sess map[string]any) []ShapePatternStep {
	pat, ok := sess["_v2_shape_pattern"].(map[string]any)
	if !ok {
		return nil
	}
	stepsAny, ok := pat["steps"].([]any)
	if !ok {
		return nil
	}
	out := make([]ShapePatternStep, 0, len(stepsAny))
	for _, raw := range stepsAny {
		step, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		var s ShapePatternStep
		if v, ok := numericFloat(step["duration_seconds"]); ok {
			s.DurationSeconds = v
		}
		if v, ok := numericFloat(step["rate_mbps"]); ok {
			s.RateMbps = v
		}
		// Default enabled=true unless explicitly false.
		s.Enabled = true
		if v, ok := step["enabled"].(bool); ok {
			s.Enabled = v
		}
		out = append(out, s)
	}
	return out
}

func applyTransportFaultPatch(s map[string]any, tf any) {
	if tf == nil {
		s["transport_failure_type"] = "none"
		s["transport_fault_type"] = "none"
		s["transport_failure_frequency"] = 0
		s["transport_consecutive_failures"] = 1
		s["transport_failure_mode"] = "failures_per_seconds"
		return
	}
	m, ok := tf.(map[string]any)
	if !ok {
		return
	}
	if v, present := m["type"]; present {
		if v == nil {
			s["transport_failure_type"] = "none"
			s["transport_fault_type"] = "none"
		} else if str, ok := v.(string); ok {
			s["transport_failure_type"] = str
			s["transport_fault_type"] = str
		}
	}
	if v, present := m["frequency"]; present {
		if v == nil {
			s["transport_failure_frequency"] = 0
		} else if f, ok := numericFloat(v); ok {
			s["transport_failure_frequency"] = int(f)
		}
	}
	if v, present := m["consecutive"]; present {
		if v == nil {
			s["transport_consecutive_failures"] = 1
		} else if f, ok := numericFloat(v); ok {
			c := int(f)
			if c < 1 {
				c = 1
			}
			s["transport_consecutive_failures"] = c
		}
	}
	if v, present := m["mode"]; present {
		if str, ok := v.(string); ok {
			s["transport_failure_mode"] = str
		}
	}
}

// applyPatternFromSession reads the v2 pattern stash + delay/loss out
// of the post-patch session map and arms (or disarms) the kernel-side
// step engine on the player's port.
//
// Empty steps disarm. Non-empty steps drive applyShapePattern.
func applyPatternFromSession(srv *Server, sess map[string]any, playerID string) {
	if srv == nil || srv.v1 == nil || sess == nil {
		return
	}
	steps := extractPatternSteps(sess)
	delayMs := 0
	if f, ok := numericFloat(sess["nftables_delay_ms"]); ok {
		delayMs = int(f)
	}
	lossPct := 0.0
	if f, ok := numericFloat(sess["nftables_packet_loss"]); ok {
		lossPct = f
	}
	_ = srv.v1.ApplyPatternToPlayer(playerID, steps, delayMs, lossPct)
}

// applyTransportFaultFromSession reads the transport-fault state out
// of the post-patch session map and arms the kernel-side loop on the
// player's port via the V1Adapter.
func applyTransportFaultFromSession(srv *Server, sess map[string]any, playerID string) {
	if srv == nil || srv.v1 == nil || sess == nil {
		return
	}
	faultType, _ := sess["transport_failure_type"].(string)
	if faultType == "" {
		faultType = "none"
	}
	consec := 1
	if f, ok := numericFloat(sess["transport_consecutive_failures"]); ok && int(f) >= 1 {
		consec = int(f)
	}
	consecUnits, _ := sess["transport_consecutive_units"].(string)
	if consecUnits == "" {
		consecUnits = "seconds"
	}
	freq := 0
	if f, ok := numericFloat(sess["transport_failure_frequency"]); ok {
		freq = int(f)
	}
	_ = srv.v1.ApplyTransportFaultToPlayer(playerID, faultType, consec, consecUnits, freq)
}

// numericFloat coerces a JSON-decoded scalar to float64. JSON numbers
// land as float64 by default; integers still arrive as float64 from
// json.Unmarshal into interface{}.
func numericFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	default:
		return 0, false
	}
}

// conflictErr is the in-band signal from the MutatePlayer fn back up to
// the handler when a TOCTOU race produced a conflict under the lock.
type conflictErr struct{ paths []string }

func (c conflictErr) Error() string { return "if-match conflict on " + strings.Join(c.paths, ",") }

// writePreconditionFailed renders the RFC 7807 + v2-spec extension.
func writePreconditionFailed(w http.ResponseWriter, currentRevision string, conflicts []string) {
	writeProblem(w,
		http.StatusPreconditionFailed,
		"https://harness/errors/precondition-failed",
		"If-Match revision mismatch",
		"one or more requested fields have been modified after the client's If-Match revision; refetch the conflicting paths and retry",
		map[string]any{
			"current_revision": currentRevision,
			"conflicts":        conflicts,
		},
	)
}

