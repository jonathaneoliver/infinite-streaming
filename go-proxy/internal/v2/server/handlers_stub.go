package server

// Stub handlers for Phase A: every endpoint not yet implemented returns
// 501 Not Implemented with an RFC 7807 problem body. Each phase moves
// methods out of this file into their real home (handlers_read.go,
// handlers_mutate.go, handlers_sse.go).

import (
	"net/http"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// ----- Streams -------------------------------------------------------------

func (*Server) GetApiV2Events(w http.ResponseWriter, r *http.Request, params oapigen.GetApiV2EventsParams) {
	notImplemented(w, "GetApiV2Events")
}

// ----- Per-rule fault sub-resources (player) -------------------------------

func (*Server) PostApiV2PlayersPlayerIdFaultRules(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, params oapigen.PostApiV2PlayersPlayerIdFaultRulesParams) {
	notImplemented(w, "PostApiV2PlayersPlayerIdFaultRules")
}

func (*Server) PatchApiV2PlayersPlayerIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, ruleId oapigen.RuleId, params oapigen.PatchApiV2PlayersPlayerIdFaultRulesRuleIdParams) {
	notImplemented(w, "PatchApiV2PlayersPlayerIdFaultRulesRuleId")
}

func (*Server) DeleteApiV2PlayersPlayerIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, ruleId oapigen.RuleId, params oapigen.DeleteApiV2PlayersPlayerIdFaultRulesRuleIdParams) {
	notImplemented(w, "DeleteApiV2PlayersPlayerIdFaultRulesRuleId")
}

// ----- Per-rule fault sub-resources (play) ---------------------------------

func (*Server) PostApiV2PlaysPlayIdFaultRules(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, params oapigen.PostApiV2PlaysPlayIdFaultRulesParams) {
	notImplemented(w, "PostApiV2PlaysPlayIdFaultRules")
}

func (*Server) PatchApiV2PlaysPlayIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, ruleId oapigen.RuleId, params oapigen.PatchApiV2PlaysPlayIdFaultRulesRuleIdParams) {
	notImplemented(w, "PatchApiV2PlaysPlayIdFaultRulesRuleId")
}

func (*Server) DeleteApiV2PlaysPlayIdFaultRulesRuleId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId, ruleId oapigen.RuleId, params oapigen.DeleteApiV2PlaysPlayIdFaultRulesRuleIdParams) {
	notImplemented(w, "DeleteApiV2PlaysPlayIdFaultRulesRuleId")
}

// ----- Player groups (mutations) -------------------------------------------

func (*Server) PostApiV2PlayerGroups(w http.ResponseWriter, r *http.Request) {
	notImplemented(w, "PostApiV2PlayerGroups")
}

func (*Server) PatchApiV2PlayerGroupsGroupId(w http.ResponseWriter, r *http.Request, groupId oapigen.GroupId, params oapigen.PatchApiV2PlayerGroupsGroupIdParams) {
	notImplemented(w, "PatchApiV2PlayerGroupsGroupId")
}

func (*Server) DeleteApiV2PlayerGroupsGroupId(w http.ResponseWriter, r *http.Request, groupId oapigen.GroupId) {
	notImplemented(w, "DeleteApiV2PlayerGroupsGroupId")
}
