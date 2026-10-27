import Foundation
import StudioLinkCore
import XCTest

/// A scripted recognizer: emits the given phrases and progress, or throws.
final class FakeFileTranscriber: FileTranscriber, @unchecked Sendable {
    var phrases: [RecognizedPhrase] = []
    var progress: [Double] = []
    var error: Error?
    var availabilityValue: TranscriptionAvailability = .ready
    /// When set, transcribe waits until cancelled (or released).
    var waitForCancel = false
    private let lock = NSLock()
    private(set) var calls: [(URL, String)] = []

    var engine: String { "fake-file" }
    func supportedLanguages() async -> [String] { ["en-US"] }
    func availability() async -> TranscriptionAvailability { availabilityValue }

    func transcribe(file: URL, language: String, onPhrase: @escaping @Sendable (RecognizedPhrase) -> Void, onProgress: @escaping @Sendable (Double) -> Void) async throws -> FileTranscriptionSummary {
        lock.withLock { calls.append((file, language)) }
        for p in phrases { onPhrase(p) }
        for s in progress { onProgress(s) }
        if waitForCancel {
            while !Task.isCancelled { try? await Task.sleep(for: .milliseconds(5)) }
            throw CancellationError()
        }
        if let error { throw error }
        return FileTranscriptionSummary(durationSec: 9.5, locale: language, asr: "fake-asr")
    }
}

final class MessageLog: @unchecked Sendable {
    private let lock = NSLock()
    private var items: [TranscriptionMessage] = []
    func add(_ m: TranscriptionMessage) { lock.withLock { items.append(m) } }
    var all: [TranscriptionMessage] { lock.withLock { items } }
    var types: [String] { all.map(\.t) }
}

func job(_ id: String = "j1", path: String = "/pkg/audio/Ana.wav", language: String? = "en-US", duration: Double? = 9.5) -> TranscriptionMessage {
    var m = TranscriptionMessage(t: TranscriptionMessage.job)
    m.jobId = id
    m.path = path
    m.language = language
    m.durationSec = duration
    return m
}

final class FileTranscriptionProtocolTests: XCTestCase {
    func testFixtureMessagesRoundTrip() throws {
        let messages = try JSONDecoder().decode([TranscriptionMessage].self, from: Fixture.data("transcription-provider-messages.json"))
        XCTAssertEqual(messages.map(\.t), ["hello", "ready", "job", "progress", "segment", "done", "cancel", "error", "status"])
        XCTAssertEqual(messages[0].protocol, 1)
        XCTAssertEqual(messages[0].state, "ready")
        XCTAssertEqual(messages[4].segment?.words.count, 4)
        XCTAssertEqual(messages[4].segment?.confidence, 0.995)
        for m in messages { XCTAssertEqual(TranscriptionMessage.parse(m.json()), m) }
        // Unused fields are omitted on the wire.
        XCTAssertEqual(String(decoding: messages[1].json(), as: UTF8.self), #"{"protocol":1,"t":"ready"}"#)
    }

    func testHelloCarriesAvailability() {
        let hello = TranscriptionMessage.makeHello(name: "N", engine: "e", languages: ["en-US"], availability: .notAuthorized("Allow it"))
        XCTAssertEqual(hello.state, "not_authorized")
        XCTAssertEqual(hello.message, "Allow it")
        XCTAssertNil(TranscriptionMessage.makeHello(name: "N", engine: "e", languages: [], availability: .ready).message)
        XCTAssertEqual(TranscriptionMessage.makeStatus(.unavailable("x")).state, "unavailable")
    }

    func testProviderURL() {
        XCTAssertEqual(TranscriptionProviderClient.providerURL(URL(string: "http://127.0.0.1:7443")!).absoluteString, "ws://127.0.0.1:7443/api/v1/transcription/provider")
    }
}

final class FileSegmentBuilderTests: XCTestCase {
    func testWordsFromRunsJoinPunctuationAndPieces() {
        let runs = [
            RecognizedRun(text: "Welcome", start: 0, end: 0.48, confidence: 0.997),
            RecognizedRun(text: " to", start: 0.48, end: 0.6, confidence: 0.9),
            RecognizedRun(text: " the", start: 0.6, end: 0.72, confidence: nil),
            RecognizedRun(text: " pod", start: 0.72, end: 0.9, confidence: 0.8),
            RecognizedRun(text: "cast", start: 0.9, end: 1.1, confidence: 0.7),
            RecognizedRun(text: ".", start: nil, end: nil, confidence: nil),
            RecognizedRun(text: " untimed", start: nil, end: nil, confidence: 0.5),
        ]
        let words = FileSegmentBuilder.words(from: runs)
        XCTAssertEqual(words.map(\.word), ["Welcome", "to", "the", "podcast."])
        XCTAssertEqual(words[3].start, 0.72)
        XCTAssertEqual(words[3].end, 1.1)
        XCTAssertEqual(words[2].confidence, 0, "missing confidence is 0, never invented")
    }

