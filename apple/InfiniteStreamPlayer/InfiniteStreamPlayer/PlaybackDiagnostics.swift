import AVFoundation
import Combine
import Foundation

fileprivate let consoleTimestampFormatter: DateFormatter = {
    let f = DateFormatter()
    f.dateFormat = "HH:mm:ss.SSS"
    f.locale = Locale(identifier: "en_US_POSIX")
    return f
}()

// Shadows Swift.print for this target — every print() call in the module
// gets a [HH:mm:ss.SSS] prefix in the Xcode console.
func print(_ items: Any..., separator: String = " ", terminator: String = "\n") {
    let stamp = consoleTimestampFormatter.string(from: Date())
    let body = items.map { String(describing: $0) }.joined(separator: separator)
    Swift.print("[\(stamp)] \(body)", terminator: terminator)
}

struct MetricSample: Identifiable {
    let id = UUID()
    let timestamp: Date
    let value: Double
}

/// Discrete time-jump event published from PlaybackDiagnostics for the
/// ViewModel to forward as a `timejump` metrics event. Fires on HLS
/// discontinuity boundaries, live-edge catchup seeks, and explicit seeks.
struct TimeJumpEvent {
    let from: Double
    let to: Double
    let origin: String
    let at: Date
}

final class PlaybackDiagnostics: ObservableObject {
    /// Discrete time-jump events. Subscribers (ViewModel) receive each
    /// jump exactly once; not a Published property because we don't want
    /// the deduplication-on-equal-values that an @Published+sink would
    /// give us if two jumps happen to land at the same `to`.
    let timeJumpSubject = PassthroughSubject<TimeJumpEvent, Never>()

    @Published var state: String = "Idle"
    @Published var currentTime: Double = 0
    @Published var bufferedEnd: Double?
    @Published var bufferDepth: Double?
    @Published var seekableEnd: Double?
    @Published var liveOffset: Double?
    @Published var windowDuration: Double?
    @Published var windowSlack: Double?
    @Published var displayWidth: Double?
    @Published var displayHeight: Double?
    @Published var videoWidth: Double?
    @Published var videoHeight: Double?
    @Published var playbackRate: Float = 0
    @Published var likelyToKeepUp: Bool = false
    @Published var bufferEmpty: Bool = false
    @Published var stallCount: Int = 0
    @Published var stallTimeSeconds: Double = 0
    @Published var lastStallDurationSeconds: Double = 0
    @Published var observedBitrate: Double?
    @Published var indicatedBitrate: Double?
    @Published var avgNetworkBitrate: Double?
    @Published var networkBitrate: Double?
    @Published var averageVideoBitrate: Double?
    @Published var droppedVideoFrames: Double?
    @Published var estimatedDisplayedFrames: Double?
    @Published var nominalFrameRate: Double?
    @Published var loopCountPlayer: Int = 0
    @Published var lastSegmentURI: String = ""
    @Published var lastError: String = ""
    @Published var itemStatus: String = "Unknown"
    @Published var itemError: String = ""
    @Published var lastFailure: String = ""
    @Published var lastErrorLog: String = ""
    @Published var lastPlaylistError: String = ""
    @Published var lastAccessLog: String = ""
    @Published var waitingReason: String = ""
    @Published var throughputSamples: [MetricSample] = []
    @Published var variantBitrateSamples: [MetricSample] = []
    @Published var observedBitrateSamples: [MetricSample] = []
    @Published var indicatedBitrateSamples: [MetricSample] = []
    @Published var averageVideoBitrateSamples: [MetricSample] = []
    @Published var playerEstimateSamples: [MetricSample] = []
    @Published var bufferDepthSamples: [MetricSample] = []
    @Published var liveOffsetSamples: [MetricSample] = []

    @Published var frozenDetected: Bool = false
    @Published var segmentStallDetected: Bool = false

    private var timeObserverToken: Any?
    private var bitrateSampleTimer: Timer?
    private var cancellables: Set<AnyCancellable> = []
    private weak var player: AVPlayer?
    private var lastBufferSampleAt: Date?
    private var lastPlayerSampleAt: Date?
    private var stallStartAt: Date?
    /// Set when AVPlayerItemPlaybackStalled fires (an unexpected
    /// mid-play rebuffer); cleared when timeControlStatus returns to
    /// .playing. Used to pick "stalled" vs "buffering" for the
    /// PLAYERSTATE lane — the latter being any other waiting state
    /// (initial pre-roll, post-seek refill, etc.).
    private var isStalled: Bool = false
    private var hasRenderedFirstFrame: Bool = false
    private var lastAdvancingTime: Double = 0
    private var lastAdvancingAt: Date?
    private var frozenLoggedAt: Date?
    private var lastObservedSegmentSequence: Int?
    private var maxObservedSegmentSequence: Int?
    private let maxSeriesSamples = 600
    private var variantDwellSeconds: [String: Double] = [:]  // variant label -> total seconds (prior + current)
    private var priorVariantDwellSeconds: [String: Double] = [:]  // accumulated across restarts
    private var priorDroppedVideoFrames: Double = 0
    private var priorEstimatedDisplayedFrames: Double = 0
    private var knownVariants: Set<String> = []
    private var lastVariantSummaryAt: Date?
    private var lastAccessLogEventCount: Int = 0
    private var lastVariantDwellTotal: Double = 0
    private var lastVariantDwellChangeAt: Date?
    private let segmentStallThresholdSeconds: TimeInterval = 30
    private let seriesWindowSeconds: TimeInterval = 300
    private let networkWindowSeconds: TimeInterval = 6
    private var networkByteSamples: [(timestamp: Date, bytes: Int64, xfer: Double)] = []
    // Rolling samples for the proxy-based bitrate. `flowingMs` is cumulative
    // ms during which a chunk arrived within the last 100 ms (from RequestTracker).
    // Using flowingMs as the denominator gives a transfer-time rate that tracks
    // the actual wire delivery rate instead of being diluted by idle gaps
    // between segment requests.
    private var wireByteSamples: [(timestamp: Date, bytes: Int64, flowingMs: Double)] = []
    private let wireWindowSeconds: TimeInterval = 6
    private let wireMinFlowingSeconds: TimeInterval = 0.3
    private static let bitrateSampleFormatter: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    func bind(to player: AVPlayer) {
        self.player = player
        observePlayer(player)
        startBitrateSampleTimer()
    }

    deinit {
        bitrateSampleTimer?.invalidate()
    }

    private func startBitrateSampleTimer() {
        bitrateSampleTimer?.invalidate()
        bitrateSampleTimer = Timer.scheduledTimer(withTimeInterval: 0.1, repeats: true) { [weak self] _ in
            self?.emitBitrateSample()
        }
    }

