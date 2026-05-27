package main

// llm_tool_query.go — Tier 3 raw ClickHouse query tool (#497).
//
// The LLM hands us SQL; we run it as the `llm_reader` user (see
// analytics/clickhouse/init.d/02-llm-reader.sql). ClickHouse enforces
// every safety bound — execution time, memory, row count, scan
// count, readonly_settings. The forwarder does NO client-side SQL
// parsing or rewriting; errors from CH come back to the LLM
// verbatim so it can self-correct.
//
// Tier 3 is the escape hatch when the typed tools don't fit.
// Encourage the LLM (via the system prompt) to reach for Tier 1
// first — typed tools are faster, cheaper, less error-prone.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LLMReaderBackend mirrors the playsBackend pattern but with the
// restricted llm_reader CH credentials. Constructed from config via
// llmReaderBackend(cfg).
type LLMReaderBackend struct {
	ClickHouseURL string
	Database      string
	User          string
	Password      string
}

func llmReaderBackend(cfg config) LLMReaderBackend {
	return LLMReaderBackend{
		ClickHouseURL: cfg.clickhouseURL,
		Database:      cfg.chDatabase,
		User:          cfg.llmReaderUser,
		Password:      cfg.llmReaderPassword,
	}
}

// QueryTool builds the Tier 3 raw-SQL tool.
func QueryTool(cfg config) Tool {
	be := llmReaderBackend(cfg)
	return Tool{
		Name: "query",
		Description: "Run a SQL SELECT against the analytics ClickHouse. " +
			"Read-only, server-enforced caps (10s execution, 10M rows " +
			"scanned, 10k rows returned, 10MB). Available tables: " +
			"infinite_streaming.{session_events, network_requests, " +
			"control_events, characterization_runs, llm_calls}. " +
			"Use Tier 1 typed tools (find_plays, get_play_summary, " +
			"get_control_events) first — query() is the escape hatch " +
			"for analyses the typed tools don't cover.",
		Tier: 3,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sql": map[string]any{"type": "string", "description": "Valid ClickHouse SQL. FORMAT clause is added automatically — don't include it."},
			},
			"required": []string{"sql"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				SQL string `json:"sql"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if strings.TrimSpace(a.SQL) == "" {
				return "", errors.New("sql required")
			}
			start := time.Now()
			rows, truncated, err := be.runSelect(ctx, a.SQL)
			elapsed := time.Since(start)
			if err != nil {
				// Pass the CH error verbatim — the LLM can read it
				// and self-correct (typo, missing column, etc.).
				return mustJSON(map[string]any{
					"error":      err.Error(),
					"elapsed_ms": elapsed.Milliseconds(),
				}), nil
			}
			return mustJSON(map[string]any{
				"rows":       rows,
				"row_count":  len(rows),
				"truncated":  truncated,
				"elapsed_ms": elapsed.Milliseconds(),
			}), nil
		},
	}
}

// runSelect executes the SQL as the llm_reader user and returns
// rows + truncated flag. ClickHouse's result_overflow_mode='break'
// truncates silently — we detect it by inspecting the row count
// against the cap (10k from the settings profile).
const llmReaderRowCap = 10000

func (b LLMReaderBackend) runSelect(ctx context.Context, sql string) ([]map[string]any, bool, error) {
	u, err := url.Parse(b.ClickHouseURL)
	if err != nil {
		return nil, false, err
	}
	qs := u.Query()
	qs.Set("default_format", "JSONEachRow")
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(sql))
	if err != nil {
		return nil, false, err
	}
	if b.User != "" {
		req.SetBasicAuth(b.User, b.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, false, fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	out := []map[string]any{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		out = append(out, row)
	}
	if err := scanner.Err(); err != nil {
		return out, false, err
	}
	// Heuristic: CH's result_overflow_mode=break truncates silently
	// at exactly max_result_rows; if we hit that count we surface
	// truncated=true so the LLM knows the answer may be incomplete.
	truncated := len(out) >= llmReaderRowCap
	return out, truncated, nil
}
