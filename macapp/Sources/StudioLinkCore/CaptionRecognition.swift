import Foundation

/// One recognizer update for a stream. Times are seconds from the stream's
/// first sample. A non-final update replaces the current partial; a final one
/// closes the current segment.
public struct RecognizerUpdate: Equatable, Sendable {
    public var text: String
    public var isFinal: Bool
    public var start: Double
    public var end: Double

    public init(text: String, isFinal: Bool, start: Double, end: Double) {
        self.text = text
        self.isFinal = isFinal
        self.start = start
        self.end = end
    }
}

/// A streaming speech recognizer for one participant's audio (16 kHz mono
/// Int16). Implementations must recognize on device only.
public protocol CaptionRecognizer: AnyObject, Sendable {
    func append(_ samples: [Int16])
    /// No more audio: deliver remaining final results, then stop.
    func finish()
    /// Stop now, discarding pending results.
    func cancel()
}

public struct CaptionRecognizerError: Error, Equatable, CustomStringConvertible, Sendable {
    public var code: String
    public var message: String
    public init(code: String, message: String) {
        self.code = code
        self.message = message
    }
    public var description: String { message }
}

/// Creates recognizers. The Apple implementation lives in StudioLinkSpeech;
/// tests use a fake.
public protocol CaptionRecognizerFactory: Sendable {
    /// Short engine identifier announced in the provider hello.
    var engine: String { get }
    func supportedLanguages() async -> [String]
    func makeRecognizer(
        language: String,
        onUpdate: @escaping @Sendable (RecognizerUpdate) -> Void,
        onError: @escaping @Sendable (CaptionRecognizerError) -> Void
    ) async throws -> CaptionRecognizer
}

/// Turns a stream's recognizer updates into provider `result` messages with
/// stable segment IDs: partials repeat the current ID (skipping unchanged
/// text), a final closes it, and the next update starts a new segment. A final
/// with no text after a partial withdraws that partial.
public struct CaptionSegmentAssembler: Sendable {
    public let streamID: String
    private var segment = 1
    private var partial: String?

    public init(streamID: String) { self.streamID = streamID }

    public mutating func apply(_ u: RecognizerUpdate) -> CaptionMessage? {
        let text = u.text.trimmingCharacters(in: .whitespacesAndNewlines)
        let id = String(segment)
        if u.isFinal {
            let hadPartial = partial != nil
            segment += 1
            partial = nil
            if text.isEmpty && !hadPartial { return nil }
            return message(id, text, true, u)
        }
        guard !text.isEmpty, text != partial else { return nil }
        partial = text
        return message(id, text, false, u)
    }

    private func message(_ id: String, _ text: String, _ final: Bool, _ u: RecognizerUpdate) -> CaptionMessage {
        let start = Self.ms(u.start)
        return .makeResult(streamID: streamID, segmentID: id, text: text, isFinal: final, startMs: start, endMs: max(start, Self.ms(u.end)))
    }

    static func ms(_ seconds: Double) -> Int64 {
        guard seconds.isFinite, seconds > 0 else { return 0 }
        return Int64((seconds * 1000).rounded())
    }
}
