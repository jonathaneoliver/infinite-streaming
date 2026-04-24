import Foundation

/// Talks to the InfiniteStream pairing rendezvous Worker.
/// Lets a camera-less TV client get paired with a server URL via a short code
/// that the user types into the dashboard on their phone/laptop.
enum RendezvousError: LocalizedError {
    case notConfigured
    case crossNetworkBlocked
    case invalidResponse
    case timedOut
    case http(Int, String)

    var errorDescription: String? {
        switch self {
        case .notConfigured:
            return "Rendezvous URL is not configured."
        case .crossNetworkBlocked:
            return "The dashboard publisher is on a different network than this TV. Pair from a device on the same Wi-Fi."
        case .invalidResponse:
            return "Unexpected response from rendezvous service."
        case .timedOut:
            return "Pairing timed out. The user did not enter the code in time."
        case .http(let status, let body):
            return "HTTP \(status): \(body)"
        }
    }
}

struct RendezvousService {
    /// Default rendezvous URL baked into the build. Override with the
    /// "InfiniteStreamRendezvousURL" key in UserDefaults (e.g. via Settings).
    /// Empty string means pairing is disabled.
    ///
    /// NOTE FOR FORKS: this points at the upstream maintainer's personal
    /// Cloudflare Worker. If you fork this repo and ship your own builds,
    /// please change this to your own Worker URL (or set it to "" to
    /// require runtime configuration) so your users don't accidentally
    /// hammer someone else's free-tier KV write budget. See
    /// `cloudflare/pair-rendezvous/` for how to deploy your own.
    static let defaultURL = "https://pair-infinitestream.jeoliver.com"

    /// Effective rendezvous URL after applying the UserDefaults override.
    static var url: String {
        let override = UserDefaults.standard.string(forKey: "InfiniteStreamRendezvousURL") ?? ""
        let trimmed = override.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? defaultURL : trimmed
    }

    /// Generates a 6-character pairing code using an ambiguity-free alphabet.
    static func generateCode(length: Int = 6) -> String {
        let alphabet = Array("ABCDEFGHJKLMNPQRSTUVWXYZ23456789")
        return String((0..<length).map { _ in alphabet.randomElement()! })
    }

    /// Polls the rendezvous endpoint until a server URL appears or timeout
    /// expires. Throws on cross-network rejection or other HTTP errors.
    /// Caller is responsible for cancellation via `Task.cancel()`.
    static func pollForServerURL(code: String,
                                 pollInterval: TimeInterval = 2.0,
                                 timeout: TimeInterval = 10 * 60) async throws -> String {
        guard !url.isEmpty,
              let endpoint = URL(string: "\(url)/pair?code=\(code)") else {
            throw RendezvousError.notConfigured
        }

        let deadline = Date().addingTimeInterval(timeout)

        while Date() < deadline {
            try Task.checkCancellation()

            var request = URLRequest(url: endpoint)
            request.httpMethod = "GET"
            request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
            request.timeoutInterval = 10

            do {
                let (data, response) = try await URLSession.shared.data(for: request)
                guard let http = response as? HTTPURLResponse else {
                    throw RendezvousError.invalidResponse
                }
                switch http.statusCode {
                case 200:
                    let body = String(data: data, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                    if !body.isEmpty {
                        return body
                    }
                case 204:
                    break // keep polling
                case 403:
                    throw RendezvousError.crossNetworkBlocked
                default:
                    let body = String(data: data, encoding: .utf8) ?? ""
                    throw RendezvousError.http(http.statusCode, body)
                }
            } catch is CancellationError {
                throw CancellationError()
            } catch let err as RendezvousError {
                throw err
            } catch {
                // Transient network error — keep polling, don't bail.
            }

            try? await Task.sleep(nanoseconds: UInt64(pollInterval * 1_000_000_000))
        }
        throw RendezvousError.timedOut
    }

    /// Best-effort cleanup of a code after consuming it. Failure is ignored.
    static func releaseCode(_ code: String) async {
        guard !url.isEmpty,
              let endpoint = URL(string: "\(url)/pair?code=\(code)") else { return }
        var request = URLRequest(url: endpoint)
        request.httpMethod = "DELETE"
        _ = try? await URLSession.shared.data(for: request)
    }

    /// One detected server reported by GET /announce. The Worker filters
    /// these to only servers visible from the caller's CF-Connecting-IP,
    /// so the list is implicitly scoped to "same public IP".
    struct DiscoveredServer: Identifiable, Hashable {
        let id: String      // server_id
        let url: String
        let label: String
    }

    /// Fetches servers visible on the caller's public IP. Returns an empty
    /// array on missing config or any error — discovery is best-effort and
    /// the UI falls back to manual entry.
    static func discoverServers() async -> [DiscoveredServer] {
        guard !url.isEmpty,
              let endpoint = URL(string: "\(url)/announce") else { return [] }
        var request = URLRequest(url: endpoint)
        request.httpMethod = "GET"
        request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        request.timeoutInterval = 8
        do {
            let (data, response) = try await URLSession.shared.data(for: request)
            guard let http = response as? HTTPURLResponse, http.statusCode == 200 else {
                return []
            }
            guard let obj = try JSONSerialization.jsonObject(with: data) as? [String: Any],
                  let arr = obj["servers"] as? [[String: Any]] else {
                return []
            }
            return arr.compactMap { entry in
                guard let id = entry["server_id"] as? String, !id.isEmpty,
                      let u = entry["url"] as? String, !u.isEmpty else { return nil }
                let label = (entry["label"] as? String) ?? ""
                return DiscoveredServer(id: id, url: u, label: label.isEmpty ? u : label)
            }
        } catch {
            return []
        }
    }
}
