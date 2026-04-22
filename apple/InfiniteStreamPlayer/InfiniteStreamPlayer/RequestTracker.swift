import Foundation

/// Thread-safe accounting for HTTP requests going through HTTPProxyResourceLoader.
/// Maintains cumulative counters that PlaybackDiagnostics samples periodically
/// and emits as columns on the [BITRATE_SAMPLE] line for offline analysis.
///
/// Conventions:
///   - `wireBytesTotal` — monotonic, cumulative bytes from URLSession data
///     delegate callbacks (post-TLS, pre-content-decryption).
///   - `wireActiveMsTotal` — wall-clock ms during which at least one request is
///     in-flight (started, not yet completed/cancelled/failed). Overlapping
///     requests do NOT double-count; it's wall time only.
///   - `wireFlowingMsTotal` — wall-clock ms during which a chunk was received
///     within the last 100 ms. Turns off after 100 ms of no new chunks.
final class RequestTracker {
    static let shared = RequestTracker()

    private let lock = NSLock()
    private var inflightCount: Int = 0
    private var totalBytes: Int64 = 0
    private var activeStartAt: Date?
    private var cumulativeActiveMs: Double = 0
    private var flowingStartAt: Date?
    private var cumulativeFlowingMs: Double = 0
    private var lastChunkAt: Date?
    // Last media-segment URI + sequence seen flowing through the proxy.
    // AVPlayer's access log URIs are per-variant playlist, not per segment,
    // so we capture segment identity here at the one place that actually
    // sees every segment fetch. Video and audio are tracked separately
    // because audio segments interleave with video — reporting a single
    // "last" would flip-flop between "audio" and the real video variant.
    private var lastSegmentURIValue: String?
    private var lastVideoSegmentURIValue: String?
    private var lastSegmentSequenceValue: Int?
    private var maxSegmentSequenceValue: Int?

    private let flowingGap: TimeInterval = 0.1

    struct Snapshot {
        let wireBytesTotal: Int64
        let wireActiveMsTotal: Double
        let wireFlowingMsTotal: Double
        let wireInflightCount: Int
        let wireLastChunkMsAgo: Double?
        let lastSegmentURI: String?
        let lastVideoSegmentURI: String?
        let lastSegmentSequence: Int?
        let maxSegmentSequence: Int?
    }

    func requestStarted() {
        lock.lock(); defer { lock.unlock() }
        if inflightCount == 0 {
            activeStartAt = Date()
        }
        inflightCount += 1
    }

    /// Record a media-segment fetch. Non-segment URLs (playlists, init data) are
    /// filtered out so that seg/max reflect only real sequence progression.
    /// Returns `(sequence, priorMax, isWrap)` for the caller to emit a log if
    /// the sequence dropped sharply (loop wrap / discontinuity).
    @discardableResult
    func recordRequestURL(_ url: URL) -> (sequence: Int, priorMax: Int, isWrap: Bool)? {
        let path = url.path
        let isSegment = path.hasSuffix(".m4s") || path.hasSuffix(".ts") || path.hasSuffix(".mp4") || path.hasSuffix(".cmfv") || path.hasSuffix(".cmfa")
        guard isSegment else { return nil }
        guard let sequence = Self.extractSegmentSequence(from: path) else { return nil }
        lock.lock(); defer { lock.unlock() }
        lastSegmentURIValue = url.absoluteString
        // Audio segment paths contain "/audio/"; anything else is video for
        // our content. Only video URIs count toward the "current variant".
        if !path.contains("/audio/") {
            lastVideoSegmentURIValue = url.absoluteString
        }
        lastSegmentSequenceValue = sequence
        let priorMax = maxSegmentSequenceValue ?? -1
        let isWrap = priorMax >= 5 && sequence <= 2
        if isWrap {
            maxSegmentSequenceValue = sequence
        } else {
            maxSegmentSequenceValue = max(priorMax, sequence)
        }
        return (sequence, priorMax, isWrap)
    }

    private static func extractSegmentSequence(from path: String) -> Int? {
        var filename = (path as NSString).lastPathComponent
        if filename.isEmpty { return nil }
        filename = (filename as NSString).deletingPathExtension
        // Accept either "segment_00006" (authoritative pattern) or any trailing
        // digit run as a fallback. Trailing match avoids picking up resolution
        // numbers embedded earlier in the path.
        let matches = filename.matches(of: /\d+/)
        guard let token = matches.last else { return nil }
        return Int(String(token.output))
    }

    func chunkReceived(byteCount: Int) {
        lock.lock(); defer { lock.unlock() }
        let now = Date()
        totalBytes += Int64(byteCount)
        lastChunkAt = now
        if flowingStartAt == nil {
            flowingStartAt = now
        }
    }

    func requestFinished() {
        lock.lock(); defer { lock.unlock() }
        inflightCount = max(0, inflightCount - 1)
        if inflightCount == 0, let start = activeStartAt {
            cumulativeActiveMs += Date().timeIntervalSince(start) * 1000
            activeStartAt = nil
        }
    }

    func snapshot(now: Date = Date()) -> Snapshot {
        lock.lock(); defer { lock.unlock() }

        // Lazy close flowing window if the gap has elapsed since the last chunk.
        if let lastChunk = lastChunkAt, let flowStart = flowingStartAt,
           now.timeIntervalSince(lastChunk) >= flowingGap {
            cumulativeFlowingMs += lastChunk.timeIntervalSince(flowStart) * 1000
            flowingStartAt = nil
        }

        var active = cumulativeActiveMs
        if let start = activeStartAt {
            active += now.timeIntervalSince(start) * 1000
        }
        var flowing = cumulativeFlowingMs
        if let flowStart = flowingStartAt {
            flowing += now.timeIntervalSince(flowStart) * 1000
        }
        let msAgo = lastChunkAt.map { now.timeIntervalSince($0) * 1000 }

        return Snapshot(
            wireBytesTotal: totalBytes,
            wireActiveMsTotal: active,
            wireFlowingMsTotal: flowing,
            wireInflightCount: inflightCount,
            wireLastChunkMsAgo: msAgo,
            lastSegmentURI: lastSegmentURIValue,
            lastVideoSegmentURI: lastVideoSegmentURIValue,
            lastSegmentSequence: lastSegmentSequenceValue,
            maxSegmentSequence: maxSegmentSequenceValue
        )
    }

    /// Reset all counters — call on player restart / new session.
    func reset() {
        lock.lock(); defer { lock.unlock() }
        inflightCount = 0
        totalBytes = 0
        activeStartAt = nil
        cumulativeActiveMs = 0
        flowingStartAt = nil
        cumulativeFlowingMs = 0
        lastChunkAt = nil
        lastSegmentURIValue = nil
        lastVideoSegmentURIValue = nil
        lastSegmentSequenceValue = nil
        maxSegmentSequenceValue = nil
    }
}
