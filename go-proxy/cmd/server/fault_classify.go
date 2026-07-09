package main

import (
	"net/url"
	"sort"
	"strings"
)

// RequestClass is the v2-native classification of a single proxy request. It
// is the input to the fault_rules evaluator (#919): a rule's FaultFilter
// (request_kind / variant / url_match) is matched against these fields instead
// of v1's surface-prefix + `*_failure_urls` substring model.
//
// This is the layer that gives init and audio segments a real identity —
// v1's classifier folded everything non-manifest into "segment" (which is why
// #917 faulted audio when only video was scoped and #918 could never label an
// init fetch). Kind is derived from the request path + the parsed master
// playlist (manifest_variants) the proxy already caches per session, NOT from
// URL substring spelling, so it survives encoding-pipeline renames.
type RequestClass struct {
	// Kind is the v2 request_kind. One of: master_manifest, manifest,
	// audio_manifest, segment, partial, audio_segment, init.
	Kind string
	// IsInit / IsAudio are the orthogonal axes Kind is derived from, exposed
	// so callers (and tests) can reason about them directly.
	IsInit  bool
	IsAudio bool
	// RungIndex is the variant's position in the bandwidth-sorted video ladder
	// (0 = lowest). -1 when the request isn't a resolvable video variant
	// (audio, init, master, or unmatched).
	RungIndex int
	// Resolution / BandwidthBps mirror the matched variant's manifest-declared
	// properties (empty / 0 when unknown). Codecs is not yet sourced from
	// manifest_variants (PlaylistInfo carries no CODECS) — left empty until
	// the parser cache exposes it.
	Resolution   string
	BandwidthBps int
	Codecs       string
	// LadderSize is the number of video variants in the cached master playlist
	// (0 when none). Needed to resolve VariantPredicate.rung_positions (top /
	// bottom / …) against the current ladder.
	LadderSize int
	// Path is the decoded request path the classification was computed from.
	Path string
}

// isInitSegmentName reports whether a segment filename is an initialization
// segment — HLS `#EXT-X-MAP:URI` targets and DASH `<Initialization>` both
// conventionally resolve to an `init.mp4` / `init.m4s` (per rendition). The
// leading-token check also catches `init-stream0.m4s`-style names.
func isInitSegmentName(base string) bool {
	b := strings.ToLower(base)
	return b == "init.mp4" || b == "init.m4s" ||
		strings.HasPrefix(b, "init.") || strings.HasPrefix(b, "init-") || strings.HasPrefix(b, "init_")
}

// isPartialSegmentName reports whether a segment filename is an LL-HLS partial
// segment. v1 folds partials into the segment surface; the v2 taxonomy keeps
// them distinct so a rule can fault whole segments without the partials (or
// vice-versa).
func isPartialSegmentName(base string) bool {
	b := strings.ToLower(base)
	return strings.Contains(b, ".part.") || strings.HasSuffix(b, ".part")
}

// audioParentTokens are the rendition-directory names the encoding pipeline
// uses for audio-only renditions. The classifier treats a request under one of
// these as audio. (Video renditions are named by resolution, e.g. 1080p.)
var audioParentTokens = map[string]bool{"audio": true, "aud": true}

// matchesAudioURI reports whether a request path corresponds to one of the
// manifest-declared audio rendition URIs (EXT-X-MEDIA TYPE=AUDIO). The URIs are
// as authored in the master (relative, e.g. "playlist_1s_audio.m3u8" or
// "audio/playlist.m3u8"), so match on full-path suffix or basename equality —
// robust to whatever the encoding pipeline names the audio rendition.
func matchesAudioURI(audioURIs []string, decodedPath, base string) bool {
	for _, u := range audioURIs {
		if u == "" {
			continue
		}
		if decodedPath == u || strings.HasSuffix(decodedPath, "/"+u) || pathBase(u) == base {
			return true
		}
	}
	return false
}

// isAudioPlaylistName reports whether a media-playlist basename is the audio
// rendition. Audio SEGMENTS live under an `/audio/` directory (caught by
// pathParent), but the audio media PLAYLIST sits in the content root named
// `playlist_<dur>_audio.m3u8` — its last `_`-delimited token is the variant
// ("audio" vs a resolution like "2160p"). Without this, the audio playlist is
// mis-classified as a generic `manifest` and a video-scoped rule
// (request_kind ⊇ manifest) wrongly faults it.
func isAudioPlaylistName(base string) bool {
	b := strings.ToLower(base)
	if !strings.HasSuffix(b, ".m3u8") {
		return false
	}
	name := strings.TrimSuffix(b, ".m3u8")
	if i := strings.LastIndex(name, "_"); i >= 0 {
		return name[i+1:] == "audio"
	}
	return name == "audio"
}

