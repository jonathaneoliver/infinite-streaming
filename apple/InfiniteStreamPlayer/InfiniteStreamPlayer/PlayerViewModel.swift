import AVFoundation
import Combine
import CoreGraphics
import Foundation
import UIKit

/// Owns every piece of persistent UI state, the AVPlayer instance, and
/// the recovery pipeline. Screens are pure rendering surfaces: they
/// observe `@Published` state, call setters / actions on the VM, and
/// pass `vm.player` to the `PlayerView` wrapper.
///
/// Designed to mirror the Android `PlayerViewModel` (Kotlin) on a
/// per-method basis — same persistence keys (so a hypothetical
/// Android→iOS shared backup format would work), same recovery model
/// (Retry + Reload), same advanced-flags surface.
@MainActor
final class PlayerViewModel: ObservableObject {

    // MARK: - Published state

    @Published private(set) var servers: [ServerProfile] = []
    @Published private(set) var activeServerID: UUID?

    @Published private(set) var content: [ContentItem] = []
    @Published private(set) var selectedContent: String = ""
    /// Last clip we rendered first frame on; powers Continue Watching hero.
    @Published private(set) var lastPlayed: String = ""
    /// Per-`clip_id` view counts. Powers preview-row "frequently viewed"
    /// ordering. Codec-agnostic — h264 / hevc / av1 plays of one clip
    /// share a tally.
    @Published private(set) var viewCounts: [String: Int] = [:]

    @Published var streamProtocol: StreamProtocol = .hls
    @Published var segment: SegmentLength = .s6
    /// Default to H.264 — every device hardware-decodes it, so first-launch
    /// playback is maximally likely to work. `Auto` widens to HEVC + AV1.
    @Published var codec: CodecFilter = .h264

    @Published private(set) var statusText: String = ""
    @Published private(set) var currentURL: URL?

    // -- Advanced flags (persisted alongside developer mode) -----------------

    @Published var developerMode: Bool = false
    /// Allow renditions above 1080p. Off → cap at 1080p.
    @Published var allow4K: Bool = true
    /// Stream URL goes through the per-session go-proxy port. Off → API port.
    @Published var localProxy: Bool = true
    /// Auto-retry the current stream on non-codec player errors.
    @Published var autoRecovery: Bool = false
    /// Seek to the live edge on every (re)load.
    @Published var goLive: Bool = false
    /// Skip Home on cold launch when a saved server + lastPlayed both exist.
    @Published var skipHomeOnLaunch: Bool = false
    /// Mute audio. Useful for HUD scrubbing / quick previewing.
    @Published var isMuted: Bool = false
    /// Override for the port-40000 player_id strip — in k3s-dev the
    /// content port (40000) doesn't accept `?player_id=…` query strings
    /// so we strip it by default. Devs working that environment can set
    /// this to keep the param for testing the proxy → content port
    /// fall-through paths. Off by default.
    @Published var allowPlayerIdOnContentPort: Bool = false
    /// User-configurable live-edge offset in seconds. After playback
    /// starts and the seekable range becomes available, seek to
    /// `liveEdge − liveOffsetSeconds`. 0 means "use whatever the
    /// manifest's HOLD-BACK or Go Live setting picks". Mirrors the
    /// `live_offset_s` query param on `testing-session.html`.
    @Published var liveOffsetSeconds: Double = 0
    /// Number of simultaneous live-preview AVPlayer decodes allowed
    /// on Home. 0 = preview video off (every tile shows its thumbnail
    /// only). Defaults to the platform-specific hardware cap on first
    /// launch (see `DecodeBudget.hardwareCap`); users can lower the
    /// number in Settings → Advanced for thermal / battery / network
    /// reasons. Cannot exceed `DecodeBudget.hardwareCap`.
    @Published var previewVideoSlots: Int = DecodeBudget.shared.hardwareCap

    // -- HUD / settings drawer state ----------------------------------------

    @Published var hudVisible: Bool = false
    @Published var settingsOpen: Bool = false

    // -- Player ---------------------------------------------------------------

    /// Current AVPlayer instance. Replaced wholesale on `reload()` —
    /// callers that hold a reference must observe `playerEpoch` and
    /// re-bind their PlayerView when it bumps.
    @Published private(set) var player: AVPlayer = AVPlayer()
    /// Bumped every time `player` is replaced. PlaybackScreen keys its
    /// `PlayerView` wrapper on this so the underlying
    /// AVPlayerViewController re-acquires the new player.
    @Published private(set) var playerEpoch: Int = 0

    /// Stable identifier passed to go-proxy as `?player_id=...`. Kept
    /// for the lifetime of the VM (i.e. one app session). Reload does
    /// **not** rotate it — proxy session continuity matters.
    let playerId: String = UUID().uuidString

    /// `play_id` (issue #280) — a UUID regenerated at every fresh
    /// playback episode (loadStream / reload / retry / variant change).
    /// Threaded through every URL the player issues as `?play_id=...`
    /// so go-proxy can scope its NetworkLogEntry ring buffer per play.
    /// HAR snapshots filter to the most-recent play_id by default.
    private var currentPlayID: String = UUID().uuidString

    // MARK: - Private state

    private var codecRetries = 0
    private let maxCodecRetries = 3
    private var statusObserver: NSKeyValueObservation?

    // MARK: - Diagnostics + metrics pipeline (legacy iOS analytics)
    //
    // These are the carry-overs from the previous PlaybackViewModel. The
    // rework replaced the *UI* surface; the metrics surface stays — same
    // diagnostics observer, same heartbeat-+-event POST contract, same
    // auto-recovery zero-buffer watchdog. Mirrors the Android
    // PlaybackMetrics class on a per-event basis.

    /// AVPlayer state observer with cumulative counters + @Published
    /// fields for every metric the dashboard consumes. Bound to `player`
    /// in init() and rebound on `reload()`.
    let diagnostics = PlaybackDiagnostics()
    private var cancellables: Set<AnyCancellable> = []

    private static let metricsTimestampFormatter: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    private var metricsHeartbeatTimer: Timer?
    private var metricsSessionId: String?
    private var metricsLastSessionLookup: Date?
    private var lastReportedRenditionMbps: Double?
    private var lastReportedState: String?
    private var playbackStartAt: Date?
    private var videoFirstFrameSeconds: Double?
    private var videoPlayingTimeSeconds: Double?
    private var firstFrameReported: Bool = false
    private var playingReported: Bool = false
    private var lastReportedStallCount: Int = 0
    private var lastReportedStallDuration: Double = 0
    private var lastReportedLoopCount: Int = 0
    private let metricsHeartbeatSeconds: TimeInterval = 1
    private let metricsSessionLookupSeconds: TimeInterval = 30
    private let autoRecoveryThresholdSeconds: TimeInterval = 60
    private let autoRecoveryBaseDelaySeconds: TimeInterval = 2
    private let autoRecoveryMaxAttempts: Int = 3
    private let autoRecoveryVerifyDelaySeconds: TimeInterval = 10
    private var autoRecoveryAttempts: Int = 0
    private var autoRecoveryRestartTimer: Timer?
    private var autoRecoveryVerifyTimer: Timer?
    private var zeroBufferStartedAt: Date?
    @Published private(set) var profileShiftCount: Int = 0
    @Published private(set) var playerRestarts: Int = 0
    private var didPlayToEndObserver: NSObjectProtocol?
    private var failedToPlayObserver: NSObjectProtocol?
    private var firstFrameObserver: AVPlayerItemMetadataOutput?
    private var hasReportedFirstFrame = false
    private var willEnterForegroundObserver: NSObjectProtocol?
    private var didEnterBackgroundObserver: NSObjectProtocol?

    // MARK: - Init

    init() {
        // Migrate legacy UserDefaults keys (boss* → is* → new is.flag.*
        // namespace) before loading so users upgrading from a pre-rework
        // build keep their saved server list, codec / segment / protocol
        // selection, and Advanced flag state.
        Self.migrateLegacyDefaults()
        loadServers()
        loadAdvancedFlags()
        attachLifecycleObservers()
        attachPlayerItemObservers()
        // Start the on-device LocalHTTPProxy on launch when the flag is
        // on so that the first stream's URL rewrite (and the wire-byte
        // tracker fed by the proxy's per-chunk accounting) is ready.
        if localProxy { LocalHTTPProxy.shared.startIfNeeded() }
        // Wire diagnostics + metrics pipeline. Mirrors the legacy
        // PlaybackViewModel init() — diagnostics observes AVPlayer,
        // bindMetricsReporting forwards @Published changes as POSTs,
        // and startMetricsHeartbeat begins the 1Hz cadence.
        diagnostics.bind(to: player)
        bindDiagnosticsLogging()
        bindMetricsReporting()
        startMetricsHeartbeat()
        // Initial mute state from persisted flag.
        player.isMuted = isMuted
        // Optimistic: surface cached content for the active server right
        // away so Home doesn't blank-flash on launch.
        if let server = activeServer {
            let cached = readContentCache(for: server)
            if !cached.isEmpty {
                self.content = cached
                self.statusText = "Loaded \(cached.count) items (cached)"
            }
        }
    }

