import AVFoundation
import Combine
import CoreGraphics
import Foundation

@MainActor
final class PlaybackViewModel: ObservableObject {
    private static let metricsTimestampFormatter: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return formatter
    }()

    @Published var baseURLString: String = "http://100.111.190.54:40081" {
        didSet {
            persist(.baseURL, baseURLString)
        }
    }
    @Published var playbackBaseURLString: String = "http://100.111.190.54:40081" {
        didSet { persist(.playbackBaseURL, playbackBaseURLString) }
    }
    @Published var availableContent: [ContentItem] = []
    @Published var selectedContent: String = "" {
        didSet {
            persist(.selectedContentFull, selectedContent)
            persist(.selectedContent, selectedContent)
            persist(.selectedContentBase, baseName(from: selectedContent))
        }
    }
    @Published var protocolOption: ProtocolOption = .hls {
        didSet { persist(.selectedProtocol, protocolOption.rawValue) }
    }
    @Published var segmentOption: SegmentOption = .s6 {
        didSet { persist(.selectedSegment, segmentOption.rawValue) }
    }
    @Published var codecOption: CodecOption = .h264 {
        didSet { persist(.selectedCodec, codecOption.rawValue) }
    }
    @Published var currentURL: String = "" {
        didSet { persist(.selectedUrl, currentURL) }
    }
    @Published var statusMessage: String = ""
    @Published var lastMasterRequestLine: String = ""
    @Published var isMuted: Bool = false {
        didSet { persist(.audioMuted, isMuted ? "true" : "false") }
    }
    @Published var logLines: [String] = []
    @Published var playerId: String = ""
    @Published var includePlayerIdInURL: Bool = true
    @Published var primePlayback: Bool = true
    @Published var forcePlayerIdOnPlayback: Bool = false
    @Published var allowPlayerIdOnContentPort: Bool = false
    @Published var prefer4kNative: Bool = false {
        didSet { persist(.prefer4kNative, prefer4kNative ? "true" : "false") }
    }
    @Published var autoRecoveryEnabled: Bool = false {
        didSet { persist(.autoRecoveryEnabled, autoRecoveryEnabled ? "true" : "false") }
    }
    @Published var playerRestarts: Int = 0
    @Published var profileShiftCount: Int = 0

    let player = AVPlayer()
    let diagnostics = PlaybackDiagnostics()

    private let decoder = JSONDecoder()
    private var cancellables: Set<AnyCancellable> = []
    private var playlistMonitorTask: Task<Void, Never>?
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
    private let metricsHeartbeatSeconds: TimeInterval = 5
    private let metricsSessionLookupSeconds: TimeInterval = 30
    private let autoRecoveryThresholdSeconds: TimeInterval = 60
    private let autoRecoveryCooldownSeconds: TimeInterval = 60
    private var zeroBufferStartedAt: Date?
    private var lastAutoRecoveryRestartAt: Date?
    private let diagnosticsProbesEnabled = false
    private let logPlaylistsOnPlay = false
    private let masterPreflightEnabled = true
    private let masterPreflightMaxAttempts = 5
    private let masterPreflightDefaultDelayMs: UInt64 = 500

    init() {
        loadDefaults()
        diagnostics.bind(to: player)
        if playerId.isEmpty {
            playerId = UUID().uuidString
            persist(.playerId, playerId)
        }
        bindDiagnosticsLogging()
        bindMetricsReporting()
        startMetricsHeartbeat()
    }

    func refreshContentList() async {
        guard let baseURL = URL(string: baseURLString) else {
            statusMessage = "Invalid base URL"
            log("Invalid base URL: \(baseURLString)")
            return
        }
        let url = baseURL.appendingPathComponent("api/content")
        log("Fetching content list: \(url.absoluteString)")
        do {
            let (data, response) = try await URLSession.shared.data(from: url)
            if let http = response as? HTTPURLResponse {
                log("Response: HTTP \(http.statusCode)")
            }
            let all = try decoder.decode([ContentItem].self, from: data)
            availableContent = all.filter { $0.hasHls || $0.hasDash }
                .filter { inferCodec(from: $0.name) == .h264 }
            if availableContent.isEmpty {
                statusMessage = "No content found"
                log("Content list empty after filtering has_hls/has_dash")
            } else {
                statusMessage = "Loaded \(availableContent.count) items"
                log("Loaded \(availableContent.count) items")
            }
            if selectedContent.isEmpty || !availableContent.contains(where: { $0.name == selectedContent }) {
                selectedContent = chooseContent(codec: codecOption, available: availableContent, stored: selectedContent)
            }
        } catch {
            statusMessage = "Failed to load content: \(error.localizedDescription)"
            log("Fetch failed: \(error.localizedDescription)")
        }
    }

    func applySelection() {
        guard let baseURL = URL(string: baseURLString) else {
            statusMessage = "Invalid base URL"
            log("Invalid base URL: \(baseURLString)")
            return
        }
        let playbackBase = resolvePlaybackBase(from: baseURL)
        log("Apply selection: protocol=\(protocolOption.label) segment=\(segmentOption.rawValue) codec=\(codecOption.label) prime=\(primePlayback) includePlayerId=\(includePlayerIdInURL) forcePlayerIdOnPlayback=\(forcePlayerIdOnPlayback)")
        log("Playback base: \(playbackBase.absoluteString)")

        if protocolOption == .dash {
            statusMessage = "DASH playback is not supported in AVFoundation on iOS/tvOS"
            log("DASH selected - AVFoundation does not support DASH")
            return
        }

        var codecFallback = false
        var contentName = selectedContent
        if contentName.isEmpty {
            contentName = chooseContent(codec: codecOption, available: availableContent, stored: selectedContent)
        }
        if codecOption != .all {
            let candidate = chooseContent(codec: codecOption, available: availableContent, stored: selectedContent)
            if !candidate.isEmpty && candidate != contentName {
                contentName = candidate
                selectedContent = candidate
                codecFallback = inferCodec(from: candidate) != codecOption
            }
        }

        guard !contentName.isEmpty else {
            statusMessage = "Select content first"
            log("No content selected")
            return
        }

        if primePlayback {
            Task { [weak self] in
                guard let self else { return }
                if let primedBase = await self.primePlaybackBaseURL(playbackBase, contentName: contentName) {
                    self.log("Prime: using redirected base for playback: \(primedBase.absoluteString)")
                    let includeId = self.forcePlayerIdOnPlayback
                    self.startPlayback(with: primedBase, contentName: contentName, codecFallback: codecFallback, includePlayerId: includeId)
                } else {
                    self.log("Prime: failed, using original playback base: \(playbackBase.absoluteString)")
                    let includeId = self.forcePlayerIdOnPlayback
                    self.startPlayback(with: playbackBase, contentName: contentName, codecFallback: codecFallback, includePlayerId: includeId)
                }
            }
        } else {
            startPlayback(with: playbackBase, contentName: contentName, codecFallback: codecFallback, includePlayerId: includePlayerIdInURL)
        }
    }

    func play() {
        player.isMuted = isMuted
        player.play()
    }

    func pause() {
        player.pause()
    }

    func reload() {
        applySelection()
        play()
    }

    func retryFetch() {
        reload()
    }

    func restartPlayback(reason: String = "manual") {
        playerRestarts = max(0, playerRestarts) + 1
        if reason == "auto_recovery_zero_buffer" {
            lastAutoRecoveryRestartAt = Date()
            zeroBufferStartedAt = nil
        }
        Task {
            await sendPlayerMetrics(event: "restart", extra: [
                "player_metrics_restart_reason": reason,
                "player_restarts": playerRestarts,
                "player_auto_recovery_enabled": autoRecoveryEnabled
            ])
        }
        player.pause()
        player.replaceCurrentItem(with: nil)
        reload()
    }

    func reloadPage() async {
        diagnostics.reset()
        statusMessage = ""
        logLines.removeAll()
        await refreshContentList()
        applySelection()
        play()
    }

    func logAction(_ message: String) {
        log("Action: \(message)")
    }

    func playVariantDirect() {
        guard let playbackBase = URL(string: playbackBaseURLString) else {
            statusMessage = "Invalid playback base URL"
            log("Invalid playback base URL: \(playbackBaseURLString)")
            return
        }
        var contentName = selectedContent
        if contentName.isEmpty {
            contentName = chooseContent(codec: codecOption, available: availableContent, stored: selectedContent)
        }
        guard !contentName.isEmpty else {
            statusMessage = "Select content first"
            log("No content selected")
            return
        }
        let durationSuffix: String
        switch segmentOption {
        case .s2: durationSuffix = "2s"
        case .s6: durationSuffix = "6s"
        default: durationSuffix = "6s"
        }
        let variantPath = "playlist_\(durationSuffix)_360p.m3u8"
        let base = playbackBase.appendingPathComponent("go-live").appendingPathComponent(contentName)
        var url = base.appendingPathComponent(variantPath)
        let includePlayerId = !primePlayback && includePlayerIdInURL && playbackBase.port != 40000
        if includePlayerId {
            url = appendPlayerId(to: url)
        } else {
            url = removePlayerId(from: url)
        }
        statusMessage = "Playing variant \(variantPath)"
        startPlaybackWithURL(url, contentName: contentName, codecFallback: false)
    }

    private func startPlayback(with playbackBase: URL, contentName: String, codecFallback: Bool, includePlayerId: Bool) {
        let is40000 = playbackBase.port == 40000
        let effectiveIncludePlayerId = includePlayerId && (!is40000 || allowPlayerIdOnContentPort)
        if is40000 && includePlayerId && !allowPlayerIdOnContentPort {
            log("Play: stripping player_id because playback base uses port 40000")
        } else if is40000 && includePlayerId && allowPlayerIdOnContentPort {
            log("Play: allowing player_id on port 40000")
        }
        log("Play config: basePort=\(playbackBase.port ?? -1) includePlayerId=\(effectiveIncludePlayerId) playerId=\(effectiveIncludePlayerId ? playerId : "<none>")")
        let playerIdParam = effectiveIncludePlayerId ? playerId : ""
        guard var url = buildStreamURL(baseURL: playbackBase, contentName: contentName, protocolOption: protocolOption, segment: segmentOption, playerId: playerIdParam) else {
            statusMessage = "Failed to build stream URL"
            log("Failed to build stream URL for \(contentName)")
            return
        }
        if !effectiveIncludePlayerId {
            url = removePlayerId(from: url)
        }
        startPlaybackWithURL(url, contentName: contentName, codecFallback: codecFallback)
    }

    private func startPlaybackWithURL(_ url: URL, contentName: String, codecFallback: Bool) {
        var finalURL = url
        if url.port == 40000 && !allowPlayerIdOnContentPort {
            finalURL = removePlayerId(from: url)
        }
        if masterPreflightEnabled && isMasterPlaylistURL(finalURL) {
            Task { @MainActor in
                await self.preflightMasterAndStart(url: finalURL, contentName: contentName, codecFallback: codecFallback)
            }
            return
        }
        startPlaybackNow(url: finalURL, contentName: contentName, codecFallback: codecFallback)
    }

    private func startPlaybackNow(url: URL, contentName: String, codecFallback: Bool) {
        currentURL = url.absoluteString
        lastMasterRequestLine = formatRequestLine(url: url)
        log("Play: URL=\(url.absoluteString)")
        log("Selected URL: \(url.absoluteString)")
        if logPlaylistsOnPlay {
            Task { [weak self] in
                guard let self else { return }
                await self.logPlaylistBodies(for: url)
            }
        }
        if diagnosticsProbesEnabled {
            Task {
                await probeURL(url)
            }
            startPlaylistMonitor()
        }

        if codecFallback {
            statusMessage = "Requested \(codecOption.label) unavailable; using \(inferCodec(from: contentName).label)"
        } else if statusMessage.isEmpty {
            statusMessage = "Playing \(contentName)"
        }

        let asset = AVURLAsset(url: url, options: nil)
        let item = AVPlayerItem(asset: asset)
        apply4kPreference(to: item)
        playbackStartAt = Date()
        videoFirstFrameSeconds = nil
        videoPlayingTimeSeconds = nil
        firstFrameReported = false
        playingReported = false
        player.replaceCurrentItem(with: item)
        player.isMuted = isMuted
        player.automaticallyWaitsToMinimizeStalling = true
        diagnostics.reset()
        lastReportedStallCount = 0
        lastReportedStallDuration = 0
        lastReportedRenditionMbps = nil
        lastReportedLoopCount = 0
        profileShiftCount = 0
        metricsSessionId = nil
        metricsLastSessionLookup = nil
        log("AVPlayerItem created for \(url.absoluteString)")
        player.play()
        log("AVPlayer play() called")
    }

    private func primePlaybackBaseURL(_ playbackBase: URL, contentName: String) async -> URL? {
        guard let primeURL = buildStreamURL(baseURL: playbackBase, contentName: contentName, protocolOption: protocolOption, segment: segmentOption, playerId: playerId) else {
            log("Prime: failed to build prime URL")
            return nil
        }
        log("Prime: URL=\(primeURL.absoluteString)")
        var request = URLRequest(url: primeURL)
        request.cachePolicy = .reloadIgnoringLocalCacheData
        let headerDump = (request.allHTTPHeaderFields ?? [:])
            .map { "\($0.key)=\($0.value)" }
            .sorted()
            .joined(separator: "; ")
        log("Prime: request GET \(request.url?.absoluteString ?? primeURL.absoluteString) headers=[\(headerDump)]")
        do {
            let logger = PrimeRedirectLogger { [weak self] fromURL, toURL, status in
                self?.log("Prime: redirect \(status ?? 0) \(fromURL.absoluteString) -> \(toURL?.absoluteString ?? "")")
            }
            let session = URLSession(configuration: .default, delegate: logger, delegateQueue: nil)
            let (_, response) = try await session.data(for: request)
            session.finishTasksAndInvalidate()
            if let http = response as? HTTPURLResponse {
                let responseHeaders = http.allHeaderFields
                    .map { "\($0.key)=\($0.value)" }
                    .sorted { "\($0)" < "\($1)" }
                    .joined(separator: "; ")
                log("Prime: HTTP \(http.statusCode) finalURL=\(http.url?.absoluteString ?? "") headers=[\(responseHeaders)]")
            }
            // Prime body logging disabled (too noisy).
            guard let finalURL = response.url else {
                log("Prime: no final URL from response")
                return nil
            }
            return baseURL(from: finalURL)
        } catch {
            log("Prime: request failed \(error.localizedDescription)")
            return nil
        }
    }

    private final class PrimeRedirectLogger: NSObject, URLSessionTaskDelegate {
        private let onRedirect: @MainActor (URL, URL?, Int?) -> Void

        init(onRedirect: @escaping @MainActor (URL, URL?, Int?) -> Void) {
            self.onRedirect = onRedirect
        }

        func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
            let fromURL = response.url ?? task.originalRequest?.url ?? request.url ?? URL(string: "about:blank")!
            Task { @MainActor in
                self.onRedirect(fromURL, request.url, response.statusCode)
            }
            completionHandler(request)
        }
    }

    private func baseURL(from url: URL) -> URL? {
        var components = URLComponents(url: url, resolvingAgainstBaseURL: false)
        components?.path = ""
        components?.query = nil
        components?.fragment = nil
        return components?.url
    }

    func toggleMute() {
        isMuted.toggle()
        player.isMuted = isMuted
    }

    private enum DefaultsKey: String {
        case selectedContentFull = "bossSelectedContentFull"
        case selectedContent = "bossSelectedContent"
        case selectedContentBase = "bossSelectedContentBase"
        case selectedCodec = "bossSelectedCodec"
        case selectedSegment = "bossSelectedSegment"
        case selectedProtocol = "bossSelectedProtocol"
        case selectedUrl = "bossSelectedUrl"
        case audioMuted = "bossAudioMuted"
        case baseURL = "bossBaseURL"
        case playbackBaseURL = "bossPlaybackBaseURL"
        case playerId = "bossPlayerId"
        case prefer4kNative = "bossPrefer4kNative"
        case autoRecoveryEnabled = "bossAutoRecovery"
    }

    private func loadDefaults() {
        let defaults = UserDefaults.standard
        if let storedBase = defaults.string(forKey: DefaultsKey.baseURL.rawValue), !storedBase.isEmpty {
            baseURLString = storedBase
        } else {
            baseURLString = "http://100.111.190.54:40000"
            defaults.setValue(baseURLString, forKey: DefaultsKey.baseURL.rawValue)
        }
        if let storedPlayback = defaults.string(forKey: DefaultsKey.playbackBaseURL.rawValue), !storedPlayback.isEmpty {
            playbackBaseURLString = storedPlayback
        } else {
            playbackBaseURLString = "http://100.111.190.54:40081"
            defaults.setValue(playbackBaseURLString, forKey: DefaultsKey.playbackBaseURL.rawValue)
        }
        if let stored = defaults.string(forKey: DefaultsKey.selectedContentFull.rawValue) {
            selectedContent = stored
        }
        if let storedCodec = defaults.string(forKey: DefaultsKey.selectedCodec.rawValue), let codec = CodecOption(rawValue: storedCodec) {
            codecOption = codec
        } else if !selectedContent.isEmpty {
            codecOption = inferCodec(from: selectedContent)
        }
        if let storedSegment = defaults.string(forKey: DefaultsKey.selectedSegment.rawValue), let segment = SegmentOption(rawValue: storedSegment) {
            segmentOption = segment
        }
        if let storedProtocol = defaults.string(forKey: DefaultsKey.selectedProtocol.rawValue), let proto = ProtocolOption(rawValue: storedProtocol) {
            protocolOption = proto
        }
        if let storedUrl = defaults.string(forKey: DefaultsKey.selectedUrl.rawValue) {
            currentURL = storedUrl
        }
        if let muted = defaults.string(forKey: DefaultsKey.audioMuted.rawValue) {
            isMuted = muted != "false"
        }
        if let storedPlayerId = defaults.string(forKey: DefaultsKey.playerId.rawValue) {
            playerId = storedPlayerId
        }
        if let storedPrefer4k = defaults.string(forKey: DefaultsKey.prefer4kNative.rawValue) {
            prefer4kNative = storedPrefer4k == "true"
        }
        if let storedAutoRecovery = defaults.string(forKey: DefaultsKey.autoRecoveryEnabled.rawValue) {
            autoRecoveryEnabled = storedAutoRecovery == "true"
        }
    }

    private func persist(_ key: DefaultsKey, _ value: String) {
        UserDefaults.standard.setValue(value, forKey: key.rawValue)
    }

    private func resolvePlaybackBase(from baseURL: URL) -> URL {
        if let explicit = URL(string: playbackBaseURLString) {
            return explicit
        }
        return baseURL
    }

    private func log(_ message: String) {
        let stamp = ISO8601DateFormatter().string(from: Date())
        let line = "[\(stamp)] \(message)"
        logLines.append(line)
        if logLines.count > 200 {
            logLines.removeFirst(logLines.count - 200)
        }
        print(line)
    }


    private func bindDiagnosticsLogging() {
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
                self?.log("Playback failure: \(value)")
                Task { await self?.sendPlayerMetrics(event: "error", extra: ["player_metrics_error": value]) }
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

        diagnostics.$lastErrorLog
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                self?.log("Error log: \(value)")
            }
            .store(in: &cancellables)

        diagnostics.$lastPlaylistError
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                self?.log("Playlist error: \(value)")
            }
            .store(in: &cancellables)

        diagnostics.$lastAccessLog
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                self?.log("Access log: \(value)")
            }
            .store(in: &cancellables)

        diagnostics.$itemStatus
            .removeDuplicates()
            .sink { [weak self] value in
                self?.log("Item status: \(value)")
            }
            .store(in: &cancellables)

        diagnostics.$waitingReason
            .removeDuplicates()
            .filter { !$0.isEmpty }
            .sink { [weak self] value in
                self?.log("Waiting reason: \(value)")
            }
            .store(in: &cancellables)

    }


    private func bindMetricsReporting() {
        diagnostics.$stallCount
            .removeDuplicates()
            .sink { [weak self] count in
                guard let self else { return }
                if count > self.lastReportedStallCount {
                    self.lastReportedStallCount = count
                    Task { await self.sendPlayerMetrics(event: "stall_start") }
                }
            }
            .store(in: &cancellables)

        diagnostics.$lastStallDurationSeconds
            .removeDuplicates()
            .sink { [weak self] duration in
                guard let self else { return }
                if duration > 0 && duration != self.lastReportedStallDuration {
                    self.lastReportedStallDuration = duration
                    Task {
                        await self.sendPlayerMetrics(event: "stall_end", extra: [
                            "player_metrics_last_stall_time_s": self.roundSeconds(duration)
                        ])
                    }
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
                } else if previous == nil {
                    Task { await self.sendPlayerMetrics(event: "state_change") }
                }
                self.lastReportedState = state
            }
            .store(in: &cancellables)

        diagnostics.$currentTime
            .removeDuplicates()
            .sink { [weak self] currentTime in
                guard let self else { return }
                guard let startAt = self.playbackStartAt else { return }
                if !self.firstFrameReported && currentTime > 0 {
                    let elapsed = self.roundSeconds(Date().timeIntervalSince(startAt))
                    self.videoFirstFrameSeconds = elapsed
                    self.firstFrameReported = true
                    self.diagnostics.markFirstFrameRendered()
                    Task {
                        await self.sendPlayerMetrics(event: "video_first_frame", extra: [
                            "player_metrics_video_first_frame_time_s": elapsed
                        ])
                    }
                }
                if !self.playingReported && currentTime >= 0.1 && self.diagnostics.playbackRate > 0 {
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
    }

    private func startMetricsHeartbeat() {
        metricsHeartbeatTimer?.invalidate()
        metricsHeartbeatTimer = Timer.scheduledTimer(withTimeInterval: metricsHeartbeatSeconds, repeats: true) { [weak self] _ in
            Task {
                self?.evaluateAutoRecoveryIfNeeded()
                await self?.sendPlayerMetrics(event: "heartbeat")
            }
        }
    }

    private func evaluateAutoRecoveryIfNeeded() {
        guard autoRecoveryEnabled else {
            zeroBufferStartedAt = nil
            return
        }
        guard !currentURL.isEmpty else {
            zeroBufferStartedAt = nil
            return
        }
        guard player.timeControlStatus != .paused else {
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
        let zeroDuration = now.timeIntervalSince(zeroBufferStartedAt ?? now)
        if zeroDuration < autoRecoveryThresholdSeconds {
            return
        }
        if let last = lastAutoRecoveryRestartAt,
           now.timeIntervalSince(last) < autoRecoveryCooldownSeconds {
            return
        }

        log("Auto-recovery: restarting playback after \(Int(zeroDuration))s at zero buffer depth")
        restartPlayback(reason: "auto_recovery_zero_buffer")
    }

    private func handleRenditionShift(indicated: Double?, average: Double?) {
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

    private func sendPlayerMetrics(event: String, extra: [String: Any] = [:]) async {
        guard !currentURL.isEmpty else { return }
        guard let baseURL = metricsBaseURL() else { return }
        guard let sessionId = await resolveMetricsSessionId(baseURL: baseURL) else { return }
        let payload = buildMetricsPayload(event: event, extra: extra)
        if payload.isEmpty { return }
        await patchSessionMetrics(sessionId: sessionId, baseURL: baseURL, payload: payload)
    }

    private func metricsBaseURL() -> URL? {
        if let url = URL(string: playbackBaseURLString) {
            return url
        }
        return URL(string: baseURLString)
    }

    private func resolveMetricsSessionId(baseURL: URL) async -> String? {
        let now = Date()
        if let existing = metricsSessionId,
           let lastLookup = metricsLastSessionLookup,
           now.timeIntervalSince(lastLookup) < metricsSessionLookupSeconds {
            return existing
        }
        let sessionsURL = baseURL.appendingPathComponent("api/sessions")
        do {
            let (data, response) = try await URLSession.shared.data(from: sessionsURL)
            if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
                return nil
            }
            guard let json = try JSONSerialization.jsonObject(with: data) as? [[String: Any]] else {
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
        } catch {
            return nil
        }
        return nil
    }

    private func buildMetricsPayload(event: String, extra: [String: Any]) -> [String: Any] {
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
            "player_metrics_position_s": roundSeconds(diagnostics.currentTime),
            "player_metrics_playback_rate": roundMetric(Double(diagnostics.playbackRate)),
            "player_metrics_buffer_depth_s": diagnostics.bufferDepth.map { roundSeconds($0) },
            "player_metrics_buffer_end_s": diagnostics.bufferedEnd.map { roundSeconds($0) },
            "player_metrics_seekable_end_s": diagnostics.seekableEnd.map { roundSeconds($0) },
            "player_metrics_live_edge_s": diagnostics.seekableEnd.map { roundSeconds($0) },
            "player_metrics_live_offset_s": diagnostics.liveOffset.map { roundSeconds($0) },
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
            "player_auto_recovery_enabled": autoRecoveryEnabled,
            "player_metrics_video_bitrate_mbps": mbps(from: diagnostics.indicatedBitrate ?? diagnostics.averageVideoBitrate),
            "player_metrics_network_bitrate_mbps": mbps(from: diagnostics.observedBitrate)
        ]
        extra.forEach { key, value in
            payload[key] = value
        }
        var compact: [String: Any] = [:]
        for (key, value) in payload {
            if let value {
                compact[key] = value
            }
        }
        return compact
    }

    private func patchSessionMetrics(sessionId: String, baseURL: URL, payload: [String: Any]) async {
        let url = baseURL.appendingPathComponent("api/session").appendingPathComponent(sessionId).appendingPathComponent("metrics")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let body: [String: Any] = [
            "set": payload,
            "fields": Array(payload.keys)
        ]
        do {
            request.httpBody = try JSONSerialization.data(withJSONObject: body, options: [])
            let (_, response) = try await URLSession.shared.data(for: request)
            if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
                log("Metrics patch failed: HTTP \(http.statusCode)")
            }
        } catch {
            log("Metrics patch failed: \(error.localizedDescription)")
        }
    }

    private func mbps(from bps: Double?) -> Double? {
        guard let bps, bps > 0 else { return nil }
        return roundMetric(bps / 1_000_000)
    }

    private func formatResolution(width: Double?, height: Double?) -> String? {
        guard let width, let height, width > 0, height > 0 else { return nil }
        return "\(Int(width))x\(Int(height))"
    }

    private func roundSeconds(_ value: Double) -> Double {
        return (value * 1000).rounded() / 1000
    }

    private func roundMetric(_ value: Double) -> Double {
        return (value * 100).rounded() / 100
    }


    private func probeURL(_ url: URL) async {
        do {
            var request = URLRequest(url: url)
            request.cachePolicy = .reloadIgnoringLocalCacheData
            let (data, response) = try await URLSession.shared.data(for: request)
            if let http = response as? HTTPURLResponse {
                log("Probe HTTP \(http.statusCode) finalURL=\(http.url?.absoluteString ?? "") contentType=\(http.value(forHTTPHeaderField: "Content-Type") ?? "")")
            }
            if let text = String(data: data.prefix(4096), encoding: .utf8) {
                let lines = text.split(separator: "\n").prefix(5).joined(separator: "\n")
                log("Probe body (first lines): \(lines)")
            }
        } catch {
            log("Probe failed: \(error.localizedDescription)")
        }
    }

    private func startPlaylistMonitor() {
        playlistMonitorTask?.cancel()
        playlistMonitorTask = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                await self.fetchPlaylistSnapshot()
                try? await Task.sleep(nanoseconds: 3_000_000_000)
            }
        }
    }

    private func fetchPlaylistSnapshot() async {
        guard let url = URL(string: currentURL) else { return }
        let masterText = await fetchAndLogPlaylist(url, label: "Playlist")

        if let masterText {
            await fetchChildPlaylists(fromMaster: masterText, baseURL: url)
        }

        let segmentURI = diagnostics.lastSegmentURI
        if segmentURI.contains(".m3u8"), let segURL = URL(string: segmentURI) {
            _ = await fetchAndLogPlaylist(segURL, label: "Segment Playlist (from access log)")
        }
    }

    private func fetchAndLogPlaylist(_ url: URL, label: String) async -> String? {
        do {
            let requestURL = appendPlayerId(to: url)
            var request = URLRequest(url: requestURL)
            request.cachePolicy = .reloadIgnoringLocalCacheData
            applyPlayerHeaders(to: &request)
            let (data, response) = try await URLSession.shared.data(for: request)
            if let http = response as? HTTPURLResponse {
                log("\(label) HTTP \(http.statusCode) url=\(http.url?.absoluteString ?? "") contentType=\(http.value(forHTTPHeaderField: "Content-Type") ?? "")")
            }
            if let text = String(data: data, encoding: .utf8) {
                if label == "Playlist" {
                    await probeByterangeSegments(playlistURL: requestURL, playlistText: text)
                }
                return text
            }
        } catch {
            log("\(label) fetch failed: \(error.localizedDescription)")
        }
        return nil
    }

    private func fetchChildPlaylists(fromMaster master: String, baseURL: URL) async {
        let lines = master.split(separator: "\n").map { String($0) }
        var audioURI: String?
        var videoURI: String?
        var expectNextVideoURI = false

        for line in lines {
            if line.hasPrefix("#EXT-X-MEDIA:") {
                if let uri = extractAttribute(line: line, key: "URI") {
                    audioURI = uri
                }
            } else if line.hasPrefix("#EXT-X-STREAM-INF:") {
                expectNextVideoURI = true
            } else if expectNextVideoURI && !line.hasPrefix("#") && !line.trimmingCharacters(in: .whitespaces).isEmpty {
                videoURI = line
                expectNextVideoURI = false
            }
        }

        if let audioURI, let audioURL = URL(string: audioURI, relativeTo: baseURL) {
            _ = await fetchAndLogPlaylist(audioURL, label: "Audio Playlist")
        }
        if let videoURI, let videoURL = URL(string: videoURI, relativeTo: baseURL) {
            _ = await fetchAndLogPlaylist(videoURL, label: "Video Playlist")
        }
    }

    private func probeByterangeSegments(playlistURL: URL, playlistText: String) async {
        let lines = playlistText.split(separator: "\n").map { String($0) }
        var mapURI: String?
        var nextByterange: String?
        var nextSegmentURI: String?

        for line in lines {
            if line.hasPrefix("#EXT-X-MAP:") {
                if let uri = extractAttribute(line: line, key: "URI") {
                    mapURI = uri
                }
            } else if line.hasPrefix("#EXT-X-BYTERANGE:") {
                nextByterange = line.replacingOccurrences(of: "#EXT-X-BYTERANGE:", with: "")
            } else if !line.hasPrefix("#") && !line.trimmingCharacters(in: .whitespaces).isEmpty {
                nextSegmentURI = line
                break
            }
        }

        if let mapURI, let mapURL = URL(string: mapURI, relativeTo: playlistURL) {
            await logRangeProbe(url: mapURL, byterange: nil, label: "Init map")
        }
        if let nextSegmentURI, let segmentURL = URL(string: nextSegmentURI, relativeTo: playlistURL) {
            await logRangeProbe(url: segmentURL, byterange: nextByterange, label: "Segment")
        }
    }

    private func extractAttribute(line: String, key: String) -> String? {
        let pattern = "\(key)=\""
        guard let start = line.range(of: pattern) else { return nil }
        let substring = line[start.upperBound...]
        if let end = substring.firstIndex(of: "\"") {
            return String(substring[..<end])
        }
        return nil
    }

    private func logRangeProbe(url: URL, byterange: String?, label: String) async {
        do {
            let requestURL = appendPlayerId(to: url)
            var request = URLRequest(url: requestURL)
            request.cachePolicy = .reloadIgnoringLocalCacheData
            applyPlayerHeaders(to: &request)
            if let byterange {
                let parts = byterange.split(separator: "@").map(String.init)
                if let length = Int(parts.first ?? ""), let offset = Int(parts.dropFirst().first ?? "") {
                    let end = offset + length - 1
                    request.setValue("bytes=\(offset)-\(end)", forHTTPHeaderField: "Range")
                    log("\(label) probe range bytes=\(offset)-\(end) url=\(requestURL.absoluteString)")
                }
            }
            let (_, response) = try await URLSession.shared.data(for: request)
            if let http = response as? HTTPURLResponse {
                let contentRange = http.value(forHTTPHeaderField: "Content-Range") ?? ""
                let acceptRanges = http.value(forHTTPHeaderField: "Accept-Ranges") ?? ""
                let contentType = http.value(forHTTPHeaderField: "Content-Type") ?? ""
                log("\(label) probe HTTP \(http.statusCode) contentType=\(contentType) contentRange=\(contentRange) acceptRanges=\(acceptRanges)")
            }
        } catch {
            log("\(label) probe failed: \(error.localizedDescription)")
        }
    }

    private func appendPlayerId(to url: URL) -> URL {
        if !includePlayerIdInURL || primePlayback {
            return url
        }
        if url.port == 40000 {
            return url
        }
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: true) else {
            return url
        }
        var items = components.queryItems ?? []
        if !items.contains(where: { $0.name == "player_id" }) {
            items.append(URLQueryItem(name: "player_id", value: playerId))
        }
        components.queryItems = items
        return components.url ?? url
    }

    private func removePlayerId(from url: URL) -> URL {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: true) else {
            return url
        }
        if let items = components.queryItems {
            let filtered = items.filter { $0.name != "player_id" }
            components.queryItems = filtered.isEmpty ? nil : filtered
        }
        return components.url ?? url
    }

    private func applyPlayerHeaders(to request: inout URLRequest) {
        request.setValue(playerId, forHTTPHeaderField: "Player-ID")
        request.setValue(playerId, forHTTPHeaderField: "X-Playback-Session-Id")
    }

    private func formatRequestLine(url: URL) -> String {
        let host = url.host ?? ""
        let port = url.port.map(String.init) ?? ""
        let path = url.path
        let query = url.query ?? ""
        return "Master request: url=\(url.absoluteString) host=\(host) port=\(port) path=\(path) query=\(query)"
    }

    private func isMasterPlaylistURL(_ url: URL) -> Bool {
        let path = url.path.lowercased()
        return path.contains("master_") && path.hasSuffix(".m3u8")
    }

    private func preflightMasterAndStart(url: URL, contentName: String, codecFallback: Bool) async {
        var lastStatus: Int?
        var finalURL: URL = url
        for attempt in 1...masterPreflightMaxAttempts {
            var request = URLRequest(url: url)
            request.cachePolicy = .reloadIgnoringLocalCacheData
            do {
                let (_, response) = try await URLSession.shared.data(for: request)
                if let http = response as? HTTPURLResponse {
                    lastStatus = http.statusCode
                    finalURL = http.url ?? url
                    log("Master preflight HTTP \(http.statusCode) attempt \(attempt)/\(masterPreflightMaxAttempts) url=\(finalURL.absoluteString)")
                    if http.statusCode == 429 {
                        let delayMs = retryDelayMs(from: http) ?? masterPreflightDefaultDelayMs
                        log("Master preflight retrying after \(delayMs)ms")
                        try? await Task.sleep(nanoseconds: delayMs * 1_000_000)
                        continue
                    }
                }
                break
            } catch {
                log("Master preflight failed: \(error.localizedDescription)")
                break
            }
        }
        if lastStatus == 429 {
            log("Master preflight giving up after \(masterPreflightMaxAttempts) attempts")
            return
        }
        if finalURL.absoluteString != url.absoluteString {
            log("Master preflight final URL: \(finalURL.absoluteString)")
        }
        startPlaybackNow(url: finalURL, contentName: contentName, codecFallback: codecFallback)
    }

    private func retryDelayMs(from response: HTTPURLResponse) -> UInt64? {
        if let retry = response.value(forHTTPHeaderField: "Retry-After"),
           let seconds = Double(retry.trimmingCharacters(in: .whitespaces)) {
            return UInt64(seconds * 1000)
        }
        return nil
    }

    private func apply4kPreference(to item: AVPlayerItem) {
        if prefer4kNative {
            item.preferredPeakBitRate = 0
            if #available(iOS 15.0, tvOS 15.0, *) {
                item.preferredMaximumResolution = CGSize(width: 3840, height: 2160)
            }
        } else if #available(iOS 15.0, tvOS 15.0, *) {
            item.preferredMaximumResolution = .zero
        }
    }

    private func logPlaylistBodies(for masterURL: URL) async {
        if let masterResult = await fetchAndLogPlaylistBody(masterURL, label: "Master Playlist") {
            await logPlayerIDPresence(in: masterResult.text, label: "Master Playlist")
            let baseURL = masterResult.finalURL
            let (audioURI, videoURI) = parsePlaylistReferences(from: masterResult.text)
            if let audioURI, let audioURL = URL(string: audioURI, relativeTo: baseURL) {
                if let audioResult = await fetchAndLogPlaylistBody(audioURL, label: "Audio Playlist") {
                    await logPlayerIDPresence(in: audioResult.text, label: "Audio Playlist")
                }
            }
            if let videoURI, let videoURL = URL(string: videoURI, relativeTo: baseURL) {
                if let videoResult = await fetchAndLogPlaylistBody(videoURL, label: "Video Playlist") {
                    await logPlayerIDPresence(in: videoResult.text, label: "Video Playlist")
                }
            }
        }
    }

    private func fetchAndLogPlaylistBody(_ url: URL, label: String) async -> (text: String, finalURL: URL)? {
        do {
            var request = URLRequest(url: url)
            request.cachePolicy = .reloadIgnoringLocalCacheData
            let (data, response) = try await URLSession.shared.data(for: request)
            let finalURL = response.url ?? url
            if let http = response as? HTTPURLResponse {
                log("\(label) HTTP \(http.statusCode) url=\(finalURL.absoluteString) contentType=\(http.value(forHTTPHeaderField: "Content-Type") ?? "")")
            }
            guard let text = String(data: data, encoding: .utf8) else {
                log("\(label) body: <non-utf8 or empty>")
                return nil
            }
            log("\(label) body (full up to 200 lines): \(limitLines(text, maxLines: 200))")
            return (text, finalURL)
        } catch {
            log("\(label) fetch failed: \(error.localizedDescription)")
            return nil
        }
    }

    private func parsePlaylistReferences(from master: String) -> (audio: String?, video: String?) {
        let lines = master.split(separator: "\n").map { String($0) }
        var audioURI: String?
        var videoURI: String?
        var expectNextVideoURI = false

        for line in lines {
            if line.hasPrefix("#EXT-X-MEDIA:") {
                if let uri = extractAttribute(line: line, key: "URI") {
                    audioURI = uri
                }
            } else if line.hasPrefix("#EXT-X-STREAM-INF:") {
                expectNextVideoURI = true
            } else if expectNextVideoURI && !line.hasPrefix("#") && !line.trimmingCharacters(in: .whitespaces).isEmpty {
                videoURI = line
                expectNextVideoURI = false
            }
        }
        return (audioURI, videoURI)
    }

    private func logPlayerIDPresence(in playlist: String, label: String) async {
        let contains = playlist.contains("player_id=")
        log("\(label) contains player_id=\(contains ? "true" : "false")")
    }

    private func limitLines(_ text: String, maxLines: Int) -> String {
        let lines = text.split(separator: "\n")
        if lines.count <= maxLines {
            return lines.joined(separator: "\n")
        }
        let prefix = lines.prefix(maxLines).joined(separator: "\n")
        return prefix + "\n... (truncated)"
    }
}
