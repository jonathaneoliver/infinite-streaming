package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Report is the canonical output of one characterization run. Tests build
// it as they sweep; the writer marshals to JSON + renders a Markdown
// summary. Schema lifted from the python characterization JSON so the
// aggregator (Phase 5) can read either source.
//
// Variants is populated for variant-aware modes (smooth/steps); Finalize
// uses it to classify each sample's bitrate to its closest variant and
// build VariantSampleCounts. Leave nil for modes that don't sweep by variant.
type Report struct {
	Mode      string        `json:"mode"`
	Platform  Platform      `json:"platform"`
	Device    Device        `json:"device"`
	PlayerID  string        `json:"player_id"`
	// PlayIDs lists every play_id observed during the sweep — usually
	// one for smooth/steps; multiple for modes that relaunch the app
	// (startup-caps). First entry is the play active at sweep start.
	PlayIDs   []string      `json:"play_ids,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	Variants  []VariantRate `json:"variants,omitempty"`
	Steps     []Step        `json:"steps"`
	Samples   []Sample      `json:"samples"`
	Summary   Summary       `json:"summary"`
	// AbortCycles is populated by the abort characterization test —
	// one entry per (fault_shape, rep) cycle. Empty for sweep modes.
	AbortCycles []AbortCycleResult `json:"abort_cycles,omitempty"`
	// StartupCycles is populated by the startup characterization
	// test — one entry per (boundary_type, rep) cold-start cycle.
	// See .claude/standards/startup-characterization-test.md.
	StartupCycles []StartupCycleResult `json:"startup_cycles,omitempty"`
	// RetryCycles is populated by the retry/backoff characterization
	// test (Phase 1 of #492) — one entry per (fault_shape, rep)
	// persistent-fault cycle. See
	// .claude/standards/retry-backoff-characterization-test.md.
	RetryCycles []RetryCycleResult `json:"retry_cycles,omitempty"`
}

// RetryCycleResult captures one persistent-fault observation. The
// test arms a fault that fires on EVERY matching request (no
// frequency=0 one-shot — that's the abort test). We observe how
// many times the player retries, what the gap between retries is
// (backoff curve), whether/when it downshifts, and whether it
// eventually gives up on the failing variant.
//
// Field semantics + how to interpret outcomes:
// see .claude/standards/retry-backoff-characterization-test.md.
type RetryCycleResult struct {
	CycleIdx   int    `json:"cycle_idx"`
	FaultShape string `json:"fault_shape"`
	PreVariant string `json:"pre_variant"`
	// PreVariantDir is the segment-directory name the test scoped
	// the fault to (e.g. "2160p"). Constant across the cycle.
	PreVariantDir string    `json:"pre_variant_dir"`
	PreBufferS    float64   `json:"pre_buffer_s"`
	ArmedAt       time.Time `json:"armed_at"`
	ObserveWindowS float64  `json:"observe_window_s"`

	// PerURLRetries is one entry per faulted URL observed during the
	// window. Order: temporal — sorted by the URL's first attempt.
	PerURLRetries []URLRetryInfo `json:"per_url_retries,omitempty"`

	// Aggregate counters across all faulted URLs in the window.
	FaultedURLs            int `json:"faulted_urls"`
	TotalFailedFetches     int `json:"total_failed_fetches"`
	MeanRetryIntervalMs    float64 `json:"mean_retry_interval_ms,omitempty"`
	MedianRetryIntervalMs  float64 `json:"median_retry_interval_ms,omitempty"`

	// Downshift markers from two independent sources.
	// "Decided" = sample's VideoResolution flips post-arm (the player
	// announced its new choice). "Committed" = a new manifest URL
	// appears in network_requests post-arm (the player actually started
	// fetching from the new variant).
	DownshiftDecidedAtS    float64 `json:"downshift_decided_at_s,omitempty"`
	DownshiftDecidedTo     string  `json:"downshift_decided_to,omitempty"`
	DownshiftCommittedAtS  float64 `json:"downshift_committed_at_s,omitempty"`
	DownshiftCommittedTo   string  `json:"downshift_committed_to,omitempty"`

	// Give-up = a URL stopped getting retries with a gap > 30s before
	// the end of the observation window. GaveUpURL is the first URL
	// the player abandoned (empty if none).
	GaveUpURL  string  `json:"gave_up_url,omitempty"`
	GaveUpAtS  float64 `json:"gave_up_at_s,omitempty"`

	// Recovery — time for the player to return to its pre-cycle
	// variant + healthy buffer after the fault is released. Captured
	// by WaitForTopAndBuffer post-cycle. 0 = never recovered in the
	// recovery window.
	RecoveryS float64 `json:"recovery_s,omitempty"`

	// PlayerStalled = position frozen for >5s post-arm.
	PlayerStalled bool `json:"player_stalled"`
}

// URLRetryInfo is one URL's retry history within an observation window.
type URLRetryInfo struct {
	URL              string   `json:"url"`
	AttemptCount     int      `json:"attempt_count"`
	// IntervalsMs[i] = ms between attempt i and attempt i+1.
	// len(IntervalsMs) = AttemptCount - 1.
	IntervalsMs      []int64  `json:"intervals_ms,omitempty"`
	FirstAttemptAtS  float64  `json:"first_attempt_at_s"`
	LastAttemptAtS   float64  `json:"last_attempt_at_s"`
	AllFaulted       bool     `json:"all_faulted"`
	// FaultKinds is the set of fault_type/fault_action values the
	// proxy stamped on this URL's rows. Usually a single value but
	// recorded as a list in case the proxy applied different shapes.
	FaultKinds []string `json:"fault_kinds,omitempty"`
}

// StartupCycleResult captures one cold-start observation. The test
// applies a network cap, triggers the boundary (app kill+launch or
// channel change), then observes the first ~30s of playback.
//
// Field semantics + how to interpret outcomes:
// see .claude/standards/startup-characterization-test.md.
type StartupCycleResult struct {
	CycleIdx int    `json:"cycle_idx"`
	// BoundaryType is "app_cold" (kill app → launch → resume playback)
	// or "channel_change" (already playing → back → tap a different
	// content tile). See standards doc.
	BoundaryType string `json:"boundary_type"`
	// ContentClipID is the content the player switched TO this cycle
	// — informational only; for app_cold cycles it's the Continue
	// Watching tile's clip_id.
	ContentClipID string `json:"content_clip_id,omitempty"`
	CapMbps       float64   `json:"cap_mbps"`
	StartedAt     time.Time `json:"started_at"`
	// PlayerID can be set if we read the home AX node before resume.
	PlayerID string `json:"player_id,omitempty"`
	// FirstMasterAtS, FirstVariantAtS, FirstSegmentAtS — seconds from
	// cycle start to the first observed network request of each kind.
	// All measured from StartedAt.
	FirstMasterAtS  float64 `json:"first_master_at_s,omitempty"`
	FirstVariantAtS float64 `json:"first_variant_at_s,omitempty"`
	FirstSegmentAtS float64 `json:"first_segment_at_s,omitempty"`
	// FirstVariantPicked is the resolution/variant the player chose
	// first (read from the first variant-playlist URL it fetched).
	// Audio playlists are skipped — only video-variant playlists
	// count.
	FirstVariantPicked string `json:"first_variant_picked,omitempty"`
	// FirstVariantAvgMbps / FirstVariantPeakMbps — manifest bandwidth
	// values for the first variant picked. Empty when the variant
	// can't be looked up (audio, unknown).
	FirstVariantAvgMbps  float64 `json:"first_variant_avg_mbps,omitempty"`
	FirstVariantPeakMbps float64 `json:"first_variant_peak_mbps,omitempty"`
	// SettledVariantAvgMbps / SettledVariantPeakMbps — same for the
	// settled variant. Lets the dashboard show "1440p (avg=10.8
	// peak=15.4)" without recomputing from the manifest.
	SettledVariantAvgMbps  float64 `json:"settled_variant_avg_mbps,omitempty"`
	SettledVariantPeakMbps float64 `json:"settled_variant_peak_mbps,omitempty"`
	// PrePlayID is the play_id active BEFORE the boundary fired.
	// Used by the result-builder to detect the new-play transition
	// for accurate TTFF measurement.
	PrePlayID string `json:"pre_play_id,omitempty"`
	// PlayID is the play_id this cycle actually MEASURES — the play
	// the boundary started. Populated from the first sample whose
	// play_id differs from PrePlayID (i.e. the new play). Empty when
	// the new play never appeared during the observation window.
	// Required by the session-viewer link on the Characterization
	// page: ?player_id=<PlayerID>&play_id=<PlayID>.
	PlayID string `json:"play_id,omitempty"`
	// TimeToFirstFrameS reads the iOS app's reported video first-frame
	// time. The most-watched UX number.
	TimeToFirstFrameS float64 `json:"time_to_first_frame_s,omitempty"`
	// First-request connection-stage timings (medians across the first
	// ~5 requests). Reveal TLS resumption (low tls_ms), TCP keepalive
	// reuse (zero connect_ms), DNS cache hit (zero dns_ms).
	FirstReqDNSMs     float64 `json:"first_req_dns_ms,omitempty"`
	FirstReqConnectMs float64 `json:"first_req_connect_ms,omitempty"`
	FirstReqTLSMs     float64 `json:"first_req_tls_ms,omitempty"`
	// Initial buffer trajectory. ReachedXBufferAtS is when buffer
	// first crossed N seconds after StartedAt. 0 = never reached.
	ReachedFiveSBufferAtS    float64 `json:"reached_5s_buffer_at_s,omitempty"`
	ReachedFifteenSBufferAtS float64 `json:"reached_15s_buffer_at_s,omitempty"`
	// Variant trajectory: VideoResolution sampled at marks.
	VariantAt5S  string `json:"variant_at_5s,omitempty"`
	VariantAt15S string `json:"variant_at_15s,omitempty"`
	VariantAt30S string `json:"variant_at_30s,omitempty"`
	// Deltas across the 30s observation window.
	UpshiftsIn30S      int `json:"upshifts_in_30s"`
	DownshiftsIn30S    int `json:"downshifts_in_30s"`
	StallsIn30S        int `json:"stalls_in_30s"`
	FramesDroppedIn30S int `json:"frames_dropped_in_30s"`
	// SettledVariant is the resolution with the majority of samples
	// in the last 10 s of the 30 s window. Empty if the player never
	// stabilised.
	SettledVariant string `json:"settled_variant,omitempty"`
	// NetworkBitrateAtStartMbps is the player's own bandwidth estimate
	// on the FIRST sample post-StartedAt. Non-zero on channel_change
	// when the player kept its previous-content estimate; zero on
	// fresh app_cold.
	NetworkBitrateAtStartMbps float64 `json:"network_bitrate_at_start_mbps,omitempty"`
	NetworkBitrateAt30SMbps   float64 `json:"network_bitrate_at_30s_mbps,omitempty"`

	// VariantActivity — per-variant breakdown over the observation
	// window. One entry per variant the player TOUCHED (fetched the
	// playlist or any segment). The dashboard renders this so the
	// operator can see "the player fetched 1440p's playlist but
	// never fetched any segments from it" — i.e. it peeked at a
	// variant without committing.
	VariantActivity []VariantActivity `json:"variant_activity,omitempty"`
}

// VariantActivity summarises one variant's activity during a startup
// cycle's observation window. Drives "where did the player spend its
// time" + "did it commit to this variant or just peek at it" analysis.
type VariantActivity struct {
	// Resolution is the variant's resolution string (e.g. "3840x2160")
	// OR the segment-directory name (e.g. "2160p") when we couldn't
	// resolve to a manifest entry. Audio is excluded.
	Resolution string `json:"resolution"`
	// VariantDir is the segment-directory name (e.g. "2160p"). This
	// is the canonical identifier — Resolution may be empty for
	// dirs we couldn't map back to the manifest.
	VariantDir string `json:"variant_dir"`
	// PlaylistFetches counts the variant playlist GETs in the window.
	PlaylistFetches int `json:"playlist_fetches"`
	// SegmentFetches counts segment GETs in the window. A variant
	// with PlaylistFetches>0 AND SegmentFetches==0 is "peeked but
	// never used" — the player evaluated it (master/manifest level)
	// but never committed to consuming bytes from it.
	SegmentFetches int `json:"segment_fetches"`
	// First/LastSegmentAtS — seconds from cycle StartedAt to the
	// first/last segment fetch from this variant. Zero when no
	// segments were fetched.
	FirstSegmentAtS float64 `json:"first_segment_at_s,omitempty"`
	LastSegmentAtS  float64 `json:"last_segment_at_s,omitempty"`
	// ActiveDurationS = LastSegmentAtS - FirstSegmentAtS. Rough
	// "time the player was actively using this variant" — accurate
	// only when the player fetched consecutively (no gaps).
	ActiveDurationS float64 `json:"active_duration_s,omitempty"`
	// PeekedButNeverUsed: playlist fetched but no segments. Operationally
	// interesting — the player considered but rejected this variant.
	PeekedButNeverUsed bool `json:"peeked_but_never_used,omitempty"`
	// Manifest-declared bandwidth context (optional). Looked up from
	// VariantBandwidthByResolution at result-build time.
	AvgMbps  float64 `json:"avg_mbps,omitempty"`
	PeakMbps float64 `json:"peak_mbps,omitempty"`
}

// AbortCycleResult captures the player's reaction to one server-
// driven segment-fetch abort. See plan:
// ~/.claude/plans/abort-characterization-test.md.
type AbortCycleResult struct {
	CycleIdx        int       `json:"cycle_idx"`
	FaultShape      string    `json:"fault_shape"` // e.g. "server_timeout" | "request_first_byte_hang"
	PreVariant      string    `json:"pre_variant"`
	PreBufferS      float64   `json:"pre_buffer_s"`
	PreBwEstMbps    float64   `json:"pre_bw_est_mbps"`
	ArmedAt         time.Time `json:"armed_at"`
	AbortDetected   bool      `json:"abort_detected"`
	AbortKind       string    `json:"abort_kind"` // fault_type/fault_action from the network row
	AbortAtS        float64   `json:"abort_at_s"`
	AbortURL        string    `json:"abort_url"`
	RetryFound      bool      `json:"retry_found"`
	RetryHadRange   bool      `json:"retry_had_range"`
	RetryRangeStart int64     `json:"retry_range_start"`
	PlayerStalled   bool      `json:"player_stalled"`
	DownshiftedTo   string    `json:"downshifted_to,omitempty"`
	DownshiftAfterS float64   `json:"downshift_after_s"`
	RecoveryS       float64   `json:"recovery_s"`
	PostBwEstMbps   float64   `json:"post_bw_est_mbps"`
}

// Step is one applied-rate hold in a sweep. For variant-aware sweeps the
// Variant + ExitReason + per-step result fields are populated; for the
// simpler ramp modes those fields stay zero.
type Step struct {
	StartedAt time.Time     `json:"started_at"`
	EndedAt   time.Time     `json:"ended_at"`
	RateMbps  float64       `json:"rate_mbps"`
	Hold      time.Duration `json:"hold_ns"`

	// Variant identifies which rung + margin produced this cap. nil for
	// plain rate-ramp modes.
	Variant *VariantRate `json:"variant,omitempty"`

	// ExitReason explains why we left this step early (or didn't):
	//   "full"          — held the full Hold duration
	//   "early-stable"  — buffer was stable over the early-exit window
	//   "cancelled"     — ctx fired (timeout / test stop)
	ExitReason string `json:"exit_reason,omitempty"`

	// Per-step result aggregates, computed in RunSweep* helpers.
	//
	// Buffer envelope. BufferAtStartS / BufferAtEndS are the first
	// and last sample's BufferDepthS; together with Min/Max they
	// paint the full per-step buffer story (start → trough → recovery
	// → end) without forcing the operator to open the session viewer.
	MinBufferS     float64 `json:"min_buffer_s,omitempty"`
	MaxBufferS     float64 `json:"max_buffer_s,omitempty"`
	BufferAtStartS float64 `json:"buffer_at_start_s,omitempty"`
	BufferAtEndS   float64 `json:"buffer_at_end_s,omitempty"`

	StallsDelta int `json:"stalls_delta,omitempty"`
	// ProfileShiftsDelta counts how many ABR transitions the player
	// reported during this step (delta of profile_shift_count). A value
	// > 1 means the player thrashed between variants within the step —
	// distinct from "no transitions" or "one clean downshift."
	ProfileShiftsDelta int     `json:"profile_shifts_delta,omitempty"`
	MeanBitrateMbps    float64 `json:"mean_bitrate_mbps,omitempty"`
	// MaxBitrateMbps is the peak video bitrate the player picked
	// during the step — distinguishes "settled at variant X" from
	// "spiked to a richer variant briefly before settling lower".
	MaxBitrateMbps float64 `json:"max_bitrate_mbps,omitempty"`
	// MeanNetworkBitrateMbps is the player's measured network throughput
	// averaged over the step. Should be close to (but slightly below) the
	// applied cap if the proxy's tc is enforcing properly.
	MeanNetworkBitrateMbps float64 `json:"mean_network_bitrate_mbps,omitempty"`
	// MaxNetworkBitrateMbps is the peak measured throughput during
	// the step. Useful for catching brief over-cap excursions that
	// the mean would hide.
	MaxNetworkBitrateMbps float64 `json:"max_network_bitrate_mbps,omitempty"`
	SampleCount           int     `json:"sample_count,omitempty"`

	// --- variant tracking (filled by Finalize on variant-aware reports) ---

	// ExpectedVariantIdx is the rung the cap was built for (index into
	// Report.Variants — lower index = higher-quality variant). -1 = no
	// variant binding on this step.
	ExpectedVariantIdx int `json:"expected_variant_idx"`
	// VariantIdxesSeen counts samples observed at each rung during this
	// step. Index aligns with Report.Variants.
	VariantIdxesSeen []int `json:"variant_idxes_seen,omitempty"`
	// MajorVariantIdx is the most-observed rung during the step. -1 = no
	// samples classified.
	MajorVariantIdx int `json:"major_variant_idx"`
	// UnexpectedUpshift = the major observed rung was *higher quality*
	// than expected for this cap. Implies the cap was loose enough that
	// the player picked a richer variant than we were targeting.
	UnexpectedUpshift bool `json:"unexpected_upshift,omitempty"`
	// UnexpectedDownshift = the major observed rung was *lower quality*
	// than expected for this cap. Implies the cap was tighter than the
	// player could sustain at the expected variant.
	UnexpectedDownshift bool `json:"unexpected_downshift,omitempty"`
}

// Summary is derived from Samples. Computed by Finalize().
//
// VariantSampleCounts is populated for variant-aware reports — index aligns
// with Report.Variants. A zero count means the player never visited that
// rung during the sweep; smooth_test.go asserts this is non-zero for every
// variant.
type Summary struct {
	TotalStalls         int     `json:"total_stalls"`
	TotalStallSeconds   float64 `json:"total_stall_seconds"`
	MaxBufferDepthS     float64 `json:"max_buffer_depth_s"`
	MinBufferDepthS     float64 `json:"min_buffer_depth_s"`
	MeanBitrateMbps     float64 `json:"mean_bitrate_mbps"`
	MinBitrateMbps      float64 `json:"min_bitrate_mbps"`
	MaxBitrateMbps      float64 `json:"max_bitrate_mbps"`
	ProfileShifts       int     `json:"profile_shifts"`
	FramesDropped       int     `json:"frames_dropped"`
	SampleCount         int     `json:"sample_count"`
	VariantSampleCounts []int   `json:"variant_sample_counts,omitempty"`

	// LowestSustainableCapMbps is the smallest applied cap that kept the
	// buffer above SustainableBufferS for the entire step AND produced no
	// stall events. The next-lower cap is the first that broke either
	// rule. 0 = sweep never produced a sustainable step (every cap stalled
	// or depleted). Computed by Finalize when len(Steps) > 0.
	LowestSustainableCapMbps float64 `json:"lowest_sustainable_cap_mbps,omitempty"`
	// HighestStallingCapMbps is the largest applied cap that depleted the
	// buffer OR stalled — i.e. the boundary between safe and unsafe.
	HighestStallingCapMbps float64 `json:"highest_stalling_cap_mbps,omitempty"`
	// BottomVariantFloorMbps is the largest applied cap that caused a
	// stall or buffer depletion while the cap's target was the BOTTOM
	// rung (lowest variant in the ladder). This is qualitatively distinct
	// from HighestStallingCapMbps — on higher rungs a "stall" can just
	// mean the player took a moment to downshift, but at the bottom rung
	// there's nowhere lower to go, so this is a definitive
	// "cap cannot deliver this content" threshold.
	BottomVariantFloorMbps float64 `json:"bottom_variant_floor_mbps,omitempty"`
}

// SustainableBufferS is the minimum buffer the smooth mode requires to call
// a step "sustainable" — anything below this means we got close enough to
// zero that real-world jitter would have stalled us.
const SustainableBufferS = 1.0

// PopulateStepResult fills the per-step result fields by aggregating over
// the samples collected during this step. The caller passes ONLY the
// samples that fell within [StartedAt, EndedAt]; we do not re-filter here.
// Cumulative counters (stalls, profile shifts) are reported as deltas
// last-first so the value reflects what changed *during this step*.
func (st *Step) PopulateStepResult(stepSamples []Sample) {
	if len(stepSamples) == 0 {
		return
	}
	st.SampleCount = len(stepSamples)
	st.MinBufferS = stepSamples[0].BufferDepthS
	st.MaxBufferS = stepSamples[0].BufferDepthS
	// First / last sample's buffer = start / end of the step window.
	// The buffer envelope (start / min / max / end) gives the full
	// shape — start tells you what cushion the step inherited; end
	// tells you what it left for the next step.
	st.BufferAtStartS = stepSamples[0].BufferDepthS
	st.BufferAtEndS = stepSamples[len(stepSamples)-1].BufferDepthS
	var bitrateSum, netSum float64
	var bitrateN, netN int
	startStalls := stepSamples[0].Stalls
	endStalls := stepSamples[len(stepSamples)-1].Stalls
	startShifts := stepSamples[0].ProfileShiftCount
	endShifts := stepSamples[len(stepSamples)-1].ProfileShiftCount
	for _, s := range stepSamples {
		if s.BufferDepthS < st.MinBufferS {
			st.MinBufferS = s.BufferDepthS
		}
		if s.BufferDepthS > st.MaxBufferS {
			st.MaxBufferS = s.BufferDepthS
		}
		if s.VideoBitrateMbps > 0 {
			bitrateSum += s.VideoBitrateMbps
			bitrateN++
			if s.VideoBitrateMbps > st.MaxBitrateMbps {
				st.MaxBitrateMbps = s.VideoBitrateMbps
			}
		}
		if s.NetworkBitrateMbps > 0 {
			netSum += s.NetworkBitrateMbps
			netN++
			if s.NetworkBitrateMbps > st.MaxNetworkBitrateMbps {
				st.MaxNetworkBitrateMbps = s.NetworkBitrateMbps
			}
		}
	}
	if bitrateN > 0 {
		st.MeanBitrateMbps = bitrateSum / float64(bitrateN)
	}
	if netN > 0 {
		st.MeanNetworkBitrateMbps = netSum / float64(netN)
	}
	if d := endStalls - startStalls; d > 0 {
		st.StallsDelta = d
	}
	if d := endShifts - startShifts; d > 0 {
		st.ProfileShiftsDelta = d
	}
}

// Finalize computes the Summary from Samples + EndedAt. Idempotent. Call
// once at end of sweep before WriteReport.
func (r *Report) Finalize(endedAt time.Time) {
	r.EndedAt = endedAt
	if len(r.Samples) == 0 {
		return
	}
	// Stalls / profile shifts are cumulative counters on the player —
	// reportable as "first sample → last sample" deltas.
	first := r.Samples[0]
	last := r.Samples[len(r.Samples)-1]
	r.Summary.TotalStalls = last.Stalls - first.Stalls
	if r.Summary.TotalStalls < 0 {
		// Player restart resets counters; treat as absolute last.
		r.Summary.TotalStalls = last.Stalls
	}
	r.Summary.TotalStallSeconds = last.StallTimeS - first.StallTimeS
	if r.Summary.TotalStallSeconds < 0 {
		r.Summary.TotalStallSeconds = last.StallTimeS
	}
	r.Summary.ProfileShifts = last.ProfileShiftCount - first.ProfileShiftCount
	if r.Summary.ProfileShifts < 0 {
		r.Summary.ProfileShifts = last.ProfileShiftCount
	}
	r.Summary.FramesDropped = last.FramesDropped - first.FramesDropped
	if r.Summary.FramesDropped < 0 {
		r.Summary.FramesDropped = last.FramesDropped
	}
	r.Summary.SampleCount = len(r.Samples)

	// Buffer + bitrate aggregates over the run.
	r.Summary.MinBufferDepthS = r.Samples[0].BufferDepthS
	r.Summary.MinBitrateMbps = r.Samples[0].VideoBitrateMbps
	var bitrateSum float64
	var bitrateN int
	for _, s := range r.Samples {
		if s.BufferDepthS > r.Summary.MaxBufferDepthS {
			r.Summary.MaxBufferDepthS = s.BufferDepthS
		}
		if s.BufferDepthS < r.Summary.MinBufferDepthS {
			r.Summary.MinBufferDepthS = s.BufferDepthS
		}
		if s.VideoBitrateMbps > 0 {
			if s.VideoBitrateMbps > r.Summary.MaxBitrateMbps {
				r.Summary.MaxBitrateMbps = s.VideoBitrateMbps
			}
			if r.Summary.MinBitrateMbps == 0 || s.VideoBitrateMbps < r.Summary.MinBitrateMbps {
				r.Summary.MinBitrateMbps = s.VideoBitrateMbps
			}
			bitrateSum += s.VideoBitrateMbps
			bitrateN++
		}
	}
	if bitrateN > 0 {
		r.Summary.MeanBitrateMbps = bitrateSum / float64(bitrateN)
	}

	if len(r.Variants) > 0 {
		// Classify every sample once. VariantIdx = leading indicator
		// (what the player is currently fetching, from bitrate);
		// DisplayedVariantIdx = lagging indicator (what's on screen, from
		// reported resolution). VariantSampleCounts below counts by
		// fetched variant — that's what aligns with cap behaviour.
		for i := range r.Samples {
			r.Samples[i].VariantIdx = closestVariantIdx(r.Samples[i].VideoBitrateMbps, r.Variants)
			r.Samples[i].DisplayedVariantIdx = displayedVariantIdx(r.Samples[i].VideoResolution, r.Variants)
		}
		r.Summary.VariantSampleCounts = make([]int, len(r.Variants))
		for _, s := range r.Samples {
			if s.VariantIdx >= 0 {
				r.Summary.VariantSampleCounts[s.VariantIdx]++
			}
		}
		// Per-step variant rollups.
		for i := range r.Steps {
			r.Steps[i].finalizeVariantTracking(r.Samples, r.Variants)
		}
	}

	// Walk steps top-down (caps are descending) and find the smallest cap
	// that kept the buffer healthy AND stalled zero times. The next-lower
	// cap is the boundary between sustainable and not. Also pick out the
	// distinct "bottom-variant floor" — failures on the lowest rung have
	// no further downshift escape, so they're qualitatively different.
	bottomRes := ""
	if n := len(r.Variants); n > 0 {
		bottomRes = r.Variants[n-1].Resolution
	}
	for i := range r.Steps {
		st := &r.Steps[i]
		if st.RateMbps <= 0 {
			continue
		}
		sustainable := st.StallsDelta == 0 && st.MinBufferS >= SustainableBufferS && st.SampleCount > 0
		if sustainable {
			r.Summary.LowestSustainableCapMbps = st.RateMbps
		} else if r.Summary.LowestSustainableCapMbps > 0 && r.Summary.HighestStallingCapMbps == 0 {
			// First failure below the lowest-good = the boundary.
			r.Summary.HighestStallingCapMbps = st.RateMbps
		}
		// Bottom-variant floor: the highest cap whose target is the
		// lowest rung AND the step failed. Definitive "can't deliver"
		// signal — the player has nowhere to downshift to.
		if !sustainable && bottomRes != "" && st.Variant != nil && st.Variant.Resolution == bottomRes {
			if r.Summary.BottomVariantFloorMbps == 0 || st.RateMbps > r.Summary.BottomVariantFloorMbps {
				r.Summary.BottomVariantFloorMbps = st.RateMbps
			}
		}
	}
}

// displayedVariantIdx looks up the variant whose resolution string matches
// the player's reported video_resolution. Lagging indicator (what's on
// screen, not what's being fetched).
func displayedVariantIdx(resolution string, variants []VariantRate) int {
	if resolution == "" {
		return -1
	}
	for i, v := range variants {
		if v.Resolution == resolution {
			return i
		}
	}
	return -1
}

// closestVariantIdx attributes the player's reported video_bitrate_mbps to
// a variant. Empirically, AVPlayer reports the variant's PEAK BANDWIDTH
// attribute (from the master playlist) — not a measured per-segment rate —
// so closest-by-peak gives exact matches. We fall back to closest-by-avg
// when peak is missing (older content where Bandwidth wasn't advertised).
// Returns -1 when the sample's bitrate is zero or there are no variants.
func closestVariantIdx(bitrateMbps float64, variants []VariantRate) int {
	if bitrateMbps <= 0 || len(variants) == 0 {
		return -1
	}
	pickRate := func(v VariantRate) float64 {
		if v.PeakBps > 0 {
			return float64(v.PeakBps) / 1_000_000
		}
		return float64(v.RawBps) / 1_000_000
	}
	best := 0
	bestDelta := math.Abs(bitrateMbps - pickRate(variants[0]))
	for i := 1; i < len(variants); i++ {
		d := math.Abs(bitrateMbps - pickRate(variants[i]))
		if d < bestDelta {
			best = i
			bestDelta = d
		}
	}
	return best
}

// finalizeVariantTracking computes the per-step variant rollup, comparing
// what the player actually visited during the step against the variant
// the cap was built for. Sets ExpectedVariantIdx, VariantIdxesSeen,
// MajorVariantIdx, UnexpectedUpshift / Downshift.
func (st *Step) finalizeVariantTracking(allSamples []Sample, variants []VariantRate) {
	st.ExpectedVariantIdx = -1
	st.MajorVariantIdx = -1
	if st.Variant == nil {
		return
	}
	for i, v := range variants {
		if v.Resolution == st.Variant.Resolution {
			st.ExpectedVariantIdx = i
			break
		}
	}
	st.VariantIdxesSeen = make([]int, len(variants))
	count := 0
	for _, s := range allSamples {
		if !s.Ts.Before(st.StartedAt) && !s.Ts.After(st.EndedAt) && s.VariantIdx >= 0 {
			st.VariantIdxesSeen[s.VariantIdx]++
			count++
		}
	}
	if count == 0 {
		return
	}
	maxCount := 0
	for i, c := range st.VariantIdxesSeen {
		if c > maxCount {
			maxCount = c
			st.MajorVariantIdx = i
		}
	}
	if st.ExpectedVariantIdx >= 0 && st.MajorVariantIdx >= 0 {
		// Variants is descending order = idx 0 is HIGHEST quality.
		// Smaller idx than expected = upshift; larger idx = downshift.
		if st.MajorVariantIdx < st.ExpectedVariantIdx {
			st.UnexpectedUpshift = true
		} else if st.MajorVariantIdx > st.ExpectedVariantIdx {
			st.UnexpectedDownshift = true
		}
	}
}

// WriteReport marshals r as <outdir>/<basename>.json and renders a Markdown
// summary at <outdir>/<basename>.md. Returns the JSON path for callers that
// want to log it.
func WriteReport(outdir, basename string, r *Report) (string, error) {
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", outdir, err)
	}
	jsonPath := filepath.Join(outdir, basename+".json")
	mdPath := filepath.Join(outdir, basename+".md")
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", jsonPath, err)
	}
	md := renderMarkdown(r)
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", mdPath, err)
	}
	// Fire-and-forget upload to the forwarder so the dashboard's
	// Automated Testing page can render the full report from CH. The
	// on-disk JSON above stays as the authoritative source — failure
	// here logs but doesn't fail the test (an unreachable forwarder
	// shouldn't make a clean test FAIL).
	go uploadReportToForwarder(jsonPath)
	return jsonPath, nil
}

// uploadReportToForwarder shells out to `harness post characterization
// <jsonPath>`. Run async because the upload can take a second or two
// against a slow forwarder and we don't want to block test cleanup.
func uploadReportToForwarder(jsonPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"post", "characterization", jsonPath}
	if HarnessInsecure {
		args = append([]string{"--insecure"}, args...)
	}
	cmd := exec.CommandContext(ctx, HarnessBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Stay quiet on success; on failure write one line so the
		// operator sees the upload didn't land. Test pass/fail
		// criteria are unaffected.
		fmt.Fprintf(os.Stderr, "characterization upload failed: %v: %s\n",
			err, strings.TrimSpace(string(out)))
		return
	}
	fmt.Fprintf(os.Stderr, "characterization upload: %s\n", strings.TrimSpace(string(out)))
}

