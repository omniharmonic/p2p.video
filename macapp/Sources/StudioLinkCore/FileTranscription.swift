import Foundation

/// The p2p.video file transcription provider protocol, version 1 [SL-P4-3].
/// Spec: docs/integrations/TRANSCRIPTS.md. A sibling of the caption provider
/// protocol on its own loopback socket: the daemon asks the provider to
/// transcribe one published export WAV and receives word-timed segments.
/// Foundation-only, so another app can reuse it with its own recognizer.
public enum FileTranscriptionProtocol {
    public static let version = 1
    /// Loopback path the provider connects to on the daemon.
    public static let providerPath = "api/v1/transcription/provider"
    /// Audio containers the provider opens (export packages hold WAV).
    public static let allowedExtensions: Set<String> = ["wav", "wave", "aif", "aiff", "caf"]
    public static let maxJobIDLength = 64
    /// Progress is reported at most once per this much recognized audio.
    public static let progressStepSec = 2.0
}

/// One recognized word, seconds from the start of the file.
public struct FileWord: Codable, Equatable, Sendable {
    public var word: String
    public var start: Double
    public var end: Double
    public var confidence: Double

    public init(word: String, start: Double, end: Double, confidence: Double) {
        self.word = word
        self.start = start
        self.end = end
        self.confidence = confidence
    }
}

/// One utterance, seconds from the start of the file. The same shape as a
/// `minutes.file-transcript/1` segment minus the speaker (the daemon names it).
public struct FileSegment: Codable, Equatable, Sendable {
    public var start: Double
    public var end: Double
    public var text: String
    public var confidence: Double?
    public var words: [FileWord]

    public init(start: Double, end: Double, text: String, confidence: Double?, words: [FileWord]) {
        self.start = start
        self.end = end
        self.text = text
        self.confidence = confidence
        self.words = words
    }
}

/// Every message on the transcription provider socket (JSON text frames).
/// Unused fields are nil and omitted when encoding.
public struct TranscriptionMessage: Codable, Equatable, Sendable {
    public var t: String
    public var `protocol`: Int?
    public var name: String?
    public var engine: String?
    public var languages: [String]?
    /// hello / status: "ready", "not_authorized" or "unavailable".
    public var state: String?
    public var message: String?
    public var jobId: String?
    public var path: String?
    public var language: String?
    public var durationSec: Double?
    public var processedSec: Double?
    public var segment: FileSegment?
    public var locale: String?
    public var asr: String?
    public var code: String?

    public init(t: String) { self.t = t }

    // Provider → daemon
    public static let hello = "hello"
    public static let status = "status"
    public static let progress = "progress"
    public static let segmentType = "segment"
    public static let done = "done"
    public static let error = "error"
    // Daemon → provider
    public static let ready = "ready"
    public static let job = "job"
    public static let cancel = "cancel"

    public func json() -> Data {
        let enc = JSONEncoder()
        enc.outputFormatting = [.sortedKeys]
        return (try? enc.encode(self)) ?? Data()
    }

    public static func parse(_ data: Data) -> TranscriptionMessage? {
        try? JSONDecoder().decode(TranscriptionMessage.self, from: data)
    }

    public static func makeHello(name: String, engine: String, languages: [String], availability: TranscriptionAvailability) -> TranscriptionMessage {
        var m = TranscriptionMessage(t: hello)
        m.protocol = FileTranscriptionProtocol.version
        m.name = name
        m.engine = engine
        m.languages = languages
        m.state = availability.state
        m.message = availability.message
        return m
    }

    public static func makeStatus(_ availability: TranscriptionAvailability) -> TranscriptionMessage {
        var m = TranscriptionMessage(t: status)
        m.state = availability.state
        m.message = availability.message
        return m
    }

    public static func makeError(jobID: String?, code: String, message: String) -> TranscriptionMessage {
        var m = TranscriptionMessage(t: error)
        m.jobId = jobID
        m.code = code
        m.message = message
        return m
    }
}

/// Whether this Mac can transcribe files now. The message is shown to the
/// host as is, so it says what to do.
public enum TranscriptionAvailability: Equatable, Sendable {
    case ready
    case notAuthorized(String)
    case unavailable(String)

    public var state: String {
        switch self {
        case .ready: return "ready"
        case .notAuthorized: return "not_authorized"
        case .unavailable: return "unavailable"
        }
    }

    public var message: String? {
        switch self {
        case .ready: return nil
        case .notAuthorized(let m), .unavailable(let m): return m
        }
    }

