import Foundation

@MainActor
final class TestingSessionViewModel: ObservableObject {
    @Published var session: SessionData?
    @Published var allSessions: [SessionData] = []
    @Published var statusMessage: String = ""
    @Published var sseMissed: Int = 0
    @Published var sseLastRevision: Int = 0
    @Published var sseDropped: Int = 0
    @Published var logs: [String] = []
    @Published var bandwidthSamples: [BandwidthSample] = []
    @Published var limitSamples: [MetricSample] = []
    @Published var nftablesEnabled: Bool = true
    @Published var nftablesMessage: String = ""

    let playerId: String
    private(set) var controlBaseURL: URL

    private var api: TestingSessionAPI
    private var sse: SSEClient?
    private var pollTask: Task<Void, Never>?
    private let maxSamples = 600
    private let samplesWindowSeconds: TimeInterval = 300
    private var pendingEditsBySession: [String: PendingEdit] = [:]
    private let pendingTTL: TimeInterval = 5
    private var lastLimitSampleAt: Date?

    init(playerId: String, controlBaseURL: URL) {
        self.playerId = playerId
        let normalized = Self.normalizeControlBaseURL(controlBaseURL)
        self.controlBaseURL = normalized
        self.api = TestingSessionAPI(controlBaseURL: normalized)
    }

    private static func normalizeControlBaseURL(_ url: URL) -> URL {
        guard var components = URLComponents(url: url, resolvingAgainstBaseURL: false) else {
            return url
        }
        if components.port == nil {
            components.port = 40000
        }
        if components.scheme == nil || components.scheme?.isEmpty == true {
            components.scheme = "http"
        }
        return components.url ?? url
    }

    func start() {
        stop()
        Task { await loadCapabilities() }
        startSSE()
        startPolling()
    }

    func stop() {
        sse?.cancel()
        sse = nil
        pollTask?.cancel()
        pollTask = nil
    }

    func updateControlBaseURL(_ url: URL) {
        let normalized = Self.normalizeControlBaseURL(url)
        if normalized == controlBaseURL {
            return
        }
        stop()
        controlBaseURL = normalized
        api = TestingSessionAPI(controlBaseURL: normalized)
        start()
    }

    func refreshSession() async {
        do {
            let sessions = try await api.listSessions()
            allSessions = sessions
            let match = sessions.first { $0.playerId == playerId }
            session = match
        } catch {
            log("Session refresh failed: \(error.localizedDescription)")
        }
    }

    func applyPatch(set: [String: JSONValue], fields: [String]) async {
        guard let sessionId = session?.sessionId, !sessionId.isEmpty else { return }
        pendingEditsBySession[sessionId] = PendingEdit(fields: set, timestamp: Date(), expectedRevision: nil)
        do {
            let updated = try await api.patchSession(sessionId: sessionId, set: set, fields: fields, baseRevision: session?.controlRevision ?? "")
            if let updated {
                session = updated
                let expected = updated.controlRevision.isEmpty ? nil : updated.controlRevision
                pendingEditsBySession[sessionId] = PendingEdit(fields: set, timestamp: Date(), expectedRevision: expected)
            }
        } catch {
            pendingEditsBySession.removeValue(forKey: sessionId)
            log("Patch failed: \(error.localizedDescription)")
        }
    }

    func deleteSession() async {
        guard let sessionId = session?.sessionId, !sessionId.isEmpty else { return }
        do {
            try await api.deleteSession(sessionId: sessionId)
            session = nil
        } catch {
            log("Delete failed: \(error.localizedDescription)")
        }
    }

    func linkSessions(targetId: String) async {
        guard let currentId = session?.sessionId, !currentId.isEmpty else { return }
        do {
            var ids: [String] = [currentId, targetId]
            if let target = allSessions.first(where: { $0.sessionId == targetId }), !target.groupId.isEmpty {
                let groupIds = allSessions.filter { $0.groupId == target.groupId }.map { $0.sessionId }
                ids.append(contentsOf: groupIds)
            }
            ids = Array(Set(ids))
            try await api.linkSessions(sessionIds: ids)
            await refreshSession()
        } catch {
            log("Group link failed: \(error.localizedDescription)")
        }
    }

    func unlinkSession() async {
        guard let sessionId = session?.sessionId, !sessionId.isEmpty else { return }
        let groupId = session?.groupId ?? ""
        guard !groupId.isEmpty else { return }
        do {
            try await api.unlinkSession(sessionId: sessionId, groupId: groupId)
            await refreshSession()
        } catch {
            log("Group unlink failed: \(error.localizedDescription)")
        }
    }

