package server

import (
	"errors"
	"io"
	"net/http"
	"strings"

	oapigen "github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/v2oapigen"
	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/v2translate"
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
	rec, ok := v2translate.PlayerFromSession(record)
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
	if !s.v1.DeletePlayer(string(playerId)) {
		writePlayerNotFound(w, string(playerId))
		return
	}
	s.dropFieldRevs(string(playerId))
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

	pidStr := string(playerId)

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
	groupBroadcast := true // default; a display-only group sets group_broadcast=false at connect
	srv := s               // capture the Server receiver before the closure shadows it
	post, found, mErr := s.v1.MutatePlayer(pidStr, func(s map[string]any) error {
		// Re-check under sessionsMu. Another v2 PATCH that won the
		// outer race would have updated FieldRevisions before
		// publishing — the second check sees that.
		if conflicts := fr.Conflicts(ifMatch, paths); len(conflicts) > 0 {
			return conflictErr{paths: conflicts}
		}
		if err := applyPatchToSession(srv, s, patch); err != nil {
			return err
		}
		// Stamp control_revision (RFC3339Nano) + FieldRevisions
		// inside the same lock so SSE subscribers never see the
		// post-patch payload paired with the prior revision.
		s["control_revision"] = rev
		fr.TouchWith(paths, rev)
		groupID = getString(s, "group_id")
		if v, ok := s["group_broadcast"].(bool); ok {
			groupBroadcast = v
		}
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

	// Per-mutation broadcast override (?broadcast=true|false). Wins over the
	// group's default (resolved above from the session's group_broadcast) for
	// THIS PATCH only, so an A/B can drive shared shaping (broadcast=true) and
	// per-member treatment such as a per-member fault (broadcast=false) in any
	// order, independent of the group-mode flag set at connect. Absent ⇒ group
	// default. content/labels are not broadcast-eligible regardless, so this
	// only changes behaviour for shape / fault_rules — exactly where it's needed.
	if ov := r.URL.Query().Get("broadcast"); ov != "" {
		groupBroadcast = ov == "true" || ov == "1"
	}

	// Auto-broadcast to other group members (DESIGN.md § Player groups
	// — auto-broadcast preserved from v1). Each member gets the same
	// new control_revision and the same patch applied; their per-
	// player FieldRevisions tracker is bumped to the same `rev` so a
	// concurrent PATCHer reading from any member sees the latest
	// revision uniformly. Skipped when the group is display-only
	// (group_broadcast=false at connect) — members share group_id for
	// charting but are shaped/labelled independently (startup fleet).
	var broadcastTouched []string
	if groupID != "" && groupBroadcast {
		touched, bErr := s.v1.BroadcastPatch(groupID, pidStr, rev, func(member map[string]any) error {
			return applyPatchToSession(srv, member, patch)
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
	//
	// Precedence model when shape.pattern AND shape.rate_mbps are both
	// touched:
	//   - pattern being SET (non-null) → pattern path only. v1's pattern
	//     step loop owns the kernel cap while enabled; the static rate
	//     would just be overwritten on the next step tick anyway.
	//   - pattern being NULLED (disarm) → BOTH paths. The pattern apply
	//     tears down the step loop; the static apply then installs the
	//     new rate cap on the kernel. Without the static apply the
	//     kernel keeps whatever cap was last written by the pattern
	//     loop (or none) and the new rate_mbps lives only in the
	//     SessionData map — the bug operators see as "harness sets a
	//     rate but the throughput doesn't change."
	//   - pattern untouched → static path only (the common slider case
	//     when no pattern was active).
	//
	// `post["_v2_shape_pattern"]` is the post-patch stash slot — set
	// by applyPatternPatch when pattern is non-null, deleted when null.
	patternIsActive := post["_v2_shape_pattern"] != nil
	if patternTouched(paths) {
		applyPatternFromSession(s, post, pidStr)
		for _, p := range broadcastTouched {
			if memberSess, ok := s.v1.SessionByPlayerID(p); ok {
				applyPatternFromSession(s, memberSess, p)
			}
		}
	}
	if shapeFieldsTouched(paths) && !patternIsActive {
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

	rec, ok := v2translate.PlayerFromSession(post)
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
//
//	endpoints have their own paths and don't run through here).
//
// Phase K: + shape.pattern (drives v1's pattern step-engine).
// #826: + shape.{jitter_ms,loss_correlation_pct,jitter_correlation_pct}
//
//	(link-impairment knobs — netem jitter + bursty-loss correlations).
//
// #800: + app_config (client-side per-play config; applyAppConfigPatch stores
//
//	it nested on the session for the player to read back — no kernel side).
func unsupportedPaths(paths []string) []string {
	var bad []string
	for _, p := range paths {
		switch {
		case p == "labels", strings.HasPrefix(p, "labels."):
		case p == "shape.rate_mbps":
		case p == "shape.delay_ms":
		case p == "shape.loss_pct":
		case p == "shape.jitter_ms": // #826
		case p == "shape.loss_correlation_pct": // #826
		case p == "shape.jitter_correlation_pct": // #826
		case p == "shape.transport_fault", strings.HasPrefix(p, "shape.transport_fault."):
		case p == "shape.pattern", strings.HasPrefix(p, "shape.pattern."):
		case p == "shape":
		case p == "fault_rules":
		case p == "transfer_timeouts", strings.HasPrefix(p, "transfer_timeouts."):
		case p == "content", strings.HasPrefix(p, "content."):
		case p == "app_config", strings.HasPrefix(p, "app_config."):
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
		case "shape", "shape.rate_mbps", "shape.delay_ms", "shape.loss_pct",
			"shape.jitter_ms", "shape.loss_correlation_pct", "shape.jitter_correlation_pct": // #826
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
func applyPatchToSession(srv *Server, s map[string]any, patch map[string]any) error {
	if labels, hasLabels := patch["labels"]; hasLabels {
		applyLabelsPatch(s, labels)
	}
	if shape, hasShape := patch["shape"]; hasShape {
		applyShapePatch(srv, s, shape)
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
	if tt, hasTT := patch["transfer_timeouts"]; hasTT {
		applyTransferTimeoutsPatch(s, tt)
	}
	if c, hasC := patch["content"]; hasC {
		applyContentPatch(s, c)
	}
	if ac, hasAC := patch["app_config"]; hasAC {
		applyAppConfigPatch(s, ac)
	}
	return nil
}

// applyTransferTimeoutsPatch — projects v2 transfer_timeouts patch onto
// the v1 `transfer_*_timeout_seconds` + `transfer_timeout_applies_*`
// fields. Setting transfer_timeouts: null wipes everything to defaults
// (segments-only, both timeouts disabled).
func applyTransferTimeoutsPatch(s map[string]any, tt any) {
	if tt == nil {
		s["transfer_active_timeout_seconds"] = 0
		s["transfer_idle_timeout_seconds"] = 0
		s["transfer_timeout_applies_segments"] = true
		s["transfer_timeout_applies_manifests"] = false
		s["transfer_timeout_applies_master"] = false
		return
	}
	m, ok := tt.(map[string]any)
	if !ok {
		return
	}
	if v, present := m["active_timeout_seconds"]; present {
		s["transfer_active_timeout_seconds"] = toIntZero(v)
	}
	if v, present := m["idle_timeout_seconds"]; present {
		s["transfer_idle_timeout_seconds"] = toIntZero(v)
	}
	if v, present := m["applies_segments"]; present {
		s["transfer_timeout_applies_segments"] = toBool(v)
	}
	if v, present := m["applies_manifests"]; present {
		s["transfer_timeout_applies_manifests"] = toBool(v)
	}
	if v, present := m["applies_master"]; present {
		s["transfer_timeout_applies_master"] = toBool(v)
	}
}

// applyContentPatch — projects v2 content patch onto the v1
// `content_*` fields consumed by the master playlist mutator (see
// main.go ~line 5062).
func applyContentPatch(s map[string]any, c any) {
	if c == nil {
		s["content_strip_codecs"] = false
		s["content_strip_average_bandwidth"] = false
		s["content_strip_resolution"] = false
		s["content_overstate_bandwidth"] = false
		s["content_live_offset"] = 0
		s["content_allowed_variants"] = []any{}
		s["content_variant_order"] = "default"
		return
	}
	m, ok := c.(map[string]any)
	if !ok {
		return
	}
	if v, present := m["strip_codecs"]; present {
		s["content_strip_codecs"] = toBool(v)
	}
	if v, present := m["strip_average_bandwidth"]; present {
		s["content_strip_average_bandwidth"] = toBool(v)
	}
	if v, present := m["strip_resolution"]; present {
		s["content_strip_resolution"] = toBool(v)
	}
	if v, present := m["overstate_bandwidth"]; present {
		s["content_overstate_bandwidth"] = toBool(v)
	}
	if v, present := m["live_offset"]; present {
		s["content_live_offset"] = toIntZero(v)
	}
	if v, present := m["allowed_variants"]; present {
		if v == nil {
			s["content_allowed_variants"] = []any{}
		} else if arr, ok := v.([]any); ok {
			s["content_allowed_variants"] = arr
		}
	}
	if v, present := m["variant_order"]; present {
		if v == nil {
			s["content_variant_order"] = "default"
		} else if str, ok := v.(string); ok {
			s["content_variant_order"] = str
		}
	}
}

// applyAppConfigPatch — projects the v2 app_config patch (#800: client-side
// behaviour the player applies at its NEXT play boundary) onto the session map
// as a nested "app_config" object. Unlike the flat content_*/shape_* fields,
// app_config is stored nested because it is read back verbatim by the player
// off GET /api/sessions (the proxy never acts on it server-side — it's a
// pass-through the client overlays onto its own segment/protocol/offset/peak
// state). JSON Merge Patch semantics: app_config:null clears the whole object;
// a field set to null drops just that field; an omitted field is untouched, so
// a partial patch (e.g. only segment) preserves the rest. Enum fields keep only
// valid values so a malformed arg can't poison the object the client trusts.
func applyAppConfigPatch(s map[string]any, c any) {
	if c == nil {
		delete(s, "app_config")
		return
	}
	m, ok := c.(map[string]any)
	if !ok {
		return
	}
	out, _ := s["app_config"].(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	setEnum := func(key string, v any, allowed ...string) {
		if v == nil {
			delete(out, key)
			return
		}
		str, ok := v.(string)
		if !ok {
			return
		}
		for _, a := range allowed {
			if str == a {
				out[key] = str
				return
			}
		}
	}
	if v, present := m["segment"]; present {
		setEnum("segment", v, "ll", "s1", "s2", "s6")
	}
	if v, present := m["protocol"]; present {
		setEnum("protocol", v, "hls", "dash")
	}
	if v, present := m["live_offset_s"]; present {
		if v == nil {
			delete(out, "live_offset_s")
		} else {
			out["live_offset_s"] = toFloatZero(v)
		}
	}
	if v, present := m["peak_bitrate_mbps"]; present {
		if v == nil {
			delete(out, "peak_bitrate_mbps")
		} else {
			out["peak_bitrate_mbps"] = toIntZero(v)
		}
	}
	// #838 mute. Stored as a real bool so the player reads it back as a JSON
	// boolean (iOS `as? Bool`, Android `optBoolean`). Config-on-connect already
	// coerces "true"/"false" → bool (coerceURLValue), same as the strip_* fields.
	if v, present := m["muted"]; present {
		if v == nil {
			delete(out, "muted")
		} else {
			out["muted"] = toBool(v)
		}
	}
	if len(out) == 0 {
		delete(s, "app_config")
		return
	}
	s["app_config"] = out
}

// toFloatZero coerces a JSON-decoded numeric (float64 from encoding/json, or an
// int form) to float64, defaulting to 0 for anything else.
func toFloatZero(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func toIntZero(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func toBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
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

// applyShapePatch writes the v2 shape patch onto the v1 session map.
// Storage carries the operator's raw intent (rate_mbps=0 stays 0 — the
// dashboard slider position). Effective enforcement of the deployment
// baseline happens at the kernel-apply sites (in package main) via
// App.effectiveRate. The derived effective_rate_limit_mbps field on
// every snapshot is what charts read for the throttle line. Issue #480.
//
// srv is currently unused here but kept on the signature so future
// patches that need Server-scoped state (e.g. per-field provenance
// trackers) can land without re-threading every call site.
func applyShapePatch(srv *Server, s map[string]any, shape any) {
	_ = srv
	if shape == nil {
		// Wholesale wipe — operator-cleared state. nftables_bandwidth_mbps
		// goes to 0 ("no override"); kernel still enforces baseline.
		s["nftables_bandwidth_mbps"] = float64(0)
		s["nftables_delay_ms"] = 0
		s["nftables_packet_loss"] = float64(0)
		s["nftables_jitter_ms"] = 0
		s["nftables_loss_correlation_pct"] = float64(0)
		s["nftables_jitter_correlation_pct"] = float64(0)
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
	// #826 link-impairment knobs. jitter_ms is int (ms); the two
	// correlation percents are floats. nil clears to zero (no impairment).
	if v, present := shapeMap["jitter_ms"]; present {
		if v == nil {
			s["nftables_jitter_ms"] = 0
		} else if f, ok := numericFloat(v); ok {
			s["nftables_jitter_ms"] = int(f)
		}
	}
	if v, present := shapeMap["loss_correlation_pct"]; present {
		if v == nil {
			s["nftables_loss_correlation_pct"] = float64(0)
		} else if f, ok := numericFloat(v); ok {
			s["nftables_loss_correlation_pct"] = f
		}
	}
	if v, present := shapeMap["jitter_correlation_pct"]; present {
		if v == nil {
			s["nftables_jitter_correlation_pct"] = float64(0)
		} else if f, ok := numericFloat(v); ok {
			s["nftables_jitter_correlation_pct"] = f
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
	_ = srv.v1.ApplyPatternToPlayer(playerID, steps, LinkImpairmentFromSession(sess))
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
