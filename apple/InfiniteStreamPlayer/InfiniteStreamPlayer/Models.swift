import Foundation

// Content + selection model layer. Mirrors the Android `state` package
// (`ServerEnvironment`, `ContentItem`, `Protocol`, `Segment`, `Codec`)
// so the URL formation and dedup rules stay byte-identical across
// platforms.

// MARK: - ContentItem

/// One content item discovered from `/api/content`.
///
/// Server fields `clip_id` / `codec` / `thumbnail_url{,_small,_large}` are
/// emitted by go-upload's `ContentInfo`. They're the source of truth on
/// modern servers; older deployments that don't emit them get a derived
/// `clipId` (lowercased name with the `_p200_<codec>` suffix stripped)
/// so the client still dedupes sensibly.
struct ContentItem: Decodable, Identifiable, Equatable, Hashable {
    let name: String
    let hasHls: Bool
    let hasDash: Bool
    /// Server-computed logical clip identifier — same value across the
    /// h264/hevc/av1 encodings of one clip. Browse rows dedupe by this.
    let clipId: String
    /// "h264" / "hevc" / "av1" / "" — server-stripped codec hint.
    let codec: String
    /// Native segment duration the source was encoded at (seconds).
    /// `nil` when the server couldn't detect it. Used by the Stream
    /// picker to honour the Segment Length preference.
    let segmentDuration: Int?
    /// Server-relative path (or absolute URL) to the 640-px-wide poster
    /// — the default size for card / tile surfaces. `nil` when the
    /// server hasn't generated a thumbnail for this clip yet.
    let thumbnailPath: String?
    /// 320-px-wide poster, for small list cells / mobile rows.
    let thumbnailPathSmall: String?
    /// 1280-px-wide poster, for hero surfaces / Continue Watching backgrounds.
    let thumbnailPathLarge: String?

    /// Stable identity for SwiftUI lists. The clip name is unique per
    /// server (it's the directory name under `dynamic_content/`), so we
    /// use it directly rather than minting a UUID — that keeps list-row
    /// identity stable across reloads.
    var id: String { name }

    enum CodingKeys: String, CodingKey {
        case name
        case hasHls = "has_hls"
        case hasDash = "has_dash"
        case clipId = "clip_id"
        case codec
        case segmentDuration = "segment_duration"
        case thumbnailPath = "thumbnail_url"
        case thumbnailPathSmall = "thumbnail_url_small"
        case thumbnailPathLarge = "thumbnail_url_large"
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let rawName = try c.decode(String.self, forKey: .name)
        self.name = rawName
        self.hasHls = (try? c.decode(Bool.self, forKey: .hasHls)) ?? false
        self.hasDash = (try? c.decode(Bool.self, forKey: .hasDash)) ?? false
        let serverClip = (try? c.decode(String.self, forKey: .clipId)) ?? ""
        self.clipId = serverClip.isEmpty ? Self.deriveClipId(from: rawName) : serverClip
        self.codec = ((try? c.decode(String.self, forKey: .codec)) ?? "").lowercased()
        self.segmentDuration = try? c.decodeIfPresent(Int.self, forKey: .segmentDuration)
        self.thumbnailPath = (try? c.decode(String.self, forKey: .thumbnailPath))?
            .nonEmpty
        self.thumbnailPathSmall = (try? c.decode(String.self, forKey: .thumbnailPathSmall))?
            .nonEmpty
        self.thumbnailPathLarge = (try? c.decode(String.self, forKey: .thumbnailPathLarge))?
            .nonEmpty
    }

    init(name: String, hasHls: Bool = true, hasDash: Bool = false,
         clipId: String? = nil, codec: String = "",
         segmentDuration: Int? = nil,
         thumbnailPath: String? = nil,
         thumbnailPathSmall: String? = nil,
         thumbnailPathLarge: String? = nil) {
        self.name = name
        self.hasHls = hasHls
        self.hasDash = hasDash
        self.clipId = clipId ?? Self.deriveClipId(from: name)
        self.codec = codec.lowercased()
        self.segmentDuration = segmentDuration
        self.thumbnailPath = thumbnailPath
        self.thumbnailPathSmall = thumbnailPathSmall
        self.thumbnailPathLarge = thumbnailPathLarge
    }

