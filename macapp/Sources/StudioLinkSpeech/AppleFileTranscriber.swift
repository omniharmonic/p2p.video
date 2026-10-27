import AVFoundation
import CoreMedia
import Foundation
import Speech
import StudioLinkCore

/// Apple on-device transcription of one audio file for post-take transcripts
/// [SL-P4-3].
///
/// macOS 26+: `SpeechAnalyzer` + `SpeechTranscriber` reading the file
/// directly (`AVAudioFile`, any sample rate and channel layout), with word
/// time ranges and confidences. It has no per-request duration limit, so a
/// 60-minute track is one request, and it needs no Speech Recognition (TCC)
/// prompt.
///
/// macOS 14–25: `SFSpeechRecognizer` with `requiresOnDeviceRecognition`
/// (fails closed), on chunks of at most `fallbackChunkSec` cut at quiet
/// points (`ChunkPlanner`), because that API limits request length. It needs
/// Speech Recognition permission; without it every job fails with
/// `not_authorized` and the host sees what to do.
public struct AppleFileTranscriber: FileTranscriber {
    public static let fallbackChunkSec = 50.0

    public let preferred: AppleSpeechRecognizerFactory.Engine

    /// `STUDIOLINK_CAPTIONS_ENGINE=sfspeech` forces the fallback (testing).
    public init(environment: [String: String] = ProcessInfo.processInfo.environment) {
        preferred = AppleSpeechRecognizerFactory(environment: environment).preferred
    }

    public var engine: String { preferred.rawValue }

    /// Shows the Speech Recognition prompt (only from the app bundle, which
    /// declares NSSpeechRecognitionUsageDescription). Needed only by the
    /// pre-macOS 26 engine.
    public static func requestAuthorization() async -> Bool {
        await withCheckedContinuation { cont in
            SFSpeechRecognizer.requestAuthorization { cont.resume(returning: $0 == .authorized) }
        }
    }

    public func supportedLanguages() async -> [String] {
        await AppleSpeechRecognizerFactory(environment: preferred == .sfSpeech ? ["STUDIOLINK_CAPTIONS_ENGINE": "sfspeech"] : [:]).supportedLanguages()
    }

    public func availability() async -> TranscriptionAvailability {
        if preferred == .speechAnalyzer, #available(macOS 26.0, *) {
            return SpeechTranscriber.isAvailable ? .ready : .unavailable("On-device speech transcription is unavailable on this Mac.")
        }
        switch SFSpeechRecognizer.authorizationStatus() {
        case .authorized: return .ready
        case .notDetermined, .denied, .restricted: return .notAuthorized(TranscriptionAvailability.permissionMessage)
        @unknown default: return .notAuthorized(TranscriptionAvailability.permissionMessage)
        }
    }

    public func transcribe(
        file: URL, language: String,
        onPhrase: @escaping @Sendable (RecognizedPhrase) -> Void,
        onProgress: @escaping @Sendable (Double) -> Void
    ) async throws -> FileTranscriptionSummary {
        let audio: AVAudioFile
        do { audio = try AVAudioFile(forReading: file) } catch {
            throw CaptionRecognizerError(code: "audio_unreadable", message: "The audio file could not be opened for transcription.")
        }
        let duration = Double(audio.length) / audio.processingFormat.sampleRate
        if preferred == .speechAnalyzer, #available(macOS 26.0, *) {
            return try await AnalyzerFile.transcribe(audio, duration: duration, language: language, onPhrase: onPhrase, onProgress: onProgress)
        }
        return try await SFSpeechFile.transcribe(audio, duration: duration, language: language, onPhrase: onPhrase, onProgress: onProgress)
    }
}

