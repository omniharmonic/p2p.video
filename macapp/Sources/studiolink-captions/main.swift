import AVFoundation
import Foundation
import StudioLinkCore
import StudioLinkSpeech

// Caption provider harness [SL-P4-2].
//
//   studiolink-captions transcribe <file.wav|.aiff|.pcm> [--language en-US]
//       Feeds the file through the same router + on-device recognizer the app
//       uses, in 100 ms frames, and prints each provider `result` message as a
//       JSON line, then the final transcript. Raw .pcm must be s16le 16 kHz mono.
//
//   studiolink-captions provider [--port 7443] [--data-dir DIR]
//       Runs as the daemon's caption provider (what the app does), headless.
//
//   studiolink-captions transcribe-file <file.wav|.aiff|.caf> [--language en-US]
//       Post-take file transcription [SL-P4-3]: runs one job through the same
//       job runner + on-device file transcriber the app uses and prints each
//       transcription provider message (progress, segment, done/error) as a
//       JSON line. Exit 1 when the job failed.
//
//   studiolink-captions file-provider [--port 7443] [--data-dir DIR]
//       Runs as the daemon's transcription provider (what the app does), headless.
//
// Audio and recognized text go to stdout only; nothing is written to disk.

let args = Array(CommandLine.arguments.dropFirst())

func option(_ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(2)
}

func loadSamples(_ path: String) throws -> [Int16] {
    let url = URL(fileURLWithPath: path)
    if url.pathExtension.lowercased() == "pcm" {
        let data = try Data(contentsOf: url)
        return data.withUnsafeBytes { Array($0.bindMemory(to: Int16.self)) }
    }
    let file = try AVAudioFile(forReading: url)
    let target = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: Double(CaptionProtocol.sampleRate), channels: 1, interleaved: true)!
    guard let converter = AVAudioConverter(from: file.processingFormat, to: target),
          let input = AVAudioPCMBuffer(pcmFormat: file.processingFormat, frameCapacity: AVAudioFrameCount(file.length)) else {
        throw CaptionRecognizerError(code: "audio", message: "Cannot convert \(path)")
    }
    try file.read(into: input)
    let capacity = AVAudioFrameCount(Double(input.frameLength) * target.sampleRate / file.processingFormat.sampleRate) + 1024
    guard let output = AVAudioPCMBuffer(pcmFormat: target, frameCapacity: capacity) else { return [] }
    nonisolated(unsafe) var consumed = false
    var error: NSError?
    converter.convert(to: output, error: &error) { _, status in
        if consumed { status.pointee = .endOfStream; return nil }
        consumed = true
        status.pointee = .haveData
        return input
    }
    if let error { throw error }
    return Array(UnsafeBufferPointer(start: output.int16ChannelData![0], count: Int(output.frameLength)))
}

final class Collector: @unchecked Sendable {
    private let lock = NSLock()
    private var finals: [String] = []
    func add(_ m: CaptionMessage) {
        print(String(decoding: m.json(), as: UTF8.self))
        fflush(stdout)
        if m.t == CaptionMessage.result, m.isFinal == true, let t = m.text, !t.isEmpty {
            lock.withLock { finals.append(t) }
        }
    }
    var transcript: String { lock.withLock { finals.joined(separator: " ") } }
}