    private func emitBitrateSample() {
        guard let item = player?.currentItem,
              let events = item.accessLog()?.events, !events.isEmpty else { return }
        var totalBytes: Int64 = 0
        var totalXfer: Double = 0
        for ev in events {
            if ev.numberOfBytesTransferred > 0 { totalBytes += ev.numberOfBytesTransferred }
            if ev.transferDuration > 0 { totalXfer += ev.transferDuration }
        }
        let observedBps = events.last?.observedBitrate ?? 0
        let now = Date()
        let ts = Self.bitrateSampleFormatter.string(from: now)
        let windowBps = avgNetworkBitrate ?? -1
        let approxBps = computeApproxActiveBitrate(now: now) ?? -1
        let wire = RequestTracker.shared.snapshot(now: now)
        let wireLastChunkMsAgo = wire.wireLastChunkMsAgo ?? -1
        updateNetworkWireBitrate(wire: wire, now: now)
        // BITRATE_SAMPLE logging disabled — re-enable when offline analysis needed.
        // let line = String(
        //     format: "[BITRATE_SAMPLE] {\"t\":\"%@\",\"bytes\":%lld,\"xfer\":%.3f,\"observed_bps\":%.0f,\"network_window_bps\":%.0f,\"approx_xfer_bps\":%.0f,\"wire_bytes\":%lld,\"wire_active_ms\":%.1f,\"wire_flowing_ms\":%.1f,\"wire_inflight\":%d,\"wire_last_chunk_ms_ago\":%.1f,\"network_wire_bps\":%.0f}",
        //     ts, totalBytes, totalXfer, observedBps, windowBps, approxBps,
        //     wire.wireBytesTotal, wire.wireActiveMsTotal, wire.wireFlowingMsTotal,
        //     wire.wireInflightCount, wireLastChunkMsAgo,
        //     networkBitrate ?? -1
        // )
        // Swift.print(line)
        _ = (ts, totalBytes, totalXfer, observedBps, windowBps, approxBps, wireLastChunkMsAgo)
    }

    // Rolling wire-bytes bitrate: like `avgNetworkBitrate`, but fed from
    // the LocalHTTPProxy's per-chunk accounting. Report nil when there are no
    // outstanding requests AND no new bytes arrived since the previous sample —
    // this gives the chart a clean gap during idle gaps between segment fetches.
    private func updateNetworkWireBitrate(wire: RequestTracker.Snapshot, now: Date) {
        wireByteSamples.append((timestamp: now, bytes: wire.wireBytesTotal, flowingMs: wire.wireFlowingMsTotal))
        let cutoff = now.addingTimeInterval(-(wireWindowSeconds + 1))
        wireByteSamples.removeAll { $0.timestamp < cutoff }
        // Idle gate: no requests in flight AND last chunk arrived > 250ms ago.
        // Covers the ~200ms gap between LL-HLS partials but goes nil promptly
        // when the player truly stops fetching (buffer full) — otherwise the
        // flowing-ms denominator would trail a stale rate for up to 6s as
        // the window rolls off.
        if wire.wireInflightCount == 0,
           (wire.wireLastChunkMsAgo ?? .infinity) > 250 {
            networkBitrate = nil
            return
        }
        let windowStart = now.addingTimeInterval(-wireWindowSeconds)
        guard let oldest = wireByteSamples.first(where: { $0.timestamp >= windowStart })
                ?? wireByteSamples.first,
              oldest.timestamp < now else {
            networkBitrate = nil
            return
        }
        // Transfer-time denominator: count only ms during which bytes were
        // actually flowing on the wire. This matches the shaper limit during
        // bursty fetches instead of diluting by idle gaps between segments.
        let flowingSec = max(0, (wire.wireFlowingMsTotal - oldest.flowingMs) / 1000.0)
        let dBytes = Double(max(0, wire.wireBytesTotal - oldest.bytes))
        guard flowingSec >= wireMinFlowingSeconds, dBytes > 0 else {
            networkBitrate = nil
            return
        }
        networkBitrate = dBytes * 8.0 / flowingSec
    }

    // Approximate "active transfer" rate: sum wall time of 100ms sub-intervals
    // where cumulative bytes grew (i.e. data was flowing). Denominator is that
    // active time only, so idle gaps between segments don't dilute the rate.
    // Result tends to match the TCP burst delivery rate rather than the
    // sustained shaper rate. Returns nil while warming up.
    private func computeApproxActiveBitrate(now: Date) -> Double? {
        let windowStart = now.addingTimeInterval(-networkWindowSeconds)
        let recent = networkByteSamples.filter { $0.timestamp >= windowStart }
        guard recent.count >= 2, let oldest = recent.first, let newest = recent.last,
              newest.timestamp > oldest.timestamp else {
            return nil
        }
        let wall = newest.timestamp.timeIntervalSince(oldest.timestamp)
        let dBytes = Double(max(0, newest.bytes - oldest.bytes))
        if wall >= networkWindowSeconds && dBytes == 0 { return 0 }
        guard wall >= 1.0 else { return nil }
        var activeTime: TimeInterval = 0
        for i in 1..<recent.count {
            let dt = recent[i].timestamp.timeIntervalSince(recent[i-1].timestamp)
            let db = recent[i].bytes - recent[i-1].bytes
            if db > 0 { activeTime += dt }
        }
        guard activeTime >= 0.1 else { return 0 }
        return dBytes * 8.0 / activeTime
    }

    /// Call before replacing the player item to preserve cumulative stats across restarts.
    func snapshotForRestart() {
        // Accumulate variant dwell times
        for (label, seconds) in variantDwellSeconds {
            priorVariantDwellSeconds[label, default: 0] = seconds
        }
        // Accumulate frame counters
        priorDroppedVideoFrames = droppedVideoFrames ?? 0
        priorEstimatedDisplayedFrames = estimatedDisplayedFrames ?? 0
    }

