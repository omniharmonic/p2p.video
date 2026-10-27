import Foundation
import StudioLinkCore
import StudioLinkSpeech
import XCTest

/// Real on-device recognition of synthetic speech [SL-P4-3]. Opt-in with
/// `STUDIOLINK_REAL_SPEECH=1 swift test --filter RealFileTranscription`:
/// it runs Apple's recognizer (and may download its model once). Speech is
/// generated with `say -o` and converted to a 48 kHz/24-bit WAV like an
/// export track. Nothing leaves the Mac.
final class RealFileTranscriptionTests: XCTestCase {
    private var dir: URL!

    override func setUpWithError() throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["STUDIOLINK_REAL_SPEECH"] == "1", "set STUDIOLINK_REAL_SPEECH=1 to run Apple speech recognition")
        dir = FileManager.default.temporaryDirectory.appendingPathComponent("sl-real-speech-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        if let dir { try? FileManager.default.removeItem(at: dir) }
    }

    private func run(_ tool: String, _ args: [String]) throws {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: tool)
        p.arguments = args
        try p.run()
        p.waitUntilExit()
        XCTAssertEqual(p.terminationStatus, 0, "\(tool) \(args)")
    }

    private func speechWAV(_ text: String, voice: String = "Samantha") throws -> URL {
        let aiff = dir.appendingPathComponent("\(voice).aiff"), wav = dir.appendingPathComponent("\(voice).wav")
        try run("/usr/bin/say", ["-v", voice, "-o", aiff.path, text])
        try run("/usr/bin/afconvert", ["-f", "WAVE", "-d", "LEI24@48000", aiff.path, wav.path])
        return wav
    }

    private func transcribe(_ file: URL) async throws -> (MessageCollector, [TranscriptionMessage]) {
        let log = MessageCollector()
        let runner = TranscriptionJobRunner(transcriber: AppleFileTranscriber()) { log.add($0) }
        var job = TranscriptionMessage(t: TranscriptionMessage.job)
        job.jobId = "real1"
        job.path = file.path
        job.language = "en-US"
        await runner.handle(job)
        await runner.waitForIdle()
        return (log, log.all)
    }

    func testSaySpeechIsRecognizedWithWordTimings() async throws {
        let transcriber = AppleFileTranscriber()
        let availability = await transcriber.availability()
        try XCTSkipUnless(availability == .ready, "recognizer not ready: \(availability)")
        let wav = try speechWAV("Welcome to the show. Today we are talking about recording podcasts on a Mac.")
        let (_, messages) = try await transcribe(wav)
        XCTAssertEqual(messages.last?.t, "done", "\(messages.last.map { String(decoding: $0.json(), as: UTF8.self) } ?? "")")
        let segments = messages.compactMap(\.segment)
        let text = segments.map(\.text).joined(separator: " ").lowercased()
        XCTAssertTrue(text.contains("welcome"), text)
        XCTAssertTrue(text.contains("show"), text)
        XCTAssertTrue(text.contains("podcast"), text)
        let words = segments.flatMap(\.words)
        XCTAssertGreaterThan(words.count, 8)
        XCTAssertTrue(words.allSatisfy { $0.end >= $0.start && $0.start >= 0 && $0.confidence >= 0 && $0.confidence <= 1 })
        XCTAssertTrue(zip(words, words.dropFirst()).allSatisfy { $0.start <= $1.start }, "words in time order")
        XCTAssertNotNil(segments.first?.confidence)
    }

    func testSilenceIsAnEmptySuccess() async throws {
        let transcriber = AppleFileTranscriber()
        let availability = await transcriber.availability()
        try XCTSkipUnless(availability == .ready, "recognizer not ready: \(availability)")
        let silence = dir.appendingPathComponent("silence.aiff")
        try run("/usr/bin/say", ["-o", silence.path, "[[slnc 8000]]"])
        let (_, messages) = try await transcribe(silence)
        XCTAssertEqual(messages.last?.t, "done")
        XCTAssertTrue(messages.compactMap(\.segment).isEmpty, "silence produced text")
    }

    func testUnreadableFileIsAnExplicitError() async throws {
        let bogus = dir.appendingPathComponent("not-audio.wav")
        try Data("not audio".utf8).write(to: bogus)
        let (_, messages) = try await transcribe(bogus)
        XCTAssertEqual(messages.last?.t, "error")
        XCTAssertEqual(messages.last?.code, "audio_unreadable")
    }
}

final class MessageCollector: @unchecked Sendable {
    private let lock = NSLock()
    private var items: [TranscriptionMessage] = []
    func add(_ m: TranscriptionMessage) { lock.withLock { items.append(m) } }
    var all: [TranscriptionMessage] { lock.withLock { items } }
}
