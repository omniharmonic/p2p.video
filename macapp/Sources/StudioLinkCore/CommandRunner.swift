import Foundation

public struct CommandResult: Equatable, Sendable {
    public var status: Int32
    public var stdout: Data
    public var stderr: Data
    public var timedOut: Bool
    /// The caller's `stopWhen` matched the output and the process was stopped.
    public var stoppedEarly: Bool

    public init(status: Int32, stdout: Data = Data(), stderr: Data = Data(), timedOut: Bool = false, stoppedEarly: Bool = false) {
        self.status = status
        self.stdout = stdout
        self.stderr = stderr
        self.timedOut = timedOut
        self.stoppedEarly = stoppedEarly
    }

    public var succeeded: Bool { status == 0 && !timedOut && !stoppedEarly }
    public var stderrText: String { String(decoding: stderr, as: UTF8.self) }
    public var stdoutText: String { String(decoding: stdout, as: UTF8.self) }
}

/// Runs an executable with an argv array — never through a shell.
public protocol CommandRunning: Sendable {
    func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval) async throws -> CommandResult
    /// Like `run`, but reads stdout and stderr while the process runs and
    /// stops it as soon as `stopWhen` returns true for the output so far
    /// (e.g. Tailscale printed an approval link and is now waiting for it).
    func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval,
             stopWhen: @escaping @Sendable (String) -> Bool) async throws -> CommandResult
}

extension CommandRunning {
    /// Runners that cannot stream check the output once the process exits.
    public func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval,
                    stopWhen: @escaping @Sendable (String) -> Bool) async throws -> CommandResult {
        var r = try await run(executable, arguments: arguments, environment: environment, timeout: timeout)
        if stopWhen(r.stdoutText + r.stderrText) { r.stoppedEarly = true }
        return r
    }
}

public struct ProcessCommandRunner: CommandRunning {
    public init() {}

    public func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval) async throws -> CommandResult {
        try await run(executable, arguments: arguments, environment: environment, timeout: timeout) { _ in false }
    }

    public func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval,
                    stopWhen: @escaping @Sendable (String) -> Bool) async throws -> CommandResult {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                do {
                    continuation.resume(returning: try Self.runBlocking(executable, arguments: arguments, environment: environment,
                                                                        timeout: timeout, stopWhen: stopWhen))
                } catch {
                    continuation.resume(throwing: error)
                }
            }
        }
    }

    static func runBlocking(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval,
                            stopWhen: @escaping @Sendable (String) -> Bool) throws -> CommandResult {
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        process.environment = environment
        process.standardInput = FileHandle.nullDevice
        let out = Pipe(), err = Pipe()
        process.standardOutput = out
        process.standardError = err
        let done = DispatchSemaphore(value: 0)
        let collector = OutputCollector(stopWhen: stopWhen) { done.signal() }
        out.fileHandleForReading.readabilityHandler = { h in collector.append(h.availableData, stderr: false) }
        err.fileHandleForReading.readabilityHandler = { h in collector.append(h.availableData, stderr: true) }
        process.terminationHandler = { _ in done.signal() }
        try process.run()
        var timedOut = false
        if done.wait(timeout: .now() + timeout) == .timedOut {
            timedOut = true
            process.terminate()
            _ = done.wait(timeout: .now() + 2)
        } else if collector.stopped, process.isRunning {
            // Stopped on the caller's match. The waiting CLI has changed nothing yet.
            process.terminate()
            _ = done.wait(timeout: .now() + 2)
        }
        out.fileHandleForReading.readabilityHandler = nil
        err.fileHandleForReading.readabilityHandler = nil
        if !collector.stopped {
            // Not after an early stop: a child that outlives the process would
            // keep the pipe open and block this read until it exits.
            collector.append(out.fileHandleForReading.readDataToEndOfFile(), stderr: false)
            collector.append(err.fileHandleForReading.readDataToEndOfFile(), stderr: true)
        }
        let (o, e) = collector.snapshot()
        return CommandResult(status: process.isRunning ? -1 : process.terminationStatus, stdout: o, stderr: e, timedOut: timedOut,
                             stoppedEarly: collector.stopped)
    }
}

private final class OutputCollector: @unchecked Sendable {
    private let lock = NSLock()
    private var out = Data()
    private var err = Data()
    /// Both streams in arrival order, for `stopWhen`.
    private var combined = Data()
    private var matched = false
    private let stopWhen: @Sendable (String) -> Bool
    private let onStop: @Sendable () -> Void

    init(stopWhen: @escaping @Sendable (String) -> Bool, onStop: @escaping @Sendable () -> Void) {
        self.stopWhen = stopWhen
        self.onStop = onStop
    }

    var stopped: Bool {
        lock.lock(); defer { lock.unlock() }
        return matched
    }

    func append(_ data: Data, stderr: Bool) {
        guard !data.isEmpty else { return }
        lock.lock()
        if stderr { err.append(data) } else { out.append(data) }
        combined.append(data)
        let fire = !matched && stopWhen(String(decoding: combined, as: UTF8.self))
        if fire { matched = true }
        lock.unlock()
        if fire { onStop() }
    }

    func snapshot() -> (Data, Data) {
        lock.lock(); defer { lock.unlock() }
        return (out, err)
    }
}
