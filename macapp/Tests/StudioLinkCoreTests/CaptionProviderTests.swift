import Foundation
import StudioLinkCore
import XCTest

/// Golden frame shared with host/internal/captions (TestFrameGoldenAndFixture):
/// key "s1", samples [0, 1, -1, 32767, -32768].
let goldenFrameHex = "0102733100000100ffffff7f0080"

final class CaptionFrameTests: XCTestCase {
    func testGoldenFrameMatchesGo() throws {
        let frame = CaptionFrame(streamID: "s1", samples: [0, 1, -1, 32767, -32768])
        XCTAssertEqual(frame.encoded().map { String(format: "%02x", $0) }.joined(), goldenFrameHex)
        XCTAssertEqual(try CaptionFrame.decode(frame.encoded()), frame)
    }

    func testDecodeRejectsMalformedFrames() {
        let cases: [(String, [UInt8], CaptionFrame.DecodeError)] = [
            ("empty", [], .version),
            ("version", [2, 1, 0x61, 0, 0], .version),
            ("no key", [1, 0, 0, 0], .key),
            ("key too long", [1, 65] + Array(repeating: 0x61, count: 65) + [0, 0], .key),
            ("bad key byte", [1, 2, 0x61, 0x2f, 0, 0], .key),
            ("no audio", [1, 1, 0x61], .audio),
            ("odd audio", [1, 1, 0x61, 0, 0, 0], .audio),
            ("over 1 s", [1, 1, 0x61] + Array(repeating: 0, count: 32002), .audio),
        ]
        for (name, bytes, want) in cases {
            XCTAssertThrowsError(try CaptionFrame.decode(Data(bytes)), name) { XCTAssertEqual($0 as? CaptionFrame.DecodeError, want, name) }
        }
    }

    func testFixtureMessagesRoundTrip() throws {
        let messages = try JSONDecoder().decode([CaptionMessage].self, from: Fixture.data("caption-provider-messages.json"))
        XCTAssertEqual(messages.map(\.t), ["hello", "ready", "stream_start", "result", "result", "stream_end", "error"])
        XCTAssertEqual(messages[0].protocol, 1)
        XCTAssertEqual(messages[2].sampleRate, 16000)
        XCTAssertEqual(messages[4].isFinal, true)
        XCTAssertNil(messages[3].isFinal)
        for m in messages { XCTAssertEqual(CaptionMessage.parse(m.json()), m) }
        // Unused fields are omitted on the wire.
        let hello = String(decoding: CaptionMessage.makeHello(name: "N", engine: "e", languages: ["en-US"]).json(), as: UTF8.self)
        XCTAssertEqual(hello, #"{"engine":"e","languages":["en-US"],"name":"N","protocol":1,"t":"hello"}"#)
    }

    func testProviderURLAndBackoff() {
        XCTAssertEqual(CaptionProviderClient.providerURL(URL(string: "http://127.0.0.1:7443")!).absoluteString, "ws://127.0.0.1:7443/api/v1/captions/provider")
        XCTAssertEqual((0...7).map { CaptionProviderClient.backoff(attempt: $0) }, [1, 2, 4, 8, 16, 30, 30, 30])
    }
}

final class CaptionSegmentAssemblerTests: XCTestCase {
    func testPartialsShareAnIDUntilFinalThenANewSegmentStarts() {
        var a = CaptionSegmentAssembler(streamID: "s1")
        let p1 = a.apply(.init(text: " Welcome", isFinal: false, start: 0, end: 4))
        XCTAssertEqual(p1?.segmentId, "1")
        XCTAssertEqual(p1?.text, "Welcome")
        XCTAssertEqual(p1?.isFinal, false)
        XCTAssertNil(a.apply(.init(text: "Welcome ", isFinal: false, start: 0, end: 4)), "unchanged text is not resent")
        XCTAssertNil(a.apply(.init(text: "   ", isFinal: false, start: 0, end: 4)), "empty partials are skipped")
        let f = a.apply(.init(text: "Welcome to the show.", isFinal: true, start: 0, end: 2.2204))
        XCTAssertEqual(f?.segmentId, "1")
        XCTAssertEqual(f?.isFinal, true)
        XCTAssertEqual(f?.startMs, 0)
        XCTAssertEqual(f?.endMs, 2220)
        let p2 = a.apply(.init(text: " Today", isFinal: false, start: 2.22, end: 3.18))
        XCTAssertEqual(p2?.segmentId, "2")
        XCTAssertEqual(p2?.startMs, 2220)
        XCTAssertEqual(p2?.streamId, "s1")
    }

