import Foundation
import os
import StudioLinkCore

let appLog = Logger(subsystem: HostConfiguration.bundleIdentifier, category: "app")

/// Starts, watches and stops the bundled `p2pvideod`.
///
/// Safety semantics carried over from `launch-mac.sh`:
/// - a daemon on the port is reused only if `/health` reports this exact
///   product and build;
/// - anything else on the port is never stopped; the user gets an actionable
///   message instead.
@MainActor
final class DaemonSupervisor: ObservableObject {
    enum Phase: Equatable {
        case idle
        case starting
        /// Healthy. `adopted` is true when we reused an already-running daemon.
        case running(adopted: Bool)
        case restarting(after: TimeInterval)
        /// Another service/build owns the port; we did not touch it.
        case blocked(String)
        case failed(String)
        case stopped
    }

    @Published private(set) var phase: Phase = .idle
    @Published private(set) var hostState: HostState?
    @Published private(set) var lastStateError: String?
    /// A restart waiting for recording/livestream/uploads to finish.
    @Published private(set) var pendingRestartReason: String?
    /// Set while the daemon is unresponsive but must not be restarted.
    @Published private(set) var healthWarning: String?

    let configuration: HostConfiguration
    let client: HostClient
    var settingsProvider: () -> DesktopSettings
    var onState: ((HostState) -> Void)?

    private var child: Process?
    private var childStartedAt: Date?
    private var expectingExit = false
    private var policy = RestartPolicy()
    private var consecutiveHealthFailures = 0
    private var pollTask: Task<Void, Never>?
    private var restartTask: Task<Void, Never>?

    init(configuration: HostConfiguration, settingsProvider: @escaping () -> DesktopSettings) {
        self.configuration = configuration
        self.client = HostClient(baseURL: configuration.baseURL)
        self.settingsProvider = settingsProvider
    }

    var isHealthy: Bool { if case .running = phase { return true } else { return false } }
    var ownsDaemon: Bool { child != nil }
    var activity: DaemonActivity? { hostState?.activity }

    // MARK: start

    func start() async {
        phase = .starting
        switch await client.health(expectedBuild: configuration.expectedBuildID) {
        case .expected:
            appLog.notice("Reusing running p2p.video daemon with the expected build")
            phase = .running(adopted: true)
            startPolling()
            return
        case let verdict where verdict != .unreachable:
            phase = .blocked(verdict.message(port: configuration.port) ?? "Port \(configuration.port) is in use.")
            return
        default:
            break
        }
        do {
            try launchChild()
        } catch {
            phase = .failed("Could not start the p2p.video host: \(error.localizedDescription)")
            return
        }
        // Wait up to 10 s for our exact build to answer (launcher waited 5 s).
        for _ in 0..<100 {
            try? await Task.sleep(for: .milliseconds(100))
            if child == nil { break } // exited during startup; termination handler decides
            switch await client.health(expectedBuild: configuration.expectedBuildID) {
            case .expected:
                phase = .running(adopted: false)
                consecutiveHealthFailures = 0
                startPolling()
                return
            case .unreachable:
                continue
            case let other:
                // Someone else won the port race. Stop only our own child.
                stopChild(force: false)
                phase = .blocked(other.message(port: configuration.port) ?? "Port \(configuration.port) is in use.")
                return
            }
        }
        if child != nil {
            stopChild(force: false)
            phase = .failed("The expected p2p.video build did not start. See \(configuration.daemonLog.path) for details.")
        }
    }

    private func launchChild() throws {
        let settings = settingsProvider()
        let args = try DaemonArguments.build(configuration: configuration, settings: settings)
        try FileManager.default.createDirectory(at: configuration.dataDirectory, withIntermediateDirectories: true,
                                                attributes: [.posixPermissions: 0o700])
        try LogRotation.rotateIfNeeded(configuration.daemonLog)
        let log = try LogRotation.openForAppend(configuration.daemonLog)
        let p = Process()
        p.executableURL = configuration.daemonExecutable
        p.arguments = args
        p.environment = DaemonArguments.environment(configuration: configuration, base: ProcessInfo.processInfo.environment)
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = log
        p.standardError = log
        p.currentDirectoryURL = configuration.dataDirectory
        p.terminationHandler = { [weak self] proc in
            let status = proc.terminationStatus
            Task { @MainActor in self?.childExited(proc, status: status) }
        }
        try p.run()
        try? log.close() // the child keeps its own descriptor
        child = p
        childStartedAt = Date()
        expectingExit = false
        appLog.notice("Started p2pvideod pid \(p.processIdentifier, privacy: .public) on port \(self.configuration.port, privacy: .public) mode \(settings.guestAccess.rawValue, privacy: .public)")
    }

    private func childExited(_ proc: Process, status: Int32) {
        guard proc === child else { return }
        child = nil
        if expectingExit {
            expectingExit = false
            return
        }
        appLog.error("p2pvideod exited unexpectedly with status \(status, privacy: .public)")
        let decision = policy.decide(.exited(status: status), lastKnown: hostState?.activity, now: Date())
        apply(decision)
    }

