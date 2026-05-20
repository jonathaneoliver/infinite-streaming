package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// HarnessBin is the harness CLI binary the framework shells out to. Override
// via HARNESS_BIN env var. Default expects the binary on $PATH (installed by
// `make harness-cli`).
var HarnessBin = func() string {
	if v := os.Getenv("HARNESS_BIN"); v != "" {
		return v
	}
	return "harness"
}()

// HarnessInsecure adds --insecure to every call when set. Required against
// test-dev's self-signed cert; harmless otherwise. Default true.
var HarnessInsecure = os.Getenv("HARNESS_INSECURE") != "0"

// PlayerRecord is the subset of the v2 player record the framework reads.
// The harness JSON is the source of truth — extend this struct as new
// fields are needed rather than parsing into map[string]any everywhere.
type PlayerRecord struct {
	ID         string     `json:"id"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	UserAgent  string     `json:"user_agent"`
	Labels     map[string]string `json:"labels"`
	CurrentPlay *struct {
		ID       string `json:"id"`
		Manifest struct {
			MasterURL string `json:"master_url"`
			Variants  []struct {
				Bandwidth        int    `json:"bandwidth"`
				AverageBandwidth int    `json:"average_bandwidth"`
				Resolution       string `json:"resolution"`
				URL              string `json:"url"`
			} `json:"variants"`
		} `json:"manifest"`
	} `json:"current_play"`
	PlayerMetrics *struct {
		State                  string  `json:"state"`
		LastEvent              string  `json:"last_event"`
		EventTime              string  `json:"event_time"`
		BufferDepthS           float64 `json:"buffer_depth_s"`
		Stalls                 int     `json:"stalls"`
		StallTimeS             float64 `json:"stall_time_s"`
		LastStallTimeS         float64 `json:"last_stall_time_s"`
		ProfileShiftCount      int     `json:"profile_shift_count"`
		PlayerRestarts         int     `json:"player_restarts"`
		VideoBitrateMbps       float64 `json:"video_bitrate_mbps"`
		VideoQualityPct        float64 `json:"video_quality_pct"`
		VideoResolution        string  `json:"video_resolution"`
		NetworkBitrateMbps     float64 `json:"network_bitrate_mbps"`
		AvgNetworkBitrateMbps  float64 `json:"avg_network_bitrate_mbps"`
		DroppedFrames          int     `json:"dropped_frames"`
		PositionS              float64 `json:"position_s"`
		LiveEdgeS              float64 `json:"live_edge_s"`
		LiveOffsetS            float64 `json:"live_offset_s"`
	} `json:"player_metrics"`
}

// IsHeartbeating reports whether the player is fresh enough to talk to.
// We prefer `player_metrics.event_time` (the wallclock of the most recent
// metrics POST) over `last_seen_at`, which isn't populated on the live
// player record — only on archive rows after the proxy hands off to the
// forwarder. Same 60s threshold the triage skill uses.
func (p *PlayerRecord) IsHeartbeating(at time.Time) bool {
	if p == nil {
		return false
	}
	if p.PlayerMetrics != nil && p.PlayerMetrics.EventTime != "" {
		if ts, err := time.Parse(time.RFC3339Nano, p.PlayerMetrics.EventTime); err == nil {
			return at.Sub(ts) < 60*time.Second
		}
	}
	if p.LastSeenAt != nil {
		return at.Sub(*p.LastSeenAt) < 60*time.Second
	}
	return false
}

func harnessArgs(extra ...string) []string {
	args := make([]string, 0, len(extra)+3)
	if HarnessInsecure {
		args = append(args, "--insecure")
	}
	args = append(args, "--json")
	return append(args, extra...)
}

func runHarness(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, HarnessBin, harnessArgs(args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("harness %s: %w: %s",
				strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("harness %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// ListPlayers returns every player the proxy currently knows about, including
// stale ones with last_seen_at=null. Filter to heartbeating via IsHeartbeating.
func ListPlayers(ctx context.Context) ([]PlayerRecord, error) {
	raw, err := runHarness(ctx, "players", "list")
	if err != nil {
		return nil, err
	}
	var players []PlayerRecord
	if err := json.Unmarshal(raw, &players); err != nil {
		return nil, fmt.Errorf("decode players: %w", err)
	}
	return players, nil
}

// ShowPlayer returns the full player record + harness ETag. The wrapper
// drops the ETag for now (Phase 0 doesn't mutate); add it back when the
// runner needs optimistic concurrency.
func ShowPlayer(ctx context.Context, target string) (*PlayerRecord, error) {
	raw, err := runHarness(ctx, "players", "show", target)
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Player PlayerRecord `json:"player"`
		ETag   string       `json:"etag"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, fmt.Errorf("decode player show: %w", err)
	}
	return &wrap.Player, nil
}