    func testEmptyFinalWithdrawsAPartialButIsOtherwiseSilent() {
        var a = CaptionSegmentAssembler(streamID: "s1")
        XCTAssertNil(a.apply(.init(text: "", isFinal: true, start: 0, end: 1)))
        _ = a.apply(.init(text: "uh", isFinal: false, start: 0, end: 1))
        let w = a.apply(.init(text: "", isFinal: true, start: 0, end: 1))
        XCTAssertEqual(w?.text, "")
        XCTAssertEqual(w?.isFinal, true)
        XCTAssertEqual(w?.segmentId, "2", "segment 1 was closed by the first empty final")
    }

    func testBadTimesAreClamped() {
        var a = CaptionSegmentAssembler(streamID: "s1")
        let m = a.apply(.init(text: "x", isFinal: true, start: .nan, end: -3))
        XCTAssertEqual(m?.startMs, 0)
        XCTAssertEqual(m?.endMs, 0)
    }
}

// MARK: router with a fake recognizer

final class FakeRecognizer: CaptionRecognizer, @unchecked Sendable {
    let language: String
    let onUpdate: @Sendable (RecognizerUpdate) -> Void
    private let lock = NSLock()
    private(set) var received: [Int16] = []
    private(set) var finished = false
    private(set) var cancelled = false

    init(language: String, onUpdate: @escaping @Sendable (RecognizerUpdate) -> Void) {
        self.language = language
        self.onUpdate = onUpdate
    }

    var sampleCount: Int { lock.withLock { received.count } }

    func append(_ samples: [Int16]) {
        let total: Int = lock.withLock { received += samples; return received.count }
        // Canned recognition: one partial, then a final once 0.3 s arrived.
        if total == samples.count { onUpdate(.init(text: "hello", isFinal: false, start: 0, end: 0.1)) }
        if total >= 4800 && total - samples.count < 4800 { onUpdate(.init(text: "hello world", isFinal: true, start: 0, end: 0.3)) }
    }
    func finish() { lock.withLock { finished = true } }
    func cancel() { lock.withLock { cancelled = true } }
}

final class FakeFactory: CaptionRecognizerFactory, @unchecked Sendable {
    let engine = "fake"
    private let lock = NSLock()
    private(set) var made: [FakeRecognizer] = []
    var delay: Duration = .zero
    func supportedLanguages() async -> [String] { ["en-US"] }
    func makeRecognizer(language: String, onUpdate: @escaping @Sendable (RecognizerUpdate) -> Void, onError: @escaping @Sendable (CaptionRecognizerError) -> Void) async throws -> CaptionRecognizer {
        if language == "xx-XX" { throw CaptionRecognizerError(code: "unsupported_language", message: "nope") }
        if delay != .zero { try? await Task.sleep(for: delay) }
        let r = FakeRecognizer(language: language, onUpdate: onUpdate)
        lock.withLock { made.append(r) }
        return r
    }
    var recognizers: [FakeRecognizer] { lock.withLock { made } }
}

final class Outbox: @unchecked Sendable {
    private let lock = NSLock()
    private var items: [CaptionMessage] = []
    func add(_ m: CaptionMessage) { lock.withLock { items.append(m) } }
    var messages: [CaptionMessage] { lock.withLock { items } }
}

final class CaptionStreamRouterTests: XCTestCase {
    private func start(_ id: String, _ lang: String = "en-US", rate: Int = 16000) -> CaptionMessage {
        var m = CaptionMessage(t: CaptionMessage.streamStart)
        m.streamId = id
        m.language = lang
        m.sampleRate = rate
        return m
    }

