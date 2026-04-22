import Foundation
import Network

/// A tiny HTTP/1.1 forward proxy running on 127.0.0.1:<ephemeral>. The app
/// rewrites the master HLS URL from http://origin/path → http://127.0.0.1:port/proxy/http/origin/path
/// and hands the rewritten URL to AVPlayer. AVPlayer then uses its native
/// HTTP stack — not AVAssetResourceLoader — so all HLS / fMP4 semantics work
/// normally. The proxy forwards each request to the real origin via URLSession
/// and streams the response back, feeding per-chunk timing events to
/// `RequestTracker` for throughput measurement.
///
/// Scope: HTTP only (no TLS termination), GET/HEAD only, HTTP/1.1 with
/// Connection: close (one request per connection). Sufficient for our test
/// environment where the origin is unencrypted HTTP on a LAN.
final class LocalHTTPProxy: NSObject {
    static let shared = LocalHTTPProxy()

    private let queue = DispatchQueue(label: "com.jeoliver.LocalHTTPProxy", qos: .userInitiated)
    fileprivate var session: URLSession!
    private var listener: NWListener?
    private(set) var port: UInt16 = 0
    fileprivate let tracker: RequestTracker
    // Current origin to forward all incoming requests to. Set from the main
    // thread (via rewrite()) and read from the proxy's delegate queue. Guarded
    // by its own NSLock rather than `queue` because delegate callbacks already
    // run on `queue` — a queue.sync from within would deadlock.
    private let originLock = NSLock()
    private var originScheme: String = "http"
    private var originHost: String = ""
    private var originPort: Int?

    // Map URLSession task id → the connection session that owns it. Serialized
    // on `queue`. URLSession delegate callbacks come in on the session's
    // operation queue (which we back with `queue`).
    fileprivate var sessionsByTaskID: [Int: ConnectionSession] = [:]

    init(tracker: RequestTracker = .shared) {
        self.tracker = tracker
        super.init()
        let config = URLSessionConfiguration.ephemeral
        config.httpAdditionalHeaders = nil
        config.timeoutIntervalForRequest = 60
        config.timeoutIntervalForResource = 300
        let opQueue = OperationQueue()
        opQueue.underlyingQueue = queue
        self.session = URLSession(configuration: config, delegate: self, delegateQueue: opQueue)
    }

    // MARK: - Lifecycle

    func startIfNeeded() {
        guard listener == nil else { return }
        do {
            let params = NWParameters.tcp
            // Bind explicitly to IPv4 loopback; otherwise NWListener may come
            // up on IPv6 only and AVPlayer's HTTP client — which resolves
            // 127.0.0.1 as IPv4 — never connects.
            params.requiredLocalEndpoint = NWEndpoint.hostPort(host: "127.0.0.1", port: .any)
            let listener = try NWListener(using: params)
            listener.stateUpdateHandler = { [weak self] state in
                guard let self else { return }
                switch state {
                case .ready:
                    if let p = listener.port?.rawValue {
                        self.port = p
                        Swift.print("[LOCAL_PROXY] listening on 127.0.0.1:\(p)")
                    }
                case .failed(let err):
                    Swift.print("[LOCAL_PROXY] listener failed: \(err)")
                default: break
                }
            }
            listener.newConnectionHandler = { [weak self] conn in
                self?.handle(conn)
            }
            listener.start(queue: queue)
            self.listener = listener
        } catch {
            Swift.print("[LOCAL_PROXY] failed to start: \(error)")
        }
    }

    /// Rewrite `<scheme>://origin[:port]/path?query` → `http://127.0.0.1:<local>/path?query`
    /// and remember the origin so every subsequent request the proxy receives
    /// is forwarded to the same origin. This preserves AVPlayer's URL
    /// resolution — absolute paths inside manifests (e.g.
    /// `#EXT-X-MAP:URI="/go-live/.../init.mp4"`) resolve against the proxy base
    /// and land on our proxy with the original path intact.
    ///
    /// Returns nil if the scheme is unsupported or the listener failed. If
    /// the listener is still starting up, this call waits up to 1s for the
    /// OS-assigned port to appear — NWListener reports it asynchronously in
    /// its state-update handler.
    func rewrite(originURL: URL) -> URL? {
        if port == 0 {
            for _ in 0..<20 {
                if port > 0 { break }
                Thread.sleep(forTimeInterval: 0.05)
            }
        }
        guard port > 0 else { return nil }
        guard let scheme = originURL.scheme, (scheme == "http" || scheme == "https"),
              let host = originURL.host else { return nil }
        originLock.lock()
        self.originScheme = scheme
        self.originHost = host
        self.originPort = originURL.port
        originLock.unlock()
        // Reconstruct via URL string — URLComponents.percentEncodedPath/Query
        // setters precondition-fail if the value isn't already percent-encoded,
        // and URLComponents.path/query auto-encoding occasionally trips on
        // query-param characters. String concatenation of already-valid pieces
        // from originURL is the safest path.
        let pathPart = originURL.path.isEmpty ? "/" : originURL.path
        var built = "http://127.0.0.1:\(port)\(pathPart)"
        if let q = originURL.query, !q.isEmpty { built += "?\(q)" }
        return URL(string: built)
    }