    deinit {
        if let o = didPlayToEndObserver { NotificationCenter.default.removeObserver(o) }
        if let o = failedToPlayObserver { NotificationCenter.default.removeObserver(o) }
        if let o = willEnterForegroundObserver { NotificationCenter.default.removeObserver(o) }
        if let o = didEnterBackgroundObserver { NotificationCenter.default.removeObserver(o) }
    }

    // MARK: - Server list

    var activeServer: ServerProfile? {
        guard let id = activeServerID else { return servers.first }
        return servers.first { $0.id == id } ?? servers.first
    }

    /// Add (or activate, if a duplicate URL exists) a server profile and
    /// kick a content fetch.
    @discardableResult
    func addServer(_ profile: ServerProfile, makeActive: Bool = true) -> ServerProfile {
        let key = profile.contentURL.lowercased()
        if let existing = servers.first(where: { $0.contentURL.lowercased() == key }) {
            if makeActive { activeServerID = existing.id }
            persistServers()
            fetchContentList()
            return existing
        }
        servers.append(profile)
        if makeActive || activeServerID == nil { activeServerID = profile.id }
        persistServers()
        fetchContentList()
        return profile
    }

    func selectServer(_ id: UUID) {
        guard servers.contains(where: { $0.id == id }) else { return }
        activeServerID = id
        persistServers()
        fetchContentList()
    }

    func forgetServer(_ id: UUID) {
        servers.removeAll { $0.id == id }
        if activeServerID == id { activeServerID = servers.first?.id }
        persistServers()
        if servers.isEmpty {
            content = []; selectedContent = ""; currentURL = nil
            player.replaceCurrentItem(with: nil)
        } else {
            fetchContentList()
        }
    }

    /// Wipe every persisted preference and return the app to its
    /// first-launch state: empty server list, default Advanced flags,
    /// no playback history, no content cache. AppRoot reads
    /// `servers.isEmpty` to route back to the ServerPicker.
    ///
    /// Stops the player and clears the current item before the wipe so
    /// nothing keeps a reference to a now-stale URL or session.
    func resetAllSettings() {
        player.pause()
        player.replaceCurrentItem(with: nil)
        if let domain = Bundle.main.bundleIdentifier {
            UserDefaults.standard.removePersistentDomain(forName: domain)
        }
        // Drop in-memory state and reload from the now-empty defaults
        // so every published value snaps back to its declared default.
        servers = []
        activeServerID = nil
        content = []
        selectedContent = ""
        currentURL = nil
        lastPlayed = ""
        viewCounts = [:]
        statusText = ""
        loadAdvancedFlags()
        setSettingsOpen(false)
    }

    // MARK: - Advanced flags

    func setDeveloperMode(_ on: Bool) { developerMode = on; persistFlags() }
    func setAllow4K(_ on: Bool) { allow4K = on; persistFlags() }
    func setLocalProxy(_ on: Bool) {
        localProxy = on
        persistFlags()
        if on { LocalHTTPProxy.shared.startIfNeeded() } else { LocalHTTPProxy.shared.stop() }
        buildURLAndLoad()
    }
    func setAutoRecovery(_ on: Bool) { autoRecovery = on; persistFlags() }
    func setGoLive(_ on: Bool) { goLive = on; persistFlags() }
    func setSkipHomeOnLaunch(_ on: Bool) { skipHomeOnLaunch = on; persistFlags() }
    func setIsMuted(_ on: Bool) {
        isMuted = on
        player.isMuted = on
        persistFlags()
    }
    func setPreviewVideoSlots(_ value: Int) {
        let clamped = max(0, min(value, DecodeBudget.shared.hardwareCap))
        previewVideoSlots = clamped
        DecodeBudget.shared.setUserCap(clamped)
        persistFlags()
    }
    func setAllowPlayerIdOnContentPort(_ on: Bool) {
        allowPlayerIdOnContentPort = on
        persistFlags()
        buildURLAndLoad()
    }
    func setLiveOffsetSeconds(_ value: Double) {
        liveOffsetSeconds = max(0, value)
        persistFlags()
        // If a stream is already loaded, apply the new offset
        // immediately rather than waiting for the next load.
        if currentURL != nil {
            scheduleLiveOffsetSeek(reason: "setting changed")
        }
    }

    // MARK: - Selection setters

    func setProtocol(_ p: StreamProtocol) {
        streamProtocol = p
        applyContentFilter()
    }
    func setSegment(_ s: SegmentLength) {
        segment = s
        buildURLAndLoad()
    }
    func setCodec(_ c: CodecFilter) {
        codec = c
        applyContentFilter()
    }
    /// Pick a content clip for the main player. If the user's codec
    /// filter (Settings → Codec) prefers a different encoding of the
    /// same `clipId` than the one the caller passed, swap to that
    /// encoding's name. This lets a tap on a preview tile (which
    /// always shows the H.264 version, regardless of codec filter)
    /// route to the user's preferred codec on the main view.
    func setSelectedContent(_ name: String) {
        let clipId = content.first(where: { $0.name == name })?.clipId
            ?? ContentItem.deriveClipId(from: name)
        // Within the codec-filtered list (which respects protocol +
        // segment + codec), pick the entry sharing this clipId. Falls
        // back to the original name if no match (e.g. user picked AV1
        // and only H264 exists for this clip).
        let preferred = filteredContent.first(where: { $0.clipId == clipId })?.name
        let chosen = preferred ?? name
        log("setSelectedContent: tapped='\(name)' clipId='\(clipId)' chosen='\(chosen)' codec=\(codec.label) segment=\(segment.label) protocol=\(streamProtocol.label)")
        selectedContent = chosen
        buildURLAndLoad()
    }

    func setHUDVisible(_ visible: Bool) { hudVisible = visible }
    func setSettingsOpen(_ open: Bool) { settingsOpen = open }

    // MARK: - Filtering / Continue Watching / preview pool

    /// Catalogue with newest-wins dedup by (clipId, codec). Multiple
    /// timestamped re-encodings of the same logical clip with the same
    /// codec collapse to just the newest encoding's `ContentItem` — but
    /// the underlying `name` (with its timestamp suffix) is preserved
    /// so URL building uses the right directory on the server.
    var contentByLatestEncode: [ContentItem] {
        var byKey: [String: ContentItem] = [:]
        var firstSeenIndex: [String: Int] = [:]
        for (idx, item) in content.enumerated() {
            let key = "\(item.clipId)::\(item.effectiveCodec)"
            if firstSeenIndex[key] == nil { firstSeenIndex[key] = idx }
            if let existing = byKey[key] {
                let existingTs = existing.encodeTimestamp ?? .distantPast
                let candidateTs = item.encodeTimestamp ?? .distantPast
                if candidateTs > existingTs {
                    byKey[key] = item
                }
            } else {
                byKey[key] = item
            }
        }
        return byKey.values.sorted {
            (firstSeenIndex["\($0.clipId)::\($0.effectiveCodec)"] ?? .max)
            < (firstSeenIndex["\($1.clipId)::\($1.effectiveCodec)"] ?? .max)
        }
    }

    /// Catalogue filtered by current protocol + segment + codec
    /// preferences. Operates on the newest-wins-deduped catalogue, so
    /// users see one entry per (clipId, codec) regardless of how many
    /// times a clip has been re-encoded.
    var filteredContent: [ContentItem] {
        contentByLatestEncode.filter { item in
            guard codec.matches(item) else { return false }
            switch streamProtocol {
            case .hls: guard item.hasHls else { return false }
            case .dash: guard item.hasDash else { return false }
            }
            switch segment {
            case .ll: return true
            case .s2: return item.segmentDuration == 2 || item.segmentDuration == nil
            case .s6: return item.segmentDuration == 6 || item.segmentDuration == nil
            }
        }
    }

    /// Catalogue filtered for the **preview row only** — protocol +
    /// segment honoured, but the user's codec filter is intentionally
    /// ignored. Preview tiles always decode the 360p H.264 rendition
    /// regardless of the user's codec preference, so showing all clips
    /// (deduped by `clipId`, preferring the H.264 entry when multiple
    /// codec encodings of the same clip exist) lets the user browse
    /// every clip while the main playback respects their codec choice.
    var previewContent: [ContentItem] {
        // Start from the newest-wins-deduped catalogue so multiple
        // re-encodings of the same (clip, codec) have already collapsed
        // to a single entry per encoding.
        let candidates = contentByLatestEncode.filter { item in
            switch streamProtocol {
            case .hls: guard item.hasHls else { return false }
            case .dash: guard item.hasDash else { return false }
            }
            switch segment {
            case .ll: return true
            case .s2: return item.segmentDuration == 2 || item.segmentDuration == nil
            case .s6: return item.segmentDuration == 6 || item.segmentDuration == nil
            }
        }
        // Dedupe by clipId. When multiple codec encodings exist for the
        // same clip, prefer h264 (the 360p tile path is H.264 anyway).
        var byClip: [String: ContentItem] = [:]
        for item in candidates {
            if let existing = byClip[item.clipId] {
                let existingIsH264 = existing.effectiveCodec == "h264"
                let candidateIsH264 = item.effectiveCodec == "h264"
                if !existingIsH264 && candidateIsH264 {
                    byClip[item.clipId] = item
                }
            } else {
                byClip[item.clipId] = item
            }
        }
        // First-appearance index of each clipId in `candidates`. Built
        // manually because multiple codec encodings of one clip share a
        // clipId, so `Dictionary(uniqueKeysWithValues:)` would crash.
        var order: [String: Int] = [:]
        for (idx, item) in candidates.enumerated() where order[item.clipId] == nil {
            order[item.clipId] = idx
        }
        return byClip.values.sorted { (order[$0.clipId] ?? .max) < (order[$1.clipId] ?? .max) }
    }

