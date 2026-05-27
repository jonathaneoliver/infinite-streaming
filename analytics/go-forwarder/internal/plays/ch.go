package plays

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// queryRows runs a JSONEachRow query and parses the rows. Mirrors
// the main package's `queryClickHouseRows` — kept private here to
// avoid coupling the two helper sets.
func (b Backend) queryRows(ctx context.Context, query string, params map[string]string) ([]map[string]any, error) {
	u, err := url.Parse(b.ClickHouseURL)
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	qs.Set("query", query)
	qs.Set("default_format", "JSONEachRow")
	for k, v := range params {
		qs.Set("param_"+k, v)
	}
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if b.User != "" {
		req.SetBasicAuth(b.User, b.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out []map[string]any
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
	return out, scanner.Err()
}

// queryBytes is the bytes-back version — used by mutations (ALTER
// UPDATE) and probes that read a single scalar. Mirrors the main
// package's `chQueryBytes`.
func (b Backend) queryBytes(ctx context.Context, query string, params map[string]string) ([]byte, error) {
	u, err := url.Parse(b.ClickHouseURL)
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	qs.Set("default_format", "JSONEachRow")
	for k, v := range params {
		qs.Set("param_"+k, v)
	}
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	if b.User != "" {
		req.SetBasicAuth(b.User, b.Password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