    func reset() {
        state = "idle"
        currentTime = 0
        bufferedEnd = nil
        bufferDepth = nil
        seekableEnd = nil
        liveOffset = nil
        displayWidth = nil
        displayHeight = nil
        videoWidth = nil
        videoHeight = nil
        playbackRate = 0
        likelyToKeepUp = false
        bufferEmpty = false
        stallCount = 0
        stallTimeSeconds = 0
        lastStallDurationSeconds = 0
        observedBitrate = nil
        indicatedBitrate = nil
        avgNetworkBitrate = nil
        networkByteSamples = []
        networkBitrate = nil
        wireByteSamples = []
        averageVideoBitrate = nil
        droppedVideoFrames = nil
        estimatedDisplayedFrames = nil
        nominalFrameRate = nil
        loopCountPlayer = 0
        lastSegmentURI = ""
        lastError = ""
        itemStatus = "Unknown"
        itemError = ""
        lastFailure = ""
        lastErrorLog = ""
        lastPlaylistError = ""
        lastAccessLog = ""
        waitingReason = ""
        throughputSamples = []
        variantBitrateSamples = []
        observedBitrateSamples = []
        indicatedBitrateSamples = []
        averageVideoBitrateSamples = []
        playerEstimateSamples = []
        bufferDepthSamples = []
        liveOffsetSamples = []
        lastBufferSampleAt = nil
        lastPlayerSampleAt = nil
        stallStartAt = nil
        isStalled = false
        hasRenderedFirstFrame = false
        lastObservedSegmentSequence = nil
        maxObservedSegmentSequence = nil
        frozenDetected = false
        lastAdvancingTime = 0
        lastAdvancingAt = nil
        frozenLoggedAt = nil
        // Reset current-item tracking but keep prior accumulators
        variantDwellSeconds = priorVariantDwellSeconds
        lastVariantSummaryAt = nil
        lastAccessLogEventCount = 0
        segmentStallDetected = false
        lastVariantDwellTotal = priorVariantDwellSeconds.values.reduce(0, +)
        lastVariantDwellChangeAt = nil
        // Don't clear: priorVariantDwellSeconds, priorDroppedVideoFrames,
        // priorEstimatedDisplayedFrames, knownVariants
    }

    func markFirstFrameRendered() {
        hasRenderedFirstFrame = true
    }

    private func observePlayer(_ player: AVPlayer) {
        cancellables.removeAll()

        player.publisher(for: \.timeControlStatus)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] status in
                guard let self else { return }
                let prev = self.state
                let rate = self.player?.rate ?? 0
                switch status {
                case .paused:
                    self.state = "paused"
                case .waitingToPlayAtSpecifiedRate:
                    // Distinguish user-induced refill (initial pre-roll,
                    // post-seek, post-play after a long pause) from an
                    // unexpected mid-play rebuffer. Only the
                    // AVPlayerItemPlaybackStalled notification (handled
                    // below) signals the unexpected case; the
                    // isStalled flag persists from there until we
                    // return to .playing. Don't call
                    // startStallIfNeeded() here — that would conflate
                    // pre-roll with stalls.
                    self.state = self.isStalled ? "stalled" : "buffering"
                case .playing:
                    self.state = "playing"
                    self.isStalled = false
                    self.endStallIfNeeded()
                @unknown default: self.state = "unknown"
                }
                print("[STATE] \(prev) -> \(self.state) rate=\(rate) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
            }
            .store(in: &cancellables)

