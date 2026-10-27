import AVFoundation
import Foundation
import Speech
import StudioLinkCore

/// Apple on-device speech for live captions [SL-P4-2].
///
/// macOS 26+: `SpeechAnalyzer` + `SpeechTranscriber`, which run on device by
/// design (no server path), have no per-request duration limit, report
/// volatile and final results with audio time ranges, and natively accept the
/// protocol's 16 kHz mono Int16 audio. They don't require the Speech
/// Recognition (TCC) prompt.
///
/// macOS 14–25: `SFSpeechRecognizer` with `requiresOnDeviceRecognition`
/// (fails closed when on-device recognition is unavailable), rotated every
/// 12 s like Minutes' live captions. That path needs Speech Recognition
/// permission, which only an app bundle can request.
public struct AppleSpeechRecognizerFactory: CaptionRecognizerFactory {
    public enum Engine: String, Sendable {
        case speechAnalyzer = "apple-speechtranscriber"
        case sfSpeech = "apple-sfspeech-ondevice"
    }

    public let preferred: Engine

    /// `STUDIOLINK_CAPTIONS_ENGINE=sfspeech` forces the fallback (testing).
    public init(environment: [String: String] = ProcessInfo.processInfo.environment) {
        if environment["STUDIOLINK_CAPTIONS_ENGINE"] == "sfspeech" {
            preferred = .sfSpeech
        } else if #available(macOS 26.0, *) {
            preferred = .speechAnalyzer
        } else {
            preferred = .sfSpeech
        }
    }

    public var engine: String { preferred.rawValue }

    public func supportedLanguages() async -> [String] {
        if preferred == .speechAnalyzer, #available(macOS 26.0, *) {
            return await SpeechTranscriber.supportedLocales.map { $0.identifier(.bcp47) }.sorted()
        }
        return SFSpeechRecognizer.supportedLocales().map { $0.identifier(.bcp47) }.sorted()
    }

    public func makeRecognizer(
        language: String,
        onUpdate: @escaping @Sendable (RecognizerUpdate) -> Void,
        onError: @escaping @Sendable (CaptionRecognizerError) -> Void
    ) async throws -> CaptionRecognizer {
        if preferred == .speechAnalyzer, #available(macOS 26.0, *) {
            return try await AnalyzerRecognizer.make(language: language, onUpdate: onUpdate, onError: onError)
        }
        return try SFSpeechStreamRecognizer(language: language, onUpdate: onUpdate, onError: onError)
    }
}

/// 16 kHz mono Int16, the protocol's audio format.
private func protocolFormat() -> AVAudioFormat {
    AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: Double(CaptionProtocol.sampleRate), channels: 1, interleaved: true)!
}

private func makeBuffer(_ samples: [Int16], format: AVAudioFormat) -> AVAudioPCMBuffer? {
    guard !samples.isEmpty, let buf = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(samples.count)) else { return nil }
    buf.frameLength = AVAudioFrameCount(samples.count)
    if let p = buf.int16ChannelData {
        samples.withUnsafeBufferPointer { p[0].update(from: $0.baseAddress!, count: samples.count) }
    } else if let p = buf.floatChannelData {
        for (i, s) in samples.enumerated() { p[0][i] = Float(s) / 32768 }
    } else {
        return nil
    }
    return buf
}

@available(macOS 26.0, *)
final class AnalyzerRecognizer: CaptionRecognizer, @unchecked Sendable {
    private let analyzer: SpeechAnalyzer
    private let input: AsyncStream<AnalyzerInput>.Continuation
    private let format: AVAudioFormat
    private let converter: AVAudioConverter?
    private let results: Task<Void, Never>
    private let lock = NSLock()
    private var done = false

    private init(analyzer: SpeechAnalyzer, input: AsyncStream<AnalyzerInput>.Continuation, format: AVAudioFormat, results: Task<Void, Never>) {
        self.analyzer = analyzer
        self.input = input
        self.format = format
        let source = protocolFormat()
        self.converter = format == source ? nil : AVAudioConverter(from: source, to: format)
        self.results = results
    }

