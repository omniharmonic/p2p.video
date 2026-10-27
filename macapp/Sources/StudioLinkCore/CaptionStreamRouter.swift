import Foundation

/// Routes daemon messages to one recognizer per stream and recognizer output
/// back as provider messages. Transport independent: the WebSocket client and
/// the CLI harness both drive it. Audio and text are never logged.
public actor CaptionStreamRouter {
    public static let maxStreams = 32
    /// Audio buffered per stream while its recognizer starts (10 s).
    public static let maxPendingSamples = 10 * CaptionProtocol.sampleRate

    private struct Stream {
        var language: String
        var recognizer: CaptionRecognizer?
        var pending: [Int16] = []
        var ended = false
        var updates: AsyncStream<RecognizerUpdate>.Continuation
    }

    private let factory: CaptionRecognizerFactory
    private let send: @Sendable (CaptionMessage) -> Void
    private var streams: [String: Stream] = [:]
    private var generation = 0

    public init(factory: CaptionRecognizerFactory, send: @escaping @Sendable (CaptionMessage) -> Void) {
        self.factory = factory
        self.send = send
    }

    public var activeStreams: Int { streams.count }

    /// A JSON control message from the daemon.
    public func handle(_ msg: CaptionMessage) {
        switch msg.t {
        case CaptionMessage.streamStart:
            guard let id = msg.streamId, !id.isEmpty else { return }
            start(id, language: msg.language ?? "en-US", sampleRate: msg.sampleRate ?? CaptionProtocol.sampleRate)
        case CaptionMessage.streamEnd:
            guard let id = msg.streamId else { return }
            end(id)
        default:
            break
        }
    }

    /// A binary audio frame from the daemon.
    public func handleAudio(_ frame: CaptionFrame) {
        guard var s = streams[frame.streamID], !s.ended else { return }
        if let r = s.recognizer {
            r.append(frame.samples)
            return
        }
        s.pending.append(contentsOf: frame.samples)
        if s.pending.count > Self.maxPendingSamples {
            s.pending.removeFirst(s.pending.count - Self.maxPendingSamples)
        }
        streams[frame.streamID] = s
    }

    /// The connection dropped: stop everything.
    public func reset() {
        generation += 1
        for s in streams.values {
            s.recognizer?.cancel()
            s.updates.finish()
        }
        streams.removeAll()
    }

    private func start(_ id: String, language: String, sampleRate: Int) {
        if streams[id] != nil { end(id) }
        guard sampleRate == CaptionProtocol.sampleRate else {
            send(.makeError(streamID: id, code: "unsupported_format", message: "Only 16 kHz PCM is supported."))
            return
        }
        guard streams.count < Self.maxStreams else {
            send(.makeError(streamID: id, code: "too_many_streams", message: "Too many caption streams."))
            return
        }
        // Updates are consumed in order by one task per stream.
        let (updates, continuation) = AsyncStream<RecognizerUpdate>.makeStream()
        streams[id] = Stream(language: language, updates: continuation)
        let send = self.send
        Task {
            var assembler = CaptionSegmentAssembler(streamID: id)
            for await u in updates {
                if let m = assembler.apply(u) { send(m) }
            }
        }
        let gen = generation
        let factory = self.factory
        Task {
            do {
                let recognizer = try await factory.makeRecognizer(
                    language: language,
                    onUpdate: { continuation.yield($0) },
                    onError: { err in
                        Task { await self.failed(id, err) }
                    })
                self.attach(id, recognizer, generation: gen)
            } catch let err as CaptionRecognizerError {
                self.failed(id, err)
            } catch {
                self.failed(id, CaptionRecognizerError(code: "recognizer_unavailable", message: "On-device speech recognition is unavailable."))
            }
        }
    }

    private func attach(_ id: String, _ recognizer: CaptionRecognizer, generation gen: Int) {
        guard gen == generation, var s = streams[id], s.recognizer == nil else {
            recognizer.cancel()
            return
        }
        if !s.pending.isEmpty {
            recognizer.append(s.pending)
            s.pending = []
        }
        s.recognizer = recognizer
        streams[id] = s
        if s.ended { finishStream(id) }
    }

    private func end(_ id: String) {
        guard var s = streams[id] else { return }
        s.ended = true
        streams[id] = s
        if s.recognizer != nil { finishStream(id) }
        // Otherwise attach() finishes it once the recognizer is ready.
    }

    private func finishStream(_ id: String) {
        guard let s = streams.removeValue(forKey: id) else { return }
        s.recognizer?.finish()
        // Let the recognizer deliver its last finals before closing the queue.
        let continuation = s.updates
        Task {
            try? await Task.sleep(for: .seconds(3))
            continuation.finish()
        }
    }

    private func failed(_ id: String, _ err: CaptionRecognizerError) {
        guard let s = streams.removeValue(forKey: id) else { return }
        s.recognizer?.cancel()
        s.updates.finish()
        send(.makeError(streamID: id, code: err.code, message: err.message))
    }
}
