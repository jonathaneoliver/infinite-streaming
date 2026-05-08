package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// Phase F handlers for /api/v2/player-groups{,/{group_id}}.
//
// v2 groups are a thin shell around v1's `group_id` string tag on
// each session — see DESIGN.md § "Player groups: auto-broadcast
// preserved from v1". The v1 wire format is preserved (one string per
// session); v2 just exposes them as first-class collections, generates
// UUIDv4 ids for v2-created groups, and routes member PATCHes through
// the auto-broadcast path in handlers_mutate.go.
//
// State-of-play in this commit:
//
//   - GET (list/one) — already in handlers_read.go (Phase B).
//   - POST (create), PATCH (mutate metadata), DELETE (disband) —
//     here.
//   - PATCH /api/v2/players/{id} broadcasts to group members
//     automatically when the player is tagged. See handlers_mutate.go.

// ----- Group ID utilities --------------------------------------------------

// groupIDForV1 maps a v2 GroupId UUID to the v1 group_id tag string.
//
// v2-created groups (POST /api/v2/player-groups) write the canonical
// lowercase-hyphenated UUID directly as the v1 tag, so the round-trip
// is just `String()`. Legacy v1 groups (e.g. "G1234") never appear
// here — the handlers below operate only on v2-routable groups.
func groupIDForV1(g oapigen.GroupId) string { return g.String() }

// ----- POST /api/v2/player-groups ------------------------------------------

// PostApiV2PlayerGroups creates a new group. Body:
//
//	{ "member_player_ids": [...], "label": "...", "labels": {...} }
//
// The server allocates a UUIDv4 group_id, tags every supplied member
// session with it, and returns the resulting PlayerGroup.
//
// Members that aren't currently connected are silently skipped —
// callers that care can list /api/v2/players first and reject up
// front. (Future: support pre-tagging an unborn synthetic.)
func (s *Server) PostApiV2PlayerGroups(w http.ResponseWriter, r *http.Request) {
	if s.v1 == nil {
		notImplemented(w, "PostApiV2PlayerGroups")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "read body failed", err.Error(), nil)
		return
	}
	var req oapigen.PostApiV2PlayerGroupsJSONRequestBody
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "invalid request body", err.Error(), nil)
		return
	}
	if len(req.MemberPlayerIds) == 0 {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "member_player_ids required", "create at least one member; an empty group has no semantic value in v2", nil)
		return
	}

	groupID := uuid.New().String()

	// Convert each requested UUID to its canonical string form.
	pids := make([]string, 0, len(req.MemberPlayerIds))
	for _, m := range req.MemberPlayerIds {
		pids = append(pids, m.String())
	}
	linked := s.v1.LinkGroup(groupID, pids)
	if len(linked) == 0 {
		writeProblem(w, http.StatusConflict, "https://harness/errors/no-eligible-members", "no requested players are currently connected", "every member_player_ids entry was unknown to the v1 store; nothing to tag", map[string]any{"requested_player_ids": pids})
		return
	}

	rec := buildGroupRecord(groupID, req.Label, req.Labels, linked)
	if rec.ControlRevision != nil {
		w.Header().Set("ETag", formatETag(*rec.ControlRevision))
	}
	w.Header().Set("Location", "/api/v2/player-groups/"+groupID)
	writeJSON(w, http.StatusCreated, rec)
}

// ----- PATCH /api/v2/player-groups/{group_id} ------------------------------

