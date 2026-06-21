package runner

import "strconv"

// ProbeConfig is the client-side knob set an appium probe pins as launch args at
// cold launch. It is the single source of truth for the launch-arg domain shared
// by TestSweepProbe (the sweep's reattach probe) and the `harness char matrix`
// runner (issue #811), so both materialise an arm's client-side knobs identically.
//
// PlayerID binds the app to the config-on-connect session the harness already
// materialised; the remaining fields are the client-side characterization knobs
// the app reads once at launch — they require a cold launch to change (the
// per-play push is #800), which is why the matrix runner cold-launches per arm
// whenever one of these moves.
type ProbeConfig struct {
	PlayerID           string // -is.player_id — the bootstrapped session id (required)
	Content            string // -is.lastPlayed — pin the resumed clip (optional)
	Segment            string // -is.segment — s2|s6|ll master variant (optional; empty = app default s6)
	LiveOffsetS        string // -is.flag.live_offset_s — app-side live-offset override; "" → "0" (always pinned)
	Protocol           string // -is.protocol — hls|dash (optional; empty = app default)
	Codec              string // -is.codec — h264|hevc|av1 (optional; empty = app default)
	PeakBitrateMbps    int    // -is.flag.peak_bitrate_mbps — startup peak-bitrate clamp (Mbps, integer); 0 = omit (#683)
	StartsFirstVariant string // -is.flag.starts_first_variant — true|false; "" = omit (false is meaningful)
}

// ProbeLaunchArgs builds the NSArgumentDomain launch-arg slice for an appium
// probe launch. It pins the three launch-state flags every characterization
// launch forces, then layers the per-arm client knobs:
//
//   - play_id rotation OFF — keep the run a single play so per-play ABR analysis
//     isn't fragmented across an extra (often unlabelled) play.
//   - skip_home OFF — land on Home so LaunchToHome + ResumePlayback drive the ONE
//     intended play instead of cold-launching straight into lastPlayed.
//   - dev_mode ON — render the on-screen HUD for live observation.
//   - live_offset_s ALWAYS pinned (default "0") — a run must never inherit the
//     app's persisted stepper value, which would silently confound a
//     manifest-only test (the launch-arg domain wins over the saved default).
//
// Segment / protocol / peak-bitrate are appended only when set, so the default
// (env-driven) call site produces the same args it always did.
func ProbeLaunchArgs(c ProbeConfig) []string {
	args := []string{
		"-is.player_id", c.PlayerID,
		"-is.flag.play_id_rotation_s", "0",
		"-is.flag.skip_home", "false",
		"-is.flag.dev_mode", "true",
	}
	// CHAR_CONTENT pins the clip the app resumes (ResumePlayback's
	// continue-watching hero resolves to it), overriding the device's lastPlayed.
	if c.Content != "" {
		args = append(args, "-is.lastPlayed", c.Content)
	}
	// Segment variant (#793 live-offset matrix): s2|s6|ll selects which master
	// the app requests (master_2s/master_6s/master). Empty leaves the app default
	// (s6). The hold-back floor scales with segment duration, so a given
	// live_offset can be legal on one and sub-spec on another.
	if c.Segment != "" {
		args = append(args, "-is.segment", c.Segment)
	}
	// App-side live-offset override (#793): is.flag.live_offset_s sets the
	// player's own target — it seeks to liveEdge−N and OVERRIDES the manifest
	// hold-back when >0. ALWAYS pin it (default "0") so a run never inherits the
	// app's persisted/stepper value.
	lo := c.LiveOffsetS
	if lo == "" {
		lo = "0"
	}
	args = append(args, "-is.flag.live_offset_s", lo)
	// Protocol (#797): hls|dash selects the manifest family the app requests.
	if c.Protocol != "" {
		args = append(args, "-is.protocol", c.Protocol)
	}
	// Codec: h264|hevc|av1 — which codec rendition the app selects.
	if c.Codec != "" {
		args = append(args, "-is.codec", c.Codec)
	}
	// Startup peak-bitrate clamp (#683): cold-start on a variant the cap can
	// sustain instead of reaching for the top rung. 0 ⇒ omit (app's natural pick).
	if c.PeakBitrateMbps > 0 {
		args = append(args, "-is.flag.peak_bitrate_mbps", strconv.Itoa(c.PeakBitrateMbps))
	}
	// Join policy: start on the first manifest rung vs let ABR pick the join rung.
	// false is meaningful, so only the empty string omits the flag.
	if c.StartsFirstVariant != "" {
		args = append(args, "-is.flag.starts_first_variant", c.StartsFirstVariant)
	}
	// Test clean-slate: drop any persisted advanced flags so this run starts from
	// code defaults overlaid only by the args above — no carry-over from a prior
	// run on this sim (the app container survives `simctl install`). Same intent as
	// pinning live_offset_s above, but covers EVERY advanced flag, including ones
	// this run doesn't pass. On by default.
	args = append(args, "-is.flag.reset_advanced", "true")
	return args
}
