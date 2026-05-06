package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeClickHouse impersonates ClickHouse's HTTP interface enough to
// exercise the QueryTool's parsing + truncation paths.
func fakeClickHouse(t *testing.T, status int, jsonBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the user header is set so the LLM gets the
		// constrained role-not the privileged default.
		if got := r.Header.Get("X-ClickHouse-User"); got != "llm_reader" {
			t.Errorf("X-ClickHouse-User = %q, want llm_reader", got)
		}
		// Body should be the SQL.
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "SELECT") {
			t.Errorf("body did not contain SELECT: %q", string(body))
		}
		// Verify default_format=JSON in the URL.
		if !strings.Contains(r.URL.RawQuery, "default_format=JSON") {
			t.Errorf("query missing default_format=JSON: %q", r.URL.RawQuery)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, jsonBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQueryTool_HappyPath(t *testing.T) {
	body := `{
		"meta": [{"name":"n","type":"UInt64"},{"name":"label","type":"String"}],
		"data": [[1,"a"],[2,"b"]]
	}`
	srv := fakeClickHouse(t, http.StatusOK, body)
	qt := NewQueryTool(srv.URL)

	res := qt.Run(context.Background(), "SELECT n, label FROM t LIMIT 2")
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if !equalStrings(res.Columns, []string{"n", "label"}) {
		t.Errorf("Columns = %v, want [n label]", res.Columns)
	}
	if len(res.Rows) != 2 {
		t.Errorf("len(Rows) = %d, want 2", len(res.Rows))
	}
	if res.Truncated {
		t.Errorf("Truncated = true, want false")
	}
	if res.ElapsedMS < 0 {
		t.Errorf("ElapsedMS = %d", res.ElapsedMS)
	}
}

func TestQueryTool_ErrorStatusFromClickHouse(t *testing.T) {
	srv := fakeClickHouse(t, http.StatusBadRequest, "Code: 159. DB::Exception: Timeout exceeded")
	qt := NewQueryTool(srv.URL)
	res := qt.Run(context.Background(), "SELECT 1")
	if res.Error == "" {
		t.Fatal("expected error from non-200 response")
	}
	if !strings.Contains(res.Error, "Timeout") {
		t.Errorf("error did not pass through clickhouse text: %q", res.Error)
	}
}

func TestQueryTool_TruncatesAt10kRows(t *testing.T) {
	// Build a JSON body with > 10k rows so the forwarder applies
	// its hard cap (server-side max_result_rows is cooperative).
	const n = 12000
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{i}
	}
	bodyMap := map[string]any{
		"meta": []map[string]string{{"name": "n", "type": "UInt64"}},
		"data": rows,
	}
	bodyBytes, _ := json.Marshal(bodyMap)
	srv := fakeClickHouse(t, http.StatusOK, string(bodyBytes))
	qt := NewQueryTool(srv.URL)

	res := qt.Run(context.Background(), "SELECT n FROM huge")
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	if len(res.Rows) != queryToolMaxRows {
		t.Errorf("len(Rows) = %d, want %d (cap)", len(res.Rows), queryToolMaxRows)
	}
	if !res.Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestQueryTool_MalformedJSONReturnsErr(t *testing.T) {
	srv := fakeClickHouse(t, http.StatusOK, "<<not-json>>")
	qt := NewQueryTool(srv.URL)
	res := qt.Run(context.Background(), "SELECT 1")
	if res.Error == "" {
		t.Fatal("expected parse error for non-JSON body")
	}
	if !strings.Contains(res.Error, "parse json") {
		t.Errorf("error didn't tag as parse error: %q", res.Error)
	}
}

func TestQueryTool_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	qt := NewQueryTool(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := qt.Run(ctx, "SELECT 1")
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("Run did not respect outer ctx; took %v", elapsed)
	}
	if res.Error == "" {
		t.Errorf("expected error from cancelled ctx")
	}
}

func TestQueryToolResult_MarshalForTool(t *testing.T) {
	r := &QueryToolResult{
		Rows:      [][]any{{1, "a"}},
		Columns:   []string{"n", "label"},
		ElapsedMS: 7,
	}
	out := r.MarshalForTool()
	if !strings.Contains(out, `"columns":["n","label"]`) {
		t.Errorf("output missing columns: %q", out)
	}
	if !strings.Contains(out, `"elapsed_ms":7`) {
		t.Errorf("output missing elapsed_ms: %q", out)
	}
}

func TestQueryTool_OpenAIToolSchema(t *testing.T) {
	qt := NewQueryTool("http://unused")
	tool := qt.OpenAITool()
	if tool.Function.Name != "query" {
		t.Errorf("Name = %q, want query", tool.Function.Name)
	}
	params, ok := tool.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters not a map: %T", tool.Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type")
	}
	if _, ok := props["sql"]; !ok {
		t.Errorf("missing sql property")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
