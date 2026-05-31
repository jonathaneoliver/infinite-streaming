// #550 Phase 4 — best-effort User-Agent parsing for external HLS
// players (VLC, ffplay/ffmpeg, hls.js, shaka, etc.) so device taxonomy
// columns get populated even when the client doesn't run our iOS app
// (which stamps the canonical values via DeviceInfo.swift). For our
// own iOS app the iOS-emitted values dominate; this parser is purely
// a fallback for non-instrumented clients.
//
// Approach: prefix/contains matching on canonical UA patterns. Partial
// info is better than no info — empty fields are LowCardinality-cheap
// in ClickHouse so misses don't bloat storage.
//
// `iOS app` UA detection short-circuits: the AVPlayer UA contains the
// Bundle identifier and "InfiniteStream" in this codebase; we leave
// the device fields empty in that case so the iOS-emitted DeviceInfo
// values take precedence (the proxy stamps device fields only when
// they aren't already populated by the iOS payload, per the merge
// logic in saveSessionByID — see the user_agent richness guard).

package main

import "strings"

// DeviceFields holds the four taxonomy columns the proxy infers from
// a User-Agent. Only player_tech and device_class are reliably
// extractable from UA strings; device_model and app_version are
// usually absent. Empty strings mean "unknown" — schema treats them
// as zero-cost LowCardinality values.
type DeviceFields struct {
	DeviceClass string
	DeviceModel string
	PlayerTech  string
	AppVersion  string
}

// parseDeviceFromUserAgent applies a sequence of UA-shape heuristics.
// Order matters — more-specific patterns win over generic prefixes.
// Returns zero-value DeviceFields when no pattern matches.
func parseDeviceFromUserAgent(ua string) DeviceFields {
	if ua == "" {
		return DeviceFields{}
	}
	lower := strings.ToLower(ua)

	// Our own iOS / tvOS app — leave the fields empty so the iOS-
	// emitted DeviceInfo values take precedence at the forwarder. If
	// for any reason the iOS payload doesn't carry device fields, the
	// dashboards will show empty here, which is honest.
	if strings.Contains(lower, "infinitestream") ||
		strings.Contains(lower, "infinite-stream") ||
		strings.Contains(lower, "infinitestreamplayer") {
		return DeviceFields{}
	}

	// VLC media player — `VLC/3.0.20 LibVLC/3.0.20` or similar.
	if strings.HasPrefix(ua, "VLC/") || strings.Contains(ua, "LibVLC/") {
		return DeviceFields{
			DeviceClass: "desktop",
			PlayerTech:  "vlc",
			AppVersion:  extractVersionAfter(ua, "VLC/"),
		}
	}

	// FFmpeg / ffplay — `Lavf/61.7.100` or `FFmpeg/n5.x` family.
	if strings.HasPrefix(ua, "Lavf/") || strings.Contains(lower, "ffmpeg/") || strings.Contains(lower, "ffplay/") {
		return DeviceFields{
			DeviceClass: "desktop",
			PlayerTech:  "ffmpeg",
			AppVersion:  extractVersionAfter(ua, "Lavf/"),
		}
	}

	// hls.js — typically embedded in browser UA like
	// `... hls.js/1.5.20`. Player_tech wins; device_class derives
	// from the surrounding browser UA below.
	playerTech := ""
	appVersion := ""
	if idx := strings.Index(ua, "hls.js/"); idx >= 0 {
		playerTech = "hls.js"
		appVersion = extractVersionAt(ua, idx+len("hls.js/"))
	} else if strings.Contains(ua, "shaka-player/") {
		playerTech = "shaka"
		if i := strings.Index(ua, "shaka-player/"); i >= 0 {
			appVersion = extractVersionAt(ua, i+len("shaka-player/"))
		}
	}

	// Browser / OS heuristics — coarse but sufficient for distinguishing
	// desktop vs phone vs tablet vs tv. Matches existing
	// hasDeviceFamilyToken style.
	deviceClass := ""
	if strings.Contains(lower, "mobile") || strings.Contains(lower, "iphone") {
		deviceClass = "phone"
	} else if strings.Contains(lower, "ipad") || strings.Contains(lower, "tablet") {
		deviceClass = "tablet"
	} else if strings.Contains(lower, "apple tv") || strings.Contains(lower, "appletv") ||
		strings.Contains(lower, "smart-tv") || strings.Contains(lower, "smarttv") ||
		strings.Contains(lower, "roku") || strings.Contains(lower, "tizen") ||
		strings.Contains(lower, "webos") {
		deviceClass = "tv"
	} else if strings.Contains(lower, "windows") || strings.Contains(lower, "macintosh") ||
		strings.Contains(lower, "linux") || strings.Contains(lower, "x11") {
		deviceClass = "desktop"
	}

	// Roku channel — `Roku/DVP-12.0 (12.0.0.4XXX)`.
	if strings.HasPrefix(ua, "Roku/") {
		return DeviceFields{
			DeviceClass: "tv",
			DeviceModel: "Roku",
			PlayerTech:  "native-roku",
			AppVersion:  extractVersionAfter(ua, "Roku/"),
		}
	}

	// Default browser case — if we found a player_tech (hls.js /
	// shaka) emit that with the inferred class; if not, return what
	// we inferred from the OS hints.
	return DeviceFields{
		DeviceClass: deviceClass,
		PlayerTech:  playerTech,
		AppVersion:  appVersion,
	}
}

// extractVersionAfter pulls a dot-separated version token immediately
// after `prefix` in `s`. Stops at the first space, semicolon, or
// close-paren. Returns "" if prefix is missing.
func extractVersionAfter(s, prefix string) string {
	i := strings.Index(s, prefix)
	if i < 0 {
		return ""
	}
	return extractVersionAt(s, i+len(prefix))
}

// extractVersionAt reads a version-shaped token starting at index `i`.
// Permits digits, dots, hyphens, and underscores; bails on anything
// else.
func extractVersionAt(s string, i int) string {
	end := i
	for end < len(s) {
		c := s[end]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '_' {
			end++
			continue
		}
		break
	}
	return s[i:end]
}

// stampDeviceFromUserAgent applies parseDeviceFromUserAgent to the
// session blob's user_agent and writes the resulting device taxonomy
// fields onto the session map IF they aren't already populated.
// Called from the request handler after setting user_agent — the
// iOS-emitted values (which arrive via the metrics POST channel)
// take precedence by not being overwritten here.
func stampDeviceFromUserAgent(sessionData map[string]interface{}) {
	ua := getString(sessionData, "user_agent")
	if ua == "" {
		return
	}
	fields := parseDeviceFromUserAgent(ua)
	setIfEmpty(sessionData, "device_class", fields.DeviceClass)
	setIfEmpty(sessionData, "device_model", fields.DeviceModel)
	setIfEmpty(sessionData, "player_tech", fields.PlayerTech)
	setIfEmpty(sessionData, "app_version", fields.AppVersion)
}

func setIfEmpty(m map[string]interface{}, key, value string) {
	if value == "" {
		return
	}
	if existing := getString(m, key); existing != "" {
		return
	}
	m[key] = value
}
