package aberration_crawl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CHClient is a minimal ClickHouse HTTP client (JSONEachRow), same
// shape as the forwarder's llm_tool_query.go runSelect.
type CHClient struct {
	URL      string
	Database string
	User     string
	Password string
	HTTP     *http.Client
}

// NewCHClientFromEnv reads ABERRATION_CH_URL (default
// http://127.0.0.1:21123 — test-dev's loopback-bound CH; tunnel with
// `ssh -L 21123:127.0.0.1:21123 $TEST_SSH`), ABERRATION_CH_USER,
// ABERRATION_CH_PASSWORD, ABERRATION_DB (default infinite_streaming).
func NewCHClientFromEnv() *CHClient {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return &CHClient{
		URL:      get("ABERRATION_CH_URL", "http://127.0.0.1:21123"),
		Database: get("ABERRATION_DB", "infinite_streaming"),
		User:     os.Getenv("ABERRATION_CH_USER"),
		Password: os.Getenv("ABERRATION_CH_PASSWORD"),
		HTTP:     &http.Client{Timeout: 120 * time.Second},
	}
}

// Query runs sql and returns JSONEachRow-decoded rows. Numbers decode
// as json.Number so UInt64 counters survive intact.
func (c *CHClient) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	qs.Set("default_format", "JSONEachRow")
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(sql))
	if err != nil {
		return nil, err
	}
	if c.User != "" {
		req.SetBasicAuth(c.User, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []map[string]any
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(string(line)))
		dec.UseNumber()
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			continue
		}
		out = append(out, row)
	}
	return out, scanner.Err()
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	case string:
		var i int64
		fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
