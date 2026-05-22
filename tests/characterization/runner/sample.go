package runner

import (
	"context"
	"sync"
	"time"
)

// Sample is one row of player state collected during a sweep.
//
// VariantIdx is populated in Finalize (variant-aware reports only) — for
// the resolved variant index see PlayerVariantIdx (when video_resolution
// is reported) vs VariantIdxByBitrate (closest-bitrate fallback).
type Sample struct {
	Ts                    time.Time `json:"ts"`
	AppliedRateMbps       float64   `json:"applied_rate_mbps"`
	State                 string    `json:"state"`
	LastEvent             string    `json:"last_event"`
	BufferDepthS          float64   `json:"buffer_depth_s"`
	// BufferEndS — most-distant loaded segment, seconds from playhead.
	// More reliable on AVPlayer where BufferDepthS often reports 0.
	BufferEndS            float64   `json:"buffer_end_s,omitempty"`
	Stalls                int       `json:"stalls"`
	StallTimeS            float64   `json:"stall_time_s"`
	ProfileShiftCount     int       `json:"profile_shift_count"`
	VideoBitrateMbps      float64   `json:"video_bitrate_mbps"`
	// VideoFirstFrameTimeS — player's own measurement of time from
	// play-start to first decoded frame. Per-play (resets on new
	// play_id). Authoritative for TTFF.
	VideoFirstFrameTimeS  float64   `json:"video_first_frame_time_s,omitempty"`
	// PlayID — the current play's UUID. Changes when the player
	// starts a new play (channel change, app relaunch, etc).
	// Used by startup_test to find the new-play transition for
	// accurate TTFF measurement.
	PlayID                string    `json:"play_id,omitempty"`
	VideoQualityPct       float64   `json:"video_quality_pct"`
	// VideoResolution is the player's currently-DISPLAYED variant
	// ("960x540"). Lags the actually-being-fetched variant by a few
	// seconds — the player switches by requesting new segments, but the
	// screen keeps showing the old variant until those segments decode
	// and render. So for "what is the user seeing" this is right; for
	// "what is the player downloading" use VariantIdx (bitrate-based).
	VideoResolution       string    `json:"video_resolution,omitempty"`
	// NetworkBitrateMbps is the player's instantaneous measured network
	// throughput, as reported in player_metrics. Useful for verifying the
	// proxy's tc cap is biting.
	NetworkBitrateMbps    float64   `json:"network_bitrate_mbps,omitempty"`
	// AvgNetworkBitrateMbps is the long-term average. The warmup step
	// (100 Mbps cap) is what seeds this with a reasonable baseline before
	// the sweep begins.
	AvgNetworkBitrateMbps float64   `json:"avg_network_bitrate_mbps,omitempty"`
	DroppedFrames         int       `json:"dropped_frames"`
	PositionS             float64   `json:"position_s"`
	// VariantIdx is what the player is currently *fetching*, derived from
	// video_bitrate_mbps (closest variant by raw rate). Leading indicator
	// of ABR decisions. -1 = unclassified (no bitrate yet, or non-variant
	// sweep).
	VariantIdx            int       `json:"variant_idx,omitempty"`
	// DisplayedVariantIdx is what the player is currently *rendering*,
	// derived from video_resolution. Lagging indicator (catches up to
	// VariantIdx a few seconds after a switch). -1 = no displayed
	// resolution reported.
	DisplayedVariantIdx   int       `json:"displayed_variant_idx,omitempty"`
	Err                   string    `json:"err,omitempty"`
}

// Sampler periodically calls ShowPlayer on the bound session and accumulates
// Samples. Cheap (one HTTP round-trip per tick); use the timeseries SSE
// instead if you need sub-second cadence.
//
// Lifecycle:
//
//	s := NewSampler(sess, time.Second)
//	s.Start(ctx)            // begins polling
//	s.SetAppliedRate(1.5)   // annotation; not what the proxy applies
//	... run sweep ...
//	samples := s.Stop()
type Sampler struct {
	Session *Session
	Period  time.Duration

	mu          sync.Mutex
	samples     []Sample
	appliedRate float64
	cancel      context.CancelFunc
	done        chan struct{}
}

func NewSampler(s *Session, period time.Duration) *Sampler {
	if period <= 0 {
		period = time.Second
	}
	return &Sampler{Session: s, Period: period}
}

// SetAppliedRate annotates subsequent samples with the rate currently being
// driven into the proxy. The sampler doesn't apply rates itself — it just
// records what the caller says was applied at the moment of the tick.
func (s *Sampler) SetAppliedRate(mbps float64) {
	s.mu.Lock()
	s.appliedRate = mbps
	s.mu.Unlock()
}

// Start begins the polling goroutine. Returns immediately. Stop must be
// called exactly once to drain the goroutine and return the samples.
func (s *Sampler) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.loop(ctx)
}

func (s *Sampler) loop(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.Period)
	defer ticker.Stop()
	// Take an immediate first sample so the report starts at t=0.
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Sampler) tick(ctx context.Context) {
	rec, err := s.Session.PlayerState(ctx)
	now := time.Now()
	s.mu.Lock()
	rate := s.appliedRate
	s.mu.Unlock()
	if err != nil {
		s.append(Sample{Ts: now, AppliedRateMbps: rate, Err: err.Error()})
		return
	}
	m := rec.PlayerMetrics
	if m == nil {
		s.append(Sample{Ts: now, AppliedRateMbps: rate})
		return
	}
	playID := ""
	if rec.CurrentPlay != nil {
		playID = rec.CurrentPlay.ID
	}
	s.append(Sample{
		Ts:                    now,
		AppliedRateMbps:       rate,
		State:                 m.State,
		LastEvent:             m.LastEvent,
		BufferDepthS:          m.BufferDepthS,
		BufferEndS:            m.BufferEndS,
		Stalls:                m.Stalls,
		StallTimeS:            m.StallTimeS,
		ProfileShiftCount:     m.ProfileShiftCount,
		VideoBitrateMbps:      m.VideoBitrateMbps,
		VideoFirstFrameTimeS:  m.VideoFirstFrameTimeS,
		PlayID:                playID,
		VideoQualityPct:       m.VideoQualityPct,
		VideoResolution:       m.VideoResolution,
		NetworkBitrateMbps:    m.NetworkBitrateMbps,
		AvgNetworkBitrateMbps: m.AvgNetworkBitrateMbps,
		DroppedFrames:         m.DroppedFrames,
		PositionS:             m.PositionS,
	})
}

func (s *Sampler) append(sample Sample) {
	s.mu.Lock()
	s.samples = append(s.samples, sample)
	s.mu.Unlock()
}

// Recent returns up to n most-recent samples without stopping the sampler.
// Used by mid-sweep early-exit predicates (e.g. "is buffer stable over the
// last 15 s?"). Safe to call concurrently with the polling loop. When
// fewer than n samples have been collected, returns whatever is available.
func (s *Sampler) Recent(n int) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.samples) {
		n = len(s.samples)
	}
	out := make([]Sample, n)
	copy(out, s.samples[len(s.samples)-n:])
	return out
}

// Stop signals the polling loop, waits for it to drain, and returns the
// accumulated samples.
func (s *Sampler) Stop() []Sample {
	if s.cancel != nil {
		s.cancel()
	}
	if s.done != nil {
		<-s.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Sample, len(s.samples))
	copy(out, s.samples)
	return out
}
