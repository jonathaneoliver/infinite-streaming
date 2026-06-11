package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Player groups (#579 / fleet Phase 2). A group is a server-side tag: shaping
// ANY member (rate, pattern, or fault) is auto-broadcast by the proxy to every
// member (go-proxy handlers_mutate BroadcastPatch). The fleet runner uses this
// to drive one pyramid that lands identically on every sim — see CreateGroup +
// the CHAR_FLEET_GROUP path in the pyramid mode.

func groupHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: HarnessInsecure}},
	}
}

// CreateGroup creates a player-group over the given member player_ids and
// returns the server-allocated group_id. Members must already be connected
// (heartbeating) — the proxy 409s if none are known — so call this after the
// fleet has bound at a barrier. label is surfaced in the dashboard.
func CreateGroup(ctx context.Context, label string, playerIDs []string) (string, error) {
	if len(playerIDs) == 0 {
		return "", fmt.Errorf("create group: no member player_ids")
	}
	body, err := json.Marshal(map[string]any{
		"label":             label,
		"member_player_ids": playerIDs,
	})
	if err != nil {
		return "", fmt.Errorf("create group: marshal: %w", err)
	}
	url := strings.TrimRight(bootstrapBaseURL(), "/") + "/api/v2/player-groups"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := groupHTTPClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("create group: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("create group: %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	// Prefer the body's group_id; fall back to the Location header.
	var rec struct {
		GroupID string `json:"group_id"`
	}
	if err := json.Unmarshal(raw, &rec); err == nil && rec.GroupID != "" {
		return rec.GroupID, nil
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if i := strings.LastIndex(loc, "/"); i >= 0 {
			return loc[i+1:], nil
		}
	}
	return "", fmt.Errorf("create group: no group_id in response: %s", strings.TrimSpace(string(raw)))
}

// DisbandGroup deletes a player-group (the member players themselves stay).
// Best-effort cleanup.
func DisbandGroup(ctx context.Context, groupID string) error {
	if groupID == "" {
		return nil
	}
	url := strings.TrimRight(bootstrapBaseURL(), "/") + "/api/v2/player-groups/" + groupID
	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	resp, err := groupHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("disband group: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("disband group: %d", resp.StatusCode)
	}
	return nil
}
