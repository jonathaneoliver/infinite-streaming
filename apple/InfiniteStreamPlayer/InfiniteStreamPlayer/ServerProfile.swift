import Foundation

/// A single server endpoint the user can pair with.
///
/// `contentURL` is the dashboard / content port (e.g. http://host:30000).
/// `playbackURL` is the per-session proxy port (typically content+81 e.g. 30081).
///
/// Persistence of the active list lives in `PlayerViewModel` since the
/// rework — the legacy `ServerProfileStore` was removed when the central
/// view-model took over UserDefaults responsibilities.
struct ServerProfile: Codable, Identifiable, Hashable {
    let id: UUID
    var label: String
    var contentURL: String
    var playbackURL: String

    init(id: UUID = UUID(), label: String, contentURL: String, playbackURL: String) {
        self.id = id
        self.label = label
        self.contentURL = contentURL
        self.playbackURL = playbackURL
    }

    /// Builds a profile from a scanned/typed dashboard URL.
    /// Convention: playback port = content port + 81 (e.g. 30000 → 30081).
    static func fromDashboardURL(_ urlString: String, label: String? = nil) -> ServerProfile? {
        let trimmed = urlString.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let url = URL(string: trimmed),
              let scheme = url.scheme?.lowercased(),
              let host = url.host,
              scheme == "http" || scheme == "https" else { return nil }
        let contentPort = url.port ?? (scheme == "https" ? 443 : 80)
        let playbackPort = contentPort + 81
        let content = "\(scheme)://\(host):\(contentPort)"
        let playback = "\(scheme)://\(host):\(playbackPort)"
        let derivedLabel = label?.isEmpty == false ? label! : "\(host):\(contentPort)"
        return ServerProfile(label: derivedLabel, contentURL: content, playbackURL: playback)
    }

    /// Builds a profile from explicit host/port fields.
    static func fromHostPort(host: String, contentPort: Int, playbackPort: Int? = nil,
                             scheme: String = "http", label: String? = nil) -> ServerProfile? {
        let h = host.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !h.isEmpty, contentPort > 0, contentPort < 65536 else { return nil }
        let pp = playbackPort ?? (contentPort + 81)
        guard pp > 0, pp < 65536 else { return nil }
        let content = "\(scheme)://\(h):\(contentPort)"
        let playback = "\(scheme)://\(h):\(pp)"
        let derivedLabel = label?.isEmpty == false ? label! : "\(h):\(contentPort)"
        return ServerProfile(label: derivedLabel, contentURL: content, playbackURL: playback)
    }
}