    static func make(language: String, onUpdate: @escaping @Sendable (RecognizerUpdate) -> Void, onError: @escaping @Sendable (CaptionRecognizerError) -> Void) async throws -> AnalyzerRecognizer {
        guard SpeechTranscriber.isAvailable else {
            throw CaptionRecognizerError(code: "recognizer_unavailable", message: "On-device speech transcription is unavailable on this Mac.")
        }
        guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: language)) else {
            throw CaptionRecognizerError(code: "unsupported_language", message: "Live captions don't support \(language) on this Mac.")
        }
        let transcriber = SpeechTranscriber(locale: locale, transcriptionOptions: [], reportingOptions: [.volatileResults], attributeOptions: [.audioTimeRange])
        do {
            // Downloads the on-device model once (from Apple, like a system
            // language pack); no audio leaves the Mac.
            if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await request.downloadAndInstall()
            }
        } catch {
            throw CaptionRecognizerError(code: "model_unavailable", message: "The on-device speech model for \(language) could not be installed.")
        }
        let format = await SpeechAnalyzer.bestAvailableAudioFormat(compatibleWith: [transcriber]) ?? protocolFormat()
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        let (stream, continuation) = AsyncStream<AnalyzerInput>.makeStream()
        let results = Task {
            do {
                for try await r in transcriber.results {
                    onUpdate(RecognizerUpdate(text: String(r.text.characters), isFinal: r.isFinal, start: r.range.start.seconds, end: r.range.end.seconds))
                }
            } catch is CancellationError {
            } catch {
                onError(CaptionRecognizerError(code: "recognizer_failed", message: "On-device recognition stopped."))
            }
        }
        do {
            try await analyzer.start(inputSequence: stream)
        } catch {
            results.cancel()
            throw CaptionRecognizerError(code: "recognizer_failed", message: "On-device recognition could not start.")
        }
        return AnalyzerRecognizer(analyzer: analyzer, input: continuation, format: format, results: results)
    }

    func append(_ samples: [Int16]) {
        guard !lock.withLock({ done }), let source = makeBuffer(samples, format: protocolFormat()) else { return }
        guard let converter else {
            input.yield(AnalyzerInput(buffer: source))
            return
        }
        let ratio = format.sampleRate / source.format.sampleRate
        guard let out = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(Double(source.frameLength) * ratio) + 32) else { return }
        nonisolated(unsafe) var consumed = false
        var err: NSError?
        converter.convert(to: out, error: &err) { _, status in
            if consumed { status.pointee = .noDataNow; return nil }
            consumed = true
            status.pointee = .haveData
            return source
        }
        if err == nil, out.frameLength > 0 { input.yield(AnalyzerInput(buffer: out)) }
    }

    func finish() {
        guard lock.withLock({ let was = done; done = true; return !was }) else { return }
        input.finish()
        let analyzer = self.analyzer
        Task { try? await analyzer.finalizeAndFinishThroughEndOfInput() }
    }

    func cancel() {
        guard lock.withLock({ let was = done; done = true; return !was }) else { return }
        input.finish()
        results.cancel()
        let analyzer = self.analyzer
        Task { await analyzer.cancelAndFinishNow() }
    }
}

/// Fallback for macOS 14–25. Recognition tasks are rotated every
/// `rotateSeconds` of audio so captions stay short and the per-request limit
/// is never hit; each task's results are offset by where it started.
final class SFSpeechStreamRecognizer: CaptionRecognizer, @unchecked Sendable {
    static let rotateSeconds = 12

    private let recognizer: SFSpeechRecognizer
    private let onUpdate: @Sendable (RecognizerUpdate) -> Void
    private let onError: @Sendable (CaptionRecognizerError) -> Void
    private let lock = NSLock()
    private var request: SFSpeechAudioBufferRecognitionRequest?
    private var task: SFSpeechRecognitionTask?
    private var taskStartSamples = 0
    private var samples = 0
    private var generation = 0
    private var done = false

    init(language: String, onUpdate: @escaping @Sendable (RecognizerUpdate) -> Void, onError: @escaping @Sendable (CaptionRecognizerError) -> Void) throws {
        guard SFSpeechRecognizer.authorizationStatus() == .authorized else {
            throw CaptionRecognizerError(code: "not_authorized", message: "Allow Speech Recognition for p2p.video in System Settings › Privacy & Security.")
        }
        // Fail closed: never fall back to server recognition.
        guard let r = SFSpeechRecognizer(locale: Locale(identifier: language)), r.isAvailable, r.supportsOnDeviceRecognition else {
            throw CaptionRecognizerError(code: "unsupported_language", message: "On-device recognition for \(language) is unavailable on this Mac.")
        }
        recognizer = r
        self.onUpdate = onUpdate
        self.onError = onError
    }

    func append(_ chunk: [Int16]) {
        guard let buf = makeBuffer(chunk, format: protocolFormat()) else { return }
        lock.withLock {
            guard !done else { return }
            if request == nil { startTaskLocked() }
            request?.append(buf)
            samples += chunk.count
            if samples - taskStartSamples >= Self.rotateSeconds * CaptionProtocol.sampleRate {
                request?.endAudio() // the task delivers its final result, then a new one starts
                request = nil
            }
        }
    }

    private func startTaskLocked() {
        let req = SFSpeechAudioBufferRecognitionRequest()
        req.requiresOnDeviceRecognition = true
        req.shouldReportPartialResults = true
        req.addsPunctuation = true
        generation += 1
        let gen = generation
        let offset = Double(samples) / Double(CaptionProtocol.sampleRate)
        taskStartSamples = samples
        request = req
        let onUpdate = self.onUpdate, onError = self.onError
        task = recognizer.recognitionTask(with: req) { [weak self] result, error in
            if let result {
                let segs = result.bestTranscription.segments
                let start = offset + (segs.first?.timestamp ?? 0)
                let end = offset + (segs.last.map { $0.timestamp + $0.duration } ?? 0)
                onUpdate(RecognizerUpdate(text: result.bestTranscription.formattedString, isFinal: result.isFinal, start: start, end: max(start, end)))
            }
            if let error = error as NSError?, result?.isFinal != true {
                // 1110: no speech in this window; not a failure.
                if !(error.domain == "kAFAssistantErrorDomain" && error.code == 1110) {
                    guard let self, self.lock.withLock({ !self.done && self.generation == gen }) else { return }
                    onError(CaptionRecognizerError(code: "recognizer_failed", message: "On-device recognition stopped."))
                }
            }
        }
    }

    func finish() {
        lock.withLock {
            done = true
            request?.endAudio()
            request = nil
        }
    }

    func cancel() {
        lock.withLock {
            done = true
            request?.endAudio()
            request = nil
            task?.cancel()
            task = nil
        }
    }
}