    /// Strip `_p200_<codec>[_TIMESTAMP]` from the lowercased name.
    /// Mirrors the regex in the Android client + go-upload server, so a
    /// pre-#251 server still gives the client a usable dedup key.
    static func deriveClipId(from name: String) -> String {
        return Self.stripCodecSuffix(name).lowercased()
    }

    /// Parse the trailing `_YYYYMMDD_HHMMSS` timestamp suffix attached
    /// to repeat encodings. Returns `nil` for a name without a
    /// timestamp (the original encode). Newer encodings sort higher
    /// than older ones; nameless encodings sort *below* any timestamped
    /// encoding (treating them as "older" for dedup purposes).
    var encodeTimestamp: Date? {
        let pattern = #"_(\d{8})_(\d{6})$"#
        guard let r = name.range(of: pattern, options: .regularExpression) else { return nil }
        let f = DateFormatter()
        f.dateFormat = "_yyyyMMdd_HHmmss"
        f.locale = Locale(identifier: "en_US_POSIX")
        f.timeZone = TimeZone(identifier: "UTC")
        return f.date(from: String(name[r]))
    }

    /// Codec hint from the server when present; otherwise inferred from
    /// the name's `_p200_(h264|hevc|h265|av1)` segment. Lowercased.
    /// Empty when neither source carries a codec hint.
    var effectiveCodec: String {
        if !codec.isEmpty { return codec.lowercased() }
        let lower = name.lowercased()
        let pattern = #"_p200_(h264|hevc|h265|av1)\b"#
        if let r = lower.range(of: pattern, options: .regularExpression) {
            let match = lower[r]
            for c in ["h264", "hevc", "h265", "av1"] where match.contains(c) {
                return c
            }
        }
        return ""
    }

    /// Human-friendly content name with the `_p200_<codec>[_TIMESTAMP]`
    /// suffix removed. Original case preserved (unlike `clipId`, which
    /// lowercases for stable comparison). Use this anywhere content
    /// names are shown to the user — preview tile labels, the
    /// "NOW PLAYING" header, the Stream picker rows, etc.
    var displayName: String { Self.displayName(from: name) }

    /// Same suffix-strip + prettification applied to a raw name string.
    /// Useful for `vm.selectedContent` and `vm.lastPlayed`, which are
    /// stored as the underlying clip-name string rather than a ContentItem.
    static func displayName(from name: String) -> String {
        prettify(stripCodecSuffix(name))
    }

    private static func stripCodecSuffix(_ name: String) -> String {
        let pattern = #"_p200_(h264|hevc|h265|av1)(_\d{8}_\d{6})?$"#
        if let r = name.range(of: pattern, options: [.regularExpression, .caseInsensitive]) {
            return String(name[..<r.lowerBound])
        }
        return name
    }

    /// `tears-of-steel-4k` → `Tears of Steel 4K`.
    ///
    /// Splits on `-` / `_`, title-cases each word, but preserves
    /// resolution / quality tokens (4K, 8K, HD, FHD, UHD, 1080p, 720p,
    /// 480p, 2160p, HDR, SDR) in their canonical case, and lowercases
    /// short connectives (of, the, and, in, on, a) for a natural-feeling
    /// title. Single source of truth — every "displayName" caller goes
    /// through here so labels stay consistent across screens.
    private static func prettify(_ stem: String) -> String {
        let preserveExact: [String: String] = [
            "4k": "4K", "8k": "8K", "uhd": "UHD",
            "hd": "HD", "fhd": "FHD", "sdr": "SDR", "hdr": "HDR",
            "1080p": "1080p", "720p": "720p", "480p": "480p", "2160p": "2160p",
            "2k": "2K"
        ]
        let lowercaseConnectives: Set<String> = ["of", "the", "and", "in", "on", "a", "to", "with"]
        let separators = CharacterSet(charactersIn: "-_ ")
        let parts = stem.components(separatedBy: separators).filter { !$0.isEmpty }
        guard !parts.isEmpty else { return stem }
        var out: [String] = []
        for (idx, raw) in parts.enumerated() {
            let lower = raw.lowercased()
            if let exact = preserveExact[lower] {
                out.append(exact)
            } else if idx > 0, lowercaseConnectives.contains(lower) {
                out.append(lower)
            } else {
                out.append(lower.prefix(1).uppercased() + lower.dropFirst())
            }
        }
        return out.joined(separator: " ")
    }
}