    func testSegmentFoldsTextClampsTimesAndAveragesConfidence() throws {
        let phrase = RecognizedPhrase(text: "  Hello\n there ", start: -1, end: 12, runs: [
            RecognizedRun(text: "Hello", start: 0.2, end: 0.6, confidence: 0.9),
            RecognizedRun(text: " there", start: 0.6, end: 11, confidence: 0.7),
        ])
        let seg = try XCTUnwrap(FileSegmentBuilder.segment(from: phrase, durationSec: 10))
        XCTAssertEqual(seg.text, "Hello there")
        XCTAssertEqual(seg.start, 0)
        XCTAssertEqual(seg.end, 10)
        XCTAssertEqual(seg.words.last?.end, 10)
        XCTAssertEqual(seg.confidence, 0.8)
        XCTAssertNil(FileSegmentBuilder.segment(from: RecognizedPhrase(text: " \n", start: 0, end: 1, runs: [])))
        let noWords = try XCTUnwrap(FileSegmentBuilder.segment(from: RecognizedPhrase(text: "Hi", start: 1, end: 2, runs: [])))
        XCTAssertNil(noWords.confidence)
        XCTAssertEqual(noWords.words, [])
    }

    func testSegmentEncodesLikeTheGoContract() throws {
        let seg = FileSegment(start: 0, end: 1.14, text: "Welcome to the show.", confidence: 0.995, words: [FileWord(word: "Welcome", start: 0, end: 0.48, confidence: 0.997)])
        let json = try JSONSerialization.jsonObject(with: JSONEncoder().encode(seg)) as? [String: Any]
        XCTAssertEqual(Set(json?.keys.map { $0 } ?? []), ["start", "end", "text", "confidence", "words"])
    }
}

final class ChunkPlannerTests: XCTestCase {
    func testShortFileIsOneChunk() {
        XCTAssertEqual(ChunkPlanner.plan(durationSec: 30, maxChunkSec: 50), [0...30])
        XCTAssertEqual(ChunkPlanner.plan(durationSec: 0, maxChunkSec: 50), [])
    }

    func testLongFileIsCutAtTheQuietestPointNearTheLimit() {
        // 120 s of "speech" (energy 1) with silence at 47.0 s and 93.5 s.
        var energy = [Float](repeating: 1, count: 1200)
        energy[470] = 0
        energy[935] = 0
        let chunks = ChunkPlanner.plan(durationSec: 120, maxChunkSec: 50, searchSec: 5, energy: energy)
        XCTAssertEqual(chunks.count, 3)
        XCTAssertEqual(chunks[0].upperBound, 47, accuracy: 0.001)
        XCTAssertEqual(chunks[1].upperBound, 93.5, accuracy: 0.001)
        XCTAssertEqual(chunks[2].upperBound, 120)
        XCTAssertTrue(chunks.allSatisfy { $0.upperBound - $0.lowerBound <= 50 })
    }

    func testSixtyMinutesWithoutEnergyUsesFixedChunks() {
        let chunks = ChunkPlanner.plan(durationSec: 3720, maxChunkSec: 50)
        XCTAssertEqual(chunks.count, 75)
        XCTAssertEqual(chunks.first?.lowerBound, 0)
        XCTAssertEqual(chunks.last?.upperBound, 3720)
        for (a, b) in zip(chunks, chunks.dropFirst()) { XCTAssertEqual(a.upperBound, b.lowerBound) }
    }
}

final class FileJobValidatorTests: XCTestCase {
    func testRejectsBadJobs() {
        let exists: (URL) -> Bool = { _ in true }
        let cases: [(TranscriptionMessage, String)] = [
            (job("", path: "/a.wav"), "bad_job"),
            (job("bad id!", path: "/a.wav"), "bad_job"),
            (job(path: "relative.wav"), "bad_job"),
            (job(path: "/pkg/../etc/a.wav"), "bad_job"),
            (job(path: "/pkg/a.mp3"), "audio_unreadable"),
            (job(language: "en US"), "unsupported_language"),
        ]
        for (msg, code) in cases {
            guard case .failure(let err) = FileJobValidator.validate(msg, fileExists: exists) else { return XCTFail("\(msg) accepted") }
            XCTAssertEqual(err.code, code, "\(msg)")
        }
        guard case .failure(let missing) = FileJobValidator.validate(job(), fileExists: { _ in false }) else { return XCTFail() }
        XCTAssertEqual(missing.code, "audio_unreadable")
        guard case .success(let ok) = FileJobValidator.validate(job(path: "/pkg/audio/Ana.WAV", language: nil), fileExists: exists) else { return XCTFail() }
        XCTAssertEqual(ok.language, "en-US")
    }
}

final class TranscriptionJobRunnerTests: XCTestCase {
    func makeRunner(_ t: FakeFileTranscriber, _ log: MessageLog) -> TranscriptionJobRunner {
        TranscriptionJobRunner(transcriber: t, fileExists: { _ in true }) { log.add($0) }
    }

