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
		ID        string `json:"id"`
		AttemptID int    `json:"attempt_id"`
		Manifest  struct {
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
		// BufferEndS is the most-distant loaded segment position, in
		// seconds from the playhead. More reliable than BufferDepthS
		// on AVPlayer (iOS), which often reports 0 even during normal
		// playback — see .claude/standards/avplayer-quirks.md.
		BufferEndS             float64 `json:"buffer_end_s"`
		Stalls                 int     `json:"stalls"`
		StallTimeS             float64 `json:"stall_time_s"`
		ProfileShiftCount      int     `json:"profile_shift_count"`
		PlayerRestarts         int     `json:"player_restarts"`
		VideoBitrateMbps       float64 `json:"video_bitrate_mbps"`
		VideoQualityPct        float64 `json:"video_quality_pct"`
		VideoResolution        string  `json:"video_resolution"`
		// VideoFirstFrameTimeS is the player's own measurement of time
		// from play-start to first decoded frame. Per-play (reset on
		// new play_id). Authoritative for TTFF, vs deriving it from
		// "first sample with bitrate > 0" which can be polluted by a
		// previous play's residual metrics.
		VideoFirstFrameTimeS   float64 `json:"first_frame_time_s"`
		NetworkBitrateMbps     float64 `json:"network_bitrate_mbps"`
		AvgNetworkBitrateMbps  float64 `json:"avg_network_bitrate_mbps"`
		FramesDropped          int     `json:"frames_dropped"`
		PositionS              float64 `json:"position_s"`
		LiveEdgeS              float64 `json:"live_edge_s"`
		LiveOffsetS            float64 `json:"live_offset_s"`

		// #550 Phase 1 residency accumulators — cumulative ms in each
		// player state since the current play started. Phase 1
		// columns in CH (see analytics/clickhouse/init.d/01-schema.sql).
		// The state_residency characterization test asserts these
		// after driving the iPad sim through targeted scenarios.
		PlayingTimeMs       uint32 `json:"playing_time_ms"`
		PausingTimeMs       uint32 `json:"pausing_time_ms"`
		BufferingTimeMs     uint32 `json:"buffering_time_ms"`
		StallingTimeMs      uint32 `json:"stalling_time_ms"`
		IdlingTimeMs        uint32 `json:"idling_time_ms"`
		SeekingTimeMs       uint32 `json:"seeking_time_ms"`
		TrickplayingTimeMs  uint32 `json:"trickplaying_time_ms"`
		PlayingCount        uint32 `json:"playing_count"`
		PausingCount        uint32 `json:"pausing_count"`
		BufferingCount      uint32 `json:"buffering_count"`
		StallingCount       uint32 `json:"stalling_count"`
		IdlingCount         uint32 `json:"idling_count"`
		SeekingCount        uint32 `json:"seeking_count"`
		TrickplayingCount   uint32 `json:"trickplaying_count"`

		// #550 Phase 2 outcome — final classification (in_progress,
		// completed, user_stopped, start_failure, mid_stream_failure,
		// abandoned_start). PlaybackReason carries the *cause* on
		// terminal rows (e.g. "natural_eof", "decoder_runtime") and
		// mirrors player_state on in_progress rows.
		PlaybackStatus      string `json:"playback_status"`
		PlaybackReason      string `json:"playback_reason"`
		ErrorCount          uint32 `json:"error_count"`
		ErrorCode           int32  `json:"error_code"`
		ErrorDomain         string `json:"error_domain"`
		TerminalErrorCode   int32  `json:"terminal_error_code"`
		TerminalErrorDomain string `json:"terminal_error_domain"`

		// JSON-string-encoded map of variant-label → cumulative
		// seconds spent at that variant. iOS emits as
		// `{"2160p@29857kbps":65.28,"1080p@…":12.4}` so test code
		// must json.Unmarshal into map[string]float64. Preserved
		// across retry()-style restarts by
		// PlaybackDiagnostics.snapshotForRestart() — the
		// RestartPreservesMetrics characterization asserts this.
		TimePerVariantS string `json:"time_per_variant_s"`
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

// RunHarnessCLI is the exported counterpart to runHarness — lets test
// packages outside `runner` (e.g. modes/) drive arbitrary harness CLI
// commands without re-implementing the exec wiring. Inherits the
// same insecure / json / base-URL flags via harnessArgs.
func RunHarnessCLI(ctx context.Context, args ...string) ([]byte, error) {
	return runHarness(ctx, args...)
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

// PreLaunchInfo returns the full player record of the heartbeating
// player currently matching the supplied device — without launching
// anything. Used by tests that need to read the *current* play's
// manifest variants BEFORE a kill+launch cycle wipes the live
// current_play state. The canonical example is rampup pre-computing
// the floor cap, so playback can start cold under throttle instead
// of cliff-diving from unconstrained → constrained mid-stream.
//
// Returns an error when no heartbeating player matches the device
// (first-ever launch on this hardware, or the previous play already
// timed out). Callers should fall back to a post-launch / warmup-then-
// adjust path in that case.
func PreLaunchInfo(ctx context.Context, d Device) (*PlayerRecord, error) {
	players, err := ListPlayers(ctx)
	if err != nil {
		return nil, fmt.Errorf("pre-launch info: %w", err)
	}
	p, ok := pickPlayerFor(d, players)
	if !ok {
		return nil, fmt.Errorf("pre-launch info: no heartbeating player matches %s", d)
	}
	return ShowPlayer(ctx, p.ID)
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
