import Foundation

/// Why the supervisor is considering a (re)start.
public enum SupervisorEvent: Equatable, Sendable {
    /// Our child daemon exited (crash or external kill).
    case exited(status: Int32)
    /// Our child is alive but `/health` failed this many times in a row.
    case unresponsive(consecutiveFailures: Int)
    /// Settings changed (e.g. guest access) and the daemon needs new flags.
    case configurationChanged
}

public enum RestartDecision: Equatable, Sendable {
    /// Nothing to do yet.
    case none
    /// Restart after the delay (seconds).
    case restart(after: TimeInterval)
    /// Not now: the daemon is doing work a restart would interrupt.
    case deferUntilIdle(reason: String)
    /// Too many crashes in a short window; stop and tell the user.
    case giveUp(reason: String)
}

/// Restart policy for the supervised daemon.
///
/// - Crashes restart with exponential backoff (1, 2, 4 … capped), and give up
///   after `maxCrashes` within `crashWindow`. A process that has exited is no
///   longer recording, so a crash restart is always allowed: restarting lets
///   participants' journaled uploads resume.
/// - A live but unresponsive daemon is never killed while the last known state
///   says a take is recording or a stream is live.
/// - Configuration restarts wait until no take is recording, no stream is live
///   and no uploads are pending.
public struct RestartPolicy: Sendable {
    public var baseDelay: TimeInterval = 1
    public var maxDelay: TimeInterval = 30
    public var maxCrashes = 5
    public var crashWindow: TimeInterval = 120
    public var unresponsiveThreshold = 3
    /// Running this long without a crash resets the backoff.
    public var stableAfter: TimeInterval = 60

    public private(set) var recentCrashes: [Date] = []

    public init() {}

    public func backoff(attempt: Int) -> TimeInterval {
        guard attempt > 0 else { return 0 }
        let exp = baseDelay * pow(2, Double(attempt - 1))
        return min(exp, maxDelay)
    }

    /// Records that the daemon has been running healthily since `since`.
    public mutating func noteHealthy(since: Date, now: Date) {
        if now.timeIntervalSince(since) >= stableAfter { recentCrashes.removeAll() }
    }

    public mutating func decide(_ event: SupervisorEvent, lastKnown activity: DaemonActivity?, now: Date) -> RestartDecision {
        switch event {
        case .exited:
            recentCrashes = recentCrashes.filter { now.timeIntervalSince($0) < crashWindow }
            recentCrashes.append(now)
            if recentCrashes.count > maxCrashes {
                return .giveUp(reason: "The p2p.video host stopped \(recentCrashes.count) times in \(Int(crashWindow / 60)) minutes. See daemon.log, then choose Restart Host.")
            }
            return .restart(after: backoff(attempt: recentCrashes.count))
        case .unresponsive(let failures):
            guard failures >= unresponsiveThreshold else { return .none }
            if let a = activity, a.recording || a.live {
                return .deferUntilIdle(reason: "The host is not answering, but a take was recording or a stream was live. It will not be restarted automatically.")
            }
            if activity == nil {
                // Never queried: be conservative; recording state is unknown.
                return .deferUntilIdle(reason: "The host is not answering and its recording state is unknown. Use Restart Host when no take is in progress.")
            }
            return .restart(after: backoff(attempt: max(1, recentCrashes.count)))
        case .configurationChanged:
            guard let a = activity else {
                return .deferUntilIdle(reason: "Waiting for the host to report its recording state before restarting.")
            }
            if a.recording { return .deferUntilIdle(reason: "A take is recording. The host restarts with the new settings when it stops.") }
            if a.live { return .deferUntilIdle(reason: "A livestream is running. The host restarts with the new settings when it ends.") }
            if a.uploadsPending > 0 { return .deferUntilIdle(reason: "Recording uploads are still pending. The host restarts with the new settings when they finish.") }
            return .restart(after: 0)
        }
    }
}
