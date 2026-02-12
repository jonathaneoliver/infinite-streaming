import AVFoundation
import Combine
import Foundation

struct MetricSample: Identifiable {
    let id = UUID()
    let timestamp: Date
    let value: Double
}

final class PlaybackDiagnostics: ObservableObject {
    @Published var state: String = "Idle"
    @Published var currentTime: Double = 0
    @Published var bufferedEnd: Double?
    @Published var bufferDepth: Double?
    @Published var seekableEnd: Double?
    @Published var liveOffset: Double?
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
    @Published var averageVideoBitrate: Double?
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

    private var timeObserverToken: Any?
    private var cancellables: Set<AnyCancellable> = []
    private weak var player: AVPlayer?
    private var lastBufferSampleAt: Date?
    private var lastPlayerSampleAt: Date?
    private var stallStartAt: Date?
    private var hasRenderedFirstFrame: Bool = false
    private let maxSeriesSamples = 600
    private let seriesWindowSeconds: TimeInterval = 300

    func bind(to player: AVPlayer) {
        self.player = player
        observePlayer(player)
    }

    func reset() {
        state = "Idle"
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
        averageVideoBitrate = nil
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
        hasRenderedFirstFrame = false
    }

    func markFirstFrameRendered() {
        hasRenderedFirstFrame = true
    }

    private func observePlayer(_ player: AVPlayer) {
        cancellables.removeAll()

        player.publisher(for: \.timeControlStatus)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] status in
                switch status {
                case .paused:
                    self?.state = "Paused"
                case .waitingToPlayAtSpecifiedRate:
                    self?.state = "Buffering"
                    self?.startStallIfNeeded()
                case .playing:
                    self?.state = "Playing"
                    self?.endStallIfNeeded()
                @unknown default: self?.state = "Unknown"
                }
            }
            .store(in: &cancellables)

        player.publisher(for: \.reasonForWaitingToPlay)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] reason in
                if let reason = reason {
                    self?.waitingReason = reason.rawValue
                } else {
                    self?.waitingReason = ""
                }
            }
            .store(in: &cancellables)

        player.publisher(for: \.rate)
            .receive(on: DispatchQueue.main)
            .assign(to: &$playbackRate)

        NotificationCenter.default.publisher(for: .AVPlayerItemPlaybackStalled)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in
                self?.startStallIfNeeded()
            }
            .store(in: &cancellables)

        NotificationCenter.default.publisher(for: .AVPlayerItemFailedToPlayToEndTime)
            .receive(on: DispatchQueue.main)
            .sink { [weak self] notification in
                if let error = notification.userInfo?[AVPlayerItemFailedToPlayToEndTimeErrorKey] as? Error {
                    self?.lastFailure = error.localizedDescription
                } else {
                    self?.lastFailure = "Playback failed"
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
        if let range = ranges.last?.timeRangeValue {
            let end = range.start.seconds + range.duration.seconds
            bufferedEnd = end
            bufferDepth = max(0, end - currentTime)
        } else {
            bufferedEnd = nil
            bufferDepth = nil
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
            seekableEnd = liveEdge
            liveOffset = max(0, liveEdge - currentTime)
        } else {
            seekableEnd = nil
            liveOffset = nil
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

    private func endStallIfNeeded() {
        guard let start = stallStartAt else { return }
        let duration = max(0, Date().timeIntervalSince(start))
        lastStallDurationSeconds = duration
        stallTimeSeconds += duration
        stallStartAt = nil
    }

    private func updateAccessLog(from item: AVPlayerItem) {
        guard let event = item.accessLog()?.events.last else { return }
        observedBitrate = event.observedBitrate
        indicatedBitrate = event.indicatedBitrate
        averageVideoBitrate = event.averageVideoBitrate
        if let uri = event.uri {
            lastSegmentURI = uri
        }
        // Samples are collected on a steady interval in samplePlayerMetrics().
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
        // numberOfSegmentsDownloaded is unavailable on iOS/tvOS; omit.
        if let server = event.serverAddress { parts.append("server=\(server)") }
        if let session = event.playbackSessionID { parts.append("session=\(session)") }
        lastAccessLog = parts.joined(separator: " ")
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
            print("Error log: \(lastErrorLog)")
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
                print("Playlist error: \(lastPlaylistError)")
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

    private func formatBps(_ bps: Double) -> String {
        if bps >= 1_000_000 {
            return String(format: "%.2fMbps", bps / 1_000_000)
        }
        if bps >= 1_000 {
            return String(format: "%.0fKbps", bps / 1_000)
        }
        return String(format: "%.0fbps", bps)
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
                switch status {
                case .unknown: self?.itemStatus = "Unknown"
                case .readyToPlay: self?.itemStatus = "Ready"
                case .failed:
                    self?.itemStatus = "Failed"
                    if let err = item.error {
                        self?.itemError = self?.describeError(err) ?? err.localizedDescription
                    }
                @unknown default: self?.itemStatus = "Unknown"
                }
            }
            .store(in: &cancellables)
    }

    private func describeError(_ error: Error) -> String {
        let ns = error as NSError
        var parts: [String] = [ns.localizedDescription]
        if !ns.domain.isEmpty { parts.append("domain=\(ns.domain)") }
        if ns.code != 0 { parts.append("code=\(ns.code)") }
        if let reason = ns.userInfo[NSLocalizedFailureReasonErrorKey] as? String, !reason.isEmpty {
            parts.append("reason=\(reason)")
        }
        if let suggestion = ns.userInfo[NSLocalizedRecoverySuggestionErrorKey] as? String, !suggestion.isEmpty {
            parts.append("suggestion=\(suggestion)")
        }
        return parts.joined(separator: " ")
    }
}
