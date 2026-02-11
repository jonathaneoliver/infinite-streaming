import Foundation

struct ContentItem: Decodable, Identifiable, Equatable {
    let id = UUID()
    let name: String
    let hasDash: Bool
    let hasHls: Bool
    let segmentDuration: Int?
    let maxResolution: String?
    let maxHeight: Int?

    enum CodingKeys: String, CodingKey {
        case name
        case hasDash = "has_dash"
        case hasHls = "has_hls"
        case segmentDuration = "segment_duration"
        case maxResolution = "max_resolution"
        case maxHeight = "max_height"
    }
}

enum ProtocolOption: String, CaseIterable, Identifiable {
    case hls
    case dash

    var id: String { rawValue }
    var label: String { rawValue.uppercased() }
}

enum SegmentOption: String, CaseIterable, Identifiable {
    case all
    case ll
    case s2 = "2s"
    case s6 = "6s"

    var id: String { rawValue }
    var label: String {
        switch self {
        case .all: return "All"
        case .ll: return "LL"
        case .s2: return "2s"
        case .s6: return "6s"
        }
    }
}

enum CodecOption: String, CaseIterable, Identifiable {
    case h264
    case hevc
    case av1
    case all

    var id: String { rawValue }
    var label: String {
        switch self {
        case .h264: return "H264"
        case .hevc: return "H265/HEVC"
        case .av1: return "AV1"
        case .all: return "Auto"
        }
    }
}

struct SelectionResult {
    let contentName: String
    let streamURL: URL?
    let codecFallback: Bool
}

func inferCodec(from name: String) -> CodecOption {
    let lower = name.lowercased()
    if lower.contains("av1") { return .av1 }
    if lower.contains("hevc") || lower.contains("h265") { return .hevc }
    return .h264
}

func baseName(from name: String) -> String {
    var base = name
    base = base.replacingOccurrences(of: "_(h264|hevc|av1|dash|ts|hw)$", with: "", options: [.regularExpression, .caseInsensitive])
    base = base.replacingOccurrences(of: "_\\d{8}_\\d{6}$", with: "", options: [.regularExpression, .caseInsensitive])
    return base
}

func chooseContent(codec: CodecOption, available: [ContentItem], stored: String) -> String {
    guard !available.isEmpty else { return "" }
    if stored.isEmpty { return available[0].name }
    let storedCodec = inferCodec(from: stored)
    if codec == .all || storedCodec == codec {
        if available.contains(where: { $0.name == stored }) {
            return stored
        }
    }
    let base = baseName(from: stored)
    let candidates = available.filter { $0.name.contains(base) }
    if codec == .all {
        return candidates.first?.name ?? stored
    }
    let codecMatch = candidates.first { inferCodec(from: $0.name) == codec }
    return codecMatch?.name ?? candidates.first?.name ?? stored
}

func buildStreamURL(baseURL: URL, contentName: String, protocolOption: ProtocolOption, segment: SegmentOption, playerId: String) -> URL? {
    let base = baseURL.appendingPathComponent("go-live").appendingPathComponent(contentName)
    let pathURL: URL
    switch protocolOption {
    case .dash:
        switch segment {
        case .s2: pathURL = base.appendingPathComponent("manifest_2s.mpd")
        case .s6: pathURL = base.appendingPathComponent("manifest_6s.mpd")
        case .all, .ll: pathURL = base.appendingPathComponent("manifest.mpd")
        }
    case .hls:
        switch segment {
        case .s2: pathURL = base.appendingPathComponent("master_2s.m3u8")
        case .s6: pathURL = base.appendingPathComponent("master_6s.m3u8")
        case .all, .ll: pathURL = base.appendingPathComponent("master.m3u8")
        }
    }
    guard var components = URLComponents(url: pathURL, resolvingAgainstBaseURL: false) else {
        return pathURL
    }
    if !playerId.isEmpty {
        var queryItems = components.queryItems ?? []
        queryItems.append(URLQueryItem(name: "player_id", value: playerId))
        components.queryItems = queryItems
    } else {
        components.queryItems = nil
    }
    return components.url
}
