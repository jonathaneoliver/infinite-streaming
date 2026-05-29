// AVMetricsSubscriber.swift — issue #486 spike.
//
// Subscribes to iOS 18 AVMetrics streams on a single AVPlayerItem and
// batches the raw events to go-proxy via POST /api/session/{id}/avmetrics.
// Runs in parallel to the existing heartbeat-derived /metrics POST so
// the dashboard can render both side-by-side and we can decide
// empirically which AVMetrics signals are worth promoting to the v2
// player-metrics wire shape.
//
// One subscriber per AVPlayerItem — PlayerViewModel rebuilds it when the
// item is replaced (see `attachAVMetrics` in PlayerViewModel.swift).

import AVFoundation
import Foundation
import ObjectiveC.runtime

@available(iOS 18.0, *)
final class AVMetricsSubscriber: @unchecked Sendable {

    /// Invoked with a batch of `{event_type, event_ts_ms, raw}` dicts
    /// ready for JSONSerialization. PlayerViewModel owns session-id
    /// resolution + URL construction so this class stays decoupled
    /// from the existing metrics bookkeeping.
    typealias OnBatch = (_ events: [[String: Any]]) async -> Void

    private let onBatch: OnBatch
    private var tasks: [Task<Void, Never>] = []
    private var flushTimer: Task<Void, Never>?
    private let bufferLock = NSLock()
    private var buffer: [[String: Any]] = []

    /// Flush once we have ~50 events buffered, OR every 500 ms — whichever
    /// fires first. The flush interval is short relative to the heartbeat
    /// cadence so the dashboard's AVMetrics lane lags the live signal by
    /// well under a second. Tune up if forwarder batch pressure grows.
    private let flushThreshold = 50
    private let flushIntervalMs: UInt64 = 500

    /// Ring buffer of recent TTFB samples (ms) extracted from each
    /// MediaResourceRequest event's `networkTransactionMetrics`. Issue
    /// #486 — gives PlayerViewModel a client-side RTT proxy to publish
    /// on the heartbeat alongside the server-side `client_rtt_ms`.
    /// 16 samples ≈ 30 s of segment fetches at 2 s segments — large
    /// enough to smooth single-segment jitter, small enough to track
    /// a real path change within a minute.
    private let ttfbRingCapacity = 16
    private let ttfbRingLock = NSLock()
    private var ttfbRing: [Double] = []

    init(item: AVPlayerItem, onBatch: @escaping OnBatch) {
        self.onBatch = onBatch
        NSLog("[AVMetrics] init — bound to AVPlayerItem \(ObjectIdentifier(item))")
        subscribe(item: item)
        startFlushTimer()
    }

    deinit { cancel() }

    func cancel() {
        for task in tasks { task.cancel() }
        tasks.removeAll()
        flushTimer?.cancel()
        flushTimer = nil
        flush()
    }

