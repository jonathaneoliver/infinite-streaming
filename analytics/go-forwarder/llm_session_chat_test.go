package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

// sseEvent is one decoded SSE event (event-name + data payload).
type sseEvent struct {
	Event string
	Data  []byte
}

// readSSE drains an SSE response body into a slice of decoded events.
// Stops at io.EOF or when the data line containing "done" appears.
func readSSE(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	out := []sseEvent{}
	cur := sseEvent{}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = []byte(strings.TrimPrefix(line, "data: "))
		case line == "":
			if cur.Event != "" || len(cur.Data) > 0 {
				out = append(out, cur)
				if cur.Event == "done" {
					return out
				}
				cur = sseEvent{}
			}
		}
	}
	return out
}

// chatTestSetup builds a mux with registerLLMHandlers wired to a
// fake LLM (scriptedLLM) and a fake ClickHouse for the QueryTool.
// Returns an httptest.Server fronting the mux + accessor for
// recorded LLM bodies.
type chatTestSetup struct {
	llmServer *scriptedLLM
	chServer  *httptest.Server
	muxServer *httptest.Server
}

func (s *chatTestSetup) Close() {
	if s.chServer != nil {
		s.chServer.Close()
	}
	if s.muxServer != nil {
		s.muxServer.Close()
	}
}

func newChatTestSetup(t *testing.T, scriptedResponses []openai.ChatCompletionResponse, chJSON string) *chatTestSetup {
	t.Helper()
	llmSrv := newScriptedLLM(t, scriptedResponses...)
	chSrv := fakeClickHouse(t, http.StatusOK, chJSON)

	const envName = "LLM_SESSION_CHAT_TEST_KEY"
	t.Setenv(envName, "test-key")

	prevProfiles := llmProfiles
	t.Cleanup(func() { llmProfiles = prevProfiles })
	llmProfiles = &LLMProfiles{
		Active: "test",
		Profiles: map[string]*LLMProfile{
			"test": {
				Name:      "test",
				BaseURL:   llmSrv.URL() + "/",
				APIKeyEnv: envName,
				Model:     "test-model",
			},
		},
	}

	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{clickhouseURL: chSrv.URL})

	muxSrv := httptest.NewServer(mux)
	t.Cleanup(muxSrv.Close)

	return &chatTestSetup{
		llmServer: llmSrv,
		chServer:  chSrv,
		muxServer: muxSrv,
	}
}

func TestSessionChat_HappyPath_NoToolCalls(t *testing.T) {
	s := newChatTestSetup(t,
		[]openai.ChatCompletionResponse{textResp("the answer is 42", 100, 7)},
		`{"meta":[],"data":[]}`,
	)

	body := `{"profile":"test","messages":[{"role":"user","content":"what's the answer?"}]}`
	resp, err := http.Post(s.muxServer.URL+"/api/session_chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q", got)
	}

	events := readSSE(t, resp.Body)
	wantEvents := []string{"assistant_message", "usage", "done"}
	if len(events) < len(wantEvents) {
		t.Fatalf("got %d events, want >= %d: %+v", len(events), len(wantEvents), events)
	}
	for i, want := range wantEvents {
		if events[i].Event != want {
			t.Errorf("events[%d].Event = %q, want %q", i, events[i].Event, want)
		}
	}
	var deltaPayload map[string]any
	_ = json.Unmarshal(events[0].Data, &deltaPayload)
	if deltaPayload["content"] != "the answer is 42" {
		t.Errorf("assistant_message content = %v", deltaPayload["content"])
	}
}