@available(macOS 26.0, *)
enum AnalyzerFile {
    static func transcribe(
        _ audio: AVAudioFile, duration: Double, language: String,
        onPhrase: @escaping @Sendable (RecognizedPhrase) -> Void,
        onProgress: @escaping @Sendable (Double) -> Void
    ) async throws -> FileTranscriptionSummary {
        guard SpeechTranscriber.isAvailable else {
            throw CaptionRecognizerError(code: "recognizer_unavailable", message: "On-device speech transcription is unavailable on this Mac.")
        }
        guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: language)) else {
            throw CaptionRecognizerError(code: "unsupported_language", message: "Transcription doesn't support \(language) on this Mac.")
        }
        let transcriber = SpeechTranscriber(locale: locale, transcriptionOptions: [], reportingOptions: [], attributeOptions: [.audioTimeRange, .transcriptionConfidence])
        do {
            // Downloads the on-device model once (a system asset); no audio leaves the Mac.
            if let request = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await request.downloadAndInstall()
            }
        } catch {
            throw CaptionRecognizerError(code: "model_unavailable", message: "The on-device speech model for \(language) could not be installed.")
        }
        try Task.checkCancellation()
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        let results = Task {
            for try await r in transcriber.results where r.isFinal {
                onPhrase(phrase(r.text, range: r.range))
                onProgress(r.range.end.seconds)
            }
        }
        do {
            try await withTaskCancellationHandler {
                try await analyzer.start(inputAudioFile: audio, finishAfterFile: true)
                try await results.value
            } onCancel: {
                results.cancel()
                Task { await analyzer.cancelAndFinishNow() }
            }
        } catch is CancellationError {
            throw CancellationError()
        } catch let err as CaptionRecognizerError {
            throw err
        } catch {
            if Task.isCancelled { throw CancellationError() }
            throw CaptionRecognizerError(code: "recognizer_failed", message: "On-device transcription stopped before the end of the file.")
        }
        try Task.checkCancellation()
        onProgress(duration)
        return FileTranscriptionSummary(durationSec: duration, locale: locale.identifier(.bcp47), asr: AppleSpeechRecognizerFactory.Engine.speechAnalyzer.rawValue)
    }

    static func phrase(_ text: AttributedString, range: CMTimeRange) -> RecognizedPhrase {
        var runs: [RecognizedRun] = []
        for run in text.runs {
            let piece = String(text[run.range].characters)
            let time = run.audioTimeRange
            runs.append(RecognizedRun(
                text: piece,
                start: time.map { $0.start.seconds },
                end: time.map { $0.end.seconds },
                confidence: run.transcriptionConfidence))
        }
        return RecognizedPhrase(text: String(text.characters), start: range.start.seconds, end: range.end.seconds, runs: runs)
    }
}

