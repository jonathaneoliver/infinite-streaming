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
    /// True while the user is on the main Playback screen. Pause-source
    /// for ancillary AVPlayers (Home preview tiles) so only the main
    /// player owns the audio session, the codec budget, and — most
    /// importantly — the metrics-emitting state. Without this gate,
    /// preview-tile AVPlayer instances and their access-log timers
    /// keep running behind the main player and pollute the analytics
    /// archive with phantom heartbeats and rendition switches that
    /// look like the main player but aren't (issue #348).
    @Published var playbackActive: Bool = false

    // -- Player ---------------------------------------------------------------

    /// Current AVPlayer instance. Replaced wholesale on `reload()` —
    /// callers that hold a reference must observe `playerEpoch` and
    /// re-bind their PlayerView when it bumps.
    @Published private(set) var player: AVPlayer = AVPlayer()
    /// Bumped every time `player` is replaced. PlaybackScreen keys its
    /// `PlayerView` wrapper on this so the underlying
    /// AVPlayerViewController re-acquires the new player.
    @Published private(set) var playerEpoch: Int = 0

    /// Stable identifier passed to go-proxy as `?player_id=...`.
    /// Persisted in `UserDefaults` under `isPlayerId` so it survives
    /// app rebuilds (Xcode reinstall), relaunches, and iOS reboots —
    /// only wiped on app uninstall or explicit "reset all data".
    /// Without persistence every rebuild produced a new player_id
    /// and the proxy / analytics layers treated it as a fresh device
    /// session, breaking continuity for the operator who's just
    /// iterating on the binary.
    let playerId: String = {
        let d = UserDefaults.standard
        if let stored = d.string(forKey: "isPlayerId"),
           UUID(uuidString: stored) != nil {
            return stored
        }
        // Migration shim: stored-property closures run BEFORE
        // init()'s body, which means migrateLegacyDefaults hasn't
        // copied bossPlayerId → isPlayerId yet on the very first
        // launch of a new build that upgrades a pre-rework install.
        // Read the legacy key here so the migration isn't lost.
        if let legacy = d.string(forKey: "bossPlayerId"),
           UUID(uuidString: legacy) != nil {
            d.set(legacy, forKey: "isPlayerId")
            d.removeObject(forKey: "bossPlayerId")
            return legacy
        }
        let fresh = UUID().uuidString
        d.set(fresh, forKey: "isPlayerId")
        return fresh
    }()

    /// `play_id` (issue #280) — a UUID regenerated only on
    /// **start-fresh** boundaries: a new content selection, a catalogue
    /// filter swap, a user-pressed Reload, or the soak-rotation timer
    /// firing. **Stable across recovery attempts** (`retry()` /
    /// auto-recovery) — those increment `currentAttemptID` instead.
    /// Threaded through every URL the player issues as `?play_id=...`
    /// so go-proxy can scope its NetworkLogEntry ring buffer per play.
    ///
    /// Mintage points:
    /// - VM init (here)
    /// - `setSelectedContent` (user picked a different video)
    /// - `applyContentFilter` when the filter forces a content swap
    /// - `reload()` (user-pressed Reload — "start fresh")
    /// - Soak rotation Task firing
    private var currentPlayID: String = UUID().uuidString

    /// `attempt_id` (bug #4) — a **monotonically-incrementing
    /// integer**, 1-based, identifying which playback attempt within
    /// this play the current activity belongs to. First playback of
    /// any content is `attempt_id=1`; every `restart` event
    /// (user-reload OR auto-recovery) increments it to 2, 3, 4…
    /// Resets to 1 at every new play boundary (new content,
    /// reload, content-filter swap).
    ///
    /// Threaded as `?attempt_id=N` on every URL alongside `play_id`
    /// so go-proxy stamps it on snapshots and network log entries.
    /// Analytics can ask "how many recovery attempts in this play"
    /// via `max(attempt_id) GROUP BY play_id`.
    ///
    /// Integer instead of UUID because the operator-facing question
    /// is "which try is this" — a 1, 2, 3 counter answers that
    /// directly. UUIDs were misleading on the first play (no restart
    /// has happened yet, but a UUID implied otherwise).
    ///
    /// Mintage / reset / increment points:
    /// - VM init (here)            → 1
    /// - `setSelectedContent`      → reset to 1 (new play)
    /// - content-swap in `applyContentFilter` → reset to 1
    /// - `reload()` user-pressed   → reset to 1 (fresh play, see currentPlayID doc)
    /// - `retry()` user-restart    → +1 (recovery within play)
    /// - auto-recovery `retry()`   → +1
    /// - Soak rotation Task firing → reset to 1
    private var currentAttemptID: Int = 1

    /// `play_id` rotation period in seconds (issue #403). 0 = disabled.
    /// User-driven knob in Settings → Advanced; persisted to UserDefaults
    /// so a soak run survives app restarts. The rotation Task computes
    /// remaining time from `playIdMintedAt` so a setting change applies
    /// to the in-progress play (no full re-arm from zero).
    @Published var playIdRotationSeconds: Int = 0
    /// Rotation Task armed after every successful loadStream and
    /// rescheduled when the user picks a new period. Cancelled on
    /// teardown / fresh `buildURLAndLoad`.
    private var playIdRotationTask: Task<Void, Never>?
    /// Wall-clock timestamp of when the current `play_id` minted.
    /// Drives the age-based rotation deadline.
    private var playIdMintedAt: Date = .distantPast
    /// Wall-clock timestamp of the last "interesting" player event
    /// (stall, error). Used by the rotation Task to defer firing if
    /// the boundary would split mid-incident — see issue #403 comment
    /// requesting a 60s quiet window. Rate shifts are *not* counted
    /// here; on healthy streams ABR switches happen routinely and
    /// would otherwise prevent rotation indefinitely.
    private var playIdLastActivityAt: Date = .distantPast

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

    /// Variant-dwell totals snapshotted at the moment of `retry()` so
    /// the new AVPlayerItem's access-log walk in `perVariantTimeSeconds()`
    /// continues from the prior play's accumulation rather than
    /// restarting at zero. AVPlayer's access log is per-item, so without
    /// this every retry would zero the dashboard's Time-per-Variant
    /// tile mid-play. Mirrors the PlaybackDiagnostics prior* pattern
    /// but lives here because the payload key format ("1080p@7060kbps")
    /// is computed in this layer.
    private var priorPerVariantTimeSeconds: [String: Double] = [:]
    // Captured when the player enters the buffering state so the
    // matching buffering_end POST can carry an authoritative duration
    // in `player_metrics_last_buffering_time_s`. Mirrors the stall
    // pair's `lastReportedStallDuration` shape. Issue #474 Milestone A.
    private var bufferingStartedAt: Date?
    private let metricsHeartbeatSeconds: TimeInterval = 1
    // Tail of the serialized chain of in-flight metrics PATCHes. Each
    // new sendPlayerMetrics call chains onto this Task so URLSession
    // requests reach the proxy in iOS-clock submission order. Without
    // this, concurrent URLSession.shared requests can complete out of
    // order and the proxy's last-writer-wins merge stomps a fresher
    // event (e.g. rate_shift_up bps=3.46) with an earlier-submitted
    // heartbeat (bps=1.84) that arrived later. See plan
    // humming-sleeping-squid.md.
    private var metricsTaskTail: Task<Void, Never>?
    // iOS 18 AVMetrics raw-event subscriber for the current AVPlayerItem
    // (issue #486 spike). Replaced on every new item; nil when no item
    // is bound or when running on a pre-iOS-18 build. Property type is
    // `Any?` so the declaration doesn't need an `@available` mark on
    // PlayerViewModel itself — the concrete cast lives inside
    // attachAVMetrics / detachAVMetrics, which are guarded.
    private var avMetricsSubscriber: Any?
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
    private var willTerminateObserver: NSObjectProtocol?
    private var didEnterBackgroundObserver: NSObjectProtocol?

    // MARK: - Init

    init() {
        // Defensive log — there should only ever be ONE PlayerViewModel
        // alive in the process. iPadOS's WindowGroup multi-window
        // would have minted one per scene before we set
        // UIApplicationSupportsMultipleScenes=false in Info.plist; if
        // a regression brings multi-window back, two PlayerViewModels
        // start emitting metrics for the same player_id and the
        // analytics archive shows interleaved heartbeats (issue #348).
        // grep `[VM-INIT]` in the device console to count them.
        print("[VM-INIT] PlayerViewModel \(ObjectIdentifier(self))")
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
        diagnostics.bind(to: player)
        bindDiagnosticsLogging()
        bindMetricsReporting()
        // Heartbeat is started by `loadStream` so the first tick lands
        // 1s AFTER the `playing` event — before any playback happens
        // there's nothing useful to report on. See `startMetricsHeartbeat`.
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
        print("[VM-DEINIT] PlayerViewModel \(ObjectIdentifier(self))")
        if let o = didPlayToEndObserver { NotificationCenter.default.removeObserver(o) }
        if let o = failedToPlayObserver { NotificationCenter.default.removeObserver(o) }
        if let o = willEnterForegroundObserver { NotificationCenter.default.removeObserver(o) }
        if let o = willTerminateObserver { NotificationCenter.default.removeObserver(o) }
        if let o = didEnterBackgroundObserver { NotificationCenter.default.removeObserver(o) }
        playIdRotationTask?.cancel()
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
    func setPlayIdRotationSeconds(_ value: Int) {
        playIdRotationSeconds = max(0, value)
        persistFlags()
        // Reschedule using the *remaining* time since the current
        // play_id minted. If the new period is less than the elapsed
        // age, the rescheduled Task fires immediately. Issue #403.
        if currentURL != nil { schedulePlayIdRotation() }
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
        // Content-selection boundary — bug #4 contract says both ids
        // rotate. A new content choice is a new play; attempt resets
        // to 1 for the fresh play.
        regeneratePlayID()
        resetAttemptID()
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
        if pick != selectedContent {
            selectedContent = pick
            // Filter forced a content swap — same boundary as
            // setSelectedContent. New play, attempt resets to 1.
            regeneratePlayID()
            resetAttemptID()
        }
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
        // play_id / attempt_id rotation is driven by the CALLER, not
        // here — buildURLAndLoad is called both for "new content / new
        // play" boundaries and for "settings tweak, same play". The
        // caller calls `regeneratePlayID()` / `resetAttemptID()` /
        // `incrementAttemptID()` explicitly when a boundary applies.
        // See the property docs on currentPlayID / currentAttemptID
        // for the mintage points.
        //
        // Anchor the age clock for the soak-rotation timer. Every
        // load resets the boundary; the Task is rescheduled at the
        // bottom once playback has been handed off.
        playIdMintedAt = Date()
        playIdLastActivityAt = .distantPast
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
        // Append play_id + attempt_id last so they survive the
        // player_id strip above for k3s-dev's content port. (For HAR
        // scoping go-proxy reads both ids from URLs that hit it; the
        // content port stripping only affects routes that don't reach
        // go-proxy.)
        final = appendPlayID(to: final)
        final = appendAttemptID(to: final)
        self.currentURL = final
        self.statusText = final.absoluteString
        loadStream(url: final)
        schedulePlayIdRotation()
    }

    /// Cancel any pending rotation Task and (if the setting is non-zero)
    /// arm a fresh one for the *remaining* time relative to
    /// `playIdMintedAt`. Called on every `buildURLAndLoad` and whenever
    /// the user changes the setting. Issue #403.
    private func schedulePlayIdRotation() {
        playIdRotationTask?.cancel()
        playIdRotationTask = nil
        let target = playIdRotationSeconds
        guard target > 0 else { return }
        let elapsedSec = Date().timeIntervalSince(playIdMintedAt)
        let remainingSec = max(0.0, Double(target) - elapsedSec)
        let quiescenceMs: Int = 60_000
        playIdRotationTask = Task { @MainActor [weak self] in
            if remainingSec > 0 {
                try? await Task.sleep(nanoseconds: UInt64(remainingSec * 1_000_000_000))
            }
            while !Task.isCancelled {
                guard let self else { return }
                let activityAt = self.playIdLastActivityAt
                if activityAt == .distantPast { break }
                let sinceMs = Int(Date().timeIntervalSince(activityAt) * 1000)
                if sinceMs >= quiescenceMs { break }
                try? await Task.sleep(nanoseconds: UInt64(quiescenceMs - sinceMs) * 1_000_000)
            }
            if Task.isCancelled { return }
            guard let self else { return }
            let age = Int(Date().timeIntervalSince(self.playIdMintedAt))
            print("[PLAY_ID] rotating after \(age)s (target \(target)s)")
            // Soak-rotation deliberately starts a fresh play boundary
            // — rotate play_id and reset attempt to 1 so the new
            // play's recovery counter starts clean.
            self.regeneratePlayID()
            self.resetAttemptID()
            self.buildURLAndLoad()
        }
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

    /// Mint a fresh `play_id` UUID. Called only at content-selection
    /// boundaries (see currentPlayID doc). NOT called on restart.
    private func regeneratePlayID() {
        currentPlayID = UUID().uuidString
    }

    /// Replace any existing `attempt_id` query item with the current
    /// `currentAttemptID`. Bug #4 fix.
    private func appendAttemptID(to url: URL) -> URL {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
        var items = components.queryItems ?? []
        items.removeAll { $0.name == "attempt_id" }
        items.append(URLQueryItem(name: "attempt_id", value: String(currentAttemptID)))
        components.queryItems = items
        return components.url ?? url
    }

    /// Reset the attempt counter to 1. Called at every new-play
    /// boundary (new content, reload, content-filter swap, soak
    /// rotation) — those start a fresh play with attempt=1.
    private func resetAttemptID() {
        currentAttemptID = 1
    }

    /// +1 the attempt counter. Called from `retry()` (user-restart
    /// AND auto-recovery) — same play, next recovery attempt.
    private func incrementAttemptID() {
        currentAttemptID += 1
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
        // iOS 18 AVMetrics raw-event subscriber for the comparison spike
        // (issue #486). Rebuilt per-item so the streams stay bound to the
        // playerItem that AVFoundation is actually observing.
        attachAVMetrics(to: item)
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
        // Single `Date()` capture — used as both `playbackStartAt` and
        // the `playing` event's `at:` stamp so elapsed-since-start in
        // the `playing` payload is exactly 0 and not skewed by the
        // few microseconds between two separate `Date()` calls.
        diagnostics.reset()
        let playingEventAt = Date()
        playbackStartAt = playingEventAt
        videoFirstFrameSeconds = nil
        videoPlayingTimeSeconds = nil
        firstFrameReported = false
        playingReported = false
        lastReportedRenditionMbps = nil
        lastReportedStallCount = 0
        lastReportedStallDuration = 0
        bufferingStartedAt = nil
        zeroBufferStartedAt = nil
        metricsSessionId = nil
        metricsLastSessionLookup = nil
        let contentName = selectedContent
        let playingPayload = buildMetricsPayload(event: "playing", at: playingEventAt, extra: [
            "player_metrics_content_url": url.absoluteString,
            "player_metrics_content_name": contentName
        ])
        Task { [weak self] in
            await self?.sendPlayerMetrics(payload: playingPayload)
        }
        // Restart the 1Hz heartbeat so its first tick lands exactly
        // `metricsHeartbeatSeconds` after this `playing` event — not at
        // some arbitrary offset inherited from a prior playback or app
        // launch.
        startMetricsHeartbeat()
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
        // A retry is a recovery attempt within the SAME play. Counters
        // persist across the AVPlayerItem replacement: snapshotForRestart
        // copies the current dropped/displayed/variant-dwell totals
        // into "prior" buckets so the new item's counters add on top
        // rather than restart from zero. Tick playerRestarts so each
        // recovery attempt registers in the per-play restart count.
        diagnostics.snapshotForRestart()
        snapshotPerVariantForRestart()
        playerRestarts += 1
        // Tick `attempt_id` (per bug #4 contract) so analytics can
        // count recovery attempts. Keep `play_id` stable — this is
        // still the same play.
        incrementAttemptID()
        var refreshed = appendPlayID(to: url)
        refreshed = appendAttemptID(to: refreshed)
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
        let eventAt = Date()
        let stamp = Self.metricsTimestampFormatter.string(from: eventAt)
        print("911 user-marked at \(stamp) currentURL=\(currentURL?.absoluteString ?? "—")")
        let payload = buildMetricsPayload(event: "user_marked", at: eventAt, extra: [
            "player_metrics_user_marked_at": stamp
        ])
        Task { [weak self] in await self?.sendPlayerMetrics(payload: payload) }
    }

    /// Mark the play as terminated with the given Phase 2 status +
    /// reason and emit a single session_end event so the dashboard /
    /// CH see the terminal row. Idempotent — only the FIRST call
    /// stamps the terminal state (a user_quit followed by a fatal
    /// error a moment later still reads as user_stopped, which matches
    /// reality from the operator's perspective).
    ///
    /// Detection-point inputs:
    ///   - User back tap → status="user_stopped"  reason="user_quit"
    ///   - App background → status="user_stopped" reason="app_backgrounded"
    ///   - App terminate → status="user_stopped"  reason="app_terminated"
    ///   - AVPlayer fatal pre-first-frame → status="start_failure"
    ///   - AVPlayer fatal post-first-frame → status="mid_stream_failure"
    ///
    /// Reason is passed through diagnostics.refineTerminalReason so a
    /// user_quit while buffering becomes ended_buffering[_long] etc.
    /// before the payload goes out.
    /// EBVS threshold (seconds). Above this, a user back-tap before
    /// first frame becomes abandoned_start / slow_startup. Below, it
    /// stays user_stopped / user_quit (user changed their mind quickly).
    /// Mirrors the forwarder's default qoe_thresholds.outcomes.ebvs_threshold_ms
    /// (10s); kept as a Swift constant so the client decision happens
    /// in one place and the row is stamped right at session_end.
    private static let ebvsThresholdSeconds: TimeInterval = 10

    /// Convenience for the playback-back-button tap (and tvOS exit
    /// command). Decides between user_stopped / abandoned_start based
    /// on whether the player ever crossed first frame and how long
    /// the play had been running. The classifier in PlaybackDiagnostics
    /// then upgrades user_quit → ended_buffering[_long] etc. if the
    /// player was stuck in those states.
    func endSessionForUserBack() {
        if !diagnostics.hasRenderedFirstFrame,
           let startedAt = diagnostics.playStartAt,
           Date().timeIntervalSince(startedAt) >= Self.ebvsThresholdSeconds {
            endSession(status: "abandoned_start", reason: "slow_startup")
        } else {
            endSession(status: "user_stopped", reason: "user_quit")
        }
    }

    func endSession(status: String, reason: String) {
        // No-op on subsequent calls — diagnostics.markTerminal enforces
        // first-call-wins. Even when terminal is already set we still
        // emit a session_end (the prior emit may have been swallowed
        // by background suspension), but the values stay stable.
        let alreadyTerminal = diagnostics.terminalStatus != nil
        diagnostics.markTerminal(status: status, reason: reason)
        if alreadyTerminal {
            NSLog("[endSession] terminal already=\(diagnostics.terminalStatus ?? "?") — re-emitting session_end for delivery")
        }
        Task { [weak self] in
            await self?.sendPlayerMetrics(event: "session_end")
        }
    }

    func reload() {
        Task { [weak self] in
            await self?.requestHARSnapshot(reason: "user_reload", force: true)
        }
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
        // Reload = fresh play. Counters start from zero so the new
        // play's dashboard widgets aren't polluted by the previous
        // play's totals. Clears both the per-item counters (via
        // diagnostics.reset()) AND the "prior" accumulators that
        // snapshotForRestart() populates. retry() does the opposite —
        // it snapshots so counters carry across recovery attempts.
        diagnostics.resetForFreshPlay()
        resetPerVariantForFreshPlay()
        playerRestarts = 0
        profileShiftCount = 0
        // Rotate play_id (new play boundary) AND reset attempt_id
        // to 1 (fresh play, no recovery attempts yet). retry()
        // differs — it keeps play_id stable and increments
        // attempt_id because it's a within-play recovery attempt.
        regeneratePlayID()
        resetAttemptID()
        let restartPayload = buildMetricsPayload(event: "restart", at: Date(), extra: [
            "player_metrics_restart_reason": "reload",
            "player_restarts": playerRestarts
        ])
        Task { [weak self] in await self?.sendPlayerMetrics(payload: restartPayload) }
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
                //
                // We intentionally do NOT mark terminal here. Background
                // is ambiguous: a 1-second app-switch and an end-of-
                // session both look identical at this notification.
                // willTerminate handles the unambiguous end-of-app case;
                // quick app-switches resume cleanly via willEnterForeground
                // without false-positive user_stopped rows. Future
                // refinement: a timer that marks terminal after N
                // seconds backgrounded.
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
        // willTerminate fires on hard-quit (swipe-up from app switcher).
        // Synchronous notification — we get a few hundred ms before iOS
        // reaps us. Mark terminal + fire-and-forget the session_end.
        // Best-effort: if iOS reaps us mid-flight the row stays
        // in_progress and the forwarder treats absent session_end as
        // "user closed without notifying."
        willTerminateObserver = nc.addObserver(
            forName: UIApplication.willTerminateNotification,
            object: nil, queue: .main
        ) { [weak self] _ in
            Task { @MainActor [weak self] in
                self?.endSession(status: "user_stopped", reason: "app_terminated")
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
        // `video_start_time` event ← canonical AVKit signal.
        // First time `timeControlStatus` flips to `.playing` after a
        // playback starts is the moment AVPlayer is actively decoding +
        // presenting at the requested rate (i.e. "playing smoothly").
        // One-shot guard via `playingReported` (cleared on each
        // `playing` reset alongside the other latches).
        statusObserver = player.observe(\.timeControlStatus, options: [.new]) { [weak self] p, _ in
            // Capture firing instant on the KVO callback thread so the
            // metric carries the true transition time, not the
            // MainActor-pickup time. Same snapshot-at-firing-context
            // pattern as every other emit site in this PR.
            let firedAt = Date()
            Task { @MainActor in
                guard let self else { return }
                if p.timeControlStatus == .playing && !self.playingReported {
                    self.markPlayingStarted(at: firedAt)
                }
            }
        }
    }

    /// Called by the PlayerView wrapper when the AVPlayerLayer reports
    /// `isReadyForDisplay = true` for the current item — the canonical
    /// AVKit "first frame rendered" signal. Bumps the lastPlayed /
    /// viewCount bookkeeping AND fires the `video_first_frame` metric
    /// event with the flip-instant timestamp.
    func markFirstFrameRendered(at firstFrameAt: Date) {
        // Idempotent: PlayerView re-installs its KVO observer on player
        // swap and the layer's `isReadyForDisplay` flips back to false
        // on item replace, but a single playback flip may still surface
        // as multiple `.initial` deliveries to the SwiftUI coordinator
        // — guard belt-and-braces.
        guard !firstFrameReported else { return }
        firstFrameReported = true
        if !hasReportedFirstFrame {
            hasReportedFirstFrame = true
            codecRetries = 0
            let current = selectedContent
            if !current.isEmpty {
                let clipId = ContentItem.deriveClipId(from: current)
                viewCounts[clipId, default: 0] += 1
                lastPlayed = current
                persistPlaybackHistory()
            }
        }
        diagnostics.markFirstFrameRendered()
        guard let startAt = playbackStartAt else { return }
        let elapsed = roundSeconds(firstFrameAt.timeIntervalSince(startAt))
        videoFirstFrameSeconds = elapsed
        let payload = buildMetricsPayload(event: "video_first_frame", at: firstFrameAt, extra: [
            "player_metrics_video_first_frame_time_s": elapsed
        ])
        Task { [weak self] in await self?.sendPlayerMetrics(payload: payload) }
    }

    /// Called from the `timeControlStatus` KVO observer when the player
    /// first transitions to `.playing` after a playback starts. Fires
    /// the `video_start_time` metric event. Distinct from
    /// `markFirstFrameRendered` — the latter is the layer-decoded signal,
    /// this is the player-actively-playing signal. Typical order:
    /// first-frame fires before .playing, but they're independent KVO
    /// observables.
    func markPlayingStarted(at startedAt: Date) {
        guard !playingReported else { return }
        playingReported = true
        guard let startAt = playbackStartAt else { return }
        let elapsed = roundSeconds(startedAt.timeIntervalSince(startAt))
        videoPlayingTimeSeconds = elapsed
        let payload = buildMetricsPayload(event: "video_start_time", at: startedAt, extra: [
            "player_metrics_video_start_time_s": elapsed
        ])
        Task { [weak self] in await self?.sendPlayerMetrics(payload: payload) }
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
            return
        }
        // No auto-recovery → this error is terminal. start_failure if
        // we never reached first frame, otherwise mid_stream_failure.
        markFatalTerminal(message: message)
    }

    private func handleErrorRetry(reason: String) {
        guard codecRetries < maxCodecRetries, let url = currentURL else {
            // Retry budget exhausted → the play is dead. Distinguish
            // pre-vs-post-first-frame for the right Phase 2 status.
            markFatalTerminal(message: reason)
            return
        }
        codecRetries += 1
        let retries = codecRetries
        statusText = "\(reason) — retry \(retries)/\(maxCodecRetries)"
        Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(150_000_000 * retries))
            self?.loadStream(url: url)
        }
    }

    /// Stamp the play as fatally terminated and emit session_end. The
    /// status is `start_failure` when the player never crossed first
    /// frame, `mid_stream_failure` after. iOS doesn't yet inspect the
    /// underlying NSError domain/code to populate a specific
    /// playback_reason — the forwarder error_classifier (4d8265f) will
    /// pick that up from terminal_error_* in a follow-up; for now we
    /// stamp the generic "unknown" so dashboards bucket correctly.
    private func markFatalTerminal(message: String) {
        let status = diagnostics.hasRenderedFirstFrame ? "mid_stream_failure" : "start_failure"
        endSession(status: status, reason: "unknown")
        NSLog("[endSession] fatal status=\(status) message=\(message)")
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
    private static let flagPlayIdRotation = "is.flag.play_id_rotation_s"
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
        playIdRotationSeconds = max(0, d.object(forKey: Self.flagPlayIdRotation) as? Int ?? 0)
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
        d.set(playIdRotationSeconds, forKey: Self.flagPlayIdRotation)
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
            .receive(on: DispatchQueue.main)
            .sink { [weak self] value in
                guard let self else { return }
                self.log("Item error: \(value)")
                let payload = self.buildMetricsPayload(event: "error", at: Date(), extra: [
                    "player_metrics_error": value,
                    "player_metrics_error_code": self.diagnostics.lastErrorCode,
                    "player_metrics_error_domain": self.diagnostics.lastErrorDomain,
                    "player_metrics_error_details": self.diagnostics.lastErrorDetails,
                ])
                Task { await self.sendPlayerMetrics(payload: payload) }
                self.markPlayIdActivity()
            }
            .store(in: &cancellables)

        diagnostics.$lastFailure
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .receive(on: DispatchQueue.main)
            .sink { [weak self] value in
                guard let self else { return }
                self.log("Playback failure: \(value)")
                let payload = self.buildMetricsPayload(event: "error", at: Date(), extra: [
                    "player_metrics_error": value,
                    "player_metrics_error_code": self.diagnostics.lastErrorCode,
                    "player_metrics_error_domain": self.diagnostics.lastErrorDomain,
                    "player_metrics_error_details": self.diagnostics.lastErrorDetails,
                ])
                Task { await self.sendPlayerMetrics(payload: payload) }
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
            .receive(on: DispatchQueue.main)
            .sink { [weak self] value in
                guard let self else { return }
                self.log("Player error: \(value)")
                let payload = self.buildMetricsPayload(event: "error", at: Date(), extra: [
                    "player_metrics_error": value,
                    "player_metrics_error_code": self.diagnostics.lastErrorCode,
                    "player_metrics_error_domain": self.diagnostics.lastErrorDomain,
                    "player_metrics_error_details": self.diagnostics.lastErrorDetails,
                ])
                Task { await self.sendPlayerMetrics(payload: payload) }
                self.markPlayIdActivity()
            }
            .store(in: &cancellables)

        diagnostics.$frozenDetected
            .removeDuplicates()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] frozen in
                guard let self, frozen else { return }
                let payload = self.buildMetricsPayload(event: "frozen", at: Date())
                Task { await self.sendPlayerMetrics(payload: payload) }
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
            .receive(on: DispatchQueue.main)
            .sink { [weak self] stalled in
                guard let self, stalled else { return }
                let payload = self.buildMetricsPayload(event: "segment_stall", at: Date())
                Task { await self.sendPlayerMetrics(payload: payload) }
                if self.autoRecovery {
                    self.scheduleAutoRecoveryRestart(reason: "auto_recovery_segment_stall")
                } else {
                    Task { await self.requestHARSnapshot(reason: "segment_stall") }
                }
            }
            .store(in: &cancellables)
    }

    fileprivate func bindMetricsReporting() {
        // Every sink below uses the same shape:
        //   1. capture `eventAt = Date()` synchronously
        //   2. build the payload via `buildMetricsPayload(event:at:extra:)`
        //   3. hand the immutable payload into a Task that does only HTTP
        // This keeps `event_time`, `state`, position,
        // playhead_wallclock, etc. all anchored to the moment the
        // underlying event fired — not to whenever the Task body
        // happens to run after chaining through `metricsTaskTail`.
        diagnostics.$stallingCount
            .removeDuplicates()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] count in
                guard let self, count > self.lastReportedStallCount else { return }
                self.lastReportedStallCount = count
                self.markPlayIdActivity()
                let payload = self.buildMetricsPayload(event: "stall_start", at: Date())
                Task { await self.sendPlayerMetrics(payload: payload) }
            }
            .store(in: &cancellables)

        diagnostics.$stallDurationS
            .removeDuplicates()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] duration in
                guard let self, duration > 0, duration != self.lastReportedStallDuration else { return }
                self.lastReportedStallDuration = duration
                let payload = self.buildMetricsPayload(event: "stall_end", at: Date(), extra: [
                    "player_metrics_stall_duration_ms": self.secondsToMs(duration)
                ])
                Task { await self.sendPlayerMetrics(payload: payload) }
            }
            .store(in: &cancellables)

        diagnostics.$state
            .removeDuplicates()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] state in
                guard let self else { return }
                // Capture once — the up-to-three sub-events (state_change
                // + optional buffering_start/end) all represent one sink
                // invocation, so they share the timestamp + diagnostics
                // snapshot.
                let eventAt = Date()
                let previous = self.lastReportedState
                if let previous, previous != state {
                    let stateChange = self.buildMetricsPayload(event: "state_change", at: eventAt, extra: [
                        "player_metrics_state_from": previous,
                        "player_metrics_state_to": state
                    ])
                    Task { await self.sendPlayerMetrics(payload: stateChange) }
                    if state == "buffering" {
                        self.bufferingStartedAt = eventAt
                        let payload = self.buildMetricsPayload(event: "buffering_start", at: eventAt)
                        Task { await self.sendPlayerMetrics(payload: payload) }
                    } else if previous == "buffering" {
                        var extra: [String: Any] = [:]
                        if let started = self.bufferingStartedAt {
                            extra["player_metrics_last_buffering_time_s"] =
                                self.roundSeconds(eventAt.timeIntervalSince(started))
                        }
                        self.bufferingStartedAt = nil
                        let payload = self.buildMetricsPayload(event: "buffering_end", at: eventAt, extra: extra)
                        Task { await self.sendPlayerMetrics(payload: payload) }
                    }
                } else if previous == nil {
                    let payload = self.buildMetricsPayload(event: "state_change", at: eventAt)
                    Task { await self.sendPlayerMetrics(payload: payload) }
                }
                self.lastReportedState = state
            }
            .store(in: &cancellables)

        // `$currentTime` no longer carries `video_first_frame` or
        // `video_start_time` as primary signals — those are now driven
        // by `AVPlayerLayer.isReadyForDisplay` (PlayerView) and
        // `timeControlStatus == .playing` (observePlayer) respectively.
        //
        // The `currentTime > 0 && state == "playing"` synthesis is kept
        // here as a *fallback* for the first-frame event only —
        // `AVPlayerViewController` on iOS doesn't reliably expose its
        // embedded `AVPlayerLayer` as a sublayer of `view.layer`, so
        // the KVO observer in PlayerView may not fire on initial
        // mount. The shared `firstFrameReported` latch guarantees we
        // still emit one event regardless of which signal fires first.
        // When the KVO observer DOES fire (post-Reload, tvOS,
        // older iOS), it always wins because it fires earlier (the
        // layer flips before AVPlayer reports `currentTime > 0`).
        diagnostics.$currentTime
            .removeDuplicates()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] currentTime in
                guard let self, !self.firstFrameReported else { return }
                let isActivelyPlaying = self.diagnostics.state == "playing"
                    && self.diagnostics.playbackRate > 0
                if currentTime > 0 && isActivelyPlaying {
                    self.markFirstFrameRendered(at: Date())
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
                let payload = self.buildMetricsPayload(event: "timejump", at: event.at, extra: [
                    "player_metrics_timejump_from_s": self.roundSeconds(event.from),
                    "player_metrics_timejump_to_s": self.roundSeconds(event.to),
                    "player_metrics_timejump_delta_s": self.roundSeconds(event.to - event.from),
                    "player_metrics_timejump_origin": event.origin
                ])
                Task { await self.sendPlayerMetrics(payload: payload) }
            }
            .store(in: &cancellables)
    }

    /// Restart the 1Hz metrics heartbeat. Called from `loadStream` so a
    /// fresh playback session always gets its first heartbeat tick
    /// exactly `metricsHeartbeatSeconds` after the `playing` event —
    /// not piggybacked onto whatever phase a previously-running timer
    /// happened to be in. Suppresses pre-playback heartbeats by
    /// virtue of not running until playback starts.
    fileprivate func startMetricsHeartbeat() {
        metricsHeartbeatTimer?.invalidate()
        metricsHeartbeatTimer = Timer.scheduledTimer(withTimeInterval: metricsHeartbeatSeconds, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.evaluateAutoRecoveryIfNeeded()
                let payload = self.buildMetricsPayload(event: "heartbeat", at: Date())
                await self.sendPlayerMetrics(payload: payload)
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
                let restartPayload = self.buildMetricsPayload(event: "restart", at: Date(), extra: [
                    "player_metrics_restart_reason": reason,
                    "player_restarts": self.playerRestarts
                ])
                Task { [weak self] in await self?.sendPlayerMetrics(payload: restartPayload) }
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
        // Capture once — both the bitrate-change event and any rate-shift
        // sub-event represent the same shift moment, so they share the
        // sink-time timestamp + diagnostics snapshot.
        let eventAt = Date()
        if let previous = lastReportedRenditionMbps {
            if mbps != previous {
                // Issue #470: one POST per bitrate transition, with a
                // directional event name (rate_shift_up / rate_shift_down).
                // Previously we sent video_bitrate_change AND a parallel
                // rate_shift_up/down for the same observation; that was
                // the same moment with a renamed wrapper and caused
                // prev/cur aliasing in the forwarder. Collapsed here.
                profileShiftCount = max(0, profileShiftCount) + 1
                let event = mbps > previous ? "rate_shift_up" : "rate_shift_down"
                let payload = buildMetricsPayload(event: event, at: eventAt, extra: [
                    "player_metrics_rate_from_mbps": previous,
                    "player_metrics_rate_to_mbps": mbps,
                    "player_metrics_profile_shift_count": profileShiftCount
                ])
                Task { [weak self] in await self?.sendPlayerMetrics(payload: payload) }
                // Note: bitrate changes are intentionally NOT marked as
                // play_id activity — on healthy ABR streams they happen
                // routinely and would block soak rotation indefinitely.
            }
        }
        lastReportedRenditionMbps = mbps
    }

    /// Stamp the wall clock for the rotation Task's quiescence check —
    /// mid-incident rotations split the row across boundaries the
    /// dashboard cares about (stalls / rate shifts / errors). Issue #403.
    private func markPlayIdActivity() {
        playIdLastActivityAt = Date()
    }

    /// Convenience that builds the payload synchronously here and forwards
    /// to `sendPlayerMetrics(payload:)`. Use this from any callsite that
    /// has the `at:` instant in hand and doesn't already need to control
    /// payload composition.
    fileprivate func sendPlayerMetrics(event: String, at eventAt: Date = Date(), extra: [String: Any] = [:]) async {
        guard currentURL != nil else { return }
        guard metricsBaseURL() != nil else { return }
        let payload = buildMetricsPayload(event: event, at: eventAt, extra: extra)
        await sendPlayerMetrics(payload: payload)
    }

    /// Snapshot-at-firing-context entry point. Callers build the payload
    /// at the moment the underlying event fires (so `event_time`,
    /// `state`, `playhead_wallclock`, etc. all reflect that instant) and
    /// hand the immutable dictionary in. This function does only the
    /// HTTP queue-and-chain, never re-reads diagnostics.
    fileprivate func sendPlayerMetrics(payload: [String: Any]) async {
        guard currentURL != nil else { return }
        guard let baseURL = metricsBaseURL() else { return }
        if payload.isEmpty { return }
        logMetricsEmit(payload: payload)
        // CRITICAL: read-tail / set-tail must be synchronous (no
        // awaits between them) or two concurrent callers can suspend
        // on a prior await, resume out of FIFO order, and end up
        // chaining backwards — wire-arrival order then doesn't match
        // iOS-clock submission order. resolveMetricsSessionId and
        // patchSessionMetrics live inside the Task so the chain
        // pointer is updated atomically within one MainActor tick.
        let prev = metricsTaskTail
        let next = Task { [weak self] in
            if let prev { await prev.value }
            guard let self else { return }
            guard let sessionId = await self.resolveMetricsSessionId(baseURL: baseURL) else { return }
            await self.patchSessionMetrics(sessionId: sessionId, baseURL: baseURL, payload: payload)
        }
        metricsTaskTail = next
        await next.value
    }

    /// Greppable one-line console emit per metrics PATCH. Filter
    /// `idevicesyslog` / Console.app to `[METRICS]` to see every event
    /// the device sends, with a key fields summary. Keep this short —
    /// the full payload goes on the wire; this is just for spotting
    /// bursts and verifying the sink-time snapshot lands correctly.
    private func logMetricsEmit(payload: [String: Any]) {
        let event = (payload["player_metrics_last_event"] as? String) ?? "?"
        let ts = (payload["player_metrics_event_time"] as? String) ?? "—"
        let state = (payload["player_metrics_state"] as? String) ?? "—"
        let from = (payload["player_metrics_state_from"] as? String) ?? ""
        let to = (payload["player_metrics_state_to"] as? String) ?? ""
        let pos = (payload["player_metrics_position_s"] as? Double).map { String(format: "%.2f", $0) } ?? "—"
        let suffix = (from.isEmpty && to.isEmpty) ? "" : " from=\(from) to=\(to)"
        print("[METRICS event=\(event) ts=\(ts) state=\(state) pos=\(pos)\(suffix)]")
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
        let lookupURL = baseURL.appendingPathComponent("api/sessions")
        var request = URLRequest(url: lookupURL)
        applyPlayerHeaders(to: &request)
        do {
            let (data, response) = try await URLSession.shared.data(for: request)
            let status = (response as? HTTPURLResponse)?.statusCode ?? -1
            if status >= 400 {
                NSLog("[SESSION-RESOLVE] HTTP \(status) — abort")
                return nil
            }
            guard let json = try JSONSerialization.jsonObject(with: data) as? [[String: Any]] else {
                NSLog("[SESSION-RESOLVE] response not [[String: Any]] — abort")
                return nil
            }
            let match = json.first { entry in
                (entry["player_id"] as? String) == playerId
            }
            if let sessionId = match?["session_id"] as? String, !sessionId.isEmpty {
                metricsSessionId = sessionId
                metricsLastSessionLookup = now
                return sessionId
            }
            // Bust the stale cache (issue #486 follow-up) — if /api/sessions
            // succeeded but our player_id isn't there, the cached sessionId
            // is dead. Without this we'd keep POSTing to a ghost session
            // forever after the proxy is restarted.
            if metricsSessionId != nil {
                NSLog("[SESSION-RESOLVE] no match for player_id=\(playerId) — busting stale cache")
                metricsSessionId = nil
                metricsLastSessionLookup = nil
            }
        } catch {
            NSLog("[SESSION-RESOLVE] EXCEPTION: \(error.localizedDescription)")
            return nil
        }
        return nil
    }

    /// Time-watched per variant, in seconds, keyed by a compact
    /// `<resolution>@<kbps>kbps` label (e.g. `"360p@725kbps"`).
    /// Sourced from `AVPlayerItemAccessLog.events` — Apple's
    /// canonical record of per-segment playback, where each event
    /// carries `durationWatched` for a contiguous run at a given
    /// indicated bitrate. The resolution half comes from a tolerant
    /// match of the access-log bitrate against the asset's variant
    /// ladder (`AVURLAsset.variants`). When no variant matches
    /// within ±10%, falls back to a bitrate-only label so the row
    /// still shows.
    ///
    /// Per issue #486: AVMetricPlayerItemPlaybackSummaryEvent does NOT
    /// expose this breakdown (the only time field there is
    /// `timeWeightedAverageBitrate`, a session-wide weighted average).
    /// The access log fills that gap — and stays valid throughout the
    /// play, so we publish on every heartbeat AND it's naturally
    /// captured by the last heartbeat before tear-down.
    /// Frames-displayed estimate from playing-time × active variant
    /// nominal fps − dropped frames. Returns nil when the variant's
    /// nominal FPS isn't available yet (caller falls back to the
    /// legacy `estimatedDisplayedFrames`). Issue #486 follow-up.
    ///
    /// `diagnostics.currentTime` is the playhead position which only
    /// advances while the player is actually playing — it freezes
    /// during stalls / pauses, which is exactly what we want as the
    /// denominator. Multiplying by nominalFrameRate gives the count
    /// of frames the player *should* have displayed; subtracting
    /// dropped frames yields the actual displayed count.
    fileprivate func framesDisplayedFromNominalFps() -> Double? {
        guard let fps = diagnostics.nominalFrameRate, fps > 0 else { return nil }
        let playing = diagnostics.currentTime
        guard playing > 0 else { return 0 }
        let dropped = Double(diagnostics.droppedVideoFrames ?? 0)
        return max(0, playing * fps - dropped)
    }

    /// More accurate frames-displayed when the player has crossed
    /// variants of differing FPS — walks the access log and weights
    /// each segment's `durationWatched` by the matching variant's
    /// `nominalFrameRate`. Returns nil when the asset doesn't expose
    /// per-variant FPS metadata (caller falls back to the
    /// single-variant nominal-fps formula). Issue #486 follow-up.
    ///
    /// All variants in our content are 25 fps today, so this exactly
    /// matches `framesDisplayedFromNominalFps()` in practice; the
    /// generalisation is here for future content with mixed-FPS
    /// ladders (e.g. 30/60 fps for sports streams).
    fileprivate func framesDisplayedFromAccessLog() -> Double? {
        guard let item = player.currentItem,
              let log = item.accessLog() else { return nil }

        // bitrate → nominalFrameRate from the asset's variants. Same
        // tolerant matching as perVariantTimeSeconds() so an EWMA-
        // jittered indicatedBitrate still matches its rung.
        var ladder: [(peak: Double, fps: Double)] = []
        if let asset = item.asset as? AVURLAsset {
            for variant in asset.variants {
                guard let peak = variant.peakBitRate, peak > 0 else { continue }
                guard let nfrVal = variant.videoAttributes?.nominalFrameRate else { continue }
                let nfr = Double(nfrVal)
                guard nfr > 0 else { continue }
                ladder.append((peak: peak, fps: nfr))
            }
        }
        if ladder.isEmpty { return nil }

        func fpsFor(bitrate: Double) -> Double {
            var best: (delta: Double, fps: Double)? = nil
            for entry in ladder {
                let delta = abs(entry.peak - bitrate)
                let tol = max(entry.peak * 0.10, 50_000)
                if delta <= tol, (best == nil || delta < best!.delta) {
                    best = (delta, entry.fps)
                }
            }
            return best?.fps ?? 0
        }

        var expected: Double = 0
        for event in log.events {
            let bitrate = event.indicatedBitrate
            let duration = event.durationWatched
            guard bitrate > 0, duration > 0 else { continue }
            let fps = fpsFor(bitrate: bitrate)
            if fps > 0 { expected += duration * fps }
        }
        if expected <= 0 { return nil }
        let dropped = Double(diagnostics.droppedVideoFrames ?? 0)
        return max(0, expected - dropped)
    }

    /// Self-healing ladder: start with variants ≤ `preferredMaximumResolution`,
    /// then if AVPlayer was ever observed playing a higher bitrate than
    /// our filtered max, expand the ladder to include every variant in
    /// the asset whose peak is between the filter ceiling and the
    /// observed max (inclusive at the top with 10% tolerance for EWMA
    /// jitter). The `preferredMaximumResolution` hint is documented as
    /// non-binding, so this absorbs the rare break-through without
    /// silently misreporting quality% as 100% of a too-low denominator.
    ///
    /// Returns the full eligible set every call — pure-function, no
    /// stateful ratchet. Items deduplicated by peakBitRate.
    fileprivate func selectableLadderPeaks() -> [(peak: Double, label: String)] {
        guard let item = player.currentItem,
              let asset = item.asset as? AVURLAsset else { return [] }
        let cap = item.preferredMaximumResolution
        let hasCap = cap.width > 0 && cap.height > 0

        func makeLabel(_ variant: AVAssetVariant) -> String {
            if let size = variant.videoAttributes?.presentationSize, size.height > 0 {
                return "\(Int(size.height))p"
            }
            return ""
        }

        // Cap-filtered set first.
        var filtered: [(peak: Double, label: String)] = []
        for variant in asset.variants {
            guard let peak = variant.peakBitRate, peak > 0 else { continue }
            if hasCap, let size = variant.videoAttributes?.presentationSize, size.height > 0 {
                if size.height > cap.height || size.width > cap.width { continue }
            }
            filtered.append((peak: peak, label: makeLabel(variant)))
        }

        // Find max observed indicatedBitrate from the access log.
        var observedMax: Double = 0
        if let logRef = item.accessLog() {
            for event in logRef.events {
                if event.indicatedBitrate > observedMax { observedMax = event.indicatedBitrate }
            }
        }

        // If the player has been observed above the filter ceiling,
        // pull in every variant from the asset whose peak fits between
        // the old ceiling and the observed max + 10% tolerance.
        let filteredMax = filtered.map(\.peak).max() ?? 0
        if observedMax > 0 && observedMax > filteredMax {
            let tolerated = observedMax * 1.10
            var seenPeaks = Set(filtered.map(\.peak))
            for variant in asset.variants {
                guard let peak = variant.peakBitRate, peak > 0 else { continue }
                if peak <= filteredMax { continue }
                if peak > tolerated { continue }
                if seenPeaks.contains(peak) { continue }
                seenPeaks.insert(peak)
                filtered.append((peak: peak, label: makeLabel(variant)))
            }
        }
        return filtered
    }

    fileprivate func perVariantTimeSeconds() -> [String: Double] {
        guard let item = player.currentItem,
              let log = item.accessLog() else { return [:] }

        // Build a bitrate → resolution lookup from the self-healing
        // selectable ladder. Tolerant match below since the access log's
        // `indicatedBitrate` is an EWMA estimate, not an exact
        // reproduction of the manifest's BANDWIDTH value.
        let ladder = selectableLadderPeaks()

        func labelFor(bitrate: Double) -> String {
            var best: (delta: Double, label: String)? = nil
            for entry in ladder {
                let delta = abs(entry.peak - bitrate)
                let tol = max(entry.peak * 0.10, 50_000) // ±10% or 50 kbps, whichever wider
                if delta <= tol, (best == nil || delta < best!.delta) {
                    best = (delta, entry.label)
                }
            }
            let kbps = Int((bitrate / 1000).rounded())
            if let b = best, !b.label.isEmpty {
                return "\(b.label)@\(kbps)kbps"
            }
            return "\(kbps)kbps"
        }

        // Seed the map with every allowed variant at 0s so the
        // dashboard renders the FULL menu the player can choose from,
        // not just the ones it's picked. Variants excluded by the
        // resolution cap above never appear in `ladder`, so the menu
        // accurately reflects the platform's actual selectable set.
        // Seed with prior-play accumulations so retry() preserves
        // continuity — the new AVPlayerItem's access log starts at
        // zero so without this every retry would zero the tile.
        var out: [String: Double] = [:]
        for entry in ladder {
            let kbps = Int((entry.peak / 1000).rounded())
            let key = entry.label.isEmpty ? "\(kbps)kbps" : "\(entry.label)@\(kbps)kbps"
            out[key] = 0
        }
        for (key, seconds) in priorPerVariantTimeSeconds {
            out[key, default: 0] += seconds
        }
        for event in log.events {
            let bitrate = event.indicatedBitrate
            let duration = event.durationWatched
            guard bitrate > 0, duration > 0 else { continue }
            let key = labelFor(bitrate: bitrate)
            // Round to 2 decimal places so the JSON stays compact and
            // the dashboard's chip rendering shows e.g. "12.34" not
            // "12.339999999". Sum first, round on read.
            out[key, default: 0] += duration
        }
        // Round at the end so the running sum stays full precision.
        return out.mapValues { (round($0 * 100) / 100) }
    }

    /// Snapshot current per-variant totals so the next access-log walk
    /// after AVPlayerItem replacement continues from here. Called from
    /// retry(). Captures the FULL merged result (priors + current
    /// access log) so subsequent retries also stack correctly.
    fileprivate func snapshotPerVariantForRestart() {
        priorPerVariantTimeSeconds = perVariantTimeSeconds()
    }

    /// Zero the priors so a fresh play (reload) starts from zero.
    /// Called from reload() before regeneratePlayID/loadStream.
    fileprivate func resetPerVariantForFreshPlay() {
        priorPerVariantTimeSeconds.removeAll()
    }

    /// Log-bitrate quality weighting with a baseline floor. Mirrors
    /// the dashboard's PlayLog `computeQualityPct` formula
    /// (Weber-Fechner perceptual model — doubling bitrate near the top
    /// barely registers, so linear `bitrate/maxPeak` over-penalises the
    /// mid-tier). Output is `log(kbps/minKbps) / log(maxKbps/minKbps)`
    /// clamped to the floor. Single-variant ladders return nil because
    /// the log ratio is undefined.
    private static let qualityBaselineFloor: Double = 0.20

    /// Quality% denominator. Shares `selectableLadderPeaks()` with the
    /// per-variant dwell map so the displayed menu and the quality
    /// math always use the same ladder — including when the
    /// self-healing path expands it after an observed cap break.
    private func variantLadderKbps() -> (minKbps: Double, maxKbps: Double, denom: Double)? {
        let entries = selectableLadderPeaks()
        let bitrates = entries.map { $0.peak / 1000.0 }
        guard bitrates.count >= 2 else { return nil }
        let minKbps = bitrates.min()!
        let maxKbps = bitrates.max()!
        guard maxKbps > minKbps else { return nil }
        return (minKbps, maxKbps, Foundation.log(maxKbps / minKbps))
    }

    private func qualityWeightForBitrate(_ bitrateBps: Double, ladder: (minKbps: Double, maxKbps: Double, denom: Double)) -> Double {
        let kbps = bitrateBps / 1000.0
        let ratio = kbps / ladder.minKbps
        let raw = ratio > 0 ? Foundation.log(ratio) / ladder.denom : 0
        // Cap at 1.0 — defensive against a race during cap change or
        // a manifest variant outside the filtered set still appearing
        // in the access log briefly. Playing the top selectable
        // variant is always 100%, never more.
        return max(Self.qualityBaselineFloor, min(1.0, raw))
    }

    /// Lifetime time-weighted log-bitrate quality across all
    /// access-log events. Returns nil when there's no ladder or no
    /// playback time.
    fileprivate func videoQualityAvgPct() -> Double? {
        guard let item = player.currentItem,
              let logRef = item.accessLog(),
              let ladder = variantLadderKbps() else { return nil }
        var weighted: Double = 0
        var total: Double = 0
        for event in logRef.events {
            let bitrate = event.indicatedBitrate
            let duration = event.durationWatched
            guard bitrate > 0, duration > 0 else { continue }
            weighted += qualityWeightForBitrate(bitrate, ladder: ladder) * duration
            total += duration
        }
        guard total > 0 else { return nil }
        return (weighted / total) * 100
    }

    /// Same log-bitrate formula but restricted to the last 60s of
    /// *watched* time. Walks events newest-first, accumulating
    /// `durationWatched` until 60s is reached. Stalls / pauses contribute
    /// zero naturally. Returns nil when there isn't enough data yet.
    fileprivate func videoQuality60sPct() -> Double? {
        guard let item = player.currentItem,
              let logRef = item.accessLog(),
              let ladder = variantLadderKbps() else { return nil }
        let windowSec: Double = 60
        var weighted: Double = 0
        var total: Double = 0
        for event in logRef.events.reversed() {
            let bitrate = event.indicatedBitrate
            let duration = event.durationWatched
            guard bitrate > 0, duration > 0 else { continue }
            let remaining = windowSec - total
            if remaining <= 0 { break }
            let take = min(duration, remaining)
            weighted += qualityWeightForBitrate(bitrate, ladder: ladder) * take
            total += take
        }
        guard total > 0 else { return nil }
        return (weighted / total) * 100
    }

    /// Resolution AVPlayer is about to fetch. Matches the most recent
    /// access-log event's `indicatedBitrate` (the ABR pick for the next
    /// download) against the asset's variant ladder by peakBitRate
    /// with ±10% tolerance. Returns nil before the first access-log
    /// event lands or when the indicated bitrate doesn't match any
    /// ladder entry within tolerance.
    fileprivate func fetchingResolution() -> String? {
        guard let item = player.currentItem,
              let logRef = item.accessLog(),
              let asset = item.asset as? AVURLAsset else { return nil }
        var indicated: Double = 0
        for event in logRef.events.reversed() {
            if event.indicatedBitrate > 0 {
                indicated = event.indicatedBitrate
                break
            }
        }
        guard indicated > 0 else { return nil }
        var best: (delta: Double, label: String)? = nil
        for variant in asset.variants {
            guard let peak = variant.peakBitRate, peak > 0 else { continue }
            guard let size = variant.videoAttributes?.presentationSize,
                  size.width > 0, size.height > 0 else { continue }
            let delta = abs(peak - indicated)
            let tol = max(peak * 0.10, 50_000)
            if delta > tol { continue }
            if best == nil || delta < best!.delta {
                best = (delta, "\(Int(size.width))x\(Int(size.height))")
            }
        }
        return best?.label
    }

    fileprivate func buildMetricsPayload(event: String, at eventAt: Date = Date(), extra: [String: Any] = [:]) -> [String: Any] {
        let timestamp = Self.metricsTimestampFormatter.string(from: eventAt)
        // Flush accruing residency into the current bucket so the
        // payload reflects time-up-to-now, not time-up-to-last-state-
        // change. Cheap; idempotent.
        diagnostics.flushStateResidencyForRead()
        let loopCount = max(0, diagnostics.loopCountPlayer)
        let loopIncrement = max(0, loopCount - lastReportedLoopCount)
        lastReportedLoopCount = loopCount
        var payload: [String: Any?] = [
            // play_id + attempt_id at the moment this event fires — go-proxy
            // merges these into session_data so the resulting snapshot row
            // (and any network log entries stamped from this session)
            // carry the current play / attempt ids even when the event
            // arrives between manifest fetches. The URL-query fallback in
            // patchSessionMetrics covers the same ground; including them
            // in the body too is belt-and-braces against a delayed event
            // that races a subsequent id rotation. Bug #4 fix.
            "play_id": currentPlayID,
            "attempt_id": currentAttemptID,
            "player_metrics_source": "ios",
            "player_metrics_last_event": event,
            "player_metrics_trigger_type": event,
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
            // ── #550 Phase 1: ms time fields (gerund-named) ────────
            "player_metrics_video_first_frame_time_ms": secondsToMs(videoFirstFrameSeconds),
            "player_metrics_video_start_time_ms": secondsToMs(videoPlayingTimeSeconds),
            "player_metrics_stalling_count": diagnostics.stallingCount,
            "player_metrics_stalling_time_ms": secondsToMs(diagnostics.stallingTimeS),
            "player_metrics_stall_duration_ms": secondsToMs(diagnostics.stallDurationS),
            "player_metrics_buffering_duration_ms": secondsToMs(diagnostics.bufferingDurationS),
            // State residency accumulators — cumulative-on-the-wire
            // since play start. Forwarder computes per-row deltas from
            // a per-play state cache and stores both as paired
            // *_time_ms + *_time_ms_delta columns in ClickHouse.
            "player_metrics_playing_time_ms": secondsToMs(diagnostics.playingTimeS),
            "player_metrics_playing_count": diagnostics.playingCount,
            "player_metrics_pausing_time_ms": secondsToMs(diagnostics.pausingTimeS),
            "player_metrics_pausing_count": diagnostics.pausingCount,
            "player_metrics_buffering_time_ms": secondsToMs(diagnostics.bufferingTimeS),
            "player_metrics_buffering_count": diagnostics.bufferingCount,
            "player_metrics_idling_time_ms": secondsToMs(diagnostics.idlingTimeS),
            "player_metrics_idling_count": diagnostics.idlingCount,
            "player_metrics_seeking_time_ms": secondsToMs(diagnostics.seekingTimeS),
            "player_metrics_seeking_count": diagnostics.seekingCount,
            "player_metrics_trickplaying_time_ms": secondsToMs(diagnostics.trickplayingTimeS),
            "player_metrics_trickplaying_count": diagnostics.trickplayingCount,
            // ── #550 Phase 2: outcome + error ──────────────────────
            // playback_status defaults to 'in_progress' for any
            // non-terminal payload; iOS sets explicit values for
            // session_end events (completed / user_stopped / failed_*).
            // playback_reason mirrors player_state during in_progress;
            // classifier-derived on terminal rows.
            // playback_status / _reason — heartbeats default to
            // in_progress + the current state. session_end events
            // (and any heartbeat after markTerminal fires) pick up
            // the terminal values diagnostics stamped. Once markTerminal
            // has run, EVERY subsequent payload carries the terminal
            // status so a late-arriving heartbeat after teardown still
            // reads correctly.
            "player_metrics_playback_status": diagnostics.terminalStatus ?? "in_progress",
            "player_metrics_playback_reason": diagnostics.terminalReason ?? diagnostics.state,
            "player_metrics_error_count": diagnostics.errorCount,
            // stall_stuck: sticky true when AVPlayer transitioned
            // from .waitingToPlay to .paused mid-stall. The player
            // WILL NOT auto-recover; the dashboard / operator needs
            // to drive a play() retry. Cleared on the next .playing
            // transition or play boundary. The state lane stays on
            // "stalled" for residency continuity — this flag is the
            // orthogonal "needs intervention" signal.
            "player_metrics_stall_stuck": diagnostics.stallStuck,
            // ── #550 Phase 4: device / platform taxonomy ───────────
            "player_metrics_os_version_major": DeviceInfo.osVersionMajor,
            "player_metrics_os_version_minor": DeviceInfo.osVersionMinor,
            "player_metrics_app_version": DeviceInfo.appVersion,
            "player_metrics_device_class": DeviceInfo.deviceClass,
            "player_metrics_device_model": DeviceInfo.deviceModel,
            "player_metrics_player_tech": DeviceInfo.playerTech,
            // Orientation-aware physical pixels — replaces the prior
            // screen_width_px / screen_height_px / screen_density
            // taxonomy fields (which were static portrait-only).
            "player_metrics_device_resolution": DeviceInfo.deviceResolution(),
            // Displayed frame count (issue #486 follow-up). Recomputed
            // from playing-time × active variant's nominal frame rate
            // minus dropped frames, instead of the legacy
            // `estimatedDisplayedFrames` that drifted because it
            // assumed a constant fps regardless of variant. The
            // dashboard's FPS chart reads delta(frames)/delta(time)
            // off this column — with the corrected formula a stall
            // shows as a 0 fps dip and a drop burst shows as a notch
            // proportional to the drop rate. Falls back to the legacy
            // estimate when nominalFrameRate hasn't loaded yet (early
            // in a play, before the first AVAssetTrack metadata
            // resolves).
            "player_metrics_frames_displayed": framesDisplayedFromAccessLog()
                ?? framesDisplayedFromNominalFps()
                ?? diagnostics.estimatedDisplayedFrames.map { roundMetric($0) },
            "player_metrics_dropped_frames": diagnostics.droppedVideoFrames.map { roundMetric($0) },
            // Publish the variant's nominal FPS alongside so the
            // dashboard can show effective vs nominal at a glance.
            "player_metrics_nominal_fps_current": diagnostics.nominalFrameRate,
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
        // HOLD-BACK / PART-HOLD-BACK from the manifest (issue #486
        // follow-up). AVFoundation parses EXT-X-SERVER-CONTROL for us
        // and exposes the result via `recommendedTimeOffsetFromLive`
        // (iOS 13+). `configured` is what the app currently has set;
        // when both are present the gap is the app's deviation from
        // Apple's manifest-derived recommendation.
        //
        // Pre-computed live_offset_true_s = live_offset_s +
        // recommended_offset_s is published here too so the dashboard
        // doesn't have to derive — all the math lives in the client.
        if let item = player.currentItem {
            var recommendedSec: Double? = nil
            let rec = item.recommendedTimeOffsetFromLive
            if rec.isValid, rec.isNumeric {
                let s = rec.seconds
                if s.isFinite, s >= 0 {
                    recommendedSec = s
                    compact["player_metrics_recommended_offset_s"] = roundSeconds(s)
                }
            }
            let cfg = item.configuredTimeOffsetFromLive
            if cfg.isValid, cfg.isNumeric {
                let s = cfg.seconds
                if s.isFinite, s >= 0 {
                    compact["player_metrics_configured_offset_s"] = roundSeconds(s)
                }
            }
            // Replace `live_offset_s` with the true offset to the end
            // of the playlist = the seekable-edge offset plus the
            // manifest's HOLD-BACK / PART-HOLD-BACK. The raw
            // seekable-edge distance alone is misleading (it sits
            // HOLD-BACK seconds short of the true live edge), so the
            // dashboard never sees that value — the field now means
            // "offset to true playlist end" for every iOS heartbeat.
            // No parallel `_true_s` field is published.
            if let liveOff = diagnostics.liveOffset, let rec = recommendedSec {
                compact["player_metrics_live_offset_s"] = roundSeconds(liveOff + rec)
            }
        }
        // Per-variant watch time (issue #486). One JSON-object string
        // mapping indicatedBitrate (bps) → seconds watched at that
        // bitrate. Encoded as a string so the existing CH long-tail
        // column (`session_json`) and the dashboard chip renderer
        // pick it up without schema changes. Empty payloads are
        // omitted entirely so heartbeats early in a play don't ship
        // `{}`. The last heartbeat before tear-down carries the
        // final accumulated values — no separate end-of-session POST
        // needed.
        let perVariant = perVariantTimeSeconds()
        if !perVariant.isEmpty,
           let data = try? JSONSerialization.data(withJSONObject: perVariant, options: [.sortedKeys]),
           let json = String(data: data, encoding: .utf8) {
            compact["player_metrics_time_per_variant_s"] = json
        }
        // Lifetime + 60s rolling quality. Both computed from the same
        // access-log + variant ladder as perVariantTimeSeconds() so the
        // dashboard never has to re-derive — single source of truth,
        // stored in CH forever. nil values omit the key (so a fresh
        // play before any access-log events doesn't ship 0%).
        if let avg = videoQualityAvgPct() {
            compact["player_metrics_video_quality_avg_pct"] = round(avg * 100) / 100
        }
        if let q60 = videoQuality60sPct() {
            compact["player_metrics_video_quality_60s_pct"] = round(q60 * 100) / 100
        }
        // Resolution AVPlayer is about to fetch — matches indicatedBitrate
        // against the asset's variant ladder. Persisted in CH so historical
        // replays show the same value. Nil → key omitted.
        if let fetching = fetchingResolution() {
            compact["player_metrics_fetching_resolution"] = fetching
        }
        // Client-side RTT proxy from AVMetrics TTFB (issue #486).
        // Median of the recent MediaResourceRequest TTFBs — only
        // populated on iOS 18+ with the AVMetrics subscriber attached.
        // Pairs with the server-side `client_rtt_ms` on the RTT chart
        // so we can see when the two views diverge.
        if #available(iOS 18.0, *), let sub = avMetricsSubscriber as? AVMetricsSubscriber {
            let medianMs = sub.medianTTFB()
            if medianMs > 0 {
                compact["player_metrics_client_rtt_avmetrics_ms"] = roundMetric(medianMs)
            }
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
            "stalling_count": diagnostics.stallingCount,
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
        let pathURL = baseURL.appendingPathComponent("api/session").appendingPathComponent(sessionId).appendingPathComponent("metrics")
        // Stamp play_id + attempt_id on the URL so go-proxy's
        // handlePostSessionMetrics picks them up via its URL query
        // read. Without this the proxy only sees them on manifest
        // GETs — meaning an iPad mid-stream that hasn't re-fetched
        // its manifest after a restart would have its `restart` event
        // land with the OLD attempt_id. Bug #4 fix.
        var url = appendPlayID(to: pathURL)
        url = appendAttemptID(to: url)
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
            let (respData, response) = try await URLSession.shared.data(for: request)
            let status = (response as? HTTPURLResponse)?.statusCode ?? -1
            if status != 200 {
                let bodyStr = String(data: respData.prefix(200), encoding: .utf8) ?? "<binary>"
                NSLog("[METRICS-POST] ← HTTP \(status) body=\(bodyStr)")
            }
        } catch {
            NSLog("[METRICS-POST] EXCEPTION: \(error.localizedDescription)")
            log("Metrics patch failed: \(error.localizedDescription)")
        }
    }

    // MARK: - iOS 18 AVMetrics spike (issue #486)

    /// Replace the AVMetrics subscriber bound to `item`. Cancels the
    /// previous one (if any) so a content swap or recovery-driven
    /// `replaceCurrentItem` doesn't leak two subscribers fighting over
    /// the buffer. No-op on pre-iOS-18 — the project min target is iOS
    /// 26 so the guard is belt-and-suspenders.
    /// Grace period between dropping our reference to a subscriber
    /// and cancelling its async loops. AVFoundation emits the
    /// `AVMetricPlayerItemPlaybackSummaryEvent` on the *old* item
    /// shortly after `replaceCurrentItem`; if we cancel synchronously
    /// the summary lands on a torn-down loop and is dropped. 1.5s is
    /// well over the observed emission window for both replace-and-
    /// swap and replace-with-nil. Issue #486 spike.
    private static let avMetricsDetachGraceNs: UInt64 = 1_500_000_000

    fileprivate func attachAVMetrics(to item: AVPlayerItem) {
        if #available(iOS 18.0, *) {
            NSLog("[AVMetrics] attachAVMetrics — replacing subscriber for new AVPlayerItem")
            scheduleAVMetricsDrain(avMetricsSubscriber as? AVMetricsSubscriber)
            // Bridge Apple's authoritative single-event-per-user-seek
            // signal (AVMetricPlayerItemSeekEvent, iOS 26+) into
            // PlaybackDiagnostics so seekingCount reflects user intent
            // rather than counting AVPlayer's internal TimeJumped
            // notifications. The bridge fires nothing on pre-iOS-26
            // OSes (subclass unavailable) and the legacy debounced
            // TimeJumped path remains authoritative there.
            let diag = diagnostics
            avMetricsSubscriber = AVMetricsSubscriber(
                item: item,
                onBatch: { [weak self] events in
                    await self?.sendAVMetricsBatch(events)
                },
                onSeek: { [weak diag] in
                    Task { @MainActor in diag?.onAVMetricSeek() }
                }
            )
        } else {
            NSLog("[AVMetrics] attachAVMetrics skipped — OS pre-iOS-18 (subscriber gated)")
        }
    }

    fileprivate func detachAVMetrics() {
        if #available(iOS 18.0, *) {
            NSLog("[AVMetrics] detachAVMetrics — draining old subscriber for PlaybackSummary")
            scheduleAVMetricsDrain(avMetricsSubscriber as? AVMetricsSubscriber)
            avMetricsSubscriber = nil
        }
    }

    /// Schedule a delayed cancel on `old` so trailing AVMetric events
    /// — chiefly `PlaybackSummaryEvent`, emitted on item termination
    /// — have time to be recorded before the async loops shut down.
    @available(iOS 18.0, *)
    private func scheduleAVMetricsDrain(_ old: AVMetricsSubscriber?) {
        guard let old else { return }
        Task {
            try? await Task.sleep(nanoseconds: Self.avMetricsDetachGraceNs)
            NSLog("[AVMetrics] drain grace elapsed — cancelling prior subscriber")
            old.cancel()
        }
    }

    /// POST a batch of AVMetric events to the proxy's
    /// `/api/session/{id}/avmetrics` endpoint. Mirrors patchSessionMetrics
    /// for the URL-construction + headers + play_id/attempt_id stamping;
    /// the body is `{events: [...]}` instead of the heartbeat's
    /// `{set, fields}` envelope so the two streams stay independently
    /// projectable on the server.
    fileprivate func sendAVMetricsBatch(_ events: [[String: Any]]) async {
        guard !events.isEmpty else { return }
        guard let baseURL = metricsBaseURL() else {
            NSLog("[AVMetrics] sendBatch skipped — no metricsBaseURL (no active server)")
            return
        }
        guard let sessionId = await resolveMetricsSessionId(baseURL: baseURL) else {
            NSLog("[AVMetrics] sendBatch skipped — no session_id resolved yet (try again on next batch)")
            return
        }
        let pathURL = baseURL
            .appendingPathComponent("api/session")
            .appendingPathComponent(sessionId)
            .appendingPathComponent("avmetrics")
        var url = appendPlayID(to: pathURL)
        url = appendAttemptID(to: url)
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        applyPlayerHeaders(to: &request)
        let body: [String: Any] = ["events": events]
        do {
            request.httpBody = try JSONSerialization.data(withJSONObject: body, options: [])
            let (data, response) = try await URLSession.shared.data(for: request)
            let status = (response as? HTTPURLResponse)?.statusCode ?? -1
            if status != 200 {
                let bodyStr = String(data: data.prefix(200), encoding: .utf8) ?? "<binary>"
                NSLog("[AVMetrics] POST ← HTTP \(status) body=\(bodyStr)")
            }
        } catch {
            NSLog("[AVMetrics] POST failed: \(error.localizedDescription)")
            log("AVMetrics batch POST failed: \(error.localizedDescription)")
        }
    }

    /// Stamp `Player-ID` + `X-Playback-Session-Id` + `User-Agent` headers
    /// on a URLSession request so go-proxy can bind the request to our
    /// session for failure-injection routing AND identify the device
    /// family across every request the app makes (metrics POST, HAR
    /// snapshot, session lookup, master preflight).
    ///
    /// The default URLSession User-Agent (`CFNetwork/… Darwin/…`) is
    /// device-family agnostic, so the proxy's stored user_agent kept
    /// getting overwritten between manifest fetches by these thinner
    /// app-side POSTs and the iPad/iPhone/AppleTV label was lost.
    /// Issue #471.
    fileprivate func applyPlayerHeaders(to request: inout URLRequest) {
        request.setValue(playerId, forHTTPHeaderField: "Player-ID")
        request.setValue(playerId, forHTTPHeaderField: "X-Playback-Session-Id")
        request.setValue(Self.appUserAgent, forHTTPHeaderField: "User-Agent")
    }

    /// Device-aware User-Agent shared across every URLSession request.
    /// Format: `InfiniteStreamPlayer/<app-version> (<idiom>; <os> <version>)`
    /// e.g. `InfiniteStreamPlayer/1.0 (iPad; iPadOS 26.1)`.
    /// Computed once at first access — UIDevice / Bundle reads are stable
    /// for the app's lifetime.
    private static let appUserAgent: String = {
        let device = UIDevice.current
        let idiom: String
        switch device.userInterfaceIdiom {
        case .pad:     idiom = "iPad"
        case .phone:   idiom = "iPhone"
        case .tv:      idiom = "AppleTV"
        case .carPlay: idiom = "CarPlay"
        case .mac:     idiom = "Mac"
        case .vision:  idiom = "Vision"
        default:       idiom = "Apple"
        }
        let appVersion = (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? "0"
        return "InfiniteStreamPlayer/\(appVersion) (\(idiom); \(device.systemName) \(device.systemVersion))"
    }()

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

    /// Convert a Double-seconds residency accumulator to UInt32-ms
    /// for the #550 Phase 1 wire contract. Clamped at 0; values
    /// beyond UInt32 max (~49 days) saturate. Round-half-to-even for
    /// stable values across snapshots that read mid-state.
    fileprivate func secondsToMs(_ value: Double) -> UInt32 {
        let scaled = (value * 1000).rounded()
        if scaled <= 0 { return 0 }
        if scaled >= Double(UInt32.max) { return UInt32.max }
        return UInt32(scaled)
    }

    fileprivate func secondsToMs(_ value: Double?) -> UInt32 {
        guard let v = value else { return 0 }
        return secondsToMs(v)
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
