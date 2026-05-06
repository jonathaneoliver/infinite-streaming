// SQL `query` tool for the AI session-analysis path (epic #412).
//
// Exposes a single tool to the LLM: query(sql) → rows. All safety is
// enforced by the ClickHouse `llm_reader` user (#413) at the server
// level — readonly, time-cap, scan-cap, GRANT scope, no SET. We pass
// LLM-authored SQL through unchecked.
//
// One thing the server does *not* enforce in 24.8 is `max_result_rows`
// on streaming SELECTs (it's cooperative; see the comment in
// 02-llm-reader.sql). So this file's runner enforces the response-
// size cap on read: if ClickHouse hands back more than maxRows or
// maxBytes, we truncate and flag `truncated=true`. The LLM is
// expected to react ("got truncated, narrow with WHERE / aggregate").

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// Hard caps the forwarder applies on *response* size. ClickHouse's
// `max_result_rows` setting is cooperative for streaming SELECTs in
// 24.8; these kick in regardless and are the real backstop.
const (
	queryToolMaxRows  = 10000
	queryToolMaxBytes = 10 * 1024 * 1024 // 10 MB
	// 12 s is slightly above the server's 10 s max_execution_time so
	// we don't race the server's own cutoff. If the server times out
	// it surfaces as a ClickHouse error, not a Go context cancel.
	queryToolTimeout = 12 * time.Second
)

// QueryToolResult is the shape returned to the LLM as a JSON-encoded
// tool result. Field names are short on purpose — the LLM pays for
// every input token of these results.
type QueryToolResult struct {
	Rows      [][]any  `json:"rows"`
	Columns   []string `json:"columns"`
	Truncated bool     `json:"truncated,omitempty"`
	Error     string   `json:"error,omitempty"`
	ElapsedMS int      `json:"elapsed_ms"`
}

// QueryTool runs llm_reader-scoped SQL against ClickHouse via the
// HTTP interface. The forwarder already posts inserts to the same
// host; this is a separate user identity over the same transport.
type QueryTool struct {
	ClickHouseURL string // e.g. http://clickhouse:8123
	User          string // llm_reader
	HTTPClient    *http.Client
}

func NewQueryTool(clickhouseURL string) *QueryTool {
	return &QueryTool{
		ClickHouseURL: strings.TrimRight(clickhouseURL, "/"),
		User:          "llm_reader",
		HTTPClient: &http.Client{
			Timeout: queryToolTimeout,
		},
	}
}

// OpenAITool returns the function-tool schema the LLM uses to invoke
// this. The description is part of the contract — the LLM reads it
// and decides when to call. Keep it tight; every word counts.
func (q *QueryTool) OpenAITool() openai.Tool {
	return openai.Tool{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name: "query",
			Description: "Run a read-only ClickHouse SQL query against the " +
				"infinite_streaming database. Returns up to 10000 rows / 10 MB. " +
				"Server caps: 10s execution, 10M rows scanned, 1 GB memory. " +
				"Use to answer questions about archived sessions: " +
				"infinite_streaming.session_snapshots and " +
				"infinite_streaming.network_requests are the main tables.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql": map[string]any{
						"type":        "string",
						"description": "ClickHouse SELECT query. SET / INSERT / DDL are rejected.",
					},
				},
				"required": []string{"sql"},
			},
		},
	}
}

// Run executes a ClickHouse query as `llm_reader` and returns a
// truncated-on-overflow JSON response shape.
func (q *QueryTool) Run(ctx context.Context, sql string) *QueryToolResult {
	started := time.Now()
	out := &QueryToolResult{}

	// 12 s per-query cap; bounded by the parent ctx if that's sooner,
	// since context.WithTimeout takes the earlier of the two deadlines.
	ctx, cancel := context.WithTimeout(ctx, queryToolTimeout)
	defer cancel()

	url := q.ClickHouseURL + "/?default_format=JSON"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(sql))
	if err != nil {
		out.Error = err.Error()
		out.ElapsedMS = int(time.Since(started).Milliseconds())
		return out
	}
	req.Header.Set("X-ClickHouse-User", q.User)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := q.HTTPClient.Do(req)
	if err != nil {
		out.Error = err.Error()
		out.ElapsedMS = int(time.Since(started).Milliseconds())
		return out
	}
	defer resp.Body.Close()

	// Read up to maxBytes+1 so we can flag truncation. We don't trust
	// Content-Length — ClickHouse streams without setting it for some
	// queries.
	body, err := io.ReadAll(io.LimitReader(resp.Body, queryToolMaxBytes+1))
	out.ElapsedMS = int(time.Since(started).Milliseconds())
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if len(body) > queryToolMaxBytes {
		body = body[:queryToolMaxBytes]
		out.Truncated = true
	}
	if resp.StatusCode != http.StatusOK {
		out.Error = fmt.Sprintf("clickhouse %d: %s", resp.StatusCode, sniffError(body))
		return out
	}

	var parsed struct {
		Meta []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"meta"`
		Data [][]any `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		// Some malformed cases (e.g. truncated JSON) — pass the raw
		// body sniff to the LLM so it can adapt.
		out.Error = fmt.Sprintf("parse json: %s", sniffError(body))
		return out
	}
	for _, m := range parsed.Meta {
		out.Columns = append(out.Columns, m.Name)
	}
	if len(parsed.Data) > queryToolMaxRows {
		out.Rows = parsed.Data[:queryToolMaxRows]
		out.Truncated = true
	} else {
		out.Rows = parsed.Data
	}
	return out
}

// sniffError takes the first ~200 chars of a body so error messages
// fed back to the LLM are bounded.
func sniffError(body []byte) string {
	const max = 200
	if len(body) <= max {
		return strings.TrimSpace(string(body))
	}
	return strings.TrimSpace(string(body[:max])) + "…"
}

// MarshalForTool returns the JSON the assistant role:tool message
// will carry. Always succeeds — encoding errors collapse to a
// minimal "{\"error\":\"…\"}" so the loop never gets stuck.
func (r *QueryToolResult) MarshalForTool() string {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	// Defense in depth: in practice the 10k-row cap upstream keeps
	// us under maxBytes, but if it ever fires return a clean
	// truncation marker rather than mid-array junk JSON.
	if len(b) > queryToolMaxBytes {
		return fmt.Sprintf(
			`{"error":"result exceeded %d bytes after row truncation","truncated":true,"elapsed_ms":%d}`,
			queryToolMaxBytes, r.ElapsedMS,
		)
	}
	return string(b)
}