    private func end(_ id: String) -> CaptionMessage {
        var m = CaptionMessage(t: CaptionMessage.streamEnd)
        m.streamId = id
        return m
    }

    private func waitFor(_ timeout: TimeInterval = 3, _ cond: @escaping () -> Bool) async {
        let deadline = Date().addingTimeInterval(timeout)
        while !cond() && Date() < deadline { try? await Task.sleep(for: .milliseconds(10)) }
    }

    func testOneRecognizerPerStreamAndResultsCarryTheStreamID() async {
        let factory = FakeFactory()
        let out = Outbox()
        let router = CaptionStreamRouter(factory: factory) { out.add($0) }
        await router.handle(start("s1"))
        await router.handle(start("s2"))
        await waitFor { factory.recognizers.count == 2 }
        for _ in 0..<3 {
            await router.handleAudio(CaptionFrame(streamID: "s1", samples: Array(repeating: 1, count: 1600)))
            await router.handleAudio(CaptionFrame(streamID: "s2", samples: Array(repeating: 2, count: 1600)))
        }
        await router.handleAudio(CaptionFrame(streamID: "unknown", samples: [1]))
        await waitFor { out.messages.filter { $0.isFinal == true }.count == 2 }
        let finals = out.messages.filter { $0.isFinal == true }
        XCTAssertEqual(Set(finals.compactMap(\.streamId)), ["s1", "s2"])
        XCTAssertEqual(finals.first?.text, "hello world")
        for id in ["s1", "s2"] {
            let msgs = out.messages.filter { $0.streamId == id }
            XCTAssertEqual(msgs.map { $0.isFinal ?? false }, [false, true], "partial precedes final for \(id)")
            XCTAssertEqual(Set(msgs.compactMap(\.segmentId)), ["1"])
        }
        let byLanguage = factory.recognizers.map { $0.sampleCount }
        XCTAssertEqual(byLanguage, [4800, 4800])
        let count = await router.activeStreams
        XCTAssertEqual(count, 2)
        await router.handle(end("s1"))
        await waitFor { factory.recognizers.first { $0.finished } != nil }
        XCTAssertEqual(factory.recognizers.filter(\.finished).count, 1)
        await router.reset()
        XCTAssertTrue(factory.recognizers.contains { $0.cancelled })
    }

    func testAudioIsBufferedWhileTheRecognizerStarts() async {
        let factory = FakeFactory()
        factory.delay = .milliseconds(200)
        let out = Outbox()
        let router = CaptionStreamRouter(factory: factory) { out.add($0) }
        await router.handle(start("s1"))
        await router.handleAudio(CaptionFrame(streamID: "s1", samples: Array(repeating: 3, count: 1600)))
        await router.handleAudio(CaptionFrame(streamID: "s1", samples: Array(repeating: 3, count: 1600)))
        await router.handle(end("s1")) // ends before the recognizer is ready
        await waitFor { factory.recognizers.first?.finished == true }
        XCTAssertEqual(factory.recognizers.first?.sampleCount, 3200)
        XCTAssertEqual(factory.recognizers.first?.finished, true)
    }

    func testUnsupportedLanguageAndFormatReportStreamErrors() async {
        let out = Outbox()
        let router = CaptionStreamRouter(factory: FakeFactory()) { out.add($0) }
        await router.handle(start("s1", "xx-XX"))
        await router.handle(start("s2", rate: 48000))
        await waitFor { out.messages.count == 2 }
        let errors = out.messages.filter { $0.t == CaptionMessage.error }
        XCTAssertEqual(Set(errors.compactMap(\.code)), ["unsupported_language", "unsupported_format"])
        XCTAssertEqual(Set(errors.compactMap(\.streamId)), ["s1", "s2"])
        let count = await router.activeStreams
        XCTAssertEqual(count, 0)
    }
}
