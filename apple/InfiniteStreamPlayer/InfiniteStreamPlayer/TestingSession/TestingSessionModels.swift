import Foundation

struct SessionsStreamPayload: Codable {
    let sessions: [SessionData]?
    let revision: Int?
    let dropped: Int?
}

struct SessionData: Codable, Equatable, Identifiable {
    let raw: [String: JSONValue]

    init(raw: [String: JSONValue]) {
        self.raw = raw
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        raw = (try? container.decode([String: JSONValue].self)) ?? [:]
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(raw)
    }

    subscript(_ key: String) -> JSONValue? { raw[key] }

    var sessionId: String { raw["session_id"]?.stringValue ?? "" }
    var playerId: String { raw["player_id"]?.stringValue ?? "" }
    var groupId: String { raw["group_id"]?.stringValue ?? "" }
    var manifestURL: String { raw["manifest_url"]?.stringValue ?? "" }
    var masterManifestURL: String { raw["master_manifest_url"]?.stringValue ?? "" }
    var lastRequestURL: String { raw["last_request_url"]?.stringValue ?? "" }
    var userAgent: String { raw["user_agent"]?.stringValue ?? "" }
    var playerIP: String { raw["player_ip"]?.stringValue ?? "" }
    var xForwardedPort: String { raw["x_forwarded_port"]?.stringValue ?? "" }
    var xForwardedPortExternal: String { raw["x_forwarded_port_external"]?.stringValue ?? "" }
    var measuredMbps: String { raw["measured_mbps"]?.stringValue ?? "" }
    var mbpsOut: Double? { raw["mbps_out"]?.doubleValue }
    var mbpsOutAvg: Double? { raw["mbps_out_avg"]?.doubleValue }
    var mbpsOut1s: Double? { raw["mbps_out_1s"]?.doubleValue }
    var mbpsOutActive: Double? { raw["mbps_out_active"]?.doubleValue }
    var lastRequest: String { raw["last_request"]?.stringValue ?? "" }
    var firstRequest: String { raw["first_request_time"]?.stringValue ?? "" }
    var sessionDuration: Double? { raw["session_duration"]?.doubleValue }
    var masterManifestCount: Int? { raw["master_manifest_requests_count"]?.intValue }
    var manifestCount: Int? { raw["manifest_requests_count"]?.intValue }
    var segmentCount: Int? { raw["segments_count"]?.intValue }
    var controlRevision: String { raw["control_revision"]?.stringValue ?? "" }

    var playerRestartRequested: Bool {
        if let value = raw["player_restart_requested"]?.boolValue {
            return value
        }
        let text = raw["player_restart_requested"]?.stringValue ?? ""
        return ["1", "true", "yes", "on"].contains(text.lowercased())
    }
    var playerRestartRequestId: String { raw["player_restart_request_id"]?.stringValue ?? "" }
    var playerRestartRequestReason: String { raw["player_restart_request_reason"]?.stringValue ?? "" }
    var playerRestartRequestState: String { raw["player_restart_request_state"]?.stringValue ?? "" }

    var transportFaultActive: Bool { raw["transport_fault_active"]?.boolValue ?? false }
    var transportFaultDropPackets: Int { raw["transport_fault_drop_packets"]?.intValue ?? 0 }
    var transportFaultRejectPackets: Int { raw["transport_fault_reject_packets"]?.intValue ?? 0 }

    var manifestVariants: [ManifestVariant] {
        guard let array = raw["manifest_variants"]?.arrayValue else { return [] }
        return array.compactMap { value in
            guard let obj = value.objectValue else { return nil }
            return ManifestVariant(
                url: obj["url"]?.stringValue ?? "",
                bandwidth: obj["bandwidth"]?.intValue ?? 0,
                resolution: obj["resolution"]?.stringValue ?? ""
            )
        }
    }

    var id: String { sessionId }
}

struct ManifestVariant: Identifiable, Hashable {
    let id = UUID()
    let url: String
    let bandwidth: Int
    let resolution: String
}

struct NftablesCapabilities: Codable {
    let status: String?
    let reason: String?
    let platform: String?
}

struct SessionGroupLinkRequest: Codable {
    let session_ids: [String]
}

struct SessionGroupUnlinkRequest: Codable {
    let session_id: String
    let group_id: String
}

struct SessionPatchRequest: Codable {
    let set: [String: JSONValue]
    let fields: [String]
    let base_revision: String
}

struct ShapeRequest: Codable {
    let rate_mbps: Double
    let delay_ms: Double
    let loss_pct: Double
}

struct PatternRequest: Codable {
    let steps: [JSONValue]
    let segment_duration_seconds: Double
    let default_segments: Double
    let default_step_seconds: Double
    let template_mode: String
    let template_margin_pct: Double
    let delay_ms: Double
    let loss_pct: Double
}