func transcribe(_ path: String) async {
    let samples: [Int16]
    do { samples = try loadSamples(path) } catch { fail("Cannot read audio: \(error)") }
    let factory = AppleSpeechRecognizerFactory()
    FileHandle.standardError.write(Data("engine: \(factory.engine), \(samples.count) samples (\(Double(samples.count) / 16000) s)\n".utf8))
    let collector = Collector()
    let router = CaptionStreamRouter(factory: factory) { collector.add($0) }
    var start = CaptionMessage(t: CaptionMessage.streamStart)
    start.streamId = "s1"
    start.language = option("--language") ?? "en-US"
    start.sampleRate = CaptionProtocol.sampleRate
    await router.handle(start)
    let frame = CaptionProtocol.sampleRate / 10
    var offset = 0
    while offset < samples.count {
        let chunk = Array(samples[offset..<min(offset + frame, samples.count)])
        await router.handleAudio(CaptionFrame(streamID: "s1", samples: chunk))
        offset += frame
        try? await Task.sleep(for: .milliseconds(20)) // 5× real time
    }
    var end = CaptionMessage(t: CaptionMessage.streamEnd)
    end.streamId = "s1"
    await router.handle(end)
    try? await Task.sleep(for: .seconds(4))
    print("TRANSCRIPT: \(collector.transcript)")
}

func runProvider() async {
    let port = Int(option("--port") ?? ProcessInfo.processInfo.environment["STUDIOLINK_PORT"] ?? "7443") ?? 7443
    let dataDir = option("--data-dir").map { URL(fileURLWithPath: $0) }
        ?? FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/p2p.video")
    let tokenFile = dataDir.appendingPathComponent(".desktop_token")
    let client = CaptionProviderClient(baseURL: URL(string: "http://127.0.0.1:\(port)")!, factory: AppleSpeechRecognizerFactory()) {
        HostClient.readDesktopToken(tokenFile)
    }
    client.start { status in
        FileHandle.standardError.write(Data("caption provider: \(status)\n".utf8))
    }
    while true { try? await Task.sleep(for: .seconds(3600)) }
}

final class Failed: @unchecked Sendable {
    private let lock = NSLock()
    private var flag = false
    func set() { lock.withLock { flag = true } }
    var value: Bool { lock.withLock { flag } }
}

func transcribeFile(_ path: String) async {
    let transcriber = AppleFileTranscriber()
    let availability = await transcriber.availability()
    FileHandle.standardError.write(Data("engine: \(transcriber.engine), availability: \(availability.state)\n".utf8))
    let failed = Failed()
    let runner = TranscriptionJobRunner(transcriber: transcriber) { m in
        print(String(decoding: m.json(), as: UTF8.self))
        fflush(stdout)
        if m.t == TranscriptionMessage.error { failed.set() }
    }
    var job = TranscriptionMessage(t: TranscriptionMessage.job)
    job.jobId = "cli1"
    job.path = URL(fileURLWithPath: path).standardizedFileURL.path
    job.language = option("--language") ?? "en-US"
    await runner.handle(job)
    await runner.waitForIdle()
    if failed.value { exit(1) }
}

func runFileProvider() async {
    let port = Int(option("--port") ?? ProcessInfo.processInfo.environment["STUDIOLINK_PORT"] ?? "7443") ?? 7443
    let dataDir = option("--data-dir").map { URL(fileURLWithPath: $0) }
        ?? FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/p2p.video")
    let tokenFile = dataDir.appendingPathComponent(".desktop_token")
    let client = TranscriptionProviderClient(baseURL: URL(string: "http://127.0.0.1:\(port)")!, transcriber: AppleFileTranscriber()) {
        HostClient.readDesktopToken(tokenFile)
    }
    client.start { status in
        FileHandle.standardError.write(Data("transcription provider: \(status)\n".utf8))
    }
    while true { try? await Task.sleep(for: .seconds(3600)) }
}

switch args.first {
case "transcribe-file":
    guard args.count >= 2 else { fail("usage: studiolink-captions transcribe-file <file> [--language en-US]") }
    await transcribeFile(args[1])
case "file-provider":
    await runFileProvider()
case "transcribe":
    guard args.count >= 2 else { fail("usage: studiolink-captions transcribe <file> [--language en-US]") }
    await transcribe(args[1])
case "provider":
    await runProvider()
default:
    fail("usage: studiolink-captions transcribe <file> [--language en-US] | provider [--port 7443] [--data-dir DIR] | transcribe-file <file> [--language en-US] | file-provider [--port 7443] [--data-dir DIR]")
}