    /// Catalogue order, deduped one-entry-per-clipId via `previewContent`,
    /// codec filter ignored. The Continue Watching tile lands wherever
    /// the catalogue placed it — the live row scrolls to centre on it
    /// at appearance time, so users get a few neighbours before and
    /// after instead of a re-shuffled "frequently watched" list.
    ///
    /// Default `limit` is 64 — large enough that "scroll the whole row"
    /// shows essentially the entire deduped catalogue, but bounded so
    /// the LazyHStack has a known item count and doesn't explode if
    /// the server returns thousands of items. Off-screen tiles self-
    /// gate decoding via `.onAppear` / `.onDisappear`, so a long pool
    /// doesn't translate to a long list of active decoders.
    func previewPool(limit: Int = 64) -> [ContentItem] {
        Array(previewContent.prefix(limit))
    }

    private func applyContentFilter() {
        let wasPlaying = currentURL != nil
        let filtered = filteredContent
        guard !filtered.isEmpty else {
            selectedContent = ""
            currentURL = nil
            player.replaceCurrentItem(with: nil)
            return
        }
        let pick = filtered.contains(where: { $0.name == selectedContent })
            ? selectedContent
            : (filtered.first?.name ?? "")
        if pick != selectedContent { selectedContent = pick }
        if wasPlaying { buildURLAndLoad() }
    }

    // MARK: - Content fetch