// MARK: - Selection enums

enum StreamProtocol: String, CaseIterable, Identifiable, Codable {
    case hls
    case dash
    var id: String { rawValue }
    var label: String { rawValue.uppercased() }
}

enum SegmentLength: String, CaseIterable, Identifiable, Codable {
    case ll
    case s2
    case s6
    var id: String { rawValue }
    var label: String {
        switch self {
        case .ll: return "LL"
        case .s2: return "2s"
        case .s6: return "6s"
        }
    }
    /// URL suffix on the manifest filename. Empty for LL (no suffix).
    var suffix: String {
        switch self {
        case .ll: return ""
        case .s2: return "_2s"
        case .s6: return "_6s"
        }
    }
}

enum CodecFilter: String, CaseIterable, Identifiable, Codable {
    case auto
    case h264
    case hevc
    case av1
    var id: String { rawValue }
    var label: String {
        switch self {
        case .auto: return "Auto"
        case .h264: return "H.264"
        case .hevc: return "HEVC"
        case .av1: return "AV1"
        }
    }
    /// Match against the item's effective codec — `ContentItem.codec`
    /// when the server populates it, otherwise inferred from the name's
    /// `_p200_<codec>` suffix. `.auto` matches everything.
    func matches(_ item: ContentItem) -> Bool {
        if self == .auto { return true }
        let effective = item.effectiveCodec
        switch self {
        case .auto: return true
        case .h264: return effective == "h264"
        case .hevc: return effective == "hevc" || effective == "h265"
        case .av1:  return effective == "av1"
        }
    }
}

// MARK: - URL builder
//
// `ServerProfile` lives in ServerProfile.swift (existing file). Its
// `contentURL` field is the API port (e.g. 30000) and `playbackURL` the
// per-session proxy port (e.g. 30081).

enum StreamURLBuilder {
    /// Build the playback URL exactly the way the Android client does.
    ///
    /// `localProxy=true` (default) routes through the per-session
    /// go-proxy port (failure injection in the loop). `false` hits the
    /// API port directly. Same `/go-live/...` route in both cases.
    static func playbackURL(
        server: ServerProfile,
        contentName: String,
        protocolOption: StreamProtocol,
        segment: SegmentLength,
        playerId: String,
        localProxy: Bool = true
    ) -> URL? {
        guard !contentName.isEmpty else { return nil }
        let manifest: String
        switch protocolOption {
        case .hls:  manifest = "master\(segment.suffix).m3u8"
        case .dash: manifest = "manifest\(segment.suffix).mpd"
        }
        let base = localProxy ? server.playbackURL : server.contentURL
        guard var components = URLComponents(string: base) else { return nil }
        components.path = "/go-live/\(contentName)/\(manifest)"
        if !playerId.isEmpty {
            components.queryItems = [URLQueryItem(name: "player_id", value: playerId)]
        }
        return components.url
    }

    /// 360p H.264 URL used by the home preview tiles. Hits the API port
    /// directly (no go-proxy in front) and never carries `player_id` —
    /// these tiles aren't part of any session. Pass a different
    /// `resolution` (e.g. 720) for surfaces that want a higher-fidelity
    /// preview at the cost of more decode work — the Continue Watching
    /// hero uses 720p since it's the visual hero of the screen.
    static func tilePreviewURL(server: ServerProfile, contentName: String, resolution: Int = 360) -> URL? {
        guard !contentName.isEmpty,
              var components = URLComponents(string: server.contentURL) else { return nil }
        components.path = "/go-live/\(contentName)/playlist_6s_\(resolution)p.m3u8"
        return components.url
    }

    /// Resolve a relative `thumbnail_url*` path against the server's API
    /// origin. Server already returns a leading slash for these.
    static func thumbnailURL(server: ServerProfile, path: String?) -> URL? {
        guard let p = path?.nonEmpty else { return nil }
        if p.lowercased().hasPrefix("http://") || p.lowercased().hasPrefix("https://") {
            return URL(string: p)
        }
        return URL(string: "\(server.contentURL)\(p.hasPrefix("/") ? "" : "/")\(p)")
    }
}

// MARK: - Helpers

private extension String {
    var nonEmpty: String? { isEmpty ? nil : self }
}