    fileprivate func currentOrigin() -> (scheme: String, host: String, port: Int?) {
        originLock.lock(); defer { originLock.unlock() }
        return (originScheme, originHost, originPort)
    }

    // MARK: - Connection handling

    private func handle(_ conn: NWConnection) {
        let session = ConnectionSession(conn: conn, proxy: self, queue: queue)
        conn.stateUpdateHandler = { state in
            switch state {
            case .ready:
                session.start()
            case .failed(let err):
                Swift.print("[LOCAL_PROXY] conn failed \(conn.endpoint) \(err)")
                conn.cancel()
            case .cancelled:
                break
            default:
                break
            }
        }
        conn.start(queue: queue)
    }

    fileprivate func registerTask(_ id: Int, session: ConnectionSession) {
        sessionsByTaskID[id] = session
    }

    fileprivate func unregisterTask(_ id: Int) -> ConnectionSession? {
        return sessionsByTaskID.removeValue(forKey: id)
    }

    fileprivate func lookupTask(_ id: Int) -> ConnectionSession? {
        return sessionsByTaskID[id]
    }
}

extension LocalHTTPProxy: URLSessionDataDelegate {
    func urlSession(_ session: URLSession,
                    dataTask: URLSessionDataTask,
                    didReceive response: URLResponse,
                    completionHandler: @escaping (URLSession.ResponseDisposition) -> Void) {
        lookupTask(dataTask.taskIdentifier)?.onUpstreamResponse(response)
        completionHandler(.allow)
    }

    func urlSession(_ session: URLSession,
                    dataTask: URLSessionDataTask,
                    didReceive data: Data) {
        lookupTask(dataTask.taskIdentifier)?.onUpstreamChunk(data)
    }

    func urlSession(_ session: URLSession,
                    task: URLSessionTask,
                    didCompleteWithError error: Error?) {
        unregisterTask(task.taskIdentifier)?.onUpstreamComplete(error: error)
    }
}

/// Handles a single inbound HTTP/1.1 connection: reads the request line and
/// headers, forwards the request to the upstream origin via URLSession, and
/// streams the response back chunk-by-chunk to the client connection. One
/// request per connection (Connection: close).
final class ConnectionSession {
    private let conn: NWConnection
    private unowned let proxy: LocalHTTPProxy
    private let queue: DispatchQueue
    private var headerBuffer = Data()
    private var didEmitStart = false
    private var finished = false
    private var method = "GET"
    private var headersSentToClient = false
    private var usingChunkedTE = false
    private var upstreamTask: URLSessionDataTask?
    // Queued writes back to the client — we process sequentially to avoid
    // overlapping sends on the same NWConnection.
    private var writeQueue: [Data] = []
    private var writing = false

    init(conn: NWConnection, proxy: LocalHTTPProxy, queue: DispatchQueue) {
        self.conn = conn
        self.proxy = proxy
        self.queue = queue
    }

    func start() {
        readMoreHeaderBytes()
    }