        player.publisher(for: \.reasonForWaitingToPlay)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] reason in
                guard let self else { return }
                if let reason = reason {
                    self.waitingReason = reason.rawValue
                    print("[WAITING] reason=\(reason.rawValue) state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
                } else {
                    if !self.waitingReason.isEmpty {
                        print("[WAITING] reason=cleared state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
                    }
                    self.waitingReason = ""
                }
            }
            .store(in: &cancellables)

        player.publisher(for: \.rate)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] rate in
                guard let self else { return }
                let prev = self.playbackRate
                self.playbackRate = rate
                if abs(rate - prev) > 0.001 {
                    print("[RATE] \(prev) -> \(rate) state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
                }
            }
            .store(in: &cancellables)

        // Closest public signal to "HLS discontinuity handled" — also fires on
        // live-edge catchup seeks and explicit seeks. Disambiguate by context
        // in the snapshot (loadedTimeRanges often splits around a discontinuity,
        // and the access log will show the segment straddling the boundary).
        NotificationCenter.default.publisher(for: .AVPlayerItemTimeJumped)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] notification in
                guard let self else { return }
                let item = notification.object as? AVPlayerItem
                let original = (notification.userInfo?[AVPlayerItem.timeJumpedOriginatingParticipantKey] as? String) ?? "unknown"
                let fromTime = self.currentTime
                let newTime = item?.currentTime().seconds ?? self.currentTime
                print("[TIMEJUMP] origin=\(original) time=\(String(format: "%.2f", newTime)) state=\(self.state) \(self.playbackSnapshot())")
                // Publish for ViewModel to forward as a metrics event.
                self.timeJumpSubject.send(TimeJumpEvent(
                    from: fromTime,
                    to: newTime,
                    origin: original,
                    at: Date()
                ))
            }
            .store(in: &cancellables)

        NotificationCenter.default.publisher(for: .AVPlayerItemPlaybackStalled)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in
                guard let self else { return }
                print("[STALL] state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
                // Mark the unexpected-rebuffer flag so the
                // timeControlStatus sink can distinguish stalled vs
                // pre-roll buffering on the next state read.
                self.isStalled = true
                if self.state == "buffering" { self.state = "stalled" }
                self.startStallIfNeeded()
            }
            .store(in: &cancellables)

        NotificationCenter.default.publisher(for: .AVPlayerItemFailedToPlayToEndTime)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] notification in
                guard let self else { return }
                if let error = notification.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? Error {
                    print("[FAILURE] \(error.localizedDescription) state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
                    self.lastFailure = error.localizedDescription
                } else {
                    print("[FAILURE] Playback failed state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
                    self.lastFailure = "Playback failed"
                }
            }
            .store(in: &cancellables)

        NotificationCenter.default.publisher(for: .AVPlayerItemNewAccessLogEntry)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] notification in
                guard let item = notification.object as? AVPlayerItem else { return }
                self?.updateAccessLog(from: item)
            }
            .store(in: &cancellables)

        NotificationCenter.default.publisher(for: .AVPlayerItemNewErrorLogEntry)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] notification in
                guard let item = notification.object as? AVPlayerItem else { return }
                self?.updateErrorLog(from: item)
            }
            .store(in: &cancellables)

        addTimeObserver(to: player)
        observeCurrentItem()
    }

    private func addTimeObserver(to player: AVPlayer) {
        if let token = timeObserverToken {
            player.removeTimeObserver(token)
            timeObserverToken = nil
        }

        let interval = CMTime(seconds: 0.5, preferredTimescale: 600)
        timeObserverToken = player.addPeriodicTimeObserver(forInterval: interval, queue: .main) { [weak self] time in
            guard let self else { return }
            self.currentTime = time.seconds
            self.updateBufferMetrics()
            self.updateItemState()
            self.checkFrozenState()
            self.periodicVariantSummary()
        }
    }

    private func updateItemState() {
        guard let item = player?.currentItem else { return }
        likelyToKeepUp = item.isPlaybackLikelyToKeepUp
        bufferEmpty = item.isPlaybackBufferEmpty
        let presentation = item.presentationSize
        if presentation.width > 0 && presentation.height > 0 {
            videoWidth = presentation.width
            videoHeight = presentation.height
        } else {
            videoWidth = nil
            videoHeight = nil
        }
        if let error = item.error {
            lastError = describeError(error)
        }
        if let itemError = item.error {
            self.itemError = describeError(itemError)
        }
        // Load nominal frame rate asynchronously using modern API
        if nominalFrameRate == nil || (nominalFrameRate ?? 0) <= 0 {
            Task { @MainActor [weak self] in
                guard let self, let item = self.player?.currentItem else { return }
                if let tracks = try? await item.asset.loadTracks(withMediaType: .video),
                   let track = tracks.first {
                    let fps = try? await track.load(.nominalFrameRate)
                    if let fps, fps > 0 {
                        self.nominalFrameRate = Double(fps)
                    }
                }
            }
        }
    }

    func updateDisplaySize(_ size: CGSize) {
        guard size.width > 0 && size.height > 0 else {
            displayWidth = nil
            displayHeight = nil
            return
        }
        displayWidth = size.width
        displayHeight = size.height
    }

    private func updateBufferMetrics() {
        guard let item = player?.currentItem else { return }
        let ranges = item.loadedTimeRanges
        if item.isPlaybackBufferEmpty {
            bufferDepth = 0
            if let range = ranges.last?.timeRangeValue {
                bufferedEnd = range.start.seconds + range.duration.seconds
            }
        } else if let range = ranges.last?.timeRangeValue {
            let end = range.start.seconds + range.duration.seconds
            bufferedEnd = end
            bufferDepth = max(0, end - currentTime)
        } else {
            bufferedEnd = nil
            bufferDepth = 0
        }
        if let depth = bufferDepth {
            let now = Date()
            if lastBufferSampleAt == nil || now.timeIntervalSince(lastBufferSampleAt ?? now) >= 1.0 {
                appendSample(MetricSample(timestamp: now, value: depth), to: &bufferDepthSamples)
                lastBufferSampleAt = now
            }
        }
        if let liveRange = item.seekableTimeRanges.last?.timeRangeValue {
            let liveEdge = liveRange.start.seconds + liveRange.duration.seconds
            let window = liveRange.duration.seconds
            seekableEnd = liveEdge
            liveOffset = max(0, liveEdge - currentTime)
            windowDuration = window
            // Slack = how much media time until currentTime falls off the
            // oldest edge of the server's sliding window. Near zero / negative
            // means the next segment the player asks for has likely rotated
            // out, producing "-12642 No matching mediaFile found from playlist".
            windowSlack = window - (liveOffset ?? 0)
        } else {
            seekableEnd = nil
            liveOffset = nil
            windowDuration = nil
            windowSlack = nil
        }
        samplePlayerMetrics(now: Date())
    }

    private func startStallIfNeeded() {
        guard hasRenderedFirstFrame else {
            return
        }
        if stallStartAt != nil {
            return
        }
        stallStartAt = Date()
        stallCount += 1
    }

    private func checkFrozenState() {
        let now = Date()
        let time = currentTime
        if abs(time - lastAdvancingTime) > 0.01 {
            lastAdvancingTime = time
            lastAdvancingAt = now
            if frozenDetected {
                frozenDetected = false
            }
            return
        }
        guard state != "Idle" && state != "Paused" else {
            lastAdvancingAt = now
            return
        }
        guard let advancedAt = lastAdvancingAt else {
            lastAdvancingAt = now
            return
        }
        let stalledFor = now.timeIntervalSince(advancedAt)
        if stalledFor >= 3.0 && !frozenDetected {
            frozenDetected = true
            let item = player?.currentItem
            let bufEmpty = item?.isPlaybackBufferEmpty ?? false
            let keepUp = item?.isPlaybackLikelyToKeepUp ?? false
            let rate = player?.rate ?? 0
            let status = player?.timeControlStatus.rawValue ?? -1
            let reason = player?.reasonForWaitingToPlay?.rawValue ?? "none"
            let ranges = item?.loadedTimeRanges.map { $0.timeRangeValue }
            let rangeDesc = ranges?.map { String(format: "%.1f-%.1f", $0.start.seconds, $0.start.seconds + $0.duration.seconds) }.joined(separator: ", ") ?? "none"
            print("[FROZEN] time=\(String(format: "%.2f", time)) stalled_for=\(String(format: "%.1fs", stalledFor)) state=\(state) rate=\(rate) timeControlStatus=\(status) waitingReason=\(reason) bufferEmpty=\(bufEmpty) likelyToKeepUp=\(keepUp) loadedRanges=[\(rangeDesc)]")
        }
        if stalledFor >= 3.0 && frozenDetected {
            // Log every 3 seconds while frozen
            if frozenLoggedAt == nil || now.timeIntervalSince(frozenLoggedAt!) >= 3.0 {
                frozenLoggedAt = now
                print("[FROZEN] still frozen at time=\(String(format: "%.2f", time)) for \(String(format: "%.0fs", stalledFor)) state=\(state) rate=\(player?.rate ?? 0)")
            }
        }
    }

    private func endStallIfNeeded() {
        guard let start = stallStartAt else { return }
        let duration = max(0, Date().timeIntervalSince(start))
        lastStallDurationSeconds = duration
        stallTimeSeconds += duration
        stallStartAt = nil
    }

    private func refreshLiveAccessLogMetrics(from item: AVPlayerItem) {
        guard let event = item.accessLog()?.events.last else { return }
        observedBitrate = event.observedBitrate
        indicatedBitrate = event.indicatedBitrate
        averageVideoBitrate = event.averageVideoBitrate
        // Segment identity/sequence is tracked in RequestTracker (the proxy
        // sees every segment fetch; AVPlayer's access log URIs are playlists).
        // We only use access-log URIs here for variant inference and metrics.
        if let uri = event.uri {
            lastSegmentURI = uri
        }
        let tSnap = RequestTracker.shared.snapshot()
        lastObservedSegmentSequence = tSnap.lastSegmentSequence
        maxObservedSegmentSequence = tSnap.maxSegmentSequence
        if let events = item.accessLog()?.events {
            var totalDropped: Double = 0
            var totalBytes: Int64 = 0
            var totalXfer: Double = 0
            for ev in events {
                if ev.numberOfDroppedVideoFrames > 0 {
                    totalDropped += Double(ev.numberOfDroppedVideoFrames)
                }
                if ev.numberOfBytesTransferred > 0 {
                    totalBytes += ev.numberOfBytesTransferred
                }
                if ev.transferDuration > 0 {
                    totalXfer += ev.transferDuration
                }
            }
            droppedVideoFrames = priorDroppedVideoFrames + totalDropped
            updateNetworkWindowBitrate(totalBytes: totalBytes, totalXfer: totalXfer, now: Date())
        }
        updateVariantDwellTimes(from: item)
    }

    private func updateNetworkWindowBitrate(totalBytes: Int64, totalXfer: Double, now: Date) {
        // Detect access-log purge (cumulative bytes decreased) — AVFoundation
        // can retire old events, which resets our running totals. Drop all
        // prior samples in that case so deltas aren't computed against stale
        // baselines.
        if let last = networkByteSamples.last,
           totalBytes < last.bytes || totalXfer < last.xfer {
            networkByteSamples.removeAll()
        }
        networkByteSamples.append((timestamp: now, bytes: totalBytes, xfer: totalXfer))
        let cutoff = now.addingTimeInterval(-(networkWindowSeconds + 1))
        networkByteSamples.removeAll { $0.timestamp < cutoff }
        // Oldest sample within the 6s wall-clock window.
        let windowStart = now.addingTimeInterval(-networkWindowSeconds)
        let withinWindow = networkByteSamples.filter { $0.timestamp >= windowStart }
        guard let oldest = withinWindow.first ?? networkByteSamples.first,
              oldest.timestamp < now else {
            avgNetworkBitrate = nil
            return
        }
        let wall = now.timeIntervalSince(oldest.timestamp)
        let dBytes = Double(max(0, totalBytes - oldest.bytes))
        // Idle detection — full window with zero byte growth means no data
        // made it through for the entire window: report a genuine 0 Mbps.
        if wall >= networkWindowSeconds && dBytes == 0 {
            avgNetworkBitrate = 0
            return
        }
        // Warmup — need at least 1s of wall-clock history for a stable rate.
        guard wall >= 1.0 else {
            avgNetworkBitrate = nil
            return
        }
        // Wall-time denominator: the rate cannot exceed the shaper limit
        // because over wall time, bytes that arrive must have passed through
        // the shaper. xfer-time denominator, by contrast, only counts active
        // transfer duration and can overshoot during TCP bursts.
        avgNetworkBitrate = dBytes * 8.0 / wall
    }

    private func updateAccessLog(from item: AVPlayerItem) {
        refreshLiveAccessLogMetrics(from: item)
        guard let event = item.accessLog()?.events.last else { return }
        var parts: [String] = []
        if let uri = event.uri {
            parts.append("uri=\(uri)")
            parts.append(contentsOf: formatURLParts(uri))
        }
        if event.observedBitrate > 0 { parts.append("observed=\(formatBps(event.observedBitrate))") }
        if event.indicatedBitrate > 0 { parts.append("indicated=\(formatBps(event.indicatedBitrate))") }
        if event.averageVideoBitrate > 0 { parts.append("avgVideo=\(formatBps(event.averageVideoBitrate))") }
        if event.transferDuration > 0 { parts.append("xfer=\(String(format: "%.2fs", event.transferDuration))") }
        if event.numberOfBytesTransferred > 0 { parts.append("bytes=\(event.numberOfBytesTransferred)") }
        if event.numberOfDroppedVideoFrames > 0 { parts.append("dropped=\(Int(event.numberOfDroppedVideoFrames))") }
        if let server = event.serverAddress { parts.append("server=\(server)") }
        if let session = event.playbackSessionID { parts.append("session=\(session)") }
        parts.append(playbackSnapshot())
        lastAccessLog = parts.joined(separator: " ")
    }

    private func variantLabel(from uri: String) -> String {
        // Extract resolution label like "360p", "1080p", "2160p" from playlist URI
        let base = (uri as NSString).lastPathComponent
            .replacingOccurrences(of: ".m3u8", with: "")
            .replacingOccurrences(of: "playlist_6s_", with: "")
            .replacingOccurrences(of: "playlist_2s_", with: "")
            .replacingOccurrences(of: "playlist_", with: "")
        return base.isEmpty ? uri : base
    }

    private func updateVariantDwellTimes(from item: AVPlayerItem) {
        guard let events = item.accessLog()?.events else { return }
        // Discover new variant labels
        let newEvents = events.dropFirst(lastAccessLogEventCount)
        for event in newEvents {
            guard let uri = event.uri else { continue }
            let label = variantLabel(from: uri)
            if label == "audio" { continue }
            knownVariants.insert(label)
        }
        // Sum dwell from current item's access log
        var currentDwell: [String: Double] = [:]
        for event in events {
            guard let uri = event.uri else { continue }
            let label = variantLabel(from: uri)
            if label == "audio" { continue }
            let watched = event.durationWatched
            if watched > 0 {
                currentDwell[label, default: 0] += watched
            }
        }
        lastAccessLogEventCount = events.count
        // Merge prior + current
        var merged = priorVariantDwellSeconds
        for (label, seconds) in currentDwell {
            merged[label, default: 0] += seconds
        }
        variantDwellSeconds = merged
    }

    private func periodicVariantSummary() {
        if let item = player?.currentItem {
            refreshLiveAccessLogMetrics(from: item)
        }
        guard !knownVariants.isEmpty else { return }
        let now = Date()
        if lastVariantSummaryAt == nil || now.timeIntervalSince(lastVariantSummaryAt!) >= 10.0 {
            lastVariantSummaryAt = now
            logVariantSummary()
        }
        checkSegmentStall(now: now)
    }

    private func checkSegmentStall(now: Date) {
        let total = variantDwellSeconds.values.reduce(0, +)
        if abs(total - lastVariantDwellTotal) > 0.1 {
            lastVariantDwellTotal = total
            lastVariantDwellChangeAt = now
            if segmentStallDetected {
                segmentStallDetected = false
                print("[SEGMENT_STALL] resolved — segments downloading again total=\(String(format: "%.1f", total))")
            }
            return
        }
        guard state == "playing" else {
            lastVariantDwellChangeAt = now
            return
        }
        guard let changeAt = lastVariantDwellChangeAt else {
            lastVariantDwellChangeAt = now
            return
        }
        let stalledFor = now.timeIntervalSince(changeAt)
        if stalledFor >= segmentStallThresholdSeconds && !segmentStallDetected {
            segmentStallDetected = true
            print("[SEGMENT_STALL] no new segments for \(String(format: "%.0fs", stalledFor)) while state=\(state) time=\(String(format: "%.2f", currentTime)) variantTotal=\(String(format: "%.1f", total))")
        }
    }

    private func logVariantSummary() {
        let allLabels = knownVariants.sorted { a, b in
            let aNum = Int(a.replacingOccurrences(of: "p", with: "")) ?? 0
            let bNum = Int(b.replacingOccurrences(of: "p", with: "")) ?? 0
            return aNum < bNum
        }
        let total = variantDwellSeconds.values.reduce(0, +)
        let parts = allLabels.map { label in
            let secs = variantDwellSeconds[label] ?? 0
            let pct = total > 0 ? (secs / total) * 100 : 0
            return "\(label)=\(String(format: "%.1fs", secs))(\(String(format: "%.0f%%", pct)))"
        }
        print("[VARIANTS] total=\(String(format: "%.1fs", total)) \(parts.joined(separator: " "))")
        logBitrateSummary()
    }

    private func logBitrateSummary() {
        // [BITRATE] summary print disabled — uncomment when investigating ABR.
        // let observedMbps = (observedBitrate ?? 0) / 1_000_000
        // let indicatedMbps = (indicatedBitrate ?? 0) / 1_000_000
        // let windowMbps = (avgNetworkBitrate ?? 0) / 1_000_000
        // print("[BITRATE] observed=\(String(format: "%.2fMbps", observedMbps)) window\(Int(networkWindowSeconds))s=\(String(format: "%.2fMbps", windowMbps)) indicated=\(String(format: "%.2fMbps", indicatedMbps))")
    }

    private func updateErrorLog(from item: AVPlayerItem) {
        guard let event = item.errorLog()?.events.last else { return }
        var parts: [String] = []
        if let uri = event.uri { parts.append("uri=\(uri)") }
        if let err = event.errorComment { parts.append("comment=\(err)") }
        if event.errorStatusCode != 0 { parts.append("status=\(event.errorStatusCode)") }
        if !event.errorDomain.isEmpty { parts.append("domain=\(event.errorDomain)") }
        if let server = event.serverAddress { parts.append("server=\(server)") }
        if let session = event.playbackSessionID { parts.append("session=\(session)") }
        lastErrorLog = parts.joined(separator: " ")
        if !lastErrorLog.isEmpty {
            print("Error log: \(lastErrorLog) \(playbackSnapshot())")
        }

        let comment = event.errorComment ?? ""
        let uri = event.uri ?? ""
        let isPlaylist = uri.contains(".m3u8")
        if isPlaylist || comment.localizedCaseInsensitiveContains("streamplaylist") {
            var playlistParts: [String] = []
            if !uri.isEmpty { playlistParts.append("uri=\(uri)") }
            if !comment.isEmpty { playlistParts.append("comment=\(comment)") }
            if event.errorStatusCode != 0 { playlistParts.append("status=\(event.errorStatusCode)") }
            if !event.errorDomain.isEmpty { playlistParts.append("domain=\(event.errorDomain)") }
            lastPlaylistError = playlistParts.joined(separator: " ")
            if !lastPlaylistError.isEmpty {
                print("Playlist error: \(lastPlaylistError) \(playbackSnapshot())")
            }
        }
    }

    private func samplePlayerMetrics(now: Date) {
        if lastPlayerSampleAt == nil || now.timeIntervalSince(lastPlayerSampleAt ?? now) >= 1.0 {
            if let liveOffset, liveOffset > 0 {
                appendSample(MetricSample(timestamp: now, value: liveOffset), to: &liveOffsetSamples)
            }
            if let observed = observedBitrate, observed > 0 {
                appendSample(MetricSample(timestamp: now, value: observed / 1_000_000), to: &observedBitrateSamples)
                appendSample(MetricSample(timestamp: now, value: observed / 1_000_000), to: &throughputSamples)
            }
            if let indicated = indicatedBitrate, indicated > 0 {
                appendSample(MetricSample(timestamp: now, value: indicated / 1_000_000), to: &indicatedBitrateSamples)
            }
            if let average = averageVideoBitrate, average > 0 {
                appendSample(MetricSample(timestamp: now, value: average / 1_000_000), to: &averageVideoBitrateSamples)
            }
            if let fps = nominalFrameRate, fps > 0 {
                let dropped = droppedVideoFrames ?? 0
                let estimated = priorEstimatedDisplayedFrames + max(0, (currentTime * fps) - (dropped - priorDroppedVideoFrames))
                estimatedDisplayedFrames = estimated
            }
            let variant = (indicatedBitrate ?? 0) > 0 ? (indicatedBitrate ?? 0) : (averageVideoBitrate ?? 0)
            if variant > 0 {
                appendSample(MetricSample(timestamp: now, value: variant / 1_000_000), to: &variantBitrateSamples)
            }
            let estimateWindowStart = now.addingTimeInterval(-30)
            let estimateSamples = observedBitrateSamples.filter { $0.timestamp >= estimateWindowStart }
            if !estimateSamples.isEmpty {
                let avg = estimateSamples.map { $0.value }.reduce(0, +) / Double(estimateSamples.count)
                appendSample(MetricSample(timestamp: now, value: avg), to: &playerEstimateSamples)
            }
            lastPlayerSampleAt = now
        }
    }

    // Compact one-line snapshot of playback state for log correlation.
    // `netBuf` is the network/segment buffer (loadedTimeRanges ahead of currentTime).
    // `empty/full/keepUp` are AVPlayer's decoder-buffer judgments — the only public
    // decoder-buffer signal available; there is no decoded-frames-ready count.
    // `seg` is the most recent media-segment sequence number: low values indicate
    // we're shortly after a discontinuity (sequence wraps to 0 at the loop point).
    private func playbackSnapshot() -> String {
        guard let item = player?.currentItem else { return "no-item" }
        let bufEmpty = item.isPlaybackBufferEmpty
        let bufFull = item.isPlaybackBufferFull
        let keepUp = item.isPlaybackLikelyToKeepUp
        // Compute netBuf inline from the same loadedTimeRanges we're about to
        // format as `ranges`, so the two always agree. The periodic timer's
        // cached `bufferDepth` can lag by up to 500ms, which previously left
        // log lines where ranges=[214.1-214.4] but netBuf=0.0s — confusing.
        let liveTime = item.currentTime().seconds
        let rangeValues = item.loadedTimeRanges.map { $0.timeRangeValue }
        let ranges = rangeValues
            .map { String(format: "%.1f-%.1f", $0.start.seconds, $0.start.seconds + $0.duration.seconds) }
            .joined(separator: ",")
        let depthStr: String
        if let last = rangeValues.last {
            let end = last.start.seconds + last.duration.seconds
            let depth = max(0, end - liveTime)
            depthStr = String(format: "%.1fs", depth)
        } else {
            depthStr = "0.0s"
        }
        let variant = currentVariantLabel() ?? "?"
        let segStr = lastObservedSegmentSequence.map { String($0) } ?? "?"
        let maxStr = maxObservedSegmentSequence.map { String($0) } ?? "?"
        let indMbps = (indicatedBitrate ?? 0) / 1_000_000
        let obsMbps = (observedBitrate ?? 0) / 1_000_000
        let liveStr = liveOffset.map { String(format: "%.1fs", $0) } ?? "nil"
        let slackStr = windowSlack.map { String(format: "%.1fs", $0) } ?? "nil"
        let winStr = windowDuration.map { String(format: "%.1fs", $0) } ?? "nil"
        return "variant=\(variant) seg=\(segStr)/max=\(maxStr) netBuf=\(depthStr) empty=\(bufEmpty) full=\(bufFull) keepUp=\(keepUp) ranges=[\(ranges)] liveOff=\(liveStr) window=\(winStr) slack=\(slackStr) ind=\(String(format: "%.1fMbps", indMbps)) obs=\(String(format: "%.1fMbps", obsMbps))"
    }

    // AVPlayer's access log URIs are per-variant playlists, not per-segment,
    // so segment-keyed info comes from RequestTracker (the proxy sees every
    // real segment fetch). Segment paths look like .../2160p/segment_00006.m4s.
    private func currentVariantLabel() -> String? {
        // Use the last *video* segment URI — audio interleaves and would
        // otherwise cause the reported variant to flip to "audio".
        if let uri = RequestTracker.shared.snapshot().lastVideoSegmentURI,
           let label = variantLabelFromURI(uri) {
            return label
        }
        return nil
    }

    // Pull the resolution label ("720p", "2160p", "audio") out of a segment URI
    // by walking path components from the end.
    private func variantLabelFromURI(_ uri: String) -> String? {
        guard let url = URL(string: uri) else { return nil }
        for p in url.pathComponents.reversed() {
            if p == "audio" { return p }
            if p.hasSuffix("p"), Int(p.dropLast()) != nil {
                return p
            }
        }
        return nil
    }

    private func formatBps(_ bps: Double) -> String {
        if bps >= 1_000_000 {
            return String(format: "%.2fMbps", bps / 1_000_000)
        }
        if bps >= 1_000 {
            return String(format: "%.0fKbps", bps / 1_000)
        }
        return String(format: "%.0fbps", bps)
    }

    private func extractSegmentSequence(from uri: String) -> Int? {
        guard let components = URLComponents(string: uri) else { return nil }
        let path = components.path
        guard !path.isEmpty else { return nil }
        var filename = (path as NSString).lastPathComponent
        guard !filename.isEmpty else { return nil }
        let stem = (filename as NSString).deletingPathExtension
        if !stem.isEmpty {
            filename = stem
        }
        let matches = filename.matches(of: /\d+/)
        guard let token = matches.last else { return nil }
        return Int(String(token.output))
    }

    private func formatURLParts(_ uri: String) -> [String] {
        guard let components = URLComponents(string: uri) else { return [] }
        var parts: [String] = []
        if let host = components.host { parts.append("host=\(host)") }
        if let port = components.port { parts.append("port=\(port)") }
        if !components.path.isEmpty { parts.append("path=\(components.path)") }
        if let query = components.percentEncodedQuery, !query.isEmpty { parts.append("query=\(query)") }
        return parts
    }

    private func appendSample(_ sample: MetricSample, to array: inout [MetricSample]) {
        array.append(sample)
        let cutoff = Date().addingTimeInterval(-seriesWindowSeconds)
        array.removeAll { $0.timestamp < cutoff }
        if array.count > maxSeriesSamples {
            array.removeFirst(array.count - maxSeriesSamples)
        }
    }

    private func observeCurrentItem() {
        guard let item = player?.currentItem else { return }
        item.publisher(for: \.status)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] status in
                guard let self else { return }
                let prev = self.itemStatus
                switch status {
                case .unknown: self.itemStatus = "Unknown"
                case .readyToPlay: self.itemStatus = "Ready"
                case .failed:
                    self.itemStatus = "Failed"
                    if let err = item.error {
                        self.itemError = self.describeError(err)
                    }
                @unknown default: self.itemStatus = "Unknown"
                }
                print("[ITEM_STATUS] \(prev) -> \(self.itemStatus) error=\(item.error?.localizedDescription ?? "none") time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
            }
            .store(in: &cancellables)

        // Decoder-buffer booleans. `dropFirst()` skips the initial value so we
        // only log genuine transitions, not the startup snapshot.
        item.publisher(for: \.isPlaybackBufferEmpty)
            .dropFirst()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] empty in
                guard let self else { return }
                print("[DECODER] bufferEmpty -> \(empty) state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
            }
            .store(in: &cancellables)

        item.publisher(for: \.isPlaybackBufferFull)
            .dropFirst()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] full in
                guard let self else { return }
                print("[DECODER] bufferFull -> \(full) state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
            }
            .store(in: &cancellables)

        item.publisher(for: \.isPlaybackLikelyToKeepUp)
            .dropFirst()
            .receive(on: DispatchQueue.main)
            .sink { [weak self] keepUp in
                guard let self else { return }
                print("[DECODER] likelyToKeepUp -> \(keepUp) state=\(self.state) time=\(String(format: "%.2f", self.currentTime)) \(self.playbackSnapshot())")
            }
            .store(in: &cancellables)
    }

    private func describeError(_ error: Error) -> String {
        let ns = error as NSError
        var parts: [String] = [ns.localizedDescription]
        if !ns.domain.isEmpty { parts.append("domain=\(ns.domain)") }
        if ns.code != 0 { 
            parts.append("code=\(ns.code)")
            // Add human-readable interpretation of common error codes
            if ns.domain == AVFoundationErrorDomain {
                parts.append("(\(interpretAVErrorCode(ns.code)))")
            } else if ns.domain == "CoreMediaErrorDomain" {
                parts.append("(\(interpretCoreMediaErrorCode(ns.code)))")
            }
        }
        if let reason = ns.userInfo[NSLocalizedFailureReasonErrorKey] as? String, !reason.isEmpty {
            parts.append("reason=\(reason)")
        }
        if let suggestion = ns.userInfo[NSLocalizedRecoverySuggestionErrorKey] as? String, !suggestion.isEmpty {
            parts.append("suggestion=\(suggestion)")
        }
        if let underlyingError = ns.userInfo[NSUnderlyingErrorKey] as? NSError {
            parts.append("underlying=[\(underlyingError.domain) \(underlyingError.code)]")
        }
        return parts.joined(separator: " ")
    }
    
    private func interpretAVErrorCode(_ code: Int) -> String {
        switch code {
        case -11800: return "AVErrorUnknown"
        case -11801: return "AVErrorOutOfMemory"
        case -11802: return "AVErrorSessionNotRunning"
        case -11803: return "AVErrorDeviceAlreadyUsedByAnotherSession"
        case -11804: return "AVErrorNoDataCaptured"
        case -11805: return "AVErrorSessionConfigurationChanged"
        case -11806: return "AVErrorDiskFull"
        case -11807: return "AVErrorDeviceWasDisconnected"
        case -11808: return "AVErrorMediaChanged"
        case -11809: return "AVErrorMaximumDurationReached"
        case -11810: return "AVErrorMaximumFileSizeReached"
        case -11811: return "AVErrorMediaDiscontinuity"
        case -11812: return "AVErrorMaximumNumberOfSamplesForFileFormatReached"
        case -11813: return "AVErrorDeviceNotConnected"
        case -11814: return "AVErrorDeviceInUseByAnotherApplication"
        case -11815: return "AVErrorDeviceLockedForConfigurationByAnotherProcess"
        case -11816: return "AVErrorSessionWasInterrupted"
        case -11817: return "AVErrorMediaServicesWereReset"
        case -11818: return "AVErrorExportFailed"
        case -11819: return "AVErrorDecodeFailed"
        case -11820: return "AVErrorInvalidSourceMedia"
        case -11821: return "AVErrorFileAlreadyExists"
        case -11822: return "AVErrorCompositionTrackSegmentsNotContiguous"
        case -11823: return "AVErrorInvalidCompositionTrackSegmentDuration"
        case -11824: return "AVErrorInvalidCompositionTrackSegmentSourceStartTime"
        case -11825: return "AVErrorInvalidCompositionTrackSegmentSourceDuration"
        case -11826: return "AVErrorFileFormatNotRecognized"
        case -11827: return "AVErrorFileFailedToParse"
        case -11828: return "AVErrorMaximumStillImageCaptureRequestsExceeded"
        case -11829: return "AVErrorContentIsProtected"
        case -11830: return "AVErrorNoImageAtTime"
        case -11831: return "AVErrorDecoderNotFound"
        case -11832: return "AVErrorEncoderNotFound"
        case -11833: return "AVErrorContentIsNotAuthorized"
        case -11834: return "AVErrorApplicationIsNotAuthorized"
        case -11835: return "AVErrorDeviceIsNotAvailableInBackground"
        case -11836: return "AVErrorOperationNotSupportedForAsset"
        case -11837: return "AVErrorDecoderTemporarilyUnavailable"
        case -11838: return "AVErrorEncoderTemporarilyUnavailable"
        case -11839: return "AVErrorInvalidVideoComposition"
        case -11840: return "AVErrorReferenceForbiddenByReferencePolicy"
        case -11841: return "AVErrorInvalidOutputURLPathExtension"
        case -11842: return "AVErrorScreenCaptureFailed"
        case -11843: return "AVErrorDisplayWasDisabled"
        case -11844: return "AVErrorTorchLevelUnavailable"
        case -11845: return "AVErrorOperationInterrupted"
        case -11846: return "AVErrorIncompatibleAsset"
        case -11847: return "AVErrorFailedToLoadMediaData"
        case -11848: return "AVErrorServerIncorrectlyConfigured"
        case -11849: return "AVErrorApplicationIsNotAuthorizedForPlayback"
        case -11850: return "AVErrorContentIsUnavailable"
        case -11851: return "AVErrorFormatUnsupported"
        case -11852: return "AVErrorAirPlayControllerRequiresInternet"
        case -11853: return "AVErrorAirPlayReceiverRequiresInternet"
        case -11854: return "AVErrorVideoCompositorFailed"
        case -11855: return "AVErrorRecordingAlreadyInProgress"
        case -11856: return "AVErrorUnsupportedOutputSettings"
        case -11857: return "AVErrorOperationNotAllowed"
        case -11858: return "AVErrorContentNotUpdated"
        case -11859: return "AVErrorNoLongerPlayable"
        case -11860: return "AVErrorNoCompatibleAlternatesForExternalDisplay"
        case -11861: return "AVErrorNoSourceTrack"
        case -11862: return "AVErrorExternalPlaybackNotSupportedForAsset"
        case -11863: return "AVErrorOperationNotSupportedForPreset"
        case -11864: return "AVErrorSessionHardwareCostOverage"
        case -11865: return "AVErrorUnsupportedDeviceActiveFormat"
        case -12782: return "AVErrorInvalidSampleCursor"
        case -12783: return "AVErrorFailedToLoadSampleData"
        case -12784: return "AVErrorAirPlayReceiverTemporarilyUnavailable"
        case -12785: return "AVErrorEncodeFailed"
        case -12786: return "AVErrorSandboxExtensionDenied"
        case -12900: return "AVErrorUnknown/Generic"
        default: return "Unknown(\(code))"
        }
    }
    
    private func interpretCoreMediaErrorCode(_ code: Int) -> String {
        switch code {
        case -12640: return "kCMFormatDescriptionError_InvalidParameter"
        case -12641: return "kCMFormatDescriptionError_AllocationFailed"
        case -12642: return "kCMFormatDescriptionError_ValueNotAvailable"
        case -12710: return "kCMSampleBufferError_AllocationFailed"
        case -12711: return "kCMSampleBufferError_RequiredParameterMissing"
        case -12712: return "kCMSampleBufferError_AlreadyHasDataBuffer"
        case -12713: return "kCMSampleBufferError_BufferNotReady"
        case -12714: return "kCMSampleBufferError_SampleIndexOutOfRange"
        case -12715: return "kCMSampleBufferError_BufferHasNoSampleSizes"
        case -12716: return "kCMSampleBufferError_BufferHasNoSampleTimingInfo"
        case -12717: return "kCMSampleBufferError_ArrayTooSmall"
        case -12718: return "kCMSampleBufferError_InvalidEntryCount"
        case -12719: return "kCMSampleBufferError_CannotSubdivide"
        case -12720: return "kCMSampleBufferError_SampleTimingInfoInvalid"
        case -12721: return "kCMSampleBufferError_InvalidMediaTypeForOperation"
        case -12722: return "kCMSampleBufferError_InvalidSampleData"
        case -12723: return "kCMSampleBufferError_InvalidMediaFormat"
        case -12724: return "kCMSampleBufferError_Invalidated"
        case -12730: return "kCMSimpleQueueError_AllocationFailed"
        case -12731: return "kCMSimpleQueueError_RequiredParameterMissing"
        case -12732: return "kCMSimpleQueueError_ParameterOutOfRange"
        case -12733: return "kCMSimpleQueueError_QueueIsFull"
        case -12760: return "kCMMemoryPoolError_AllocationFailed"
        case -12761: return "kCMMemoryPoolError_InvalidParameter"
        case -12770: return "kCMTimeRangeError_InvalidParameter"
        case -12780: return "kCMSyncError_MissingRequiredParameter"
        case -12781: return "kCMSyncError_InvalidParameter"
        case -12782: return "kCMSyncError_AllocationFailed"
        case -12783: return "kCMSyncError_RateMismatch"
        default: return "CoreMediaError(\(code))"
        }
    }
}
