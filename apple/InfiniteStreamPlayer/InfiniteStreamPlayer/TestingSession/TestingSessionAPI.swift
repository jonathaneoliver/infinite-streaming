import Foundation

final class TestingSessionAPI {
    let controlBaseURL: URL

    init(controlBaseURL: URL) {
        self.controlBaseURL = controlBaseURL
    }

    func sessionsStreamURL(playerIdFilter: String? = nil) -> URL {
        let base = controlBaseURL.appendingPathComponent("api/sessions/stream")
        guard let playerId = playerIdFilter, !playerId.isEmpty,
              var components = URLComponents(url: base, resolvingAgainstBaseURL: false) else {
            return base
        }
        components.queryItems = [URLQueryItem(name: "player_id", value: playerId)]
        return components.url ?? base
    }

    func listSessions() async throws -> [SessionData] {
        let url = controlBaseURL.appendingPathComponent("api/sessions")
        let (data, response) = try await URLSession.shared.data(from: url)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
        return try JSONDecoder().decode([SessionData].self, from: data)
    }

    func patchSession(sessionId: String, set: [String: JSONValue], fields: [String], baseRevision: String) async throws -> SessionData? {
        let url = controlBaseURL.appendingPathComponent("api/session/").appendingPathComponent(sessionId)
        var request = URLRequest(url: url)
        request.httpMethod = "PATCH"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        let payload = SessionPatchRequest(set: set, fields: fields, base_revision: baseRevision)
        request.httpBody = try JSONEncoder().encode(payload)
        let (data, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
        if let json = try? JSONDecoder().decode([String: SessionData].self, from: data) {
            return json["session"]
        }
        return nil
    }

    func deleteSession(sessionId: String) async throws {
        let url = controlBaseURL.appendingPathComponent("api/session/").appendingPathComponent(sessionId)
        var request = URLRequest(url: url)
        request.httpMethod = "DELETE"
        let (_, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
    }

    func linkSessions(sessionIds: [String]) async throws {
        let url = controlBaseURL.appendingPathComponent("api/session-group/link")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(SessionGroupLinkRequest(session_ids: sessionIds))
        let (_, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
    }

    func unlinkSession(sessionId: String, groupId: String) async throws {
        let url = controlBaseURL.appendingPathComponent("api/session-group/unlink")
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(SessionGroupUnlinkRequest(session_id: sessionId, group_id: groupId))
        let (_, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
    }

    func applyShape(port: String, rate: Double, delay: Double, loss: Double) async throws {
        let url = controlBaseURL.appendingPathComponent("api/nftables/shape/").appendingPathComponent(port)
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(ShapeRequest(rate_mbps: rate, delay_ms: delay, loss_pct: loss))
        let (_, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
    }

    func applyPattern(port: String, pattern: PatternRequest) async throws {
        let url = controlBaseURL.appendingPathComponent("api/nftables/pattern/").appendingPathComponent(port)
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(pattern)
        let (_, response) = try await URLSession.shared.data(for: request)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
    }

    func capabilities() async throws -> NftablesCapabilities {
        let url = controlBaseURL.appendingPathComponent("api/nftables/capabilities")
        let (data, response) = try await URLSession.shared.data(from: url)
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            throw NSError(domain: "TestingSessionAPI", code: http.statusCode)
        }
        return try JSONDecoder().decode(NftablesCapabilities.self, from: data)
    }
}