// classifyRequest maps one proxy request to its v2 RequestClass. The three
// bools are the same isSegment / isManifest / isMasterManifest split
// getContentType already computes, threaded in so classification stays a pure
// function of (session, path, kind-bools) and is unit-testable without a live
// upstream.
func classifyRequest(session SessionData, filename string, isSegment, isManifest, isMasterManifest bool) RequestClass {
	decoded := filename
	if unescaped, err := url.PathUnescape(filename); err == nil && unescaped != "" {
		decoded = unescaped
	}
	base := pathBase(decoded)
	parent := strings.ToLower(pathParent(decoded))
	isAudio := audioParentTokens[parent]
	// Audio media playlists sit in the content root (not under /audio/), so the
	// pathParent check misses them. Prefer the STRUCTURE-DRIVEN signal: the
	// EXT-X-MEDIA audio URIs parsed from the master (manifest_audio_uris). Only
	// when the master hasn't been parsed yet do we fall back to the filename
	// heuristic. #919 Tier 1.
	if !isAudio && isManifest {
		if audioURIs := getStringSlice(session, "manifest_audio_uris"); len(audioURIs) > 0 {
			isAudio = matchesAudioURI(audioURIs, decoded, base)
		} else {
			isAudio = isAudioPlaylistName(base)
		}
	}

	variants := getManifestVariants(session)
	rc := RequestClass{RungIndex: -1, IsAudio: isAudio, LadderSize: len(variants), Path: decoded}

	switch {
	case isMasterManifest:
		rc.Kind = "master_manifest"
		return rc
	case isManifest:
		if isAudio {
			rc.Kind = "audio_manifest"
		} else {
			rc.Kind = "manifest"
		}
		return rc
	case isSegment:
		switch {
		case isInitSegmentName(base):
			rc.Kind = "init"
			rc.IsInit = true
		case isPartialSegmentName(base):
			rc.Kind = "partial"
		case isAudio:
			rc.Kind = "audio_segment"
		default:
			rc.Kind = "segment"
		}
	default:
		rc.Kind = "other"
		return rc
	}

	// Video variant resolution: only meaningful for non-audio media requests
	// (segment / partial). init keeps RungIndex -1 — an init fetch is
	// per-rendition but carries no ABR-decision semantics of its own.
	if !isAudio && !rc.IsInit {
		if idx, info, ok := matchVariantIndex(variants, decoded); ok {
			rc.RungIndex = idx
			rc.Resolution = info.Resolution
			rc.BandwidthBps = info.Bandwidth
		}
	}
	return rc
}

// matchVariantIndex resolves a media request path to a video variant in the
// supplied ladder and returns its rung index in the bandwidth-sorted ladder
// (0 = lowest), the matched PlaylistInfo, and ok.
//
// The path→variant scoring mirrors inferServerVideoRendition (path / parent /
// base containment) so the fault evaluator and the server_video_rendition
// telemetry agree on which rung a request belongs to. Returns ok=false when
// there is no cached ladder or nothing scores a match.
func matchVariantIndex(variants []PlaylistInfo, decodedPath string) (int, PlaylistInfo, bool) {
	if len(variants) == 0 {
		return 0, PlaylistInfo{}, false
	}

	bestIdx, bestScore := -1, 0
	for idx, variant := range variants {
		variantPath := variant.URL
		if parsed, err := url.Parse(variant.URL); err == nil && parsed.Path != "" {
			variantPath = parsed.Path
		}
		variantPath = strings.TrimPrefix(variantPath, "/")
		variantParent := pathParent(variantPath)
		variantBase := pathBase(variantPath)

		score := 0
		if variantPath != "" && strings.Contains(decodedPath, variantPath) {
			score += 8
		}
		if variantParent != "" && strings.Contains(decodedPath, "/"+variantParent+"/") {
			score += 6
		}
		if variantBase != "" && strings.Contains(decodedPath, variantBase) {
			score += 3
		}
		if score > bestScore {
			bestScore, bestIdx = score, idx
		}
	}
	if bestIdx < 0 || bestScore == 0 {
		return 0, PlaylistInfo{}, false
	}

	// Rung index = position in the bandwidth-ascending ladder, independent of
	// the master playlist's authored order.
	return rungIndexOf(variants, bestIdx), variants[bestIdx], true
}

// rungIndexOf returns the bandwidth-ascending rung position of variants[target]
// (0 = lowest BANDWIDTH). Ties break by the variant's original order so the
// mapping is stable.
func rungIndexOf(variants []PlaylistInfo, target int) int {
	order := make([]int, len(variants))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return variants[order[a]].Bandwidth < variants[order[b]].Bandwidth
	})
	for rung, origIdx := range order {
		if origIdx == target {
			return rung
		}
	}
	return -1
}