    private func apply(_ decision: RestartDecision) {
        switch decision {
        case .none:
            break
        case .restart(let delay):
            phase = .restarting(after: delay)
            restartTask?.cancel()
            restartTask = Task { [weak self] in
                try? await Task.sleep(for: .seconds(delay))
                guard let self, !Task.isCancelled else { return }
                await self.start()
            }
        case .deferUntilIdle(let reason):
            pendingRestartReason = reason
        case .giveUp(let reason):
            stopPolling()
            phase = .failed(reason)
        }
    }

    // MARK: polling

    private func startPolling() {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.pollOnce()
                try? await Task.sleep(for: .seconds(2))
            }
        }
    }

    private func stopPolling() {
        pollTask?.cancel()
        pollTask = nil
    }

    func pollOnce() async {
        let verdict = await client.health(expectedBuild: configuration.expectedBuildID)
        switch verdict {
        case .expected:
            consecutiveHealthFailures = 0
            healthWarning = nil
            if let since = childStartedAt { policy.noteHealthy(since: since, now: Date()) }
            if case .running = phase {} else { phase = .running(adopted: child == nil) }
        case .unreachable:
            consecutiveHealthFailures += 1
            if child == nil, case .running(adopted: true) = phase, consecutiveHealthFailures >= 2 {
                // The daemon we reused went away: start our own.
                stopPolling()
                await start()
                return
            }
            if child != nil {
                let decision = policy.decide(.unresponsive(consecutiveFailures: consecutiveHealthFailures), lastKnown: hostState?.activity, now: Date())
                switch decision {
                case .restart:
                    appLog.error("p2pvideod unresponsive and idle; restarting it")
                    stopChild(force: true)
                    stopPolling()
                    apply(decision)
                case .deferUntilIdle(let reason):
                    if healthWarning == nil {
                        appLog.error("p2pvideod unresponsive but not restarted: \(reason, privacy: .public)")
                    }
                    healthWarning = reason
                default:
                    apply(decision)
                }
            }
            return
        default:
            // A different service took the port; never touch it.
            stopPolling()
            phase = .blocked(verdict.message(port: configuration.port) ?? "Port in use.")
            return
        }
        guard let token = HostClient.readDesktopToken(configuration.desktopTokenFile) else {
            lastStateError = "The host has not written its desktop token yet."
            return
        }
        do {
            let state = try await client.state(desktopToken: token)
            hostState = state
            lastStateError = nil
            onState?(state)
            if pendingRestartReason != nil, case .restart = policy.decide(.configurationChanged, lastKnown: state.activity, now: Date()) {
                pendingRestartReason = nil
                await restart()
            }
        } catch {
            // Older daemons (no desktop endpoint) still run; status is just less rich.
            lastStateError = "Host status unavailable: \(error.localizedDescription)"
        }
    }

    // MARK: restart / stop

    /// Restart to apply new settings, waiting while anything would be interrupted.
    func restartForNewSettings() async {
        let decision = policy.decide(.configurationChanged, lastKnown: hostState?.activity, now: Date())
        if case .restart = decision {
            pendingRestartReason = nil
            await restart()
        } else {
            apply(decision)
        }
    }

    /// Restart now. Callers confirm with the user first if work is active.
    func restart() async {
        pendingRestartReason = nil
        await stop()
        await start()
    }

    /// Stops the daemon we own, or one we adopted that reports our exact build.
    func stop() async {
        stopPolling()
        restartTask?.cancel()
        if child != nil {
            stopChild(force: false)
            for _ in 0..<100 where child != nil { try? await Task.sleep(for: .milliseconds(100)) }
            if child != nil { stopChild(force: true) }
        } else if case .running(adopted: true) = phase, let state = hostState,
                  state.product == HostConfiguration.product, state.buildId == configuration.expectedBuildID, state.pid > 1 {
            // Same product and exact build that we verified via /health and the
            // token-protected state endpoint: ours to stop.
            kill(state.pid, SIGTERM)
            for _ in 0..<100 {
                if await client.health(expectedBuild: configuration.expectedBuildID) == .unreachable { break }
                try? await Task.sleep(for: .milliseconds(100))
            }
        }
        hostState = nil
        phase = .stopped
    }

    private func stopChild(force: Bool) {
        guard let p = child else { return }
        expectingExit = true
        if force { kill(p.processIdentifier, SIGKILL) } else { p.terminate() }
        if force { child = nil }
    }

    /// Synchronous best-effort stop used from `applicationWillTerminate`.
    func stopSynchronouslyForQuit() {
        guard let p = child else { return }
        expectingExit = true
        p.terminate()
        let deadline = Date().addingTimeInterval(10)
        while p.isRunning && Date() < deadline { usleep(50_000) }
        if p.isRunning { kill(p.processIdentifier, SIGKILL) }
        child = nil
    }
}
