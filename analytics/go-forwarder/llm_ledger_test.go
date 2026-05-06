package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubCH simulates ClickHouse for ledger ops with scripted responses
// per query type. Records inserts so tests can verify what was sent.
type stubCH struct {
	srv             *httptest.Server
	mu              sync.Mutex
	spentResponse   string // returned for sum(cost_usd) queries
	countResponse   string // returned for count() queries
	insertedBodies  [][]byte
	insertStatus    int    // 200 by default
	queryFailStatus int    // if > 0, fail SELECT queries with this
}

func (s *stubCH) Close() { s.srv.Close() }
func (s *stubCH) URL() string { return s.srv.URL }
func (s *stubCH) Inserts() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.insertedBodies))
	for i, b := range s.insertedBodies {
		dup := make([]byte, len(b))
		copy(dup, b)
		out[i] = dup
	}
	return out
}

func newStubCH(t *testing.T) *stubCH {
	t.Helper()
	s := &stubCH{spentResponse: "0", countResponse: "0", insertStatus: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		switch {
		case strings.Contains(query, "INSERT INTO") && strings.Contains(query, "llm_calls"):
			body, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.insertedBodies = append(s.insertedBodies, body)
			s.mu.Unlock()
			w.WriteHeader(s.insertStatus)
		case strings.Contains(query, "sum(cost_usd)"):
			if s.queryFailStatus > 0 {
				http.Error(w, "stub fail", s.queryFailStatus)
				return
			}
			_, _ = io.WriteString(w, s.spentResponse+"\n")
		case strings.Contains(query, "count()"):
			if s.queryFailStatus > 0 {
				http.Error(w, "stub fail", s.queryFailStatus)
				return
			}
			_, _ = io.WriteString(w, s.countResponse+"\n")
		default:
			http.Error(w, "unhandled stub query: "+query, http.StatusBadRequest)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func TestLedger_WriteCall_HappyPath(t *testing.T) {
	ch := newStubCH(t)
	l := NewLLMLedger(ch.URL(), "infinite_streaming")
	err := l.WriteCall(context.Background(), LLMCallRecord{
		SessionID:    "sess-1",
		Profile:      "p",
		Model:        "m",
		Status:       statusOK,
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.0015,
	})
	if err != nil {
		t.Fatalf("WriteCall: %v", err)
	}
	inserts := ch.Inserts()
	if len(inserts) != 1 {
		t.Fatalf("got %d inserts, want 1", len(inserts))
	}
	if !strings.Contains(string(inserts[0]), `"session_id":"sess-1"`) {
		t.Errorf("insert payload missing session_id: %s", inserts[0])
	}
	if !strings.Contains(string(inserts[0]), `"cost_usd":0.0015`) {
		t.Errorf("insert payload missing cost_usd: %s", inserts[0])
	}
}

func TestLedger_WriteCall_PropagatesNon2xx(t *testing.T) {
	ch := newStubCH(t)
	ch.insertStatus = http.StatusBadRequest
	l := NewLLMLedger(ch.URL(), "infinite_streaming")
	err := l.WriteCall(context.Background(), LLMCallRecord{Status: statusOK})
	if err == nil {
		t.Fatal("expected error from 400 response")
	}
}

func TestLedger_TodaysSpendUSD(t *testing.T) {
	ch := newStubCH(t)
	ch.spentResponse = "2.345"
	l := NewLLMLedger(ch.URL(), "infinite_streaming")
	got, err := l.TodaysSpendUSD(context.Background())
	if err != nil {
		t.Fatalf("TodaysSpendUSD: %v", err)
	}
	if got != 2.345 {
		t.Errorf("got %f, want 2.345", got)
	}
}

func TestLedger_TodaysSpend_EmptyResultIsZero(t *testing.T) {
	ch := newStubCH(t)
	ch.spentResponse = ""
	l := NewLLMLedger(ch.URL(), "infinite_streaming")
	got, err := l.TodaysSpendUSD(context.Background())
	if err != nil {
		t.Fatalf("TodaysSpendUSD: %v", err)
	}
	if got != 0 {
		t.Errorf("got %f, want 0", got)
	}
}

func TestLedger_CallsTodayCount(t *testing.T) {
	ch := newStubCH(t)
	ch.countResponse = "42"
	l := NewLLMLedger(ch.URL(), "infinite_streaming")
	got, err := l.CallsTodayCount(context.Background())
	if err != nil {
		t.Fatalf("CallsTodayCount: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestEstimateInputTokens(t *testing.T) {
	msgs := []openaiMessageLike{
		{Role: "user", Content: "abcd"},     // 4+4 = 8 chars
		{Role: "assistant", Content: "abcd"}, // 9+4 = 13 chars
	}
	got := EstimateInputTokens(msgs, "tools")
	want := (8 + 13 + 5) / 4 // 26 / 4 = 6
	if got != want {
		t.Errorf("EstimateInputTokens = %d, want %d", got, want)
	}
}

func TestCostUSD(t *testing.T) {
	prof := &LLMProfile{Pricing: Pricing{InputPerMTok: 15, OutputPerMTok: 75}}
	got := CostUSD(prof, 100_000, 10_000)
	want := 0.1*15 + 0.01*75 // 1.5 + 0.75 = 2.25
	if got != want {
		t.Errorf("CostUSD = %f, want %f", got, want)
	}
}

func TestCostUSD_NilProfile(t *testing.T) {
	if got := CostUSD(nil, 1000, 1000); got != 0 {
		t.Errorf("got %f, want 0 for nil profile", got)
	}
}

func TestSecondsUntilUTCMidnight(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	got := SecondsUntilUTCMidnight(now)
	want := 12 * 3600
	if got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

// Integration tests for the chat handler's budget guards.

func TestSessionChat_BudgetExceeded_429(t *testing.T) {
	llmSrv := newScriptedLLM(t /* no scripted responses needed; should refuse */)
	stub := newStubCH(t)
	stub.spentResponse = "10.0" // over default $5 cap

	const envName = "LLM_BUDGET_TEST_KEY"
	t.Setenv(envName, "k")
	prev := llmProfiles
	t.Cleanup(func() { llmProfiles = prev })
	llmProfiles = &LLMProfiles{
		Active: "p",
		Profiles: map[string]*LLMProfile{
			"p": {Name: "p", BaseURL: llmSrv.URL() + "/", APIKeyEnv: envName, Model: "m"},
		},
	}
	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{
		clickhouseURL:     stub.URL(),
		chDatabase:        "infinite_streaming",
		llmDailyBudgetUSD: 5.0,
		llmMaxInputTokens: defaultMaxInputTokensPerCall,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/session_chat", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("missing Retry-After header")
	}
	// The budget-exceeded refusal should have written a ledger row.
	if len(stub.Inserts()) != 1 {
		t.Errorf("expected 1 ledger insert (the refusal), got %d", len(stub.Inserts()))
	}
	if !strings.Contains(string(stub.Inserts()[0]), `"status":"budget_exceeded"`) {
		t.Errorf("refusal ledger row missing status=budget_exceeded: %s", stub.Inserts()[0])
	}
}

func TestSessionChat_InputTooLarge_413(t *testing.T) {
	llmSrv := newScriptedLLM(t)
	stub := newStubCH(t)

	const envName = "LLM_INPUT_TEST_KEY"
	t.Setenv(envName, "k")
	prev := llmProfiles
	t.Cleanup(func() { llmProfiles = prev })
	llmProfiles = &LLMProfiles{
		Active: "p",
		Profiles: map[string]*LLMProfile{
			"p": {Name: "p", BaseURL: llmSrv.URL() + "/", APIKeyEnv: envName, Model: "m"},
		},
	}
	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{
		clickhouseURL:     stub.URL(),
		chDatabase:        "infinite_streaming",
		llmDailyBudgetUSD: 5.0,
		llmMaxInputTokens: 10, // tiny cap so any real message trips it
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	bigContent := strings.Repeat("hello world ", 100) // ~1200 chars / 4 = ~300 tokens
	resp, err := http.Post(srv.URL+"/api/session_chat", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"`+bigContent+`"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	if len(stub.Inserts()) != 1 {
		t.Errorf("expected 1 ledger insert (refusal), got %d", len(stub.Inserts()))
	}
	if !strings.Contains(string(stub.Inserts()[0]), `"status":"input_too_large"`) {
		t.Errorf("refusal ledger row missing status=input_too_large: %s", stub.Inserts()[0])
	}
}

func TestSessionChat_OK_WritesLedgerRow(t *testing.T) {
	llmSrv := newScriptedLLM(t, textResp("hi", 100, 7))
	stub := newStubCH(t) // tracks inserts so we can verify status=ok shape

	const envName = "LLM_OK_LEDGER_TEST"
	t.Setenv(envName, "k")
	prev := llmProfiles
	t.Cleanup(func() { llmProfiles = prev })
	llmProfiles = &LLMProfiles{
		Active: "p",
		Profiles: map[string]*LLMProfile{
			"p": {
				Name: "p", BaseURL: llmSrv.URL() + "/", APIKeyEnv: envName, Model: "m",
				Pricing: Pricing{InputPerMTok: 1.0, OutputPerMTok: 2.0},
			},
		},
	}
	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{
		clickhouseURL:     stub.URL(),
		chDatabase:        "infinite_streaming",
		llmDailyBudgetUSD: 5.0,
		llmMaxInputTokens: defaultMaxInputTokensPerCall,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := `{"profile":"p","session_id":"s1","messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(srv.URL+"/api/session_chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}

	inserts := stub.Inserts()
	if len(inserts) != 1 {
		t.Fatalf("got %d ledger inserts, want 1", len(inserts))
	}
	row := string(inserts[0])
	for _, want := range []string{
		`"status":"ok"`,
		`"session_id":"s1"`,
		`"profile":"p"`,
		`"input_tokens":100`,
		`"output_tokens":7`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("ledger row missing %q; got: %s", want, row)
		}
	}
	// cost_usd should be 100/1M*1 + 7/1M*2 = 0.0001 + 0.000014 = 0.000114
	if !strings.Contains(row, `"cost_usd":0.000114`) && !strings.Contains(row, `"cost_usd":0.0001140`) {
		t.Errorf("cost_usd not computed correctly; row: %s", row)
	}
}

func TestLLMBudget_Endpoint(t *testing.T) {
	llmSrv := newScriptedLLM(t)
	stub := newStubCH(t)
	stub.spentResponse = "1.234"
	stub.countResponse = "17"

	const envName = "LLM_BUDGET_ENDPOINT_TEST"
	t.Setenv(envName, "k")
	prev := llmProfiles
	t.Cleanup(func() { llmProfiles = prev })
	llmProfiles = &LLMProfiles{
		Active: "p",
		Profiles: map[string]*LLMProfile{
			"p": {Name: "p", BaseURL: llmSrv.URL() + "/", APIKeyEnv: envName, Model: "m"},
		},
	}
	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{
		clickhouseURL:     stub.URL(),
		chDatabase:        "infinite_streaming",
		llmDailyBudgetUSD: 5.0,
		llmMaxInputTokens: defaultMaxInputTokensPerCall,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/llm_budget")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`"spent_usd":1.234`, `"calls_today":17`, `"cap_usd":5`, `"resets_at"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("response missing %q; body=%s", want, body)
		}
	}
}

func TestLLMBudget_RejectsPOST(t *testing.T) {
	stub := newStubCH(t)
	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{
		clickhouseURL:     stub.URL(),
		chDatabase:        "infinite_streaming",
		llmDailyBudgetUSD: 5.0,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/llm_budget", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