    private func readMoreHeaderBytes() {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 16 * 1024) { [weak self] data, _, isComplete, error in
            guard let self else { return }
            if let error = error {
                self.fail("receive error: \(error)")
                return
            }
            if let data = data, !data.isEmpty {
                self.headerBuffer.append(data)
                if self.headerBuffer.count > 64 * 1024 {
                    self.fail("headers too long")
                    return
                }
                if let terminator = self.findHeaderTerminator() {
                    self.onHeadersReady(terminatorEnd: terminator)
                    return
                }
            }
            if isComplete {
                self.fail("connection closed before headers")
                return
            }
            self.readMoreHeaderBytes()
        }
    }

    private func findHeaderTerminator() -> Int? {
        // Look for \r\n\r\n; return index of end-of-headers.
        let pattern = Data([0x0D, 0x0A, 0x0D, 0x0A])
        if let r = headerBuffer.range(of: pattern) {
            return r.upperBound
        }
        return nil
    }

    private func onHeadersReady(terminatorEnd: Int) {
        let headerData = headerBuffer.subdata(in: 0..<terminatorEnd)
        guard let raw = String(data: headerData, encoding: .utf8) else {
            fail("non-utf8 headers")
            return
        }
        let lines = raw.replacingOccurrences(of: "\r\n", with: "\n")
                       .split(separator: "\n", omittingEmptySubsequences: false)
        guard let requestLine = lines.first else {
            fail("empty request")
            return
        }
        let parts = requestLine.split(separator: " ", maxSplits: 2)
        guard parts.count >= 2 else {
            fail("bad request line: \(requestLine)")
            return
        }
        let method = String(parts[0])
        let path = String(parts[1])
        guard method == "GET" || method == "HEAD" else {
            writeStatusLineAndClose(status: 405, reason: "Method Not Allowed")
            return
        }
        // Parse headers into dict (last value wins — good enough).
        var headers: [String: String] = [:]
        for line in lines.dropFirst() {
            if line.isEmpty { break }
            if let colon = line.firstIndex(of: ":") {
                let key = String(line[..<colon]).trimmingCharacters(in: .whitespaces)
                let value = String(line[line.index(after: colon)...]).trimmingCharacters(in: .whitespaces)
                if !key.isEmpty { headers[key.lowercased()] = value }
            }
        }
        guard let originURL = buildOriginURL(fromPath: path) else {
            writeStatusLineAndClose(status: 400, reason: "No Origin Set")
            return
        }
        forward(method: method, originURL: originURL, clientHeaders: headers)
    }

    /// Maps inbound `/path?query` 1:1 to the currently configured origin.
    private func buildOriginURL(fromPath pathWithQuery: String) -> URL? {
        let origin = proxy.currentOrigin()
        guard !origin.host.isEmpty else { return nil }
        let hostPart: String = {
            if let p = origin.port { return "\(origin.host):\(p)" }
            return origin.host
        }()
        // pathWithQuery is what came in on the HTTP request line, so it's
        // already valid URL syntax (AVPlayer / URLSession always sends
        // pre-encoded). Pass through as-is.
        var built = "\(origin.scheme)://\(hostPart)"
        if pathWithQuery.isEmpty { built += "/" } else { built += pathWithQuery }
        return URL(string: built)
    }

    private func forward(method: String, originURL: URL, clientHeaders: [String: String]) {
        self.method = method
        var req = URLRequest(url: originURL)
        req.httpMethod = method
        req.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        // Forward a small safe subset of client headers.
        let passThrough = ["range", "accept", "accept-encoding", "if-none-match", "if-modified-since", "user-agent"]
        for key in passThrough {
            if let v = clientHeaders[key] { req.setValue(v, forHTTPHeaderField: key.capitalized) }
        }
        let task = proxy.session.dataTask(with: req)
        upstreamTask = task
        proxy.registerTask(task.taskIdentifier, session: self)
        proxy.tracker.requestStarted()
        if let result = proxy.tracker.recordRequestURL(originURL), result.isWrap {
            print("[DISCONTINUITY_FETCHED] seq=\(result.sequence) priorMax=\(result.priorMax) uri=\(originURL.absoluteString)")
        }
        didEmitStart = true
        task.resume()
    }

    // MARK: - URLSession delegate callbacks (forwarded from LocalHTTPProxy)

    fileprivate func onUpstreamResponse(_ response: URLResponse) {
        guard let http = response as? HTTPURLResponse else {
            writeStatusLineAndClose(status: 502, reason: "Non-HTTP upstream")
            return
        }
        writeHeaders(status: http.statusCode, headers: http.allHeaderFields, responseURL: http.url)
    }

    fileprivate func onUpstreamChunk(_ data: Data) {
        guard !data.isEmpty else { return }
        // If the client has gone away, drop the chunk — enqueueing would
        // produce one ECANCELED-failed send per chunk, flooding logs.
        if finished { return }
        proxy.tracker.chunkReceived(byteCount: data.count)
        if method != "HEAD" {
            enqueueWrite(data)
        }
    }

    fileprivate func onUpstreamComplete(error: Error?) {
        if let error = error {
            if !headersSentToClient {
                writeStatusLineAndClose(status: 502, reason: "Upstream Error")
            } else {
                Swift.print("[LOCAL_PROXY] upstream error after headers: \(error)")
                finishAndClose()
            }
            return
        }
        finishAndClose()
    }

    // MARK: - Client-facing writes

    private func writeHeaders(status: Int, headers: [AnyHashable: Any], responseURL: URL?) {
        var lines: [String] = ["HTTP/1.1 \(status) \(Self.reason(for: status))"]
        var headersOut: [String: String] = [:]
        var upstreamContentLength: String?
        for (k, v) in headers {
            guard let key = k as? String, let value = v as? String else { continue }
            let lower = key.lowercased()
            if lower == "content-length" {
                upstreamContentLength = value
                continue
            }
            // Drop hop-by-hop framing/encoding headers; URLSession has already
            // decoded any Content-Encoding (e.g. gzip) before handing bytes to us.
            if ["connection", "keep-alive", "transfer-encoding",
                "content-encoding", "te", "trailer", "upgrade",
                "proxy-authenticate", "proxy-authorization"].contains(lower) {
                continue
            }
            headersOut[key] = value
        }
        // Prefer Content-Length framing when upstream told us the size — AVURLAsset
        // is sensitive to chunked TE on 206 Partial Content responses (the byte-range
        // path used for HLS partials), and emits err=-12174 in its URLAsset internals
        // when it sees chunked framing where it expected a sized body. Fall back to
        // chunked only when the size is unknown (e.g. upstream itself used chunked TE).
        if let len = upstreamContentLength {
            headersOut["Content-Length"] = len
            usingChunkedTE = false
        } else {
            headersOut["Transfer-Encoding"] = "chunked"
            usingChunkedTE = true
        }
        headersOut["Connection"] = "close"
        for (k, v) in headersOut { lines.append("\(k): \(v)") }
        lines.append("")
        lines.append("")
        let headerBytes = lines.joined(separator: "\r\n").data(using: .utf8) ?? Data()
        headersSentToClient = true
        enqueueRaw(headerBytes)
    }

    private func enqueueWrite(_ chunk: Data) {
        if usingChunkedTE {
            // HTTP/1.1 chunked-transfer framing: "<hex-size>\r\n<bytes>\r\n"
            let size = String(chunk.count, radix: 16)
            var framed = Data()
            framed.append((size + "\r\n").data(using: .utf8) ?? Data())
            framed.append(chunk)
            framed.append("\r\n".data(using: .utf8) ?? Data())
            enqueueRaw(framed)
        } else {
            enqueueRaw(chunk)
        }
    }

    private func enqueueTerminator() {
        // "0\r\n\r\n" ends a chunked body.
        enqueueRaw(Data("0\r\n\r\n".utf8))
    }

    private func enqueueRaw(_ data: Data) {
        writeQueue.append(data)
        drainWrites()
    }

    private func drainWrites() {
        guard !writing, let next = writeQueue.first else { return }
        writing = true
        writeQueue.removeFirst()
        conn.send(content: next, completion: .contentProcessed { [weak self] error in
            guard let self else { return }
            self.writing = false
            if let error = error {
                // ECANCELED is expected — AVPlayer routinely abandons LL-HLS
                // partial fetches and variant-switch races, which cancels the
                // NWConnection. Log only unexpected errors.
                let isCancel: Bool = {
                    if case let .posix(code) = error { return code == .ECANCELED }
                    return false
                }()
                if !isCancel {
                    Swift.print("[LOCAL_PROXY] send error: \(error)")
                }
                self.finishAndClose()
                return
            }
            self.drainWrites()
        })
    }

    private func writeStatusLineAndClose(status: Int, reason: String) {
        let payload = "HTTP/1.1 \(status) \(reason)\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
        enqueueRaw(payload.data(using: .utf8) ?? Data())
        finishAndClose(afterDrain: true)
    }

    private func fail(_ why: String) {
        Swift.print("[LOCAL_PROXY] \(why)")
        finishAndClose()
    }

    private func finishAndClose(afterDrain: Bool = false) {
        guard !finished else { return }
        finished = true
        // Cancel the upstream task so URLSession stops delivering chunks we'd
        // only fail to forward and log noise about.
        upstreamTask?.cancel()
        upstreamTask = nil
        if didEmitStart { proxy.tracker.requestFinished() }
        if headersSentToClient && method != "HEAD" && usingChunkedTE {
            // Flush the chunked-encoding terminator so the client sees a clean
            // end-of-body before we tear the socket down. Only needed for
            // chunked TE — Content-Length-framed responses end implicitly when
            // the byte count is reached.
            let terminator = Data("0\r\n\r\n".utf8)
            writeQueue.append(terminator)
            drainWrites()
        }
        // Give the queue a moment to drain before cancelling. Simple deferred cancel.
        queue.asyncAfter(deadline: .now() + .milliseconds(50)) { [conn = self.conn] in
            conn.cancel()
        }
    }

    static func reason(for status: Int) -> String {
        switch status {
        case 200: return "OK"
        case 206: return "Partial Content"
        case 301: return "Moved Permanently"
        case 302: return "Found"
        case 304: return "Not Modified"
        case 400: return "Bad Request"
        case 404: return "Not Found"
        case 405: return "Method Not Allowed"
        case 416: return "Range Not Satisfiable"
        case 500: return "Internal Server Error"
        case 502: return "Bad Gateway"
        case 504: return "Gateway Timeout"
        default: return "OK"
        }
    }
}
