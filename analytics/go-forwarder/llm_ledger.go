package main

// llm_ledger.go — writes + reads against infinite_streaming.llm_calls
// (#497). One row per chat request, with token + cost + outcome.
// Drives the global daily budget guard and per-key spend visibility.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LLMCallRow mirrors the llm_calls CH schema. JSON tags are column
// names so the JSONEachRow INSERT body uses them as-is.
type LLMCallRow struct {
	Ts              string  `json:"ts"`
	ChatID          string  `json:"chat_id"`
	RequestID       string  `json:"request_id"`
	KeyHash         string  `json:"key_hash"`
	Profile         string  `json:"profile"`
	BaseURL         string  `json:"base_url"`
	Model           string  `json:"model"`
	OneShot         uint8   `json:"one_shot"`
	ScopeKind       string  `json:"scope_kind"`
	ScopePlayID     string  `json:"scope_play_id"`
	ScopeRunID      string  `json:"scope_run_id"`
	InputTokens     uint32  `json:"input_tokens"`
	OutputTokens    uint32  `json:"output_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	DurationMs      uint32  `json:"duration_ms"`
	ToolCallsCount  uint16  `json:"tool_calls_count"`
	Status          string  `json:"status"`
	ErrorKind       string  `json:"error_kind"`
	PromptVersion   string  `json:"prompt_version"`
}

// Status values for the ledger's status column. Aligned with the
// dashboard's outcome surface.
const (
	LLMStatusOK             = "ok"
	LLMStatusBudgetExceeded = "budget_exceeded"
	LLMStatusInputTooLarge  = "input_too_large"
	LLMStatusError          = "error"
	LLMStatusCancelled      = "cancelled"
)

// HashAPIKey returns the lowercase-hex sha256 of an API key. Used as
// the ledger's key_hash so per-key spend can be aggregated without
// the key ever touching the disk.
func HashAPIKey(key string) string {
	if key == "" {
		// Anonymous slot for Ollama-style no-key configs. Distinct
		// from a real key hash so aggregations don't conflate users.
		return strings.Repeat("0", 64)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// InsertLLMCall writes a single row to llm_calls. Best-effort — if
// the insert fails we log via the caller's logger but don't fail
// the user's chat turn over an accounting glitch.
func InsertLLMCall(ctx context.Context, cfg config, row LLMCallRow) error {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(row); err != nil {
		return fmt.Errorf("ledger: encode row: %w", err)
	}
	q := fmt.Sprintf("INSERT INTO %s.llm_calls FORMAT JSONEachRow", cfg.chDatabase)
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return err
	}
	qs := u.Query()
	qs.Set("query", q)
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// SpentTodayUSD returns the sum of cost_usd for today (UTC). cost_usd
// = -1 (unknown pricing) is treated as 0 so unknown-model calls don't
// poison the budget.
func SpentTodayUSD(ctx context.Context, cfg config) (float64, error) {
	q := fmt.Sprintf(`
		SELECT sum(if(cost_usd >= 0, cost_usd, 0))
		FROM %s.llm_calls
		WHERE toDate(ts, 'UTC') = today()
		FORMAT TSV`, cfg.chDatabase)
	body, err := chQueryBytes(ctx, cfg, q, nil)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(body))
	if s == "" {
		return 0, nil
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return 0, fmt.Errorf("ledger: parse SUM: %w (raw %q)", err, s)
	}
	return v, nil
}

// BudgetStatus is the shape /api/v2/chat/budget returns.
type BudgetStatus struct {
	SpentUSD   float64 `json:"spent_usd"`
	CapUSD     float64 `json:"cap_usd"`
	CallsToday uint32  `json:"calls_today"`
	ResetsAt   string  `json:"resets_at"`
}

// ReadBudget loads the budget surface in one round-trip.
func ReadBudget(ctx context.Context, cfg config, capUSD float64) (*BudgetStatus, error) {
	// toUInt32 on count() — CH renders UInt64 as JSON strings to
	// avoid JS precision loss, which would break the json.Unmarshal
	// into our uint32 field. Cap is symbolic: at ~10^9 calls/day
	// we have bigger problems than overflow.
	q := fmt.Sprintf(`
		SELECT sum(if(cost_usd >= 0, cost_usd, 0)) AS spent, toUInt32(count()) AS calls
		FROM %s.llm_calls
		WHERE toDate(ts, 'UTC') = today()
		FORMAT JSONEachRow`, cfg.chDatabase)
	body, err := chQueryBytes(ctx, cfg, q, nil)
	if err != nil {
		return nil, err
	}
	var row struct {
		Spent float64 `json:"spent"`
		Calls uint32  `json:"calls"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(bytes.TrimSpace(body), &row); err != nil {
			return nil, fmt.Errorf("ledger: parse budget row: %w", err)
		}
	}
	// Next UTC midnight.
	now := time.Now().UTC()
	reset := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return &BudgetStatus{
		SpentUSD:   row.Spent,
		CapUSD:     capUSD,
		CallsToday: row.Calls,
		ResetsAt:   reset.Format(time.RFC3339),
	}, nil
}