    func applyShape(rate: Double, delay: Double, loss: Double) async {
        let port = session?.xForwardedPortExternal.isEmpty == false ? (session?.xForwardedPortExternal ?? "") : (session?.xForwardedPort ?? "")
        guard !port.isEmpty else { return }
        if let sessionId = session?.sessionId, !sessionId.isEmpty {
            let pending: [String: JSONValue] = [
                "nftables_bandwidth_mbps": .number(rate),
                "nftables_delay_ms": .number(delay),
                "nftables_packet_loss": .number(loss)
            ]
            pendingEditsBySession[sessionId] = PendingEdit(fields: pending, timestamp: Date(), expectedRevision: nil)
        }
        do {
            try await api.applyShape(port: port, rate: rate, delay: delay, loss: loss)
        } catch {
            log("Shape failed: \(error.localizedDescription)")
        }
    }

    func applyPattern(port: String, pattern: PatternRequest) async {
        if let sessionId = session?.sessionId, !sessionId.isEmpty {
            var pending: [String: JSONValue] = [
                "nftables_pattern_enabled": .bool(true),
                "nftables_pattern_steps": .array(pattern.steps),
                "nftables_pattern_default_step_seconds": .number(pattern.default_step_seconds),
                "nftables_pattern_default_segments": .number(pattern.default_segments),
                "nftables_pattern_segment_duration_seconds": .number(pattern.segment_duration_seconds),
                "nftables_pattern_template_mode": .string(pattern.template_mode),
                "nftables_pattern_margin_pct": .number(pattern.template_margin_pct),
                "nftables_delay_ms": .number(pattern.delay_ms),
                "nftables_packet_loss": .number(pattern.loss_pct)
            ]
            pendingEditsBySession[sessionId] = PendingEdit(fields: pending, timestamp: Date(), expectedRevision: nil)
        }
        do {
            log("Pattern apply: port=\(port) mode=\(pattern.template_mode) steps=\(pattern.steps.count) margin=\(pattern.template_margin_pct) step=\(pattern.default_step_seconds)")
            try await api.applyPattern(port: port, pattern: pattern)
            log("Pattern apply: sent OK")
        } catch {
            log("Pattern failed: \(error.localizedDescription)")
        }
    }

    func logAction(_ message: String) {
        log(message)
    }

    private func loadCapabilities() async {
        do {
            let caps = try await api.capabilities()
            if caps.status != "enabled" {
                nftablesEnabled = false
                let reason = caps.reason ?? "Traffic shaping unavailable."
                let platform = caps.platform ?? "unknown"
                nftablesMessage = "Network shaping disabled (\(platform)): \(reason)"
            } else {
                nftablesEnabled = true
                nftablesMessage = ""
            }
        } catch {
            nftablesEnabled = false
            nftablesMessage = "Network shaping disabled (error)"
        }
    }

