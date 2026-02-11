import Foundation

final class SSEClient: NSObject, URLSessionDataDelegate {
    struct Event {
        let id: String?
        let event: String?
        let data: String
    }

    private let url: URL
    private let onEvent: @MainActor (Event) -> Void
    private let onError: @MainActor (Error?) -> Void
    private var buffer = ""
    private var session: URLSession?
    private var task: URLSessionDataTask?

    init(url: URL, onEvent: @escaping @MainActor (Event) -> Void, onError: @escaping @MainActor (Error?) -> Void) {
        self.url = url
        self.onEvent = onEvent
        self.onError = onError
    }

    func start() {
        var request = URLRequest(url: url)
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        request.timeoutInterval = 0
        let config = URLSessionConfiguration.default
        config.timeoutIntervalForRequest = 0
        config.timeoutIntervalForResource = 0
        let session = URLSession(configuration: config, delegate: self, delegateQueue: nil)
        self.session = session
        let task = session.dataTask(with: request)
        self.task = task
        task.resume()
    }

    func cancel() {
        task?.cancel()
        session?.invalidateAndCancel()
        task = nil
        session = nil
    }

    func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        guard let chunk = String(data: data, encoding: .utf8) else { return }
        Task { @MainActor [weak self] in
            self?.handleChunk(chunk)
        }
    }

    func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        Task { @MainActor [weak self] in
            guard let self else { return }
            self.onError(error)
        }
    }

    @MainActor
    private func handleChunk(_ chunk: String) {
        buffer.append(chunk)
        let parts = buffer.components(separatedBy: "\n\n")
        for part in parts.dropLast() {
            if let event = parseEvent(part) {
                onEvent(event)
            }
        }
        buffer = parts.last ?? ""
    }

    private func parseEvent(_ raw: String) -> Event? {
        var eventName: String?
        var eventId: String?
        var dataLines: [String] = []

        raw.split(separator: "\n").forEach { line in
            if line.hasPrefix("event:") {
                eventName = line.replacingOccurrences(of: "event:", with: "").trimmingCharacters(in: .whitespaces)
            } else if line.hasPrefix("id:") {
                eventId = line.replacingOccurrences(of: "id:", with: "").trimmingCharacters(in: .whitespaces)
            } else if line.hasPrefix("data:") {
                let value = line.replacingOccurrences(of: "data:", with: "").trimmingCharacters(in: .whitespaces)
                dataLines.append(value)
            }
        }
        guard !dataLines.isEmpty else { return nil }
        return Event(id: eventId, event: eventName, data: dataLines.joined(separator: "\n"))
    }
}