    private func subscribe(item: AVPlayerItem) {
        // Each `subscribeStream` helper spawns one async loop per
        // metric type. The high-volume types (MediaResourceRequest,
        // RateChange) confirm the pipeline is alive even during a
        // perfectly smooth play; the low-volume ones (LikelyToKeepUp,
        // VariantSwitch, PlaybackSummary) carry the diagnostic value
        // the spike is actually measuring.
        //
        // `metrics(forType:)` is EXACT-TYPE matching — subclasses are
        // not delivered through a base-class subscription. So Stall /
        // Seek / VariantSwitchStart / InitialLikelyToKeepUp each need
        // their own subscribe call even though they inherit from a
        // class we're already subscribed to. iOS 26 SDK confirmed
        // these subclasses exist (AVMetrics.h lines 250, 290, 300, 385).
        NSLog("[AVMetrics] subscribing — 10 types incl. iOS 26 additions")

        // Base / high-volume types
        subscribeStream(name: "MediaResourceRequest",
                        stream: item.metrics(forType: AVMetricMediaResourceRequestEvent.self))
        subscribeStream(name: "RateChange",
                        stream: item.metrics(forType: AVMetricPlayerItemRateChangeEvent.self))
        subscribeStream(name: "LikelyToKeepUp",
                        stream: item.metrics(forType: AVMetricPlayerItemLikelyToKeepUpEvent.self))
        subscribeStream(name: "VariantSwitch",
                        stream: item.metrics(forType: AVMetricPlayerItemVariantSwitchEvent.self))
        subscribeStream(name: "PlaybackSummary",
                        stream: item.metrics(forType: AVMetricPlayerItemPlaybackSummaryEvent.self))
        subscribeStream(name: "ErrorEvent",
                        stream: item.metrics(forType: AVMetricErrorEvent.self))

        // iOS 26 SDK subclasses — exact-type filter means each needs
        // its own subscribe even though they extend types we're
        // already draining.
        subscribeStream(name: "InitialLikelyToKeepUp",
                        stream: item.metrics(forType: AVMetricPlayerItemInitialLikelyToKeepUpEvent.self))
        subscribeStream(name: "VariantSwitchStart",
                        stream: item.metrics(forType: AVMetricPlayerItemVariantSwitchStartEvent.self))
        subscribeStream(name: "Stall",
                        stream: item.metrics(forType: AVMetricPlayerItemStallEvent.self))
        subscribeStream(name: "Seek",
                        stream: item.metrics(forType: AVMetricPlayerItemSeekEvent.self))

        // HLS request subclasses (issue #486). For HLS content iOS
        // delivers these instead of the base AVMetricMediaResource-
        // RequestEvent — the base type fires for non-HLS resource
        // loads only. Without these we'd never see segment, playlist,
        // or DRM-key fetches and the derived_mbps + TTFB ring would
        // stay empty.
        subscribeStream(name: "HLSMediaSegmentRequest",
                        stream: item.metrics(forType: AVMetricHLSMediaSegmentRequestEvent.self))
        subscribeStream(name: "HLSPlaylistRequest",
                        stream: item.metrics(forType: AVMetricHLSPlaylistRequestEvent.self))
        subscribeStream(name: "ContentKeyRequest",
                        stream: item.metrics(forType: AVMetricContentKeyRequestEvent.self))
    }

    /// Generic spawner: one Task per metric type, draining its
    /// AsyncSequence into `record(_:)`. Keeps the per-type subscribe
    /// site to a single line and gives the loop-lifecycle logs a
    /// uniform shape regardless of type.
    private func subscribeStream<S: AsyncSequence>(
        name: String,
        stream: S
    ) where S.Element: AVMetricEvent {
        tasks.append(Task { [weak self] in
            NSLog("[AVMetrics] \(name) loop started")
            do {
                for try await event in stream {
                    if Task.isCancelled { return }
                    self?.record(event)
                }
                NSLog("[AVMetrics] \(name) loop ended cleanly")
            } catch {
                NSLog("[AVMetrics] \(name) stream ended: \(error)")
            }
        })
    }

    private func record(_ event: AVMetricEvent) {
        let type = String(describing: Swift.type(of: event))
        // Use the AVMetric event's own `date` so the timeline stays
        // independent of when our async loop happened to resume.
        let tsMs = Int64((event.date.timeIntervalSince1970 * 1000).rounded())
        var raw = objcPropertyDump(event)
        // Type-specific derivations — issue #486. Computed here while
        // we have typed access; the dashboard chip renderer surfaces
        // any extra keys we add without per-type frontend code.
        appendThroughputDerivation(into: &raw, event: event)
        let row: [String: Any] = [
            "event_type": type,
            "event_ts_ms": tsMs,
            "raw": raw
        ]
        bufferLock.lock()
        buffer.append(row)
        let shouldFlush = buffer.count >= flushThreshold
        bufferLock.unlock()
        if shouldFlush {
            flush()
        }
    }