    /// Shown when the fallback engine lacks Speech Recognition permission.
    public static let permissionMessage = "Speech Recognition is not allowed for p2p.video. Open p2p.video › Settings › Transcripts and choose Allow, or turn it on in System Settings › Privacy & Security › Speech Recognition, then retry."
}

/// What a recognizer reports for one finished phrase: the text, its audio
/// range and word-level runs. Times are seconds from the start of the file.
public struct RecognizedPhrase: Equatable, Sendable {
    public var text: String
    public var start: Double
    public var end: Double
    public var runs: [RecognizedRun]

    public init(text: String, start: Double, end: Double, runs: [RecognizedRun]) {
        self.text = text
        self.start = start
        self.end = end
        self.runs = runs
    }
}

/// One attributed run of a recognized phrase (a word, a word piece or
/// punctuation), with its audio range and confidence when the engine gave them.
public struct RecognizedRun: Equatable, Sendable {
    public var text: String
    public var start: Double?
    public var end: Double?
    public var confidence: Double?

    public init(text: String, start: Double?, end: Double?, confidence: Double?) {
        self.text = text
        self.start = start
        self.end = end
        self.confidence = confidence
    }
}

/// Summary returned by a finished file transcription.
public struct FileTranscriptionSummary: Equatable, Sendable {
    public var durationSec: Double
    public var locale: String
    public var asr: String

    public init(durationSec: Double, locale: String, asr: String) {
        self.durationSec = durationSec
        self.locale = locale
        self.asr = asr
    }
}

/// Transcribes one audio file on device. The Apple implementation lives in
/// StudioLinkSpeech; tests use a fake.
public protocol FileTranscriber: Sendable {
    var engine: String { get }
    func supportedLanguages() async -> [String]
    func availability() async -> TranscriptionAvailability
    /// Reports each final phrase in order and the seconds of audio processed.
    /// Throws `CaptionRecognizerError` (with a protocol error code) or
    /// `CancellationError` when the task is cancelled.
    func transcribe(
        file: URL,
        language: String,
        onPhrase: @escaping @Sendable (RecognizedPhrase) -> Void,
        onProgress: @escaping @Sendable (Double) -> Void
    ) async throws -> FileTranscriptionSummary
}

/// Turns recognizer phrases into protocol segments: whitespace folded, empty
/// phrases dropped, punctuation runs joined to the previous word, times
/// clamped to the file, segment confidence = mean word confidence.
public enum FileSegmentBuilder {
    public static let maxTextLength = 2000
    public static let maxWords = 500

    public static func segment(from phrase: RecognizedPhrase, durationSec: Double? = nil) -> FileSegment? {
        let text = fold(phrase.text)
        guard !text.isEmpty else { return nil }
        let limit = durationSec.flatMap { $0.isFinite && $0 > 0 ? $0 : nil }
        let words = Array(self.words(from: phrase.runs, durationSec: limit).prefix(maxWords))
        var start = clamp(phrase.start, limit)
        var end = max(start, clamp(phrase.end, limit))
        if let first = words.first, let last = words.last {
            start = min(start, first.start)
            end = max(end, last.end)
        }
        let confidence: Double? = words.isEmpty ? nil : round3(words.map(\.confidence).reduce(0, +) / Double(words.count))
        return FileSegment(start: round3(start), end: round3(end), text: String(text.prefix(maxTextLength)), confidence: confidence, words: words)
    }

    /// Word tokens from attributed runs. A run without letters or digits
    /// (punctuation) is appended to the previous word, and so is a run that
    /// continues a word (no whitespace between it and the previous run). Runs
    /// without a time range are skipped: they cannot be placed on the timeline.
    public static func words(from runs: [RecognizedRun], durationSec: Double? = nil) -> [FileWord] {
        var out: [FileWord] = []
        var previousRaw = ""
        for run in runs {
            let raw = run.text
            let token = fold(raw)
            defer { previousRaw = raw }
            guard !token.isEmpty else { continue }
            let continues = !out.isEmpty && !startsWithSpace(raw) && !endsWithSpace(previousRaw) && !previousRaw.isEmpty
            let isWord = token.unicodeScalars.contains { CharacterSet.alphanumerics.contains($0) }
            if !isWord || continues {
                if !out.isEmpty {
                    out[out.count - 1].word += token
                    if let e = run.end, e.isFinite { out[out.count - 1].end = round3(max(out[out.count - 1].end, clamp(e, durationSec))) }
                }
                continue
            }
            guard let s = run.start, let e = run.end, s.isFinite, e.isFinite else { continue }
            let start = clamp(s, durationSec)
            let end = max(start, clamp(e, durationSec))
            let conf = run.confidence.flatMap { $0.isFinite ? min(1, max(0, $0)) : nil } ?? 0
            out.append(FileWord(word: token, start: round3(start), end: round3(end), confidence: round3(conf)))
        }
        return out
    }

