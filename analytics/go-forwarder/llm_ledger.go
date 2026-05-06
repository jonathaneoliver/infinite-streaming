// USD ledger + budget guards for the AI session-analysis path
// (epic #412, issue #417).
//
// Every call to /api/session_chat — whether successful, refused
// pre-flight, or cancelled mid-stream — writes one row to
// infinite_streaming.llm_calls. The daily-spend gate sums today's
// rows before each new call and refuses with 429 when over budget.
//
// Two writes, one read:
//   - WriteCall   → INSERT one row (post-flight)
//   - TodaysSpend → SELECT sum(cost_usd) WHERE ts >= today() (pre-flight)
//   - EstimateTokens → cheap char/4 fallback for the input cap

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LLMCallRecord is what gets written to infinite_streaming.llm_calls.
// Field names match the JSONEachRow column names; ClickHouse's writer
// is JSON-tolerant about extra fields, so adding optional fields here
// later is safe even on existing tables.
type LLMCallRecord struct {
	SessionID      string  `json:"session_id"`
	Profile        string  `json:"profile"`
	Model          string  `json:"model"`
	PromptVersion  string  `json:"prompt_version"`
	OneShot        uint8   `json:"one_shot"`
	InputTokens    uint32  `json:"input_tokens"`
	OutputTokens   uint32  `json:"output_tokens"`
	CostUSD        float64 `json:"cost_usd"`
	DurationMS     uint32  `json:"duration_ms"`
	Iterations     uint16  `json:"iterations"`
	ToolCallsCount uint16  `json:"tool_calls_count"`
	Status         string  `json:"status"`
	ErrorKind      string  `json:"error_kind,omitempty"`
	ErrorDetail    string  `json:"error_detail,omitempty"`
}

const (
	statusOK             = "ok"
	statusBudgetExceeded = "budget_exceeded"
	statusInputTooLarge  = "input_too_large"
	statusError          = "error"
	statusCancelled      = "cancelled"

	// Default daily spend cap. Overridden by LLM_DAILY_BUDGET_USD.
	// 5 USD ≈ a half-day of moderate Opus 4.7 use; tune per project.
	defaultDailyBudgetUSD = 5.0
	// Default per-call input cap. Overridden by LLM_MAX_INPUT_TOKENS_PER_CALL.
	// 80k is generous for an analytical chat; the schema dump system
	// prompt is ~2k, leaves plenty of room for messages + tool results.
	defaultMaxInputTokensPerCall = 80000
)

type LLMLedger struct {
	ClickHouseURL string // http://clickhouse:8123 (no /, default DB)
	Database      string // infinite_streaming
	HTTPClient    *http.Client
}

func NewLLMLedger(clickhouseURL, database string) *LLMLedger {
	return &LLMLedger{
		ClickHouseURL: strings.TrimRight(clickhouseURL, "/"),
		Database:      database,
		// Tight timeout: ledger ops are tiny single-row queries that
		// shouldn't block the user-facing chat for more than this.
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// WriteCall inserts one ledger row. Errors are logged + swallowed by
// the caller; we never want a ledger hiccup to crash a successful
// chat. The cost of an occasional missing row is small compared to
// the cost of refusing a call because ClickHouse blipped.
func (l *LLMLedger) WriteCall(ctx context.Context, rec LLMCallRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	u, err := l.queryURL("INSERT INTO " + l.Database + ".llm_calls FORMAT JSONEachRow")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ledger insert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("ledger insert status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// TodaysSpendUSD sums cost_usd for today (UTC). Used by the budget
// gate before every chat call. ClickHouse-side this is a tiny scan
// over today's partition; the index granularity (8192) and partition
// (toYYYYMM(ts)) keep it cheap even at high call volume.
func (l *LLMLedger) TodaysSpendUSD(ctx context.Context) (float64, error) {
	body, err := l.runScalarQuery(ctx,
		"SELECT sum(cost_usd) FROM "+l.Database+".llm_calls WHERE toDate(ts) = today() FORMAT TabSeparated")
	if err != nil {
		return 0, err
	}
	if body == "" || body == "0" {
		return 0, nil
	}
	var v float64
	if _, err := fmt.Sscanf(body, "%f", &v); err != nil {
		return 0, fmt.Errorf("parse daily spend %q: %w", body, err)
	}
	return v, nil
}

// CallsTodayCount returns the number of llm_calls rows for today.
// Surfaced through /api/llm_budget so the UI can show "N calls
// today" alongside the dollar amount.
func (l *LLMLedger) CallsTodayCount(ctx context.Context) (int, error) {
	body, err := l.runScalarQuery(ctx,
		"SELECT count() FROM "+l.Database+".llm_calls WHERE toDate(ts) = today() FORMAT TabSeparated")
	if err != nil {
		return 0, err
	}
	var n int
	_, _ = fmt.Sscanf(body, "%d", &n)
	return n, nil
}

// queryURL builds a ClickHouse HTTP URL with the SQL in the `query`
// parameter, matching the existing forwarder pattern in
// proxyClickHouseJSON.
func (l *LLMLedger) queryURL(sql string) (string, error) {
	u, err := url.Parse(l.ClickHouseURL)
	if err != nil {
		return "", err
	}
	qs := u.Query()
	qs.Set("query", sql)
	u.RawQuery = qs.Encode()
	return u.String(), nil
}

func (l *LLMLedger) runScalarQuery(ctx context.Context, sql string) (string, error) {
	u, err := l.queryURL(sql)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("ch query status %d: %s", resp.StatusCode, body)
	}
	return strings.TrimSpace(string(body)), nil
}

// EstimateInputTokens returns a cheap char-based estimate of the
// total input token count for the messages + tools list. Real cost
// is computed post-flight from response Usage; this is just for the
// pre-flight 413 guard. tiktoken-style accurate counting is a future
// upgrade — for now char/4 is conservative within ±25%, which is
// good enough for "is this clearly too big?" gating.
func EstimateInputTokens(messages []openaiMessageLike, toolsJSON string) int {
	chars := len(toolsJSON)
	for _, m := range messages {
		chars += len(m.Role) + len(m.Content)
	}
	return chars / 4
}

// openaiMessageLike narrows the dependency on go-openai's
// ChatCompletionMessage to just what EstimateInputTokens needs;
// keeps the function testable without the full openai package.
type openaiMessageLike struct {
	Role    string
	Content string
}

// CostUSD computes the per-call dollar amount from a profile's
// pricing × usage tokens. Profile pricing is per-million-tokens.
func CostUSD(profile *LLMProfile, inputTokens, outputTokens int) float64 {
	if profile == nil {
		return 0
	}
	in := float64(inputTokens) / 1_000_000 * profile.Pricing.InputPerMTok
	out := float64(outputTokens) / 1_000_000 * profile.Pricing.OutputPerMTok
	return in + out
}

// SecondsUntilUTCMidnight reports how long until the daily budget
// resets. Surfaced via /api/llm_budget so the UI can render the
// reset countdown without doing math itself.
func SecondsUntilUTCMidnight(now time.Time) int {
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	d := tomorrow.Sub(now.UTC())
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}