func renderMarkdown(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", r.Mode, r.Device)
	fmt.Fprintf(&b, "- player_id: `%s`\n", r.PlayerID)
	if len(r.PlayIDs) > 0 {
		fmt.Fprintf(&b, "- play_ids:  `%s`\n", strings.Join(r.PlayIDs, "`, `"))
	}
	fmt.Fprintf(&b, "- platform: %s\n", r.Platform)
	fmt.Fprintf(&b, "- started: %s\n", r.StartedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "- ended:   %s\n", r.EndedAt.UTC().Format(time.RFC3339))
	if !r.StartedAt.IsZero() && !r.EndedAt.IsZero() {
		fmt.Fprintf(&b, "- duration: %s\n", r.EndedAt.Sub(r.StartedAt).Round(time.Second))
	}
	fmt.Fprintf(&b, "\n## Summary\n\n")
	fmt.Fprintf(&b, "| metric | value |\n|---|---|\n")
	fmt.Fprintf(&b, "| samples              | %d |\n", r.Summary.SampleCount)
	fmt.Fprintf(&b, "| stalls               | %d |\n", r.Summary.TotalStalls)
	fmt.Fprintf(&b, "| stall seconds        | %.1f |\n", r.Summary.TotalStallSeconds)
	fmt.Fprintf(&b, "| profile shifts       | %d |\n", r.Summary.ProfileShifts)
	fmt.Fprintf(&b, "| dropped frames       | %d |\n", r.Summary.FramesDropped)
	fmt.Fprintf(&b, "| buffer min / max (s) | %.1f / %.1f |\n", r.Summary.MinBufferDepthS, r.Summary.MaxBufferDepthS)
	fmt.Fprintf(&b, "| bitrate min / mean / max (Mbps) | %.2f / %.2f / %.2f |\n",
		r.Summary.MinBitrateMbps, r.Summary.MeanBitrateMbps, r.Summary.MaxBitrateMbps)
	if r.Summary.LowestSustainableCapMbps > 0 {
		fmt.Fprintf(&b, "| **lowest sustainable cap (Mbps)** | **%.3f** |\n",
			r.Summary.LowestSustainableCapMbps)
	}
	if r.Summary.HighestStallingCapMbps > 0 {
		fmt.Fprintf(&b, "| highest stalling cap (Mbps) | %.3f |\n",
			r.Summary.HighestStallingCapMbps)
	}
	if r.Summary.BottomVariantFloorMbps > 0 {
		fmt.Fprintf(&b, "| **bottom-variant floor (Mbps)** | **%.3f** — definitive cap below which the lowest rung can't deliver |\n",
			r.Summary.BottomVariantFloorMbps)
	}

	if len(r.Variants) > 0 {
		fmt.Fprintf(&b, "\n## Variants (%d)\n\n", len(r.Variants))
		fmt.Fprintf(&b, "| resolution | avg Mbps | peak Mbps | source | samples observed |\n|---|---|---|---|---|\n")
		for i, v := range r.Variants {
			count := 0
			if i < len(r.Summary.VariantSampleCounts) {
				count = r.Summary.VariantSampleCounts[i]
			}
			fmt.Fprintf(&b, "| %s | %.3f | %.3f | %s | %d |\n",
				v.Resolution,
				float64(v.AvgBps)/1_000_000,
				float64(v.PeakBps)/1_000_000,
				v.Source,
				count)
		}
	}

	if len(r.Steps) > 0 {
		fmt.Fprintf(&b, "\n## Steps (%d)\n\n", len(r.Steps))
		fmt.Fprintf(&b, "| # | cap Mbps | variant | margin | exit | held | min/max buf | stalls | shifts | major obs | flag |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|---|---|\n")
		steps := append([]Step(nil), r.Steps...)
		sort.Slice(steps, func(i, j int) bool { return steps[i].StartedAt.Before(steps[j].StartedAt) })
		for i, st := range steps {
			variant := "-"
			margin := "-"
			if st.Variant != nil {
				variant = st.Variant.Resolution
				margin = fmt.Sprintf("%+d%%", st.Variant.MarginPct)
			}
			major := "-"
			if st.MajorVariantIdx >= 0 && st.MajorVariantIdx < len(r.Variants) {
				major = r.Variants[st.MajorVariantIdx].Resolution
			}
			flag := ""
			if st.UnexpectedUpshift {
				flag = "↑ upshift"
			} else if st.UnexpectedDownshift {
				flag = "↓ downshift"
			}
			exit := st.ExitReason
			if exit == "" {
				exit = "-"
			}
			held := st.EndedAt.Sub(st.StartedAt).Round(time.Second)
			shiftsCell := fmt.Sprintf("%d", st.ProfileShiftsDelta)
			if st.ProfileShiftsDelta > 1 {
				shiftsCell = fmt.Sprintf("**%d**", st.ProfileShiftsDelta) // thrash
			}
			fmt.Fprintf(&b, "| %d | %.3f | %s | %s | %s | %s | %.1f / %.1f | %d | %s | %s | %s |\n",
				i+1, st.RateMbps, variant, margin, exit, held,
				st.MinBufferS, st.MaxBufferS, st.StallsDelta, shiftsCell, major, flag)
		}
	}
	return b.String()
}

// DefaultOutDir picks the artifacts directory in this priority:
//   1. $CHARACTERIZATION_OUTDIR if set
//   2. ./artifacts under the test working dir (persists across runs —
//      Go's `-C tests/characterization` makes this resolve to
//      tests/characterization/artifacts when run from the repo root)
//   3. supplied fallback (typically t.TempDir(), wipes after the test)
//
// (2) keeps reports findable without any env var on the cmd line — the
// canonical `go test -C tests/characterization …` form Just Works.
// Reach for the env var when you want CI to land artifacts somewhere
// else.
func DefaultOutDir(fallback string) string {
	if v := os.Getenv("CHARACTERIZATION_OUTDIR"); v != "" {
		return v
	}
	const persistent = "./artifacts"
	if err := os.MkdirAll(persistent, 0o755); err == nil {
		return persistent
	}
	return fallback
}