    private static func startsWithSpace(_ s: String) -> Bool { s.first?.isWhitespace ?? false }
    private static func endsWithSpace(_ s: String) -> Bool { s.last?.isWhitespace ?? false }

    static func fold(_ s: String) -> String {
        s.unicodeScalars.map { CharacterSet.controlCharacters.contains($0) ? " " : String($0) }.joined()
            .split(whereSeparator: { $0.isWhitespace }).joined(separator: " ")
    }

    static func clamp(_ v: Double, _ limit: Double?) -> Double {
        guard v.isFinite else { return 0 }
        var x = max(0, v)
        if let limit { x = min(x, limit) }
        return x
    }

    static func round3(_ v: Double) -> Double { (v * 1000).rounded() / 1000 }
}

/// Splits long audio for engines with a per-request limit (the pre-macOS 26
/// fallback). Each chunk is at most `maxChunkSec`; a cut is moved to the
/// quietest 100 ms frame within the last `searchSec` of the chunk so words are
/// rarely split. `energy` holds one RMS value per 100 ms frame (optional).
public enum ChunkPlanner {
    public static let frameSec = 0.1

    public static func plan(durationSec: Double, maxChunkSec: Double, searchSec: Double = 5, energy: [Float] = []) -> [ClosedRange<Double>] {
        guard durationSec.isFinite, durationSec > 0, maxChunkSec > 0 else { return [] }
        var chunks: [ClosedRange<Double>] = []
        var start = 0.0
        while durationSec - start > maxChunkSec {
            var cut = start + maxChunkSec
            let window = min(searchSec, maxChunkSec / 2)
            let lo = Int(((cut - window) / frameSec).rounded(.up))
            let hi = min(Int((cut / frameSec).rounded(.down)), energy.count - 1)
            if lo <= hi, lo >= 0 {
                var best = hi
                for i in stride(from: hi, through: lo, by: -1) where energy[i] < energy[best] { best = i }
                cut = Double(best) * frameSec
            }
            if cut <= start { cut = start + maxChunkSec }
            chunks.append(start...cut)
            start = cut
        }
        chunks.append(start...durationSec)
        return chunks
    }
}

/// Validates a daemon job before any file is opened.
public enum FileJobValidator {
    public static func validate(_ msg: TranscriptionMessage, fileExists: (URL) -> Bool) -> Result<(id: String, url: URL, language: String), CaptionRecognizerError> {
        guard let id = msg.jobId, !id.isEmpty, id.count <= FileTranscriptionProtocol.maxJobIDLength,
              id.unicodeScalars.allSatisfy({ CharacterSet.alphanumerics.contains($0) || $0 == "-" || $0 == "_" }) else {
            return .failure(CaptionRecognizerError(code: "bad_job", message: "The host sent a transcription job without a valid id."))
        }
        guard let path = msg.path, path.hasPrefix("/"), !path.split(separator: "/").contains(".."), !path.contains("\0") else {
            return .failure(CaptionRecognizerError(code: "bad_job", message: "The host sent a transcription job without an absolute file path."))
        }
        let url = URL(fileURLWithPath: path)
        guard FileTranscriptionProtocol.allowedExtensions.contains(url.pathExtension.lowercased()) else {
            return .failure(CaptionRecognizerError(code: "audio_unreadable", message: "Only WAV, AIFF and CAF audio can be transcribed."))
        }
        guard fileExists(url) else {
            return .failure(CaptionRecognizerError(code: "audio_unreadable", message: "The audio file to transcribe does not exist."))
        }
        let language = msg.language ?? "en-US"
        guard language.count <= 16, language.range(of: "^[a-z]{2,3}(-[A-Za-z0-9]{2,8}){0,2}$", options: .regularExpression) != nil else {
            return .failure(CaptionRecognizerError(code: "unsupported_language", message: "Unsupported transcription language."))
        }
        return .success((id, url, language))
    }