    /// Cold-fetch + cache update. Cached content is already shown at
    /// init() — this refreshes from the server without blanking the UI
    /// even if the network call fails or is slow.
    func fetchContentList() {
        guard let server = activeServer else {
            self.content = []
            self.statusText = "No server selected"
            return
        }
        self.statusText = "Loading content list…"
        Task {
            do {
                let url = URL(string: "\(server.contentURL)/api/content")!
                var request = URLRequest(url: url)
                request.timeoutInterval = 5
                let (data, response) = try await URLSession.shared.data(for: request)
                guard let http = response as? HTTPURLResponse,
                      (200..<300).contains(http.statusCode) else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    self.statusText = "Refresh failed: HTTP \(code)"
                    return
                }
                let items = (try? JSONDecoder().decode([ContentItem].self, from: data)) ?? []
                self.content = items
                self.statusText = "Loaded \(items.count) items"
                self.writeContentCache(items, for: server)
                self.applyContentFilter()
            } catch {
                self.statusText = "Refresh failed: \(error.localizedDescription)"
            }
        }
    }

    // MARK: - URL build + load

    private func buildURLAndLoad() {
        guard let server = activeServer, !selectedContent.isEmpty else { return }
        // Fresh play_id at every loadStream boundary — issue #280.
        regeneratePlayID()
        var url = StreamURLBuilder.playbackURL(
            server: server,
            contentName: selectedContent,
            protocolOption: streamProtocol,
            segment: segment,
            playerId: playerId,
            localProxy: localProxy
        )
        guard let resolved = url else { return }
        // k3s-dev content port (40000) doesn't accept ?player_id= —
        // strip it unless the user explicitly opted in via the Advanced
        // toggle. Mirrors the legacy `is40000 && !allowPlayerIdOnContentPort`
        // behaviour from PlaybackViewModel.startPlayback.
        if resolved.port == 40000 && !allowPlayerIdOnContentPort {
            url = Self.removePlayerId(from: resolved)
        } else {
            url = resolved
        }
        guard var final = url else { return }
        // Append play_id last so it survives the player_id strip above
        // for k3s-dev's content port. (For HAR scoping go-proxy reads
        // play_id from URLs that hit it; the content port stripping
        // only affects routes that don't reach go-proxy.)
        final = appendPlayID(to: final)
        self.currentURL = final
        self.statusText = final.absoluteString
        loadStream(url: final)
    }

    private static func removePlayerId(from url: URL) -> URL {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
        components.queryItems = components.queryItems?.filter { $0.name != "player_id" }
        if components.queryItems?.isEmpty == true { components.queryItems = nil }
        return components.url ?? url
    }

    /// Replace any existing `play_id` query item with the current
    /// `currentPlayID`. Issue #280.
    private func appendPlayID(to url: URL) -> URL {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
        var items = components.queryItems ?? []
        items.removeAll { $0.name == "play_id" }
        items.append(URLQueryItem(name: "play_id", value: currentPlayID))
        components.queryItems = items
        return components.url ?? url
    }

    /// Mint a fresh `play_id` UUID. Called at every new playback
    /// episode boundary so go-proxy can scope its network log per play.
    private func regeneratePlayID() {
        currentPlayID = UUID().uuidString
    }

    private func loadStream(url: URL) {
        // Master playlist preflight — for HLS master URLs, hit the URL
        // up to 5× to capture redirects (go-proxy hands out a 302 to a
        // per-session port) and back off on 429s before handing the
        // *resolved* URL to AVPlayer. Without this, a transient 429 on
        // the first request can wedge the AVPlayer in a permanent
        // failed state.
        if isMasterPlaylistURL(url) {
            Task { @MainActor [weak self] in
                guard let self else { return }
                let resolved = await self.preflightMasterPlaylist(url: url)
                self.startPlayback(resolvedURL: resolved)
            }
        } else {
            startPlayback(resolvedURL: url)
        }
    }

    private func startPlayback(resolvedURL url: URL) {
        // Tear down any prior item so the audio renderer doesn't briefly
        // overlap with the new one. Mirrors the Android stop+clear pattern.
        player.replaceCurrentItem(with: nil)

        // Route through the on-device LocalHTTPProxy when enabled —
        // gives us per-chunk wire-byte accounting via RequestTracker
        // (powers the dashboard's NETWORK lane / network_bitrate_mbps
        // field) and is the surface the failure-injection harness hits.
        let assetURL: URL
        if localProxy {
            LocalHTTPProxy.shared.startIfNeeded()
            assetURL = LocalHTTPProxy.shared.rewrite(originURL: url) ?? url
        } else {
            assetURL = url
        }
        RequestTracker.shared.reset()

        let asset = AVURLAsset(url: assetURL, options: nil)
        let item = AVPlayerItem(asset: asset)
        // Preserve the server-advertised live offset across stall
        // recoveries — without this, AVPlayer snaps back to the live
        // edge on every stall and leaves zero safety margin before the
        // oldest-edge of the window.
        item.automaticallyPreservesTimeOffsetFromLive = true
        apply4kPreference(to: item)
        player.replaceCurrentItem(with: item)
        player.isMuted = isMuted
        player.automaticallyWaitsToMinimizeStalling = true
        player.play()
        if goLive {
            seekToLiveEdge(item: item)
        } else if liveOffsetSeconds > 0 {
            // User-configured custom offset — overrides the manifest's
            // HOLD-BACK on first ready-to-play. Mirrors testing-session.html.
            scheduleLiveOffsetSeek(reason: "playback started")
        }
        hasReportedFirstFrame = false

        // Reset per-playback metrics state and emit the `playing` event.
        diagnostics.reset()
        playbackStartAt = Date()
        videoFirstFrameSeconds = nil
        videoPlayingTimeSeconds = nil
        firstFrameReported = false
        playingReported = false
        lastReportedRenditionMbps = nil
        lastReportedStallCount = 0
        lastReportedStallDuration = 0
        zeroBufferStartedAt = nil
        metricsSessionId = nil
        metricsLastSessionLookup = nil
        Task { [weak self] in
            await self?.sendPlayerMetrics(event: "playing", extra: [
                "player_metrics_content_url": url.absoluteString,
                "player_metrics_content_name": self?.selectedContent ?? ""
            ])
        }
    }

    private func isMasterPlaylistURL(_ url: URL) -> Bool {
        let path = url.path.lowercased()
        return path.contains("master_") && path.hasSuffix(".m3u8")
    }

    /// Preflight the master URL — captures redirects (so AVPlayer's
    /// item is built off the resolved per-session URL) and falls back
    /// across segment-length variants if the requested one 404s.
    ///
    /// The encoding pipeline produces master.m3u8 + master_2s.m3u8 +
    /// master_6s.m3u8 independently per clip. Some older content has
    /// only the original master.m3u8; some has master.m3u8 + a sub-
    /// playlist (e.g. playlist_6s_360p.m3u8) but no top-level
    /// master_6s.m3u8. Probe the candidates in user-preference order
    /// and use the first one that responds 200, so a clip with a
    /// missing master_6s still plays at master_2s or LL master.
    private func preflightMasterPlaylist(url: URL) async -> URL {
        let candidates = masterFallbackChain(for: url)
        for candidate in candidates {
            if let resolved = await preflightSingleMaster(url: candidate) {
                if candidate.absoluteString != url.absoluteString {
                    log("Master preflight: requested \(url.lastPathComponent) missing, using \(candidate.lastPathComponent)")
                }
                return resolved
            }
        }
        // All candidates failed — hand back the original URL and let
        // AVPlayer surface the error via the normal item-status path.
        return url
    }

    /// Generate the ordered fallback chain for a master URL. If the
    /// caller asked for `master_6s.m3u8`, we try `_6s` then `_2s` then
    /// the un-suffixed master. If they asked for `_2s`, `_6s` is the
    /// next candidate, then plain master. LL master (`master.m3u8`)
    /// only falls back to itself — there's no shorter form.
    private func masterFallbackChain(for url: URL) -> [URL] {
        let path = url.path
        let suffixes = ["_6s.m3u8", "_2s.m3u8", ".m3u8"]
        guard let matched = suffixes.first(where: { path.hasSuffix("/master\($0)") || path.hasSuffix("/manifest\($0.replacingOccurrences(of: ".m3u8", with: ".mpd"))") })
        else { return [url] }
        let stem = String(path.dropLast(matched.count))
        // For each candidate suffix not equal to the matched one, build a
        // new URL preserving query (player_id) + scheme + host + port.
        var chain: [URL] = []
        // Always try the user's requested variant first.
        chain.append(url)
        let isHLS = matched.hasSuffix(".m3u8")
        let manifestBase = isHLS ? "/master" : "/manifest"
        for s in suffixes where s != matched {
            // Strip the leading underscore for ".m3u8" — the un-suffixed
            // variant uses just "master.m3u8" / "manifest.mpd".
            let altSuffix = isHLS ? s : s.replacingOccurrences(of: ".m3u8", with: ".mpd")
            var components = URLComponents(url: url, resolvingAgainstBaseURL: false)!
            // stem already excludes the matched suffix and includes the
            // path up to "/master" or "/manifest" — build the new path
            // by appending the alt suffix.
            let stemWithoutBasename = String(stem.dropLast((isHLS ? "/master".count : "/manifest".count)))
            components.path = stemWithoutBasename + manifestBase + altSuffix
            if let alt = components.url { chain.append(alt) }
        }
        return chain
    }

    /// Single-URL preflight with retry-after-aware 429 handling.
    /// Returns the resolved URL (after redirects) on 200, or nil on
    /// any non-200 status / network error.
    private func preflightSingleMaster(url: URL) async -> URL? {
        let maxAttempts = 5
        let defaultDelayMs: UInt64 = 500
        for attempt in 1...maxAttempts {
            var request = URLRequest(url: url)
            request.cachePolicy = .reloadIgnoringLocalCacheData
            applyPlayerHeaders(to: &request)
            do {
                let (_, response) = try await URLSession.shared.data(for: request)
                guard let http = response as? HTTPURLResponse else { return nil }
                if http.statusCode == 200 { return http.url ?? url }
                if http.statusCode == 429 {
                    let retry = http.value(forHTTPHeaderField: "Retry-After")
                        .flatMap { Double($0.trimmingCharacters(in: .whitespaces)) }
                        .map { UInt64($0 * 1000) } ?? defaultDelayMs
                    try? await Task.sleep(nanoseconds: retry * 1_000_000)
                    continue
                }
                // 4xx / 5xx other than 429 — caller will try next candidate.
                log("Master preflight \(http.statusCode) for \(url.lastPathComponent)")
                return nil
            } catch {
                log("Master preflight attempt \(attempt)/\(maxAttempts) failed: \(error.localizedDescription)")
                return nil
            }
        }
        return nil
    }

    /// Apply the `allow4K` flag to a freshly-built AVPlayerItem.
    /// `preferredMaximumResolution` caps the renditions AVPlayer is
    /// willing to switch to — when 4K is off we cap at 1080p so the
    /// device decoder isn't asked to do 4K H.264.
    ///
    /// The simulator used to be hard-capped at 1080p regardless of the
    /// toggle because Intel-host sims couldn't reliably decode 4K HEVC.
    /// On Apple-Silicon hosts the simulator routes decode through the
    /// host's hardware HEVC decoder and 4K plays fine, so the override
    /// is gone — sim now honours `allow4K`. If a particular host can't
    /// decode, AVPlayer surfaces `decodeFailedNotification` and the
    /// existing recovery pipeline kicks in (same path real devices use).
    private func apply4kPreference(to item: AVPlayerItem) {
        item.preferredPeakBitRate = 0
        guard #available(iOS 15.0, tvOS 15.0, *) else { return }
        if allow4K {
            item.preferredMaximumResolution = CGSize(width: 3840, height: 2160)
        } else {
            item.preferredMaximumResolution = CGSize(width: 1920, height: 1080)
        }
    }

    private func seekToLiveEdge(item: AVPlayerItem) {
        // Playable-range end = live edge for HLS; jump there once it's known.
        Task { @MainActor [weak self] in
            for _ in 0..<10 {
                try? await Task.sleep(nanoseconds: 200_000_000)
                guard let self else { return }
                if let last = item.seekableTimeRanges.last?.timeRangeValue {
                    let edge = CMTimeAdd(last.start, last.duration)
                    await self.player.seek(to: edge)
                    return
                }
            }
        }
    }

    /// Schedule a seek to `liveEdge - liveOffsetSeconds`. Polls every
    /// 250 ms (up to 20 s) until the seekable range becomes available
    /// — same shape as `scheduleLiveOffsetSeek` in testing-session.html.
    /// No-op when the offset is 0 or negative.
    private var liveOffsetSeekTask: Task<Void, Never>?
    private func scheduleLiveOffsetSeek(reason: String) {
        liveOffsetSeekTask?.cancel()
        guard liveOffsetSeconds > 0 else { return }
        let target = liveOffsetSeconds
        liveOffsetSeekTask = Task { @MainActor [weak self] in
            let deadline = Date().addingTimeInterval(20)
            while Date() < deadline {
                guard !Task.isCancelled, let self else { return }
                if let item = self.player.currentItem,
                   let lastSeek = item.seekableTimeRanges.last?.timeRangeValue {
                    let edge = CMTimeGetSeconds(CMTimeAdd(lastSeek.start, lastSeek.duration))
                    if edge.isFinite, edge > 0 {
                        let seekTarget = max(0, edge - target)
                        await self.player.seek(to: CMTime(seconds: seekTarget, preferredTimescale: 600))
                        self.log("LIVE OFFSET: applied \(target)s behind live (\(reason))")
                        return
                    }
                }
                try? await Task.sleep(nanoseconds: 250_000_000)
            }
            self?.log("LIVE OFFSET: gave up after 20s (seekable not ready)")
        }
    }

    /// Light `currentURL` reset. Mirrors Android `clearCurrentUrl()` —
    /// signals to applyContentFilter that we're not actively playing.
    func clearCurrentURL() {
        currentURL = nil
        player.replaceCurrentItem(with: nil)
    }

    // MARK: - Recovery (Retry + Reload)

    /// Manual trigger for the auto-recovery path. Same call the
    /// codec-error handler and the auto-recovery branch make: replace
    /// the *same* URL on the *same* AVPlayer.
    func retry() {
        guard let url = currentURL else { return }
        // A retry is a new playback episode — fresh play_id so the
        // proxy's network log scopes the next round of requests
        // separately from the one that just failed. Issue #280.
        regeneratePlayID()
        let refreshed = appendPlayID(to: url)
        self.currentURL = refreshed
        Task { [weak self] in
            await self?.requestHARSnapshot(reason: "user_retry", force: true)
        }
        loadStream(url: refreshed)
    }

    /// Full tear-down + recreate. Replaces the AVPlayer instance,
    /// re-subscribes the player-item observers, bumps `playerEpoch` so
    /// the PlayerView remounts, then re-issues the original URL — the
    /// proxy hands out a fresh redirect target on the next request.
    /// Right tool after a server restart.
    ///
    /// Same `playerId` (proxy session continuity), no catalogue refetch.
    /// 911 button — marks "something interesting just happened" so a
    /// HAR snapshot of the current session timeline lands on the
    /// server's incidents directory. The metrics POST also surfaces
    /// the marker on the dashboard's events swim lane. Doesn't touch
    /// playback — purely a forensic capture. Also writes a "911" line
    /// to the device console so screen-recording / OS log captures
    /// have a synchronisation point.
    func mark911() {
        let stamp = Self.metricsTimestampFormatter.string(from: Date())
        print("911 user-marked at \(stamp) currentURL=\(currentURL?.absoluteString ?? "—")")
        Task { [weak self] in
            await self?.sendPlayerMetrics(event: "user_marked", extra: [
                "player_metrics_user_marked_at": stamp
            ])
        }
    }

    func reload() {
        Task { [weak self] in
            await self?.requestHARSnapshot(reason: "user_reload", force: true)
        }
        // Snapshot cumulative counters into diagnostics' "prior" buckets
        // so the new playback continues to add to the same dropped /
        // displayed totals — matches legacy behaviour.
        diagnostics.snapshotForRestart()
        playerRestarts += 1
        let oldPlayer = player
        let newPlayer = AVPlayer()
        self.player = newPlayer
        playerEpoch &+= 1
        oldPlayer.replaceCurrentItem(with: nil)
        // Rebind diagnostics to the new AVPlayer instance — without this
        // the @Published metric fields would freeze on the old player's
        // observations and the heartbeat would emit stale data.
        diagnostics.bind(to: newPlayer)
        attachPlayerItemObservers()
        Task { [weak self] in
            await self?.sendPlayerMetrics(event: "restart", extra: [
                "player_metrics_restart_reason": "reload",
                "player_restarts": self?.playerRestarts ?? 0
            ])
        }
        currentURL = nil
        buildURLAndLoad()
    }

    // MARK: - App lifecycle

    private func attachLifecycleObservers() {
        let nc = NotificationCenter.default
        didEnterBackgroundObserver = nc.addObserver(
            forName: UIApplication.didEnterBackgroundNotification,
            object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in
                // Drop the codec resources when backgrounded — match
                // Android's onActivityStopped behaviour. AVPlayer doesn't
                // hold a hardware decoder when nil item is set, and pause
                // alone wouldn't free anything.
                self?.player.pause()
                self?.player.replaceCurrentItem(with: nil)
            }
        }
        willEnterForegroundObserver = nc.addObserver(
            forName: UIApplication.willEnterForegroundNotification,
            object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in
                // Re-prepare from the URL we had loaded before backgrounding.
                guard let url = self?.currentURL else { return }
                self?.loadStream(url: url)
            }
        }
    }

    // MARK: - Player item observers

    private func attachPlayerItemObservers() {
        let nc = NotificationCenter.default
        if let o = didPlayToEndObserver { nc.removeObserver(o) }
        if let o = failedToPlayObserver { nc.removeObserver(o) }
        didPlayToEndObserver = nc.addObserver(
            forName: .AVPlayerItemDidPlayToEndTime, object: nil, queue: .main
        ) { [weak self] _ in
            // Live streams shouldn't end. If they do, treat as a stall
            // and Retry (same path auto-recovery uses).
            Task { @MainActor [weak self] in
                self?.handleErrorRetry(reason: "playedToEnd")
            }
        }
        failedToPlayObserver = nc.addObserver(
            forName: .AVPlayerItemFailedToPlayToEndTime, object: nil, queue: .main
        ) { [weak self] note in
            let err = (note.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? Error)
            Task { @MainActor [weak self] in
                self?.handlePlayerError(err)
            }
        }
        // First-frame callback — bump lastPlayed + viewCounts.
        // AVPlayer doesn't expose a direct "first frame rendered" event,
        // so we observe `isReadyForDisplay` on the AVPlayerLayer in the
        // PlayerView wrapper. The wrapper calls `markFirstFrameRendered`
        // when it sees the layer flip.
        statusObserver = player.observe(\.timeControlStatus, options: [.new]) { [weak self] p, _ in
            Task { @MainActor in
                guard let self else { return }
                if p.timeControlStatus == .playing && !self.hasReportedFirstFrame {
                    self.markFirstFrameRendered()
                }
            }
        }
    }

    /// Called by the PlayerView wrapper when the AVPlayerLayer reports
    /// `isReadyForDisplay = true` for the current item.
    func markFirstFrameRendered() {
        guard !hasReportedFirstFrame else { return }
        hasReportedFirstFrame = true
        codecRetries = 0
        let current = selectedContent
        guard !current.isEmpty else { return }
        let clipId = ContentItem.deriveClipId(from: current)
        viewCounts[clipId, default: 0] += 1
        lastPlayed = current
        persistPlaybackHistory()
    }

    private func handlePlayerError(_ error: Error?) {
        let message = error?.localizedDescription ?? "playback error"
        // Codec errors on Apple devices are extremely rare (the chips
        // hardware-decode H.264 / HEVC / AV1 with plenty of headroom),
        // so we don't classify them — the only retry surface is the
        // auto-recovery flag. AndroidView counterpart's NO_MEMORY path
        // doesn't apply here.
        statusText = "Error: \(message)"
        if autoRecovery, let url = currentURL {
            Task { @MainActor [weak self] in
                try? await Task.sleep(nanoseconds: 500_000_000)
                self?.loadStream(url: url)
            }
        }
    }

    private func handleErrorRetry(reason: String) {
        guard codecRetries < maxCodecRetries, let url = currentURL else { return }
        codecRetries += 1
        let retries = codecRetries
        statusText = "\(reason) — retry \(retries)/\(maxCodecRetries)"
        Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(150_000_000 * retries))
            self?.loadStream(url: url)
        }
    }

    // MARK: - Persistence (UserDefaults)

    private static let serversKey = "is.servers.v2"
    private static let activeServerKey = "is.servers.active"
    private static let flagDevMode = "is.flag.dev_mode"
    private static let flag4K = "is.flag.4k"
    private static let flagLocalProxy = "is.flag.local_proxy"
    private static let flagAutoRecovery = "is.flag.auto_recovery"
    private static let flagGoLive = "is.flag.go_live"
    private static let flagSkipHome = "is.flag.skip_home"
    private static let flagMuted = "is.flag.muted"
    private static let flagLiveOffset = "is.flag.live_offset_s"
    private static let flagPreviewVideoSlots = "is.flag.preview_video_slots"
    private static let lastPlayedKey = "is.lastPlayed"
    private static let viewCountsKey = "is.viewCounts"
    private static let codecKey = "is.codec"
    private static let segmentKey = "is.segment"
    private static let protocolKey = "is.protocol"
    private static let contentCachePrefix = "is.contentCache."

    private func loadServers() {
        let d = UserDefaults.standard
        if let data = d.data(forKey: Self.serversKey),
           let list = try? JSONDecoder().decode([ServerProfile].self, from: data) {
            servers = list
        }
        if let s = d.string(forKey: Self.activeServerKey), let id = UUID(uuidString: s) {
            activeServerID = id
        } else {
            activeServerID = servers.first?.id
        }
    }

    private func persistServers() {
        let d = UserDefaults.standard
        if let data = try? JSONEncoder().encode(servers) {
            d.set(data, forKey: Self.serversKey)
        }
        if let id = activeServerID {
            d.set(id.uuidString, forKey: Self.activeServerKey)
        } else {
            d.removeObject(forKey: Self.activeServerKey)
        }
    }

    private func loadAdvancedFlags() {
        let d = UserDefaults.standard
        developerMode    = d.bool(forKey: Self.flagDevMode)
        allow4K          = d.object(forKey: Self.flag4K) as? Bool ?? true
        localProxy       = d.object(forKey: Self.flagLocalProxy) as? Bool ?? true
        autoRecovery     = d.bool(forKey: Self.flagAutoRecovery)
        goLive           = d.bool(forKey: Self.flagGoLive)
        skipHomeOnLaunch = d.bool(forKey: Self.flagSkipHome)
        isMuted = d.bool(forKey: Self.flagMuted)
        liveOffsetSeconds = d.object(forKey: Self.flagLiveOffset) as? Double ?? 0
        // First launch: no key yet → use the device's hardware cap so
        // the user starts with the richest preview their hardware can
        // run. After that, persist whatever they've chosen.
        let storedSlots = d.object(forKey: Self.flagPreviewVideoSlots) as? Int
        let hwCap = DecodeBudget.shared.hardwareCap
        previewVideoSlots = storedSlots.map { max(0, min($0, hwCap)) } ?? hwCap
        DecodeBudget.shared.setUserCap(previewVideoSlots)
        lastPlayed       = d.string(forKey: Self.lastPlayedKey) ?? ""
        if let data = d.data(forKey: Self.viewCountsKey),
           let map = try? JSONDecoder().decode([String: Int].self, from: data) {
            viewCounts = map
        }
        if let raw = d.string(forKey: Self.codecKey), let v = CodecFilter(rawValue: raw) {
            codec = v
        }
        if let raw = d.string(forKey: Self.segmentKey), let v = SegmentLength(rawValue: raw) {
            segment = v
        }
        if let raw = d.string(forKey: Self.protocolKey), let v = StreamProtocol(rawValue: raw) {
            streamProtocol = v
        }
    }

    private func persistFlags() {
        let d = UserDefaults.standard
        d.set(developerMode, forKey: Self.flagDevMode)
        d.set(allow4K, forKey: Self.flag4K)
        d.set(localProxy, forKey: Self.flagLocalProxy)
        d.set(autoRecovery, forKey: Self.flagAutoRecovery)
        d.set(goLive, forKey: Self.flagGoLive)
        d.set(skipHomeOnLaunch, forKey: Self.flagSkipHome)
        d.set(isMuted, forKey: Self.flagMuted)
        d.set(liveOffsetSeconds, forKey: Self.flagLiveOffset)
        d.set(previewVideoSlots, forKey: Self.flagPreviewVideoSlots)
        d.set(codec.rawValue, forKey: Self.codecKey)
        d.set(segment.rawValue, forKey: Self.segmentKey)
        d.set(streamProtocol.rawValue, forKey: Self.protocolKey)
    }

    private func persistPlaybackHistory() {
        let d = UserDefaults.standard
        d.set(lastPlayed, forKey: Self.lastPlayedKey)
        if let data = try? JSONEncoder().encode(viewCounts) {
            d.set(data, forKey: Self.viewCountsKey)
        }
    }

    // -- Per-server content cache (stale-while-revalidate) ------------------

    private func cacheKey(for server: ServerProfile) -> String {
        guard let host = URL(string: server.contentURL)?.host else {
            return "\(Self.contentCachePrefix)unknown"
        }
        let port = URL(string: server.contentURL)?.port ?? 0
        return "\(Self.contentCachePrefix)\(host):\(port)"
    }

    private func readContentCache(for server: ServerProfile) -> [ContentItem] {
        guard let data = UserDefaults.standard.data(forKey: cacheKey(for: server)),
              let list = try? JSONDecoder().decode([ContentItem].self, from: data) else {
            return []
        }
        return list
    }

    private func writeContentCache(_ list: [ContentItem], for server: ServerProfile) {
        if let data = try? JSONEncoder().encode(list) {
            UserDefaults.standard.set(data, forKey: cacheKey(for: server))
        }
    }
}

