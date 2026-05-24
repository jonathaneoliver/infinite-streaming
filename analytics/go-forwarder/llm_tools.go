package main

// llm_tools.go — tool registry + dispatcher for the AI chat backend
// (#497).
//
// Tiers (from the design in #497):
//   Tier 1 — typed domain tools (find_plays, triage_plays, …)
//            Wired to internal/plays so they share code with v2
//            HTTP handlers. Fast, cheap, cover ~80% of questions.
//   Tier 2 — context tools (read_finding, read_standard, read_skill,
//            list_*, propose_finding). Ground the bot in project
//            knowledge.
//   Tier 3 — query(sql) raw ClickHouse access via the llm_reader
//            user. Long tail of questions; CH enforces safety
//            server-side.
//
// Citation is a side-channel: the cite() tool emits citation SSE
// events to the dashboard while returning a brief JSON
// acknowledgement to the LLM. That keeps the LLM's tool budget
// tight and the dashboard's citation cards reactive.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// ToolEmitter is the side-channel callback a tool can use to push
// SSE events to the dashboard mid-execution (e.g. cite() emitting
// citation events). Returns the error from the underlying SSE writer
// if the client has disconnected.
type ToolEmitter func(eventType string, payload any) error

// ToolExecuteFn is the actual work. args is the raw JSON the model
// supplied for the tool call. emit pushes side-channel SSE events
// (may be nil for tools that don't need it). The returned string is
// the JSON result fed back to the LLM as the tool's output.
type ToolExecuteFn func(ctx context.Context, args json.RawMessage, emit ToolEmitter) (string, error)

// Tool is a single callable function the LLM can use.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema describing the function's args. Used
	// verbatim as the `parameters` field on the OAI tool definition.
	Parameters map[string]any
	// Tier identifies which tier this tool is in. Used by the
	// system prompt's tier-awareness messaging and by the budget
	// guard for per-tier accounting (future).
	Tier int
	// Execute runs the tool. Must be cheap to call and idempotent
	// where the LLM might retry.
	Execute ToolExecuteFn
}

// ToolRegistry holds the per-process tool set. Lazily-built; a
// single instance is shared across requests (tools are stateless).
type ToolRegistry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewToolRegistry returns an empty registry. Tools are registered
// via Register or RegisterAll.
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]Tool)}
}

// Register adds a tool. Duplicates panic — registration is
// startup-time, not request-time, so a duplicate is a programming
// error worth surfacing loudly.
func (r *ToolRegistry) Register(t Tool) {
	if t.Name == "" {
		panic("llm tools: empty tool name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[t.Name]; dup {
		panic(fmt.Sprintf("llm tools: duplicate registration: %q", t.Name))
	}
	r.tools[t.Name] = t
}

// RegisterAll is a convenience for batch registration.
func (r *ToolRegistry) RegisterAll(tools []Tool) {
	for _, t := range tools {
		r.Register(t)
	}
}

// Get returns a tool by name, ok=false if not registered.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// All returns every registered tool, sorted by name for deterministic
// ordering in API responses and the system prompt.
func (r *ToolRegistry) All() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToOpenAITools projects the registered tools into the OAI
// tools[] array shape the upstream call expects.
func (r *ToolRegistry) ToOpenAITools() []LLMTool {
	all := r.All()
	out := make([]LLMTool, len(all))
	for i, t := range all {
		out[i] = LLMTool{
			Type: "function",
			Function: LLMToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return out
}

// MaxToolResultBytes caps the size of any single tool result that
// goes back into LLM history. find_plays / query() on a wide window
// can naturally emit 100K-1M of JSON; carrying that across turns
// blows past every model's context. When a tool exceeds the cap,
// Dispatch replaces the result with a short stub that names the
// tool and hints at narrower follow-ups. The bot reads the stub
// and either re-calls with tighter args (mode='summary', a
// smaller limit, a labels_has filter) or proceeds from earlier
// reasoning.
//
// 100 KB is a generous default — keeps a get_play_timeline window
// or a labels histogram intact, catches the catastrophic dumps.
const MaxToolResultBytes = 100_000

// Dispatch runs the named tool with the given args. Returns the
// JSON result string. If the tool isn't registered or fails, returns
// an error-shaped JSON string the LLM can read and self-correct from.
//
// Enforces MaxToolResultBytes after every successful call — even
// if the underlying tool ignores its own limit args, the dispatcher
// won't let an oversized payload land in history.
func (r *ToolRegistry) Dispatch(ctx context.Context, name string, args json.RawMessage, emit ToolEmitter) string {
	t, ok := r.Get(name)
	if !ok {
		return mustJSON(map[string]any{"error": fmt.Sprintf("unknown tool: %q", name)})
	}
	out, err := t.Execute(ctx, args, emit)
	if err != nil {
		return mustJSON(map[string]any{"error": err.Error()})
	}
	if len(out) > MaxToolResultBytes {
		return mustJSON(map[string]any{
			"_truncated":   true,
			"_tool":        name,
			"_orig_bytes":  len(out),
			"_cap_bytes":   MaxToolResultBytes,
			"_preview":     out[:512] + "…",
			"_hint":        "result exceeded the dispatcher byte cap; re-call with mode='summary', a tighter labels_has filter, or a smaller limit. raw_query users: add an aggregation (count(), group by) instead of fetching rows.",
		})
	}
	return out
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"json encode failed"}`
	}
	return string(b)
}