    /// A regular file (no directory, no symlink to a directory).
    public static func isRegularFile(_ url: URL) -> Bool {
        var isDir: ObjCBool = false
        return FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir) && !isDir.boolValue
    }
}

/// Runs daemon jobs one at a time against a `FileTranscriber` and reports
/// progress, segments and the result as protocol messages. Transport
/// independent: the WebSocket client and the CLI harness both drive it.
/// Audio and recognized text are never logged.
public actor TranscriptionJobRunner {
    private let transcriber: FileTranscriber
    private let fileExists: @Sendable (URL) -> Bool
    private let send: @Sendable (TranscriptionMessage) -> Void
    private var current: (id: String, task: Task<Void, Never>)?

    public init(transcriber: FileTranscriber, fileExists: @escaping @Sendable (URL) -> Bool = FileJobValidator.isRegularFile, send: @escaping @Sendable (TranscriptionMessage) -> Void) {
        self.transcriber = transcriber
        self.fileExists = fileExists
        self.send = send
    }

    /// The running job's id, if any.
    public var runningJob: String? { current?.id }

    public func handle(_ msg: TranscriptionMessage) {
        switch msg.t {
        case TranscriptionMessage.job: start(msg)
        case TranscriptionMessage.cancel:
            if let id = msg.jobId, id == current?.id { current?.task.cancel() }
        default: break
        }
    }

    /// The connection dropped: stop the running job without reporting.
    public func reset() {
        current?.task.cancel()
        current = nil
    }

    /// Waits for the running job to end (tests and the CLI).
    public func waitForIdle() async {
        while let task = current?.task { await task.value }
    }

    private func start(_ msg: TranscriptionMessage) {
        if current != nil {
            send(.makeError(jobID: msg.jobId, code: "busy", message: "This Mac is already transcribing another file."))
            return
        }
        let job: (id: String, url: URL, language: String)
        switch FileJobValidator.validate(msg, fileExists: fileExists) {
        case .failure(let err):
            send(.makeError(jobID: msg.jobId, code: err.code, message: err.message))
            return
        case .success(let ok): job = ok
        }
        let duration = msg.durationSec.flatMap { $0.isFinite && $0 > 0 ? $0 : nil }
        let transcriber = self.transcriber, send = self.send
        let task = Task {
            let reporter = ProgressReporter(jobID: job.id, send: send)
            do {
                let summary = try await transcriber.transcribe(
                    file: job.url, language: job.language,
                    onPhrase: { phrase in
                        guard let seg = FileSegmentBuilder.segment(from: phrase, durationSec: duration) else { return }
                        var m = TranscriptionMessage(t: TranscriptionMessage.segmentType)
                        m.jobId = job.id
                        m.segment = seg
                        send(m)
                    },
                    onProgress: { reporter.report($0) })
                try Task.checkCancellation()
                var done = TranscriptionMessage(t: TranscriptionMessage.done)
                done.jobId = job.id
                done.durationSec = FileSegmentBuilder.round3(summary.durationSec)
                done.locale = summary.locale
                done.asr = summary.asr
                send(done)
            } catch is CancellationError {
                send(.makeError(jobID: job.id, code: "cancelled", message: "Transcription was cancelled."))
            } catch let err as CaptionRecognizerError {
                send(.makeError(jobID: job.id, code: err.code, message: err.message))
            } catch {
                send(.makeError(jobID: job.id, code: "recognizer_failed", message: "On-device transcription failed."))
            }
            await self.finished(job.id)
        }
        current = (job.id, task)
    }

    private func finished(_ id: String) {
        if current?.id == id { current = nil }
    }
}

/// Sends `progress` at most once per `progressStepSec` of audio.
final class ProgressReporter: @unchecked Sendable {
    private let jobID: String
    private let send: @Sendable (TranscriptionMessage) -> Void
    private let lock = NSLock()
    private var last = -Double.infinity

    init(jobID: String, send: @escaping @Sendable (TranscriptionMessage) -> Void) {
        self.jobID = jobID
        self.send = send
    }

    func report(_ seconds: Double) {
        guard seconds.isFinite, seconds >= 0 else { return }
        let due: Bool = lock.withLock {
            guard seconds - last >= FileTranscriptionProtocol.progressStepSec else { return false }
            last = seconds
            return true
        }
        guard due else { return }
        var m = TranscriptionMessage(t: TranscriptionMessage.progress)
        m.jobId = jobID
        m.processedSec = FileSegmentBuilder.round3(seconds)
        send(m)
    }
}