    /// Derive `derived_mbps` + transfer time + body bytes from any
    /// event that carries (or back-references) an
    /// AVMetricMediaResourceRequestEvent. Issue #486 — the per-request
    /// throughput is the cleanest network-quality signal AVMetrics
    /// exposes, but only on COMPLETED requests. A hung connection
    /// produces no event; pair with the heartbeat-driven
    /// `network_bitrate_mbps` (from RequestTracker, which updates
    /// per-chunk) for hang detection.
    private func appendThroughputDerivation(into raw: inout [String: Any], event: AVMetricEvent) {
        let mrr: AVMetricMediaResourceRequestEvent? = {
            if let r = event as? AVMetricMediaResourceRequestEvent { return r }
            if let s = event as? AVMetricHLSMediaSegmentRequestEvent { return s.mediaResourceRequestEvent }
            if let p = event as? AVMetricHLSPlaylistRequestEvent     { return p.mediaResourceRequestEvent }
            if let k = event as? AVMetricContentKeyRequestEvent      { return k.mediaResourceRequestEvent }
            return nil
        }()
        guard let req = mrr else { return }
        // Per-request derivations (issue #486). Extract TTFB for the
        // client-RTT ring AND per-segment throughput for the
        // dashboard bandwidth-chart overlay. Both pull from the same
        // NSURLSessionTaskMetrics → we walk the transactions once.
        var bestTtfbMs: Double = 0
        var bytesFromTx: Int = 0
        var durMsFromTx: Double = 0
        if let metrics = req.networkTransactionMetrics {
            for tx in metrics.transactionMetrics {
                if let reqEnd = tx.requestEndDate,
                   let respStart = tx.responseStartDate {
                    let ttfbMs = respStart.timeIntervalSince(reqEnd) * 1000.0
                    if ttfbMs.isFinite, ttfbMs > 0, ttfbMs < 60_000 {
                        pushTTFB(ttfbMs)
                        bestTtfbMs = ttfbMs
                    }
                }
                // Per-segment body throughput. body-received +
                // (responseEnd - responseStart) = wire-rate of the
                // body only, isolated from header/setup phase. The
                // legacy `byteRange.length` is 0 for non-ranged GETs,
                // so falling back to the transaction's actual
                // received-bytes counter is what unlocks dots on the
                // chart for HLS segment fetches.
                let bodyBytes = Int(tx.countOfResponseBodyBytesReceived)
                if bodyBytes > bytesFromTx { bytesFromTx = bodyBytes }
                if let respStart = tx.responseStartDate,
                   let respEnd = tx.responseEndDate {
                    let ms = respEnd.timeIntervalSince(respStart) * 1000.0
                    if ms > durMsFromTx { durMsFromTx = ms }
                }
            }
        }
        if bestTtfbMs > 0 {
            raw["derived_ttfb_ms"] = String(format: "%.1f", bestTtfbMs)
        }
        // Prefer the transaction-level (bytes, duration); fall back to
        // the legacy byteRange/responseEndTime path when transactions
        // are missing (Apple sometimes omits them for early-stage
        // errors).
        var bytes = bytesFromTx
        var durSec = durMsFromTx / 1000.0
        if bytes == 0 { bytes = req.byteRange.length }
        if durSec <= 0 { durSec = req.responseEndTime.timeIntervalSince(req.responseStartTime) }
        guard bytes > 0, durSec > 0, durSec < 3600 else { return }
        let mbps = (Double(bytes) * 8.0) / (durSec * 1_000_000.0)
        raw["derived_mbps"] = String(format: "%.3f", mbps)
        raw["derived_transfer_ms"] = String(format: "%.1f", durSec * 1000.0)
        raw["derived_bytes"] = String(bytes)
        raw["derived_from_cache"] = req.wasReadFromCache ? "1" : "0"
    }

