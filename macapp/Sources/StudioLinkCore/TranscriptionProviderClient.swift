import Foundation

/// Connects to the local daemon as its file transcription provider and keeps
/// the connection up (loopback only, desktop token) [SL-P4-3]. A sibling of
/// `CaptionProviderClient` on its own socket, so post-take transcripts work
/// whether or not live captions are on.
public final class TranscriptionProviderClient: @unchecked Sendable {
    public enum Status: Equatable, Sendable {
        case stopped
        case connecting
        /// Connected; `availability` is what the host was told.
        case connected(TranscriptionAvailability)
        /// Transcribing a file for the host.
        case transcribing
        /// Not connected; the reason is safe to show (no tokens or speech).
        case waiting(String)
    }

    private let baseURL: URL
    private let token: @Sendable () -> String?
    private let transcriber: FileTranscriber
    private let name: String
    private let session: URLSession
    private let lock = NSLock()
    private var loop: Task<Void, Never>?
    private var socket: URLSessionWebSocketTask?
    private var onStatus: (@Sendable (Status) -> Void)?

    public init(baseURL: URL, name: String = "p2p.video on-device transcription", transcriber: FileTranscriber, token: @escaping @Sendable () -> String?) {
        self.baseURL = baseURL
        self.name = name
        self.transcriber = transcriber
        self.token = token
        let cfg = URLSessionConfiguration.ephemeral
        cfg.connectionProxyDictionary = [:] // never route loopback through a proxy
        cfg.timeoutIntervalForRequest = 3600
        self.session = URLSession(configuration: cfg)
    }

    /// The provider WebSocket URL for a daemon base URL (http → ws).
    public static func providerURL(_ base: URL) -> URL {
        var c = URLComponents(url: base.appendingPathComponent(FileTranscriptionProtocol.providerPath), resolvingAgainstBaseURL: false)!
        c.scheme = c.scheme == "https" ? "wss" : "ws"
        return c.url!
    }

    public func start(onStatus: @escaping @Sendable (Status) -> Void) {
        lock.lock()
        defer { lock.unlock() }
        guard loop == nil else { return }
        self.onStatus = onStatus
        loop = Task { [weak self] in await self?.run() }
    }

    public func stop() {
        lock.lock()
        let task = loop
        let ws = socket
        loop = nil
        socket = nil
        let status = onStatus
        lock.unlock()
        task?.cancel()
        ws?.cancel(with: .goingAway, reason: nil)
        status?(.stopped)
    }

    /// Re-announces availability (for example after Speech permission was
    /// granted) on the current connection.
    public func refreshAvailability() async {
        guard let ws = lock.withLock({ socket }) else { return }
        let availability = await transcriber.availability()
        ws.send(.string(String(decoding: TranscriptionMessage.makeStatus(availability).json(), as: UTF8.self))) { _ in }
        report(.connected(availability))
    }

    private func report(_ s: Status) {
        let status = lock.withLock { onStatus }
        status?(s)
    }

    private func run() async {
        var attempt = 0
        while !Task.isCancelled {
            let connected = await connectOnce()
            if Task.isCancelled { break }
            attempt = connected ? 0 : attempt + 1
            try? await Task.sleep(for: .seconds(CaptionProviderClient.backoff(attempt: attempt)))
        }
    }

    private func connectOnce() async -> Bool {
        guard let token = token() else {
            report(.waiting("The host has not created its desktop token yet."))
            return false
        }
        report(.connecting)
        var req = URLRequest(url: Self.providerURL(baseURL))
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let ws = session.webSocketTask(with: req)
        ws.maximumMessageSize = 1 << 20
        lock.withLock { socket = ws }
        ws.resume()
        let availability = await transcriber.availability()
        let runner = TranscriptionJobRunner(transcriber: transcriber) { [weak self] msg in
            ws.send(.string(String(decoding: msg.json(), as: UTF8.self))) { _ in }
            if msg.t == TranscriptionMessage.done || msg.t == TranscriptionMessage.error {
                self?.report(.connected(availability))
            }
        }
        defer {
            Task { await runner.reset() }
            ws.cancel(with: .normalClosure, reason: nil)
        }
        do {
            let hello = TranscriptionMessage.makeHello(name: name, engine: transcriber.engine, languages: await transcriber.supportedLanguages(), availability: availability)
            try await ws.send(.string(String(decoding: hello.json(), as: UTF8.self)))
            guard case .string(let first) = try await ws.receive(),
                  let ready = TranscriptionMessage.parse(Data(first.utf8)), ready.t == TranscriptionMessage.ready else {
                report(.waiting("The host refused the transcription provider."))
                return false
            }
            report(.connected(availability))
            while !Task.isCancelled {
                if case .string(let text) = try await ws.receive(), let msg = TranscriptionMessage.parse(Data(text.utf8)) {
                    if msg.t == TranscriptionMessage.job { report(.transcribing) }
                    await runner.handle(msg)
                }
            }
            return true
        } catch {
            let code = (ws.response as? HTTPURLResponse)?.statusCode
            switch code {
            case 101?:
                report(.waiting("The connection to the host closed. Reconnecting…"))
                return true
            case 409?: report(.waiting("Another transcription provider is connected to this host."))
            case 401?, 403?: report(.waiting("The host did not accept this app's desktop token."))
            case 404?, 503?: report(.waiting("This host build has no built-in transcription."))
            default: report(.waiting("The host is not reachable."))
            }
            return false
        }
    }
}
