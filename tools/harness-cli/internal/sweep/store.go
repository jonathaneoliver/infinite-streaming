package sweep

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ErrNotFound is returned when an experiment isn't present (in a status).
var ErrNotFound = errors.New("sweep: experiment not found")

// Store is the ClickHouse-backed sweep queue, reached over the forwarder API.
// ClickHouse is the MASTER store (#772 CH-master migration) — there is no local
// .sweep/ file store. Concurrency (the claim race) is resolved server-side by
// the /claim endpoint, so the CLI never holds a lock.
type Store struct {
	base  string // deploy root, e.g. https://host:21000
	httpc *http.Client
	auth  string // "user:pass" or ""
}

// OpenCH builds a CH-backed Store from the harness's API connection.
func OpenCH(base string, httpc *http.Client, auth string) *Store {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &Store{base: strings.TrimRight(base, "/"), httpc: httpc, auth: auth}
}

// Label for human-facing messages (replaces the old .sweep/ Root path).
func (s *Store) Label() string { return s.base + " (clickhouse)" }

func (s *Store) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+"/analytics/api/v2/sweep"+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.auth != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(s.auth)))
	}
	resp, err := s.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("sweep api %s %s: %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// expRow is the upsert wire shape: the queryable/display columns + the full
// serialized Experiment in raw_json (the recipe of record the runner replays).
type expRow struct {
	ExpID     string  `json:"exp_id"`
	Class     string  `json:"class"`
	Status    string  `json:"status"`
	Kind      string  `json:"kind"`
	Platform  string  `json:"platform"`
	Protocol  string  `json:"protocol"`
	Mode      string  `json:"mode"`
	Recipe    string  `json:"recipe"`
	Arm       string  `json:"arm"`
	GroupID   string  `json:"group_id"`
	Parent    string  `json:"parent"`
	Depth     int     `json:"depth"`
	Why       string  `json:"why"`
	WhyText   string  `json:"why_text"`
	Verdict   string  `json:"verdict"`
	PlayerID  string  `json:"player_id"`
	PlayID    string  `json:"play_id"`
	Owner     string  `json:"owner"`
	ClaimedAt string  `json:"claimed_at"`
	RawJSON   string  `json:"raw_json"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"`
}

// rowOf maps an Experiment to its wire row for a given status. recipe / verdict /
// score are derived (mirrors the old `publish`); raw_json carries everything.
func rowOf(st Status, e *Experiment) (expRow, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return expRow{}, fmt.Errorf("sweep: marshal %s: %w", e.ID, err)
	}
	verdict := ""
	if e.Result != nil {
		verdict = string(e.Result.Verdict)
	}
	return expRow{
		ExpID: e.ID, Class: string(e.ClassOrDefault()), Status: string(st), Kind: string(e.Kind),
		Platform: e.Platform, Protocol: e.Protocol, Mode: e.Mode, Recipe: RecipeSlug(e), Arm: string(e.Arm),
		GroupID: e.Group, Parent: e.Parent, Depth: e.Depth, Why: e.Why, WhyText: e.WhyText, Verdict: verdict,
		PlayerID: e.PlayerID, PlayID: e.PlayID, Owner: e.Owner, ClaimedAt: e.ClaimedAt,
		RawJSON: string(raw), Score: DefaultWeights().Score(e), CreatedAt: e.CreatedAt,
	}, nil
}

// Save upserts e into CH with the given status. (ReplacingMergeTree by exp_id, so
// this both creates and transitions.)
func (s *Store) Save(st Status, e *Experiment) error {
	row, err := rowOf(st, e)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"experiments": []expRow{row}})
	_, err = s.do(context.Background(), http.MethodPost, "/experiments", body)
	return err
}

// List returns every experiment in a status (or all, if st == ""), reconstructed
// from raw_json and sorted by id.
func (s *Store) List(st Status) ([]*Experiment, error) {
	path := "/experiments?limit=2000"
	if st != "" {
		path += "&status=" + string(st)
	}
	body, err := s.do(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("sweep: decode list: %w", err)
	}
	out := make([]*Experiment, 0, len(resp.Items))
	for _, it := range resp.Items {
		var row struct {
			RawJSON   string `json:"raw_json"`
			Owner     string `json:"owner"`
			ClaimedAt string `json:"claimed_at"`
		}
		if json.Unmarshal(it, &row) != nil || row.RawJSON == "" {
			continue
		}
		var e Experiment
		if json.Unmarshal([]byte(row.RawJSON), &e) != nil {
			continue
		}
		// owner / claimed_at are stamped on the CH columns at claim time (the
		// server promote doesn't rewrite raw_json), so the columns are
		// authoritative for those runtime fields — without this, reap would see
		// an empty ClaimedAt and yank a live claim (treated as stale).
		if row.Owner != "" {
			e.Owner = row.Owner
		}
		if t, err := time.Parse("2006-01-02 15:04:05.000", row.ClaimedAt); err == nil && t.Year() > 1971 {
			e.ClaimedAt = t.UTC().Format(time.RFC3339)
		}
		out = append(out, &e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Load returns one experiment from a status, or ErrNotFound.
func (s *Store) Load(st Status, id string) (*Experiment, error) {
	es, err := s.List(st)
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, ErrNotFound
}

// Counts tallies experiments by status (one list call, grouped client-side).
func (s *Store) Counts() (map[Status]int, error) {
	body, err := s.do(context.Background(), http.MethodGet, "/experiments?limit=2000", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Items []struct {
			Status string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	counts := make(map[Status]int, len(AllStatuses))
	for _, st := range AllStatuses {
		counts[st] = 0
	}
	for _, it := range resp.Items {
		counts[Status(it.Status)]++
	}
	return counts, nil
}

// ClaimNext atomically claims the top-scored eligible backlog experiment for
// owner (server-side concurrency-safe claim). Returns (nil, nil) when the
// backlog is empty / fully scope-gated. The returned Experiment is already
// marked running in CH with owner + claim time stamped.
func (s *Store) ClaimNext(owner string) (*Experiment, error) {
	body, _ := json.Marshal(map[string]string{"owner": owner})
	out, err := s.do(context.Background(), http.MethodPost, "/claim", body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Experiment json.RawMessage `json:"experiment"`
		Owner      string          `json:"owner"`
		ClaimedAt  string          `json:"claimed_at"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("sweep: decode claim: %w", err)
	}
	if len(resp.Experiment) == 0 || string(resp.Experiment) == "null" {
		return nil, nil
	}
	var e Experiment
	if err := json.Unmarshal(resp.Experiment, &e); err != nil {
		return nil, fmt.Errorf("sweep: decode claimed experiment: %w", err)
	}
	e.Owner = resp.Owner
	e.ClaimedAt = resp.ClaimedAt
	return &e, nil
}

// Move transitions e to the `to` status — an upsert (CH is keyed by exp_id, so
// there is no physical move and `from` is informational).
func (s *Store) Move(from, to Status, e *Experiment) error {
	return s.Save(to, e)
}

// Delete tombstones an experiment (status='deleted'); list/claim ignore it.
func (s *Store) Delete(id string) error {
	body, _ := json.Marshal(map[string]string{"exp_id": id})
	_, err := s.do(context.Background(), http.MethodPost, "/delete", body)
	return err
}
