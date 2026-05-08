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

// PostApiV2Players: synthetic-player upsert. Awaits the v1-side port
// allocator; returns 501 with a clear problem detail in the meantime.
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
		writeProblem(
			w,
			http.StatusNotImplemented,
			"https://harness/errors/not-implemented",
			"synthetic player creation not yet wired",
			cerr.Error(),
			map[string]any{"operation": "PostApiV2Players"},
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
		applyPatchToSession(s, patch)
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
	if groupID != "" {
		touched, bErr := s.v1.BroadcastPatch(groupID, pidStr, rev, func(member map[string]any) error {
			applyPatchToSession(member, patch)
			return nil
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

// ----- Plays (mutations) ---------------------------------------------------

// PatchApiV2PlaysPlayId stays 404 until Phase E surfaces play boundaries
// from the SSE stream and the v1 store grows a play-id index.
func (*Server) PatchApiV2PlaysPlayId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, params oapigen.PatchApiV2PlaysPlayIdParams) {
	writeProblem(
		w,
		http.StatusNotFound,
		"https://harness/errors/play-not-found",
		"play not found",
		"play-scope mutation needs a live play index (Phase E)",
		map[string]any{"play_id": playId.String()},
	)
}

// ----- helpers -------------------------------------------------------------

// unsupportedPaths returns the leaf paths whose v2→v1 translator hasn't
// been written yet. Phase D supports labels.* only.
func unsupportedPaths(paths []string) []string {
	var bad []string
	for _, p := range paths {
		if p == "labels" || strings.HasPrefix(p, "labels.") {
			continue
		}
		bad = append(bad, p)
	}
	return bad
}

// applyPatchToSession projects the v2 Merge Patch onto the v1
// SessionData map. Phase D handles labels only — this function will
// grow as shape and fault_rules translators land.
//
// Labels are stored on the v1 session under the `_v2_labels` key as a
// `map[string]any` (string keys, string values). v1's existing read
// handlers ignore unknown keys, so this is invisible to the dashboard.
func applyPatchToSession(s map[string]any, patch map[string]any) {
	labels, hasLabels := patch["labels"]
	if !hasLabels {
		return
	}
	if labels == nil {
		delete(s, "_v2_labels")
		return
	}
	current, _ := s["_v2_labels"].(map[string]any)
	if current == nil {
		current = map[string]any{}
	}
	patchMap, ok := labels.(map[string]any)
	if !ok {
		return
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

