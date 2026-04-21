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

    private let flowingGap: TimeInterval = 0.1

    struct Snapshot {
        let wireBytesTotal: Int64
        let wireActiveMsTotal: Double
        let wireFlowingMsTotal: Double
        let wireInflightCount: Int
        let wireLastChunkMsAgo: Double?
    }

    func requestStarted() {
        lock.lock(); defer { lock.unlock() }
        if inflightCount == 0 {
            activeStartAt = Date()
        }
        inflightCount += 1
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
            wireLastChunkMsAgo: msAgo
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
    }
}
