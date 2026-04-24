import Foundation
import SwiftUI

/// A single server endpoint the user can pair with.
/// `contentURL` is the dashboard/content port (e.g. http://host:30000).
/// `playbackURL` is the per-session proxy port (typically content+81 e.g. 30081).
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
    static func fromHostPort(host: String, contentPort: Int, playbackPort: Int? = nil, scheme: String = "http", label: String? = nil) -> ServerProfile? {
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

/// Persists the user's known servers + active selection.
/// Storage shape: profiles as JSON array string in UserDefaults; active id as string.
@MainActor
final class ServerProfileStore: ObservableObject {
    @Published private(set) var profiles: [ServerProfile] = []
    @Published private(set) var activeID: UUID?

    private let profilesKey = "server_profiles_v1"
    private let activeKey = "active_server_profile_id"

    static let shared = ServerProfileStore()

    init() {
        load()
        // No seed — empty list is the first-launch state. The user adds a
        // server via "+ Add server" (discovery, pair-with-code, QR, or
        // manual). Matches the Android TV behaviour after the same change.
        migrateRemoveSeededLocalhost()
        if activeID == nil, let first = profiles.first {
            activeID = first.id
            save()
        }
    }

    /// One-shot migration: remove the auto-seeded "Localhost" profile that
    /// older builds wrote on first launch. Matched precisely (label +
    /// content URL + playback URL) so we don't clobber a manually-named
    /// "Localhost" the user actually wants to keep.
    private func migrateRemoveSeededLocalhost() {
        let before = profiles.count
        profiles.removeAll { p in
            p.label == "Localhost"
                && p.contentURL.lowercased() == "http://localhost:30000"
                && p.playbackURL.lowercased() == "http://localhost:30081"
        }
        if profiles.count != before {
            if let id = activeID, !profiles.contains(where: { $0.id == id }) {
                activeID = profiles.first?.id
            }
            save()
        }
    }

    var active: ServerProfile? {
        guard let id = activeID else { return profiles.first }
        return profiles.first { $0.id == id } ?? profiles.first
    }

    func add(_ profile: ServerProfile, makeActive: Bool = true) {
        // Dedup by contentURL (case-insensitive). If the URL already exists,
        // just activate that one.
        if let existing = profiles.first(where: { $0.contentURL.lowercased() == profile.contentURL.lowercased() }) {
            if makeActive { activeID = existing.id }
        } else {
            profiles.append(profile)
            if makeActive { activeID = profile.id }
        }
        save()
    }

    func remove(_ id: UUID) {
        // Allow removing all profiles — empty list is a valid state now that
        // there's no seed. UI shows the "+ Add server" affordance for empty.
        profiles.removeAll { $0.id == id }
        if activeID == id { activeID = profiles.first?.id }
        save()
    }

    func setActive(_ id: UUID) {
        guard profiles.contains(where: { $0.id == id }) else { return }
        activeID = id
        save()
    }

    private func load() {
        let defaults = UserDefaults.standard
        if let data = defaults.string(forKey: profilesKey)?.data(using: .utf8),
           let decoded = try? JSONDecoder().decode([ServerProfile].self, from: data) {
            profiles = decoded
        }
        if let s = defaults.string(forKey: activeKey), let uuid = UUID(uuidString: s) {
            activeID = uuid
        }
    }

    private func save() {
        let defaults = UserDefaults.standard
        if let data = try? JSONEncoder().encode(profiles),
           let s = String(data: data, encoding: .utf8) {
            defaults.set(s, forKey: profilesKey)
        }
        if let id = activeID {
            defaults.set(id.uuidString, forKey: activeKey)
        }
    }
}
