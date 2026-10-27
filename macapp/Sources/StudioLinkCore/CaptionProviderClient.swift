import Foundation

/// Connects to the local daemon as its caption provider and keeps the
/// connection up (loopback only, desktop token) [SL-P4-2].
public final class CaptionProviderClient: @unchecked Sendable {
    public enum Status: Equatable, Sendable {
        case stopped
        case connecting
        case connected
        /// Not connected; the reason is safe to show (no tokens or speech).
        case waiting(String)
    }

    private let baseURL: URL
    private let token: @Sendable () -> String?
    private let factory: CaptionRecognizerFactory
    private let name: String
    private let session: URLSession
    private let lock = NSLock()
    private var loop: Task<Void, Never>?
    private var socket: URLSessionWebSocketTask?
    private var onStatus: (@Sendable (Status) -> Void)?

    public init(baseURL: URL, name: String = "p2p.video on-device speech", factory: CaptionRecognizerFactory, token: @escaping @Sendable () -> String?) {
        self.baseURL = baseURL
        self.name = name
        self.factory = factory
        self.token = token
        let cfg = URLSessionConfiguration.ephemeral
        cfg.connectionProxyDictionary = [:] // never route loopback through a proxy
        cfg.timeoutIntervalForRequest = 3600
        self.session = URLSession(configuration: cfg)
    }

    /// The provider WebSocket URL for a daemon base URL (http → ws).
    public static func providerURL(_ base: URL) -> URL {
        var c = URLComponents(url: base.appendingPathComponent(CaptionProtocol.providerPath), resolvingAgainstBaseURL: false)!
        c.scheme = c.scheme == "https" ? "wss" : "ws"
        return c.url!
    }

    /// Reconnect delays: 1, 2, 4, 8, 16, then 30 s.
    public static func backoff(attempt: Int) -> Double {
        min(30, pow(2, Double(min(attempt, 5))))
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
            try? await Task.sleep(for: .seconds(Self.backoff(attempt: attempt)))
        }
    }

    /// One connection lifetime. Returns true if the daemon accepted us.
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
        let router = CaptionStreamRouter(factory: factory) { msg in
            ws.send(.string(String(decoding: msg.json(), as: UTF8.self))) { _ in }
        }
        defer {
            Task { await router.reset() }
            ws.cancel(with: .normalClosure, reason: nil)
        }
        do {
            let hello = CaptionMessage.makeHello(name: name, engine: factory.engine, languages: await factory.supportedLanguages())
            try await ws.send(.string(String(decoding: hello.json(), as: UTF8.self)))
            guard case .string(let first) = try await ws.receive(),
                  let ready = CaptionMessage.parse(Data(first.utf8)), ready.t == CaptionMessage.ready else {
                report(.waiting("The host refused the caption provider."))
                return false
            }
            report(.connected)
            while !Task.isCancelled {
                switch try await ws.receive() {
                case .data(let data):
                    if let frame = try? CaptionFrame.decode(data) { await router.handleAudio(frame) }
                case .string(let text):
                    if let msg = CaptionMessage.parse(Data(text.utf8)) { await router.handle(msg) }
                @unknown default:
                    break
                }
            }
            return true
        } catch {
            let code = (ws.response as? HTTPURLResponse)?.statusCode
            switch code {
            case 101?:
                report(.waiting("The connection to the host closed. Reconnecting…"))
                return true
            case 409?: report(.waiting("Another caption provider is connected to this host."))
            case 401?, 403?: report(.waiting("The host did not accept this app's desktop token."))
            case 503?: report(.waiting("This host build has no live captions."))
            default: report(.waiting("The host is not reachable."))
            }
            return false
        }
    }
}