/// Fallback for macOS 14–25: one on-device request per chunk, results offset
/// by where the chunk starts.
enum SFSpeechFile {
    static func transcribe(
        _ audio: AVAudioFile, duration: Double, language: String,
        onPhrase: @escaping @Sendable (RecognizedPhrase) -> Void,
        onProgress: @escaping @Sendable (Double) -> Void
    ) async throws -> FileTranscriptionSummary {
        guard SFSpeechRecognizer.authorizationStatus() == .authorized else {
            throw CaptionRecognizerError(code: "not_authorized", message: TranscriptionAvailability.permissionMessage)
        }
        guard let recognizer = SFSpeechRecognizer(locale: Locale(identifier: language)), recognizer.isAvailable, recognizer.supportsOnDeviceRecognition else {
            throw CaptionRecognizerError(code: "unsupported_language", message: "On-device transcription for \(language) is unavailable on this Mac.")
        }
        let format = audio.processingFormat
        let rate = format.sampleRate
        let energy = try energyEnvelope(audio)
        for chunk in ChunkPlanner.plan(durationSec: duration, maxChunkSec: AppleFileTranscriber.fallbackChunkSec, energy: energy) {
            try Task.checkCancellation()
            let startFrame = AVAudioFramePosition(chunk.lowerBound * rate)
            let frames = AVAudioFrameCount(max(0, (chunk.upperBound - chunk.lowerBound) * rate))
            guard frames > 0, let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: frames) else { continue }
            audio.framePosition = startFrame
            do { try audio.read(into: buffer, frameCount: frames) } catch {
                throw CaptionRecognizerError(code: "audio_unreadable", message: "The audio file could not be read to the end.")
            }
            let request = SFSpeechAudioBufferRecognitionRequest()
            request.requiresOnDeviceRecognition = true
            request.shouldReportPartialResults = false
            request.addsPunctuation = true
            request.append(buffer)
            request.endAudio()
            if let phrase = try await recognize(recognizer, request, offset: chunk.lowerBound) {
                onPhrase(phrase)
            }
            onProgress(chunk.upperBound)
        }
        return FileTranscriptionSummary(durationSec: duration, locale: language, asr: AppleSpeechRecognizerFactory.Engine.sfSpeech.rawValue)
    }

    private static func recognize(_ recognizer: SFSpeechRecognizer, _ request: SFSpeechAudioBufferRecognitionRequest, offset: Double) async throws -> RecognizedPhrase? {
        final class Box: @unchecked Sendable {
            var task: SFSpeechRecognitionTask?
            var resumed = false
            let lock = NSLock()
        }
        let box = Box()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (cont: CheckedContinuation<RecognizedPhrase?, Error>) in
                let task = recognizer.recognitionTask(with: request) { result, error in
                    let finish: Bool = box.lock.withLock {
                        guard !box.resumed, result?.isFinal == true || error != nil else { return false }
                        box.resumed = true
                        return true
                    }
                    guard finish else { return }
                    if let result, result.isFinal { cont.resume(returning: phrase(result, offset: offset)); return }
                    let ns = error as NSError?
                    // 1110: no speech in this chunk (silence), not a failure.
                    if ns?.domain == "kAFAssistantErrorDomain", ns?.code == 1110 { cont.resume(returning: nil); return }
                    cont.resume(throwing: CaptionRecognizerError(code: "recognizer_failed", message: "On-device transcription failed."))
                }
                box.lock.withLock { box.task = task }
            }
        } onCancel: {
            box.lock.withLock { box.task }?.cancel()
        }
    }

    private static func phrase(_ result: SFSpeechRecognitionResult, offset: Double) -> RecognizedPhrase? {
        let segs = result.bestTranscription.segments
        guard !segs.isEmpty else { return nil }
        let runs = segs.enumerated().map { i, s in
            RecognizedRun(text: (i == 0 ? "" : " ") + s.substring, start: offset + s.timestamp, end: offset + s.timestamp + s.duration, confidence: Double(s.confidence))
        }
        return RecognizedPhrase(text: result.bestTranscription.formattedString, start: runs.first?.start ?? offset, end: runs.last?.end ?? offset, runs: runs)
    }

    /// RMS per 100 ms frame of the first channel, for quiet-point cuts.
    private static func energyEnvelope(_ audio: AVAudioFile) throws -> [Float] {
        let rate = audio.processingFormat.sampleRate
        let hop = AVAudioFrameCount(max(1, rate * ChunkPlanner.frameSec))
        guard let buf = AVAudioPCMBuffer(pcmFormat: audio.processingFormat, frameCapacity: hop * 600) else { return [] }
        var out: [Float] = []
        audio.framePosition = 0
        while audio.framePosition < audio.length {
            do { try audio.read(into: buf) } catch { break }
            guard buf.frameLength > 0, let ch = buf.floatChannelData?[0] else { break }
            var i = 0
            let n = Int(buf.frameLength)
            while i < n {
                let end = min(n, i + Int(hop))
                var sum: Float = 0
                for j in i..<end { sum += ch[j] * ch[j] }
                out.append((sum / Float(end - i)).squareRoot())
                i = end
            }
        }
        audio.framePosition = 0
        return out
    }
}