    func testJobReportsSegmentsThrottledProgressAndDone() async {
        let t = FakeFileTranscriber()
        t.phrases = [
            RecognizedPhrase(text: "Welcome back.", start: 0.3, end: 1.5, runs: [RecognizedRun(text: "Welcome", start: 0.3, end: 0.8, confidence: 0.9), RecognizedRun(text: " back.", start: 0.8, end: 1.5, confidence: 0.8)]),
            RecognizedPhrase(text: "   ", start: 2, end: 3, runs: []),
        ]
        t.progress = [0.5, 1, 2.6, 3, 5, 9.5]
        let log = MessageLog()
        let runner = makeRunner(t, log)
        await runner.handle(job())
        await runner.waitForIdle()
        XCTAssertEqual(log.types, ["segment", "progress", "progress", "progress", "progress", "done"])
        XCTAssertEqual(log.all.filter { $0.t == "progress" }.compactMap(\.processedSec), [0.5, 2.6, 5, 9.5])
        let seg = log.all[0].segment
        XCTAssertEqual(seg?.words.map(\.word), ["Welcome", "back."])
        XCTAssertEqual(seg?.confidence, 0.85)
        XCTAssertEqual(log.all.last?.asr, "fake-asr")
        XCTAssertEqual(log.all.last?.jobId, "j1")
        XCTAssertEqual(t.calls.first?.0.path, "/pkg/audio/Ana.wav")
        let running = await runner.runningJob
        XCTAssertNil(running)
    }

    func testSilenceIsDoneWithNoSegments() async {
        let t = FakeFileTranscriber()
        let log = MessageLog()
        let runner = makeRunner(t, log)
        await runner.handle(job())
        await runner.waitForIdle()
        XCTAssertEqual(log.types, ["done"])
    }

    func testRecognizerErrorsKeepTheirCode() async {
        let t = FakeFileTranscriber()
        t.error = CaptionRecognizerError(code: "not_authorized", message: TranscriptionAvailability.permissionMessage)
        let log = MessageLog()
        let runner = makeRunner(t, log)
        await runner.handle(job())
        await runner.waitForIdle()
        XCTAssertEqual(log.all.last?.t, "error")
        XCTAssertEqual(log.all.last?.code, "not_authorized")
        XCTAssertTrue(log.all.last?.message?.contains("Speech Recognition") ?? false)

        t.error = NSError(domain: "x", code: 1)
        await runner.handle(job("j2"))
        await runner.waitForIdle()
        XCTAssertEqual(log.all.last?.code, "recognizer_failed")
        XCTAssertEqual(log.all.last?.jobId, "j2")
    }

    func testCancelAndBusy() async throws {
        let t = FakeFileTranscriber()
        t.waitForCancel = true
        let log = MessageLog()
        let runner = makeRunner(t, log)
        await runner.handle(job("j1"))
        await runner.handle(job("j2"))
        XCTAssertEqual(log.all.first?.code, "busy")
        XCTAssertEqual(log.all.first?.jobId, "j2")
        var wrong = TranscriptionMessage(t: TranscriptionMessage.cancel)
        wrong.jobId = "j9"
        await runner.handle(wrong)
        let still = await runner.runningJob
        XCTAssertEqual(still, "j1")
        var cancel = TranscriptionMessage(t: TranscriptionMessage.cancel)
        cancel.jobId = "j1"
        await runner.handle(cancel)
        await runner.waitForIdle()
        XCTAssertEqual(log.all.last?.code, "cancelled")
        XCTAssertEqual(log.all.last?.jobId, "j1")
    }

    func testInvalidJobIsRejectedWithoutTranscribing() async {
        let t = FakeFileTranscriber()
        let log = MessageLog()
        let runner = makeRunner(t, log)
        await runner.handle(job(path: "relative.wav"))
        XCTAssertEqual(log.types, ["error"])
        XCTAssertEqual(log.all[0].code, "bad_job")
        XCTAssertTrue(t.calls.isEmpty)
    }

    func testDesktopSettingsDefaultTranscriptionOnAndTolerantDecode() throws {
        XCTAssertTrue(DesktopSettings().transcriptionProvider)
        let old = try JSONDecoder().decode(DesktopSettings.self, from: Data(#"{"guestAccess":"off","captionProvider":false}"#.utf8))
        XCTAssertTrue(old.transcriptionProvider, "settings from an older app default to on")
        var s = DesktopSettings()
        s.transcriptionProvider = false
        let again = try JSONDecoder().decode(DesktopSettings.self, from: JSONEncoder().encode(s))
        XCTAssertFalse(again.transcriptionProvider)
    }
}
