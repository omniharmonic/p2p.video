import Foundation

/// The p2p.video caption provider protocol, version 1 [SL-P4-2].
/// Spec: docs/integrations/CAPTIONS.md. Kept free of Speech/AVFoundation so
/// another app (e.g. Minutes) can reuse it with its own recognizer.
public enum CaptionProtocol {
    public static let version = 1
    public static let sampleRate = 16_000
    public static let frameVersion: UInt8 = 1
    public static let maxKeyLength = 64
    public static let maxFrameSamples = 16_000
    /// Loopback path the provider connects to on the daemon.
    public static let providerPath = "api/v1/captions/provider"
}

/// One binary audio frame: version, key length, ASCII stream key, then
/// signed 16-bit little-endian mono PCM at 16 kHz.
public struct CaptionFrame: Equatable, Sendable {
    public var streamID: String
    public var samples: [Int16]

    public init(streamID: String, samples: [Int16]) {
        self.streamID = streamID
        self.samples = samples
    }

    public enum DecodeError: Error, Equatable {
        case version, key, audio
    }

    public func encoded() -> Data {
        var data = Data([CaptionProtocol.frameVersion, UInt8(streamID.utf8.count)])
        data.append(contentsOf: Array(streamID.utf8))
        data.reserveCapacity(data.count + samples.count * 2)
        for s in samples {
            let v = UInt16(bitPattern: s.littleEndian)
            data.append(UInt8(v & 0xff))
            data.append(UInt8(v >> 8))
        }
        return data
    }

    public static func decode(_ data: Data) throws -> CaptionFrame {
        let bytes = [UInt8](data)
        guard bytes.count >= 2, bytes[0] == CaptionProtocol.frameVersion else { throw DecodeError.version }
        let n = Int(bytes[1])
        guard n > 0, n <= CaptionProtocol.maxKeyLength, bytes.count >= 2 + n else { throw DecodeError.key }
        let keyBytes = bytes[2..<(2 + n)]
        guard keyBytes.allSatisfy(Self.isKeyByte), let key = String(bytes: keyBytes, encoding: .ascii) else { throw DecodeError.key }
        let pcm = bytes[(2 + n)...]
        guard !pcm.isEmpty, pcm.count % 2 == 0, pcm.count <= CaptionProtocol.maxFrameSamples * 2 else { throw DecodeError.audio }
        var samples = [Int16]()
        samples.reserveCapacity(pcm.count / 2)
        var i = pcm.startIndex
        while i < pcm.endIndex {
            samples.append(Int16(bitPattern: UInt16(pcm[i]) | UInt16(pcm[i + 1]) << 8))
            i += 2
        }
        return CaptionFrame(streamID: key, samples: samples)
    }

    static func isKeyByte(_ b: UInt8) -> Bool {
        (b >= 0x30 && b <= 0x39) || (b >= 0x41 && b <= 0x5a) || (b >= 0x61 && b <= 0x7a) || b == 0x5f || b == 0x2d || b == 0x2e || b == 0x3a
    }
}

/// Every provider control message (JSON text frame). Unused fields are nil
/// and omitted when encoding.
public struct CaptionMessage: Codable, Equatable, Sendable {
    public var t: String
    public var `protocol`: Int?
    public var name: String?
    public var engine: String?
    public var languages: [String]?
    public var streamId: String?
    public var language: String?
    public var sampleRate: Int?
    public var encoding: String?
    public var segmentId: String?
    public var text: String?
    public var isFinal: Bool?
    public var startMs: Int64?
    public var endMs: Int64?
    public var code: String?
    public var message: String?

    public init(t: String) { self.t = t }

    public static let hello = "hello"
    public static let ready = "ready"
    public static let streamStart = "stream_start"
    public static let streamEnd = "stream_end"
    public static let result = "result"
    public static let error = "error"

    public static func makeHello(name: String, engine: String, languages: [String]) -> CaptionMessage {
        var m = CaptionMessage(t: hello)
        m.protocol = CaptionProtocol.version
        m.name = name
        m.engine = engine
        m.languages = languages
        return m
    }

    public static func makeResult(streamID: String, segmentID: String, text: String, isFinal: Bool, startMs: Int64, endMs: Int64) -> CaptionMessage {
        var m = CaptionMessage(t: result)
        m.streamId = streamID
        m.segmentId = segmentID
        m.text = text
        m.isFinal = isFinal
        m.startMs = startMs
        m.endMs = endMs
        return m
    }

    public static func makeError(streamID: String?, code: String, message: String) -> CaptionMessage {
        var m = CaptionMessage(t: error)
        m.streamId = streamID
        m.code = code
        m.message = message
        return m
    }

    public func json() -> Data {
        let enc = JSONEncoder()
        enc.outputFormatting = [.sortedKeys]
        return (try? enc.encode(self)) ?? Data()
    }

    public static func parse(_ data: Data) -> CaptionMessage? {
        try? JSONDecoder().decode(CaptionMessage.self, from: data)
    }
}