// MARK: - Diagnostics + metrics extension (lifted from legacy PlaybackViewModel)
//
// Verbatim port of the metrics-emit pipeline from the pre-rework
// PlaybackViewModel — same heartbeat cadence, same event-set, same
// @Published-driven sinks. Kept in this file so it can read the VM's
// private fields directly.

extension PlayerViewModel {

    fileprivate func bindDiagnosticsLogging() {
        diagnostics.$itemError
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                self?.log("Item error: \(value)")
                Task { await self?.sendPlayerMetrics(event: "error", extra: ["player_metrics_error": value]) }
            }
            .store(in: &cancellables)

        diagnostics.$lastFailure
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                guard let self else { return }
                self.log("Playback failure: \(value)")
                Task { await self.sendPlayerMetrics(event: "error", extra: ["player_metrics_error": value]) }
                if self.autoRecovery {
                    // Per-attempt HAR is captured by scheduleAutoRecoveryRestart
                    // when the timer fires (force=true). Don't double-capture
                    // here.
                    self.scheduleAutoRecoveryRestart(reason: "auto_recovery_failure")
                } else {
                    Task { await self.requestHARSnapshot(reason: "playback_failure") }
                }
            }
            .store(in: &cancellables)

        diagnostics.$lastError
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                self?.log("Player error: \(value)")
                Task { await self?.sendPlayerMetrics(event: "error", extra: ["player_metrics_error": value]) }
            }
            .store(in: &cancellables)

        diagnostics.$frozenDetected
            .removeDuplicates()
            .sink { [weak self] frozen in
                guard let self, frozen else { return }
                Task { await self.sendPlayerMetrics(event: "frozen") }
                if self.autoRecovery {
                    // The recovery timer's per-attempt HAR (force=true)
                    // captures the same incident a moment later.
                    self.scheduleAutoRecoveryRestart(reason: "auto_recovery_frozen")
                } else {
                    Task { await self.requestHARSnapshot(reason: "frozen") }
                }
            }
            .store(in: &cancellables)

        diagnostics.$segmentStallDetected
            .removeDuplicates()
            .sink { [weak self] stalled in
                guard let self, stalled else { return }
                Task { await self.sendPlayerMetrics(event: "segment_stall") }
                if self.autoRecovery {
                    self.scheduleAutoRecoveryRestart(reason: "auto_recovery_segment_stall")
                } else {
                    Task { await self.requestHARSnapshot(reason: "segment_stall") }
                }
            }
            .store(in: &cancellables)
    }

    fileprivate func bindMetricsReporting() {
        diagnostics.$stallCount
            .removeDuplicates()
            .sink { [weak self] count in
                guard let self, count > self.lastReportedStallCount else { return }
                self.lastReportedStallCount = count
                Task { await self.sendPlayerMetrics(event: "stall_start") }
            }
            .store(in: &cancellables)

        diagnostics.$lastStallDurationSeconds
            .removeDuplicates()
            .sink { [weak self] duration in
                guard let self, duration > 0, duration != self.lastReportedStallDuration else { return }
                self.lastReportedStallDuration = duration
                Task {
                    await self.sendPlayerMetrics(event: "stall_end", extra: [
                        "player_metrics_last_stall_time_s": self.roundSeconds(duration)
                    ])
                }
            }
            .store(in: &cancellables)

        diagnostics.$state
            .removeDuplicates()
            .sink { [weak self] state in
                guard let self else { return }
                let previous = self.lastReportedState
                if let previous, previous != state {
                    Task {
                        await self.sendPlayerMetrics(event: "state_change", extra: [
                            "player_metrics_state_from": previous,
                            "player_metrics_state_to": state
                        ])
                    }
                    if state == "buffering" {
                        Task { await self.sendPlayerMetrics(event: "buffering_start") }
                    } else if previous == "buffering" {
                        Task { await self.sendPlayerMetrics(event: "buffering_end") }
                    }
                } else if previous == nil {
                    Task { await self.sendPlayerMetrics(event: "state_change") }
                }
                self.lastReportedState = state
            }
            .store(in: &cancellables)

        diagnostics.$currentTime
            .removeDuplicates()
            .sink { [weak self] currentTime in
                guard let self, let startAt = self.playbackStartAt else { return }
                let isActivelyPlaying = self.diagnostics.state == "playing"
                    && self.diagnostics.playbackRate > 0
                if !self.firstFrameReported && currentTime > 0 && isActivelyPlaying {
                    let elapsed = self.roundSeconds(Date().timeIntervalSince(startAt))
                    self.videoFirstFrameSeconds = elapsed
                    self.firstFrameReported = true
                    self.diagnostics.markFirstFrameRendered()
                    self.markFirstFrameRendered()
                    Task {
                        await self.sendPlayerMetrics(event: "video_first_frame", extra: [
                            "player_metrics_video_first_frame_time_s": elapsed
                        ])
                    }
                }
                if !self.playingReported && currentTime >= 0.1 && isActivelyPlaying {
                    let elapsed = self.roundSeconds(Date().timeIntervalSince(startAt))
                    self.videoPlayingTimeSeconds = elapsed
                    self.playingReported = true
                    Task {
                        await self.sendPlayerMetrics(event: "video_start_time", extra: [
                            "player_metrics_video_start_time_s": elapsed
                        ])
                    }
                }
            }
            .store(in: &cancellables)

        Publishers.CombineLatest(diagnostics.$indicatedBitrate, diagnostics.$averageVideoBitrate)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] indicated, average in
                self?.handleRenditionShift(indicated: indicated, average: average)
            }
            .store(in: &cancellables)

        diagnostics.timeJumpSubject
            .receive(on: DispatchQueue.main)
            .sink { [weak self] event in
                guard let self else { return }
                Task {
                    await self.sendPlayerMetrics(event: "timejump", extra: [
                        "player_metrics_timejump_from_s": self.roundSeconds(event.from),
                        "player_metrics_timejump_to_s": self.roundSeconds(event.to),
                        "player_metrics_timejump_delta_s": self.roundSeconds(event.to - event.from),
                        "player_metrics_timejump_origin": event.origin
                    ])
                }
            }
            .store(in: &cancellables)
    }

    fileprivate func startMetricsHeartbeat() {
        metricsHeartbeatTimer?.invalidate()
        metricsHeartbeatTimer = Timer.scheduledTimer(withTimeInterval: metricsHeartbeatSeconds, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in
                self?.evaluateAutoRecoveryIfNeeded()
                await self?.sendPlayerMetrics(event: "heartbeat")
            }
        }
    }

    fileprivate func scheduleAutoRecoveryRestart(reason: String) {
        if autoRecoveryRestartTimer != nil { return }
        if autoRecoveryAttempts >= autoRecoveryMaxAttempts {
            log("Auto-recovery: exhausted after \(autoRecoveryAttempts) consecutive attempts — giving up")
            return
        }
        let attempt = autoRecoveryAttempts + 1
        let delay = autoRecoveryBaseDelaySeconds * pow(2, Double(attempt - 1))
        let scheduledAtTime = diagnostics.currentTime
        log("Auto-recovery: attempt \(attempt)/\(autoRecoveryMaxAttempts) scheduled in \(Int(delay))s (\(reason))")
        autoRecoveryRestartTimer = Timer.scheduledTimer(withTimeInterval: delay, repeats: false) { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.autoRecoveryRestartTimer = nil
                let timeAdvanced = self.diagnostics.currentTime > scheduledAtTime + 0.5
                let recovered = self.diagnostics.state == "playing"
                    && !self.diagnostics.frozenDetected
                    && !self.diagnostics.segmentStallDetected
                    && timeAdvanced
                if recovered {
                    self.log("Auto-recovery: cancelled — recovered naturally (\(reason))")
                    self.autoRecoveryAttempts = 0
                    return
                }
                self.autoRecoveryAttempts = attempt
                self.log("Auto-recovery: attempt \(attempt)/\(self.autoRecoveryMaxAttempts) restarting (\(reason))")
                self.retry()
                Task { [weak self] in
                    await self?.sendPlayerMetrics(event: "restart", extra: [
                        "player_metrics_restart_reason": reason,
                        "player_restarts": self?.playerRestarts ?? 0
                    ])
                }
                // Each auto-recovery attempt deserves its own HAR — bypass
                // the per-player debounce so back-to-back attempts inside
                // the 30s window each produce a snapshot.
                Task { [weak self] in
                    await self?.requestHARSnapshot(reason: reason, attempt: attempt, force: true)
                }
                self.scheduleAutoRecoverySuccessCheck()
            }
        }
    }

    fileprivate func scheduleAutoRecoverySuccessCheck() {
        autoRecoveryVerifyTimer?.invalidate()
        autoRecoveryVerifyTimer = Timer.scheduledTimer(withTimeInterval: autoRecoveryVerifyDelaySeconds, repeats: false) { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.autoRecoveryVerifyTimer = nil
                if self.autoRecoveryRestartTimer != nil { return }
                let playingCleanly = self.diagnostics.state == "playing"
                    && !self.diagnostics.frozenDetected
                    && !self.diagnostics.segmentStallDetected
                if playingCleanly {
                    self.autoRecoveryAttempts = 0
                }
            }
        }
    }

    fileprivate func evaluateAutoRecoveryIfNeeded() {
        guard autoRecovery, currentURL != nil, player.timeControlStatus != .paused else {
            zeroBufferStartedAt = nil
            return
        }
        let depth = diagnostics.bufferDepth ?? -1
        if depth > 0.01 {
            zeroBufferStartedAt = nil
            return
        }
        let now = Date()
        if zeroBufferStartedAt == nil {
            zeroBufferStartedAt = now
            return
        }
        if now.timeIntervalSince(zeroBufferStartedAt ?? now) >= autoRecoveryThresholdSeconds {
            scheduleAutoRecoveryRestart(reason: "auto_recovery_zero_buffer")
        }
    }

    fileprivate func handleRenditionShift(indicated: Double?, average: Double?) {
        let bps = indicated ?? average
        guard let bps, bps > 0 else { return }
        let mbps = roundMetric(bps / 1_000_000)
        if let previous = lastReportedRenditionMbps {
            if mbps != previous {
                profileShiftCount = max(0, profileShiftCount) + 1
                Task {
                    await sendPlayerMetrics(event: "video_bitrate_change", extra: [
                        "player_metrics_video_bitrate_from_mbps": previous,
                        "player_metrics_video_bitrate_to_mbps": mbps,
                        "player_metrics_profile_shift_count": profileShiftCount
                    ])
                }
            }
            let delta = mbps - previous
            if abs(delta) >= 0.1 {
                let event = delta > 0 ? "rate_shift_up" : "rate_shift_down"
                Task {
                    await sendPlayerMetrics(event: event, extra: [
                        "player_metrics_rate_from_mbps": previous,
                        "player_metrics_rate_to_mbps": mbps
                    ])
                }
            }
        }
        lastReportedRenditionMbps = mbps
    }

    fileprivate func sendPlayerMetrics(event: String, extra: [String: Any] = [:]) async {
        guard currentURL != nil else { return }
        guard let baseURL = metricsBaseURL() else { return }
        guard let sessionId = await resolveMetricsSessionId(baseURL: baseURL) else { return }
        let payload = buildMetricsPayload(event: event, extra: extra)
        if payload.isEmpty { return }
        await patchSessionMetrics(sessionId: sessionId, baseURL: baseURL, payload: payload)
    }

    fileprivate func metricsBaseURL() -> URL? {
        guard let server = activeServer else { return nil }
        // Per legacy policy, prefer the playback (proxy) port for the
        // session lookup since the per-session API lives there. Fall
        // back to the content port if the playback URL is missing.
        if let url = URL(string: server.playbackURL) { return url }
        return URL(string: server.contentURL)
    }

    fileprivate func resolveMetricsSessionId(baseURL: URL) async -> String? {
        let now = Date()
        if let existing = metricsSessionId,
           let lastLookup = metricsLastSessionLookup,
           now.timeIntervalSince(lastLookup) < metricsSessionLookupSeconds {
            return existing
        }
        var request = URLRequest(url: baseURL.appendingPathComponent("api/sessions"))
        applyPlayerHeaders(to: &request)
        do {
            let (data, response) = try await URLSession.shared.data(for: request)
            if let http = response as? HTTPURLResponse, http.statusCode >= 400 { return nil }
            guard let json = try JSONSerialization.jsonObject(with: data) as? [[String: Any]] else { return nil }
            let match = json.first { entry in
                (entry["player_id"] as? String) == playerId
            }
            if let sessionId = match?["session_id"] as? String, !sessionId.isEmpty {
                metricsSessionId = sessionId
                metricsLastSessionLookup = now
                return sessionId
            }
        } catch {
            return nil
        }
        return nil
    }

    fileprivate func buildMetricsPayload(event: String, extra: [String: Any]) -> [String: Any] {
        let timestamp = Self.metricsTimestampFormatter.string(from: Date())
        let loopCount = max(0, diagnostics.loopCountPlayer)
        let loopIncrement = max(0, loopCount - lastReportedLoopCount)
        lastReportedLoopCount = loopCount
        var payload: [String: Any?] = [
            "player_metrics_source": "ios",
            "player_metrics_last_event": event,
            "player_metrics_trigger_type": event,
            "player_metrics_last_event_at": timestamp,
            "player_metrics_event_time": timestamp,
            "player_metrics_state": diagnostics.state,
            "player_metrics_waiting_reason": diagnostics.waitingReason,
            "player_metrics_position_s": roundSeconds(diagnostics.currentTime),
            "player_metrics_playback_rate": roundMetric(Double(diagnostics.playbackRate)),
            "player_metrics_buffer_depth_s": diagnostics.bufferDepth.map { roundSeconds($0) },
            "player_metrics_buffer_end_s": diagnostics.bufferedEnd.map { roundSeconds($0) },
            "player_metrics_seekable_end_s": diagnostics.seekableEnd.map { roundSeconds($0) },
            "player_metrics_live_edge_s": diagnostics.seekableEnd.map { roundSeconds($0) },
            "player_metrics_live_offset_s": diagnostics.liveOffset.map { roundSeconds($0) },
            // Encoded wall-clock at the playhead (epoch ms) — sourced from
            // EXT-X-PROGRAM-DATE-TIME via AVPlayerItem.currentDate(). Pairs
            // with the receiving side's clock to compute a ground-truth live
            // offset that survives stalls and HOLD-BACK shifts. Powers the
            // testing.html buffer-depth chart's "Wall-Clock Offset" trace.
            "player_metrics_playhead_wallclock_ms": diagnostics.playheadWallClock.map {
                Int64(($0.timeIntervalSince1970 * 1000).rounded())
            },
            // Client-side trueOffset = client_now - playhead_wallclock. Used
            // when the server's received_at stamp isn't available; biased by
            // client clock skew but useful as a fallback.
            "player_metrics_true_offset_s": diagnostics.playheadWallClock.map {
                roundSeconds(Date().timeIntervalSince($0))
            },
            "player_metrics_display_resolution": formatResolution(width: diagnostics.displayWidth, height: diagnostics.displayHeight),
            "player_metrics_video_resolution": formatResolution(width: diagnostics.videoWidth, height: diagnostics.videoHeight),
            "player_metrics_video_first_frame_time_s": videoFirstFrameSeconds,
            "player_metrics_video_start_time_s": videoPlayingTimeSeconds,
            "player_metrics_stall_count": diagnostics.stallCount,
            "player_metrics_stall_time_s": roundSeconds(diagnostics.stallTimeSeconds),
            "player_metrics_last_stall_time_s": roundSeconds(diagnostics.lastStallDurationSeconds),
            "player_metrics_frames_displayed": diagnostics.estimatedDisplayedFrames.map { roundMetric($0) },
            "player_metrics_dropped_frames": diagnostics.droppedVideoFrames.map { roundMetric($0) },
            "player_metrics_loop_count_player": loopCount,
            "player_metrics_loop_count_increment": loopIncrement,
            "player_metrics_profile_shift_count": profileShiftCount,
            "player_restarts": playerRestarts,
            "player_auto_recovery_enabled": autoRecovery,
            "player_metrics_video_bitrate_mbps": mbps(from: diagnostics.indicatedBitrate)
        ]
        extra.forEach { key, value in payload[key] = value }
        var compact: [String: Any] = [:]
        for (key, value) in payload {
            if let value { compact[key] = value }
        }
        if let mbpsValue = mbps(from: diagnostics.observedBitrate) {
            compact["player_metrics_avg_network_bitrate_mbps"] = mbpsValue
        } else {
            compact["player_metrics_avg_network_bitrate_mbps"] = NSNull()
        }
        if let mbpsValue = mbps(from: diagnostics.networkBitrate) {
            compact["player_metrics_network_bitrate_mbps"] = mbpsValue
        } else {
            compact["player_metrics_network_bitrate_mbps"] = NSNull()
        }
        return compact
    }

    /// Tell go-proxy to dump the current session timeline to disk as a HAR
    /// file. Called from auto-recovery / freeze / Reload sites — server
    /// debounces by player_id (default 30s). Issue #273.
    fileprivate func requestHARSnapshot(reason: String, attempt: Int = 0, force: Bool = false) async {
        guard let baseURL = metricsBaseURL() else { return }
        guard let sessionId = await resolveMetricsSessionId(baseURL: baseURL) else { return }
        let url = baseURL
            .appendingPathComponent("api/session")
            .appendingPathComponent(sessionId)
            .appendingPathComponent("har/snapshot")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        applyPlayerHeaders(to: &request)
        var metadata: [String: Any] = [
            "player_state": diagnostics.state,
            "buffer_depth_s": diagnostics.bufferDepth as Any,
            "stall_count": diagnostics.stallCount,
            "auto_recovery_attempt": attempt,
            "player_restarts": playerRestarts
        ]
        if !diagnostics.lastError.isEmpty { metadata["last_error"] = diagnostics.lastError }
        if !diagnostics.lastFailure.isEmpty { metadata["last_failure"] = diagnostics.lastFailure }
        let body: [String: Any] = [
            "reason": reason,
            "source": "player",
            "force": force,
            "metadata": metadata
        ]
        do {
            request.httpBody = try JSONSerialization.data(withJSONObject: body, options: [])
            _ = try await URLSession.shared.data(for: request)
        } catch {
            log("HAR snapshot request failed: \(error.localizedDescription)")
        }
    }

    fileprivate func patchSessionMetrics(sessionId: String, baseURL: URL, payload: [String: Any]) async {
        let url = baseURL.appendingPathComponent("api/session").appendingPathComponent(sessionId).appendingPathComponent("metrics")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        applyPlayerHeaders(to: &request)
        let body: [String: Any] = [
            "set": payload,
            "fields": Array(payload.keys)
        ]
        do {
            request.httpBody = try JSONSerialization.data(withJSONObject: body, options: [])
            _ = try await URLSession.shared.data(for: request)
        } catch {
            log("Metrics patch failed: \(error.localizedDescription)")
        }
    }

    /// Stamp `Player-ID` + `X-Playback-Session-Id` headers on a URLSession
    /// request so go-proxy can bind the request to our session for
    /// failure-injection routing. Mirrors the legacy `applyPlayerHeaders`.
    fileprivate func applyPlayerHeaders(to request: inout URLRequest) {
        request.setValue(playerId, forHTTPHeaderField: "Player-ID")
        request.setValue(playerId, forHTTPHeaderField: "X-Playback-Session-Id")
    }

    /// Migrate legacy `boss…` and `is…` UserDefaults keys to the new
    /// `is.flag.…` namespace so users upgrading from a pre-rework build
    /// keep their saved server list, codec / segment / protocol
    /// selection, and Advanced flag state.
    static func migrateLegacyDefaults() {
        let d = UserDefaults.standard
        // Pass 1: boss → is (matches the old in-app first-pass migration).
        let bossToIs: [(String, String)] = [
            ("bossSelectedContentFull", "isSelectedContentFull"),
            ("bossSelectedContent",     "isSelectedContent"),
            ("bossSelectedContentBase", "isSelectedContentBase"),
            ("bossSelectedCodec",       "isSelectedCodec"),
            ("bossSelectedSegment",     "isSelectedSegment"),
            ("bossSelectedProtocol",    "isSelectedProtocol"),
            ("bossSelectedUrl",         "isSelectedUrl"),
            ("bossAudioMuted",          "isAudioMuted"),
            ("bossBaseURL",             "isBaseURL"),
            ("bossPlaybackBaseURL",     "isPlaybackBaseURL"),
            ("bossPlayerId",            "isPlayerId"),
            ("bossPrefer4kNative",      "isPrefer4kNative"),
            ("bossAutoRecovery",        "isAutoRecovery"),
            ("bossGoLiveMode",          "isGoLiveMode"),
            ("bossLocalProxyEnabled",   "isLocalProxyEnabled"),
        ]
        for (old, new) in bossToIs {
            if let value = d.object(forKey: old) {
                if d.object(forKey: new) == nil { d.set(value, forKey: new) }
                d.removeObject(forKey: old)
            }
        }
        // Pass 2: legacy is* → new is.flag.* / is.* keys this VM uses.
        // String values were stored as "true" / "false" strings in the
        // legacy persist() (DefaultsKey enum). Coerce to Bool when copying.
        func copyBoolFromString(_ legacy: String, to current: String) {
            guard let s = d.string(forKey: legacy), d.object(forKey: current) == nil else {
                d.removeObject(forKey: legacy)
                return
            }
            d.set(s == "true", forKey: current)
            d.removeObject(forKey: legacy)
        }
        func copyStringIfMissing(_ legacy: String, to current: String) {
            guard let s = d.string(forKey: legacy) else { return }
            if d.object(forKey: current) == nil { d.set(s, forKey: current) }
            d.removeObject(forKey: legacy)
        }
        copyBoolFromString("isAudioMuted",         to: "is.flag.muted")
        copyBoolFromString("isPrefer4kNative",     to: "is.flag.4k")
        copyBoolFromString("isAutoRecovery",       to: "is.flag.auto_recovery")
        copyBoolFromString("isGoLiveMode",         to: "is.flag.go_live")
        copyBoolFromString("isLocalProxyEnabled",  to: "is.flag.local_proxy")
        copyStringIfMissing("isSelectedCodec",     to: "is.codec")
        copyStringIfMissing("isSelectedSegment",   to: "is.segment")
        copyStringIfMissing("isSelectedProtocol",  to: "is.protocol")
        // Server profiles: the legacy ServerProfileStore wrote
        // "server_profiles_v1" as a JSON string; the new VM stores Data
        // at "is.servers.v2". The struct shape (id/label/contentURL/
        // playbackURL) matches, so the JSON is binary-compatible — just
        // copy + re-encode if the new key is empty.
        if d.data(forKey: "is.servers.v2") == nil,
           let raw = d.string(forKey: "server_profiles_v1"),
           let data = raw.data(using: .utf8) {
            d.set(data, forKey: "is.servers.v2")
        }
        if d.string(forKey: "is.servers.active") == nil,
           let active = d.string(forKey: "active_server_profile_id") {
            d.set(active, forKey: "is.servers.active")
        }
    }

    fileprivate func mbps(from bps: Double?) -> Double? {
        guard let bps, bps > 0 else { return nil }
        return roundMetric(bps / 1_000_000)
    }

    fileprivate func formatResolution(width: Double?, height: Double?) -> String? {
        guard let width, let height, width > 0, height > 0 else { return nil }
        return "\(Int(width))x\(Int(height))"
    }

    fileprivate func roundSeconds(_ value: Double) -> Double {
        (value * 1000).rounded() / 1000
    }

    fileprivate func roundMetric(_ value: Double) -> Double {
        (value * 100).rounded() / 100
    }

    fileprivate func log(_ message: String) {
        // Lightweight console log — the legacy VM also persisted these
        // in a UI panel. Reintroduce that surface later if needed.
        Swift.print("[InfiniteStream] \(message)")
    }
}

// MARK: - ContentItem encoding

extension ContentItem: Encodable {
    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(name, forKey: .name)
        try c.encode(hasHls, forKey: .hasHls)
        try c.encode(hasDash, forKey: .hasDash)
        try c.encode(clipId, forKey: .clipId)
        try c.encode(codec, forKey: .codec)
        try c.encodeIfPresent(segmentDuration, forKey: .segmentDuration)
        try c.encodeIfPresent(thumbnailPath, forKey: .thumbnailPath)
        try c.encodeIfPresent(thumbnailPathSmall, forKey: .thumbnailPathSmall)
        try c.encodeIfPresent(thumbnailPathLarge, forKey: .thumbnailPathLarge)
    }
}