    /// Reflective dump via the Obj-C runtime.
    ///
    /// AVMetric* events are NSObject subclasses whose payload lives in
    /// `@property` declarations on the Obj-C side — Swift's `Mirror`
    /// skips them entirely (it only sees Swift stored properties), so
    /// the first cut of this dumper produced `raw_json={}` for every
    /// row even though the events were arriving fine. Walks the class
    /// hierarchy back through `AVMetricEvent` and reads each declared
    /// property via KVC. NSObject itself is skipped (its props are
    /// `description`/`debugDescription`/etc. and just add noise).
    ///
    /// Values are stringified via `String(describing:)` for the spike;
    /// once we know which fields matter per type we'll project them
    /// explicitly into stable JSON keys for SQL.
    private func objcPropertyDump(_ obj: AnyObject) -> [String: Any] {
        var out: [String: Any] = [:]
        let nsObj = obj as? NSObject
        var cls: AnyClass? = Swift.type(of: obj)
        while let c = cls, c !== NSObject.self {
            var count: UInt32 = 0
            if let props = class_copyPropertyList(c, &count) {
                for i in 0..<Int(count) {
                    let name = String(cString: property_getName(props[i]))
                    if out[name] != nil { continue }     // child class wins
                    guard let v = nsObj?.value(forKey: name) else { continue }
                    out[name] = formatValue(v)
                }
                free(props)
            }
            cls = class_getSuperclass(c)
        }
        return out
    }

    /// Type-aware stringifier for the property dump. The generic
    /// `String(describing:)` falls back to NSObject `description` for
    /// opaque objects like AVMetricMediaRendition, which prints just
    /// the class+pointer (`<AVMetricMediaRendition: 0x600…>`) and
    /// loses every actual field. Specialise for the iOS 26 AVMetrics
    /// types whose `description` isn't useful. Issue #486.
    private func formatValue(_ v: Any) -> String {
        if let rend = v as? AVMetricMediaRendition {
            var parts: [String] = []
            if let id = rend.stableID, !id.isEmpty { parts.append("stableID=\(id)") }
            if let url = rend.url { parts.append("URL=\(url.absoluteString)") }
            return parts.isEmpty ? "<rendition>" : parts.joined(separator: " ")
        }
        return String(describing: v)
    }

    private func startFlushTimer() {
        flushTimer = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: (self?.flushIntervalMs ?? 500) * 1_000_000)
                if Task.isCancelled { return }
                self?.flush()
            }
        }
    }

    /// Push a TTFB sample into the ring buffer (FIFO, drop oldest).
    /// Thread-safe — called from the AVMetric async loop.
    private func pushTTFB(_ ms: Double) {
        ttfbRingLock.lock()
        ttfbRing.append(ms)
        if ttfbRing.count > ttfbRingCapacity {
            ttfbRing.removeFirst(ttfbRing.count - ttfbRingCapacity)
        }
        ttfbRingLock.unlock()
    }

    /// Median TTFB over the ring buffer, in milliseconds. Returns 0
    /// when the ring is empty. PlayerViewModel reads this on each
    /// heartbeat and publishes it as `player_metrics_client_rtt_avmetrics_ms`.
    /// Median (not mean) so a single fat outlier doesn't yank the
    /// charted line away from steady-state.
    func medianTTFB() -> Double {
        ttfbRingLock.lock()
        let snap = ttfbRing
        ttfbRingLock.unlock()
        if snap.isEmpty { return 0 }
        let sorted = snap.sorted()
        let mid = sorted.count / 2
        if sorted.count % 2 == 1 { return sorted[mid] }
        return (sorted[mid - 1] + sorted[mid]) / 2.0
    }

    /// Snapshot the buffer under the lock, then fire the POST on a
    /// detached Task. Kept sync so callers can invoke it from the
    /// flush-timer body without the Swift-6 "lock in async context"
    /// diagnostic — NSLock is fine in sync contexts.
    private func flush() {
        bufferLock.lock()
        let snap = buffer
        buffer.removeAll()
        bufferLock.unlock()
        guard !snap.isEmpty else { return }
        let onBatch = self.onBatch
        Task { await onBatch(snap) }
    }
}
