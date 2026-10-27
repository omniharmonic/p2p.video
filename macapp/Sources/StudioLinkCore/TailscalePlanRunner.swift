import Foundation

/// How a confirmed list of Tailscale commands ended.
public enum TailscaleRunOutcome: Equatable, Sendable {
    /// Every command exited 0.
    case completed
    /// Tailscale printed an approval link and waited; p2p.video stopped
    /// waiting. Nothing after `command` ran. Approve, then run the plan again.
    case needsApproval(TailscaleApproval, command: TailscaleCommand)
    case failed(command: TailscaleCommand, status: Int32)
    case timedOut(command: TailscaleCommand)
    case couldNotRun(command: TailscaleCommand, reason: String)

    /// A plain-language message for failures (nil for success and approval).
    public var failureMessage: String? {
        switch self {
        case .completed, .needsApproval: return nil
        case .failed(let c, let status):
            return "Tailscale couldn’t finish the change (\(c.display) exited with \(status)). Open Show details for its output."
        case .timedOut(let c):
            return "Tailscale didn’t answer in time (\(c.display)). Check that Tailscale is running, then try again."
        case .couldNotRun(_, let reason):
            return "Couldn’t run Tailscale: \(reason)"
        }
    }
}

/// Runs a confirmed plan's commands in order, streaming each command's
/// output so an approval prompt is caught as soon as it is printed [SL-UX-2].
public struct TailscalePlanRunner: Sendable {
    public var cli: URL
    public var runner: CommandRunning
    public var environment: [String: String]
    public var timeout: TimeInterval

    public init(cli: URL, runner: CommandRunning, environment: [String: String], timeout: TimeInterval = 45) {
        self.cli = cli
        self.runner = runner
        self.environment = environment
        self.timeout = timeout
    }

    /// Runs the commands; the transcript holds "$ command" lines and each
    /// command's output, for the "Show details" view. Nothing here is logged.
    public func run(_ commands: [TailscaleCommand]) async -> (outcome: TailscaleRunOutcome, transcript: [String]) {
        var lines: [String] = []
        let outcome = await run(commands) { lines.append($0) }
        return (outcome, lines)
    }

    func run(_ commands: [TailscaleCommand], transcript: (String) -> Void) async -> TailscaleRunOutcome {
        for command in commands {
            transcript("$ \(command.display)")
            let result: CommandResult
            do {
                result = try await runner.run(cli, arguments: command.arguments, environment: environment, timeout: timeout) {
                    TailscaleApproval.find(in: $0) != nil
                }
            } catch {
                return .couldNotRun(command: command, reason: error.localizedDescription)
            }
            let text = (result.stdoutText + result.stderrText).trimmingCharacters(in: .whitespacesAndNewlines)
            if !text.isEmpty { transcript(text) }
            if result.succeeded { continue }
            if let approval = TailscaleApproval.find(in: result.stdoutText + "\n" + result.stderrText) {
                // Stopped on the link (or exited non-zero after printing it): approve first.
                return .needsApproval(approval, command: command)
            }
            if result.timedOut { return .timedOut(command: command) }
            if !result.succeeded { return .failed(command: command, status: result.status) }
        }
        return .completed
    }
}