    private func startPolling() {
        pollTask = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                await self.refreshSession()
                try? await Task.sleep(nanoseconds: 1_000_000_000)
            }
        }
    }

    private func startSSE() {
        let url = api.sessionsStreamURL()
        sse = SSEClient(url: url, onEvent: { [weak self] event in
            self?.handleSSE(event)
        }, onError: { [weak self] error in
            self?.log("SSE error: \(error?.localizedDescription ?? "closed")")
        })
        sse?.start()
    }

    private func handleSSE(_ event: SSEClient.Event) {
        guard event.event == "sessions" || event.event == nil else { return }
        if let data = event.data.data(using: .utf8) {
            if let payload = try? JSONDecoder().decode(SessionsStreamPayload.self, from: data) {
                applySessionsPayload(payload)
                return
            }
            if let sessions = try? JSONDecoder().decode([SessionData].self, from: data) {
                applySessionsPayload(SessionsStreamPayload(sessions: sessions, revision: nil, dropped: nil))
                return
            }
        }
    }

    private func applySessionsPayload(_ payload: SessionsStreamPayload) {
        let now = Date()
        let sessions = payload.sessions ?? []
        let revision = payload.revision ?? 0
        let dropped = payload.dropped ?? 0
        if revision > 0, sseLastRevision > 0, revision <= sseLastRevision {
            return
        }
        var missed = 0
        if sseLastRevision > 0 && revision > sseLastRevision + 1 {
            missed += revision - sseLastRevision - 1
        }
        if dropped > 0 { missed += dropped }
        if missed > 0 { sseMissed += missed }
        if revision > 0 { sseLastRevision = revision }
        prunePendingEdits(now: now)
        let mergedSessions = sessions.map { mergePendingEdits(for: $0, now: now) }
        allSessions = mergedSessions
        let match = mergedSessions.first { $0.playerId == playerId }
        session = match
        if let match {
            appendBandwidthSample(from: match)
        }
    }

    private func mergePendingEdits(for session: SessionData, now: Date) -> SessionData {
        let sessionId = session.sessionId
        guard !sessionId.isEmpty, let pending = pendingEditsBySession[sessionId] else { return session }
        if let expected = pending.expectedRevision, !expected.isEmpty, session.controlRevision == expected {
            pendingEditsBySession.removeValue(forKey: sessionId)
            return session
        }
        if now.timeIntervalSince(pending.timestamp) > pendingTTL {
            pendingEditsBySession.removeValue(forKey: sessionId)
            return session
        }
        var merged = session.raw
        for (key, value) in pending.fields {
            merged[key] = value
        }
        return SessionData(raw: merged)
    }

    private func prunePendingEdits(now: Date) {
        let expired = pendingEditsBySession.filter { now.timeIntervalSince($0.value.timestamp) > pendingTTL }.map { $0.key }
        expired.forEach { pendingEditsBySession.removeValue(forKey: $0) }
    }

    private func appendBandwidthSample(from session: SessionData) {
        let now = Date()
        let sample = BandwidthSample(
            timestamp: now,
            mbpsOut: session.mbpsOut ?? 0,
            mbpsOut1s: session.mbpsOut1s ?? 0,
            mbpsOutActive: session.mbpsOutActive ?? 0,
            mbpsOutAvg: session.mbpsOutAvg ?? 0
        )
        bandwidthSamples.append(sample)
        let cutoff = now.addingTimeInterval(-samplesWindowSeconds)
        bandwidthSamples.removeAll { $0.timestamp < cutoff }
        if bandwidthSamples.count > maxSamples {
            bandwidthSamples.removeFirst(bandwidthSamples.count - maxSamples)
        }
        appendLimitSample(from: session, now: now)
    }

    private func log(_ message: String) {
        let stamp = ISO8601DateFormatter().string(from: Date())
        let line = "[\(stamp)] \(message)"
        logs.append(line)
        if logs.count > 200 {
            logs.removeFirst(logs.count - 200)
        }
        print(line)
    }

    private func appendLimitSample(from session: SessionData, now: Date) {
        if lastLimitSampleAt == nil || now.timeIntervalSince(lastLimitSampleAt ?? now) >= 1.0 {
            if let target = computeLimitRate(from: session), target.isFinite, target >= 0 {
                limitSamples.append(MetricSample(timestamp: now, value: target))
            }
            let cutoff = now.addingTimeInterval(-samplesWindowSeconds)
            limitSamples.removeAll { $0.timestamp < cutoff }
            if limitSamples.count > maxSamples {
                limitSamples.removeFirst(limitSamples.count - maxSamples)
            }
            lastLimitSampleAt = now
        }
    }

    private func computeLimitRate(from session: SessionData) -> Double? {
        let patternEnabled = isPatternEnabled(session)
        let runtimeTarget = session.raw["nftables_pattern_rate_runtime_mbps"]?.doubleValue
        if patternEnabled, let runtimeTarget, runtimeTarget.isFinite, runtimeTarget >= 0 {
            return runtimeTarget
        }
        let stepIndexRaw = session.raw["nftables_pattern_step_runtime"]?.doubleValue
            ?? session.raw["nftables_pattern_step"]?.doubleValue
            ?? 0
        let stepIndex = Int(stepIndexRaw)
        if stepIndex > 0, let steps = session.raw["nftables_pattern_steps"]?.arrayValue, stepIndex <= steps.count {
            if let obj = steps[stepIndex - 1].objectValue,
               let rate = obj["rate_mbps"]?.doubleValue,
               rate.isFinite {
                return rate
            }
        }
        return session.raw["nftables_bandwidth_mbps"]?.doubleValue
    }

    private func isPatternEnabled(_ session: SessionData) -> Bool {
        if let boolValue = session.raw["nftables_pattern_enabled"]?.boolValue {
            return boolValue
        }
        if let intValue = session.raw["nftables_pattern_enabled"]?.intValue {
            return intValue == 1
        }
        let stringValue = session.raw["nftables_pattern_enabled"]?.stringValue ?? ""
        return stringValue.lowercased() == "true"
    }
}

struct BandwidthSample: Identifiable {
    let id = UUID()
    let timestamp: Date
    let mbpsOut: Double
    let mbpsOut1s: Double
    let mbpsOutActive: Double
    let mbpsOutAvg: Double
}

private struct PendingEdit {
    let fields: [String: JSONValue]
    let timestamp: Date
    let expectedRevision: String?
}