// PatchApiV2PlayerGroupsGroupId mutates group metadata. Supported
// fields: `label`, `labels`, `member_player_ids` (which adds + removes
// against the current set).
//
// `If-Match` is required and checked against the group's metadata
// revision (which is the max of the per-leaf revisions for `label`,
// `labels`, `member_player_ids`). Field-level concurrency applies the
// same way it does on PlayerRecord.
func (s *Server) PatchApiV2PlayerGroupsGroupId(w http.ResponseWriter, r *http.Request, groupId oapigen.GroupId, params oapigen.PatchApiV2PlayerGroupsGroupIdParams) {
	if s.v1 == nil {
		notImplemented(w, "PatchApiV2PlayerGroupsGroupId")
		return
	}
	gid := groupIDForV1(groupId)
	current := s.v1.GroupMembers(gid)
	if len(current) == 0 {
		writeGroupNotFound(w, gid)
		return
	}
	ifMatch := parseIfMatch(string(params.IfMatch))
	if ifMatch == "" {
		writeProblem(w, http.StatusPreconditionRequired,
			"https://harness/errors/precondition-required",
			"If-Match required",
			"PATCH /api/v2/player-groups requires a strong-tag If-Match header",
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
	if unsupported := unsupportedGroupPaths(paths); len(unsupported) > 0 {
		writeProblem(w, http.StatusBadRequest, "https://harness/errors/bad-request", "unknown group field(s)", "PlayerGroupPatch accepts: label, labels, member_player_ids", map[string]any{"unsupported_paths": unsupported})
		return
	}

	fr := s.groupRevs(gid)
	if conflicts := fr.Conflicts(ifMatch, paths); len(conflicts) > 0 {
		writePreconditionFailed(w, fr.Top(), conflicts)
		return
	}

	rev := newRevision()

	// Apply membership changes against the live store. Other fields
	// (`label`, `labels`) are tracked in the group-side state but not
	// persisted to v1 — v1 has no first-class group resource. Phase
	// F+ will route those through a dedicated group store.
	if newMembersAny, hasMembers := patch["member_player_ids"]; hasMembers {
		newMembers := []string{}
		if arr, ok := newMembersAny.([]any); ok {
			for _, v := range arr {
				if str, ok := v.(string); ok {
					newMembers = append(newMembers, str)
				}
			}
		}
		// Compute add / remove diff against current membership.
		currentSet := map[string]struct{}{}
		for _, p := range current {
			currentSet[p] = struct{}{}
		}
		nextSet := map[string]struct{}{}
		for _, p := range newMembers {
			nextSet[p] = struct{}{}
		}
		var toAdd []string
		for p := range nextSet {
			if _, exists := currentSet[p]; !exists {
				toAdd = append(toAdd, p)
			}
		}
		var toRemove []string
		for p := range currentSet {
			if _, stay := nextSet[p]; !stay {
				toRemove = append(toRemove, p)
			}
		}
		if len(toRemove) > 0 {
			for _, p := range toRemove {
				s.v1.RemoveFromGroup(p)
			}
		}
		if len(toAdd) > 0 {
			s.v1.LinkGroup(gid, toAdd)
		}
	}

	fr.TouchWith(paths, rev)

	rec := buildGroupRecord(gid, ptrFromAny(patch["label"]), labelsFromAny(patch["labels"]), s.v1.GroupMembers(gid))
	rec.ControlRevision = &rev
	w.Header().Set("ETag", formatETag(rev))
	writeJSON(w, http.StatusOK, rec)
}

// ----- DELETE /api/v2/player-groups/{group_id} -----------------------------

// DeleteApiV2PlayerGroupsGroupId disbands the group: every member's
// `group_id` tag is cleared. Member players themselves stay
// connected — they just lose group affinity.
func (s *Server) DeleteApiV2PlayerGroupsGroupId(w http.ResponseWriter, r *http.Request, groupId oapigen.GroupId) {
	if s.v1 == nil {
		notImplemented(w, "DeleteApiV2PlayerGroupsGroupId")
		return
	}
	gid := groupIDForV1(groupId)
	cleared := s.v1.UnlinkGroup(gid)
	if len(cleared) == 0 {
		writeGroupNotFound(w, gid)
		return
	}
	s.dropGroupRevs(gid)
	w.WriteHeader(http.StatusNoContent)
}

// ----- helpers -------------------------------------------------------------

// buildGroupRecord assembles the v2 PlayerGroup response from the v1
// state + supplied metadata. The metadata fields (`label`, `labels`)
// are caller-supplied for now since v1 doesn't store them. Callers
// from POST/PATCH pass through the request body's values; future
// consumers can plumb through a dedicated group store.
func buildGroupRecord(gid string, label *string, labels *oapigen.Labels, memberIDs []string) oapigen.PlayerGroup {
	groupUUID, err := uuid.Parse(gid)
	if err != nil {
		// Should never happen for v2-created groups (always UUIDv4),
		// but legacy v1 group_ids aren't parseable. Fall back to
		// stableGroupUUID.
		stable, _ := stableGroupUUID(gid)
		groupUUID = stable
	}
	members := make([]uuid.UUID, 0, len(memberIDs))
	for _, p := range memberIDs {
		if u, err := uuid.Parse(p); err == nil {
			members = append(members, u)
		}
	}
	return oapigen.PlayerGroup{
		Id:              groupUUID,
		Label:           label,
		Labels:          labels,
		MemberPlayerIds: members,
	}
}

// unsupportedGroupPaths lists the leaf paths that aren't part of
// PlayerGroupPatch. Anything else returns 400.
func unsupportedGroupPaths(paths []string) []string {
	var bad []string
	for _, p := range paths {
		switch {
		case p == "label":
		case p == "labels", labelChild(p):
		case p == "member_player_ids":
		default:
			bad = append(bad, p)
		}
	}
	return bad
}

// labelChild reports whether p is `labels.<key>` — used to admit
// nested label patches without listing every key.
func labelChild(p string) bool {
	return len(p) > len("labels.") && p[:len("labels.")] == "labels."
}

// ptrFromAny coerces a Merge Patch field to *string (or nil).
func ptrFromAny(v any) *string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// labelsFromAny coerces a Merge Patch field to *Labels (or nil).
func labelsFromAny(v any) *oapigen.Labels {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := oapigen.Labels{}
	for k, val := range m {
		if str, ok := val.(string); ok {
			out[k] = str
		}
	}
	return &out
}