func TestSessionChat_ToolCallSequence(t *testing.T) {
	s := newChatTestSetup(t,
		[]openai.ChatCompletionResponse{
			toolCallResp("c1", "query", `{"sql":"SELECT 1"}`, 50, 10),
			textResp("looked it up: one", 60, 5),
		},
		`{"meta":[{"name":"v","type":"UInt8"}],"data":[[1]]}`,
	)

	body := `{"profile":"test","session_id":"abc","messages":[{"role":"user","content":"check"}]}`
	resp, err := http.Post(s.muxServer.URL+"/api/session_chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	events := readSSE(t, resp.Body)
	got := []string{}
	for _, e := range events {
		got = append(got, e.Event)
	}
	want := []string{"tool_call", "tool_result", "assistant_message", "usage", "done"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("event sequence = %v, want %v", got, want)
	}

	// tool_call event should carry the SQL.
	var tc map[string]any
	_ = json.Unmarshal(events[0].Data, &tc)
	if tc["name"] != "query" {
		t.Errorf("tool_call name = %v", tc["name"])
	}
	if !strings.Contains(tc["arguments"].(string), "SELECT 1") {
		t.Errorf("tool_call arguments = %v, expected to carry SQL", tc["arguments"])
	}

	// tool_result event should carry row count from fake ClickHouse.
	var tr map[string]any
	_ = json.Unmarshal(events[1].Data, &tr)
	if got := int(tr["rows"].(float64)); got != 1 {
		t.Errorf("tool_result rows = %d, want 1", got)
	}
}

func TestSessionChat_RejectsNonPOST(t *testing.T) {
	s := newChatTestSetup(t, nil, `{"meta":[],"data":[]}`)
	resp, err := http.Get(s.muxServer.URL + "/api/session_chat")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestSessionChat_RejectsMissingMessages(t *testing.T) {
	s := newChatTestSetup(t, nil, `{"meta":[],"data":[]}`)
	resp, err := http.Post(s.muxServer.URL+"/api/session_chat", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSessionChat_RejectsBadJSON(t *testing.T) {
	s := newChatTestSetup(t, nil, `{"meta":[],"data":[]}`)
	resp, err := http.Post(s.muxServer.URL+"/api/session_chat", "application/json", strings.NewReader(`<<<`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestLLMProfiles_GET_ReturnsList(t *testing.T) {
	s := newChatTestSetup(t, nil, `{"meta":[],"data":[]}`)
	resp, err := http.Get(s.muxServer.URL + "/api/llm_profiles")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Enabled  bool                `json:"enabled"`
		Profiles []llmProfileSummary `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Enabled {
		t.Errorf("enabled should be true when profiles loaded")
	}
	if len(body.Profiles) != 1 {
		t.Fatalf("got %d profiles, want 1", len(body.Profiles))
	}
	if body.Profiles[0].Name != "test" {
		t.Errorf("profile name = %q", body.Profiles[0].Name)
	}
	if !body.Profiles[0].Active {
		t.Errorf("test profile should be Active")
	}
}

func TestLLMProfiles_RejectsPOST(t *testing.T) {
	s := newChatTestSetup(t, nil, `{"meta":[],"data":[]}`)
	resp, err := http.Post(s.muxServer.URL+"/api/llm_profiles", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestRegisterLLMHandlers_DisabledWhenNoProfiles(t *testing.T) {
	prev := llmProfiles
	t.Cleanup(func() { llmProfiles = prev })
	llmProfiles = nil

	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{clickhouseURL: "http://unused"})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Profiles endpoint stays alive but reports enabled=false so the
	// UI can render a "feature off" state without ambiguity.
	resp, err := http.Get(srv.URL + "/api/llm_profiles")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Enabled {
		t.Errorf("enabled = true with nil profiles, want false")
	}

	// Chat endpoint is NOT registered when profiles are nil.
	resp2, err := http.Post(srv.URL+"/api/session_chat", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("session_chat status = %d, want 404 (not registered)", resp2.StatusCode)
	}
}

func TestSessionChat_InjectsSessionContext(t *testing.T) {
	s := newChatTestSetup(t,
		[]openai.ChatCompletionResponse{textResp("ok", 5, 5)},
		`{"meta":[],"data":[]}`,
	)
	body := `{"profile":"test","session_id":"my-session-xyz","range":{"from":1000,"to":2000},"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(s.muxServer.URL+"/api/session_chat", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	bodies := s.llmServer.Bodies()
	if len(bodies) == 0 {
		t.Fatal("LLM saw no bodies")
	}
	if !bytes.Contains(bodies[0], []byte("my-session-xyz")) {
		t.Errorf("session_id not injected into preamble: %s", bodies[0])
	}
	if !bytes.Contains(bodies[0], []byte("Focus range")) {
		t.Errorf("range not injected into preamble: %s", bodies[0])
	}
}

// Verifies the heartbeat ticker fires while the upstream LLM is
// slow, so nginx / corporate proxies don't time out the connection
// during long tool runs.
func TestSessionChat_HeartbeatDuringSlowUpstream(t *testing.T) {
	// LLM that takes ~250ms to respond. With a heartbeat interval
	// override of ~80ms (test-only), we expect at least 2 keepalive
	// comments before the assistant_message arrives.
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(textResp("done", 5, 5))
	}))
	t.Cleanup(slowSrv.Close)

	const envName = "LLM_HB_TEST_KEY"
	t.Setenv(envName, "k")
	prev := llmProfiles
	t.Cleanup(func() { llmProfiles = prev })
	llmProfiles = &LLMProfiles{
		Active: "s",
		Profiles: map[string]*LLMProfile{
			"s": {Name: "s", BaseURL: slowSrv.URL + "/", APIKeyEnv: envName, Model: "m"},
		},
	}
	mux := http.NewServeMux()
	registerLLMHandlers(mux, config{clickhouseURL: "http://unused"})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Override heartbeat interval via the seam below — simplest to
	// monkey-patch by swapping the function. Not ideal, but the
	// alternative is plumbing yet another option through the handler.
	prevInterval := heartbeatIntervalForTest
	heartbeatIntervalForTest = 80 * time.Millisecond
	t.Cleanup(func() { heartbeatIntervalForTest = prevInterval })

	resp, err := http.Post(srv.URL+"/api/session_chat", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"x"}]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	keepaliveCount := strings.Count(string(body), ": keepalive\n\n")
	if keepaliveCount < 2 {
		t.Errorf("expected ≥2 keepalive comments during 250ms upstream, got %d. Body:\n%s", keepaliveCount, body)
	}
}

// Cancellation propagation in production is wired via a CloseNotifier
// watchdog in handleSessionChat (Go's HTTP/1.1 r.Context() doesn't
// proactively cancel on client disconnect for handlers blocked on
// upstream calls without active writes). End-to-end timing under
// httptest is unreliable — connection-close detection depends on TCP
// timing and httptest's transport layer behaves differently from
// production nginx → forwarder. The code path is in place; manual
// verification: deploy, open a chat, kill the browser tab, check
// `llm_calls.status='cancelled'` in the ledger (#417).
