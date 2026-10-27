import Foundation
import StudioLinkCore

/// Guided Tailscale setup. Read-only status commands run automatically;
/// Serve/Funnel changes run only from `run(_:)`, which the UI calls after the
/// user has chosen an option and clicked to confirm (the exact commands are
/// one "Show details" click away).
@MainActor
final class TailscaleSetup: ObservableObject {
    struct Plan: Identifiable, Equatable {
        enum Kind: Equatable { case enablePrivate, enablePublic, disablePublic, disablePrivate }
        var id: String { "\(kind)" }
        var kind: Kind
        /// What the user is doing, in their words.
        var title: String
        var explanation: String
        /// The technical description shown under "Show details".
        var details: String
        /// The confirm button ("Turn On", "Turn Off").
        var actionLabel: String
        var commands: [TailscaleCommand]
        var warnings: [String]
        /// Guest-access mode recorded after the plan succeeds.
        var resultingMode: GuestAccessMode
    }

    /// Tailscale asked the owner to approve a feature; "Try again" re-runs `kind`.
    struct PendingApproval: Equatable {
        var approval: TailscaleApproval
        var kind: Plan.Kind
    }

    @Published private(set) var cli: URL?
    @Published private(set) var status: TailscaleStatus?
    @Published private(set) var serve: ServeConfig?
    @Published private(set) var observation: GuestAccessObservation?
    @Published private(set) var lastError: String?
    @Published private(set) var refreshing = false
    @Published private(set) var running = false
    @Published private(set) var transcript: [String] = []
    /// Set when the last run stopped at Tailscale's approval link [SL-UX-2].
    @Published var pendingApproval: PendingApproval?

    private let configuration: HostConfiguration
    private let runner: CommandRunning

    init(configuration: HostConfiguration, runner: CommandRunning = ProcessCommandRunner()) {
        self.configuration = configuration
        self.runner = runner
    }

    /// Guest access status in words (menu bar, control center, setup).
    var summary: String {
        guard cli != nil else { return "Tailscale isn’t installed" }
        guard let s = status else { return lastError == nil ? "Checking Tailscale…" : "Tailscale isn’t running" }
        if s.needsLogin { return "Sign in to Tailscale" }
        if !s.loggedIn { return "Tailscale is \(s.backendState.lowercased())" }
        switch observation?.mode {
        case .publicFunnel?: return "Guests can join from anywhere"
        case .privateTailnet?: return "Guests on your Tailscale network can join"
        case .off?: return "Only this Mac can join"
        case nil: return "Guest links are partly set up"
        }
    }

    func refresh() async {
        refreshing = true
        defer { refreshing = false }
        let env = ProcessInfo.processInfo.environment
        cli = TailscaleCLILocator.locate(path: env["PATH"], home: FileManager.default.homeDirectoryForCurrentUser) {
            FileManager.default.isExecutableFile(atPath: $0.path)
        }
        guard let cli else {
            status = nil; serve = nil; observation = nil
            lastError = "Install Tailscale from tailscale.com/download, sign in, then check again."
            return
        }
        do {
            let st = try await runner.run(cli, arguments: TailscaleCommands.status.arguments,
                                          environment: TailscaleCommands.environment(base: env), timeout: 8)
            // `tailscale status --json` exits non-zero when logged out but still prints JSON.
            let parsed = try TailscaleStatus.parse(st.stdout)
            status = parsed
            lastError = nil
            if parsed.loggedIn, let name = parsed.dnsName {
                let sv = try await runner.run(cli, arguments: TailscaleCommands.serveStatus.arguments,
                                              environment: TailscaleCommands.environment(base: env), timeout: 8)
                let cfg = sv.succeeded ? try ServeConfig.parse(sv.stdout) : .empty
                serve = cfg
                observation = GuestAccessObservation.observe(cfg, dnsName: name, port: configuration.port, turnPort: configuration.turnPort)
            } else {
                serve = nil
                observation = nil
            }
        } catch {
            status = nil
            lastError = "Tailscale is not running or did not answer. Open Tailscale, sign in, then check again."
        }
    }

    // MARK: plans

    func plan(_ kind: Plan.Kind) -> Plan? {
        let port = configuration.port
        var warnings: [String] = []
        switch kind {
        case .enablePrivate:
            if let other = observation?.conflictingAppTarget {
                warnings.append("This Mac’s secure link currently points to something else (\(other)). Turning this on replaces it.")
            }
            return Plan(kind: kind, title: "Let guests on your Tailscale network join",
                        explanation: "People on your Tailscale network, or people you’ve shared this Mac with, can open your meeting links. Nothing is shared on the public internet.",
                        details: "Publishes the studio with Tailscale Serve at this Mac’s HTTPS address (port 443, tailnet only), then restarts p2p.video’s background service with --no-funnel and --public-url.",
                        actionLabel: "Turn On",
                        commands: TailscaleCommands.enablePrivate(port: port), warnings: warnings, resultingMode: .privateTailnet)
        case .enablePublic:
            guard let turn = configuration.turnPort else { return nil }
            if let other = observation?.conflictingAppTarget {
                warnings.append("This Mac’s secure link currently points to something else (\(other)). Turning this on replaces it and makes it public.")
            }
            if let other = observation?.conflictingTURNTarget {
                warnings.append("Port 8443 currently forwards to \(other). Turning this on replaces it.")
            }
            return Plan(kind: kind, title: "Let guests join from anywhere",
                        explanation: "Anyone with a meeting link can open it in their browser and ask to join, no Tailscale needed. You still let each guest in, and your p2p.video controls stay on this Mac.",
                        details: "Publishes the guest page through Tailscale Funnel on port 443, and a relay on port 8443 (TURN over TLS) for guests behind strict networks, then restarts p2p.video’s background service with --public-url and --funnel-turn. The first time, Tailscale may ask you to allow public links (Funnel) for this Mac.",
                        actionLabel: "Turn On",
                        commands: TailscaleCommands.enablePublic(port: port, turnPort: turn), warnings: warnings, resultingMode: .publicFunnel)
        case .disablePublic:
            return Plan(kind: kind, title: "Stop letting guests join from anywhere",
                        explanation: "Meeting links stop working on the public internet. Guests on your Tailscale network can still join.",
                        details: "Re-serves port 443 without Funnel and removes the port 8443 relay route.",
                        actionLabel: "Turn Off",
                        commands: TailscaleCommands.disablePublic(port: port), warnings: [], resultingMode: .privateTailnet)
        case .disablePrivate:
            return Plan(kind: kind, title: "Turn off guest links",
                        explanation: "Only this Mac can open p2p.video. Meeting links stop working for guests.",
                        details: "Removes the Tailscale Serve route on port 443.",
                        actionLabel: "Turn Off",
                        commands: TailscaleCommands.disablePrivate(),
                        warnings: observation?.anythingPublic == true ? ["Stop letting guests join from anywhere first."] : [],
                        resultingMode: .off)
        }
    }

    /// Runs a confirmed plan, stopping at the first failure or approval
    /// prompt, then re-reads and verifies the configuration.
    func run(_ plan: Plan) async -> Result<GuestAccessMode, SetupError> {
        guard let cli else { return .failure(.message("Tailscale isn’t installed.")) }
        running = true
        defer { running = false }
        transcript = []
        pendingApproval = nil
        // Log the commands only; their output can carry links and stays in the UI.
        for command in plan.commands {
            appLog.notice("Running confirmed Tailscale command: \(command.display, privacy: .public)")
        }
        let env = TailscaleCommands.environment(base: ProcessInfo.processInfo.environment)
        let (outcome, lines) = await TailscalePlanRunner(cli: cli, runner: runner, environment: env).run(plan.commands)
        transcript = lines
        switch outcome {
        case .completed:
            break
        case .needsApproval(let approval, _):
            appLog.notice("Tailscale asked for approval (\(approval.feature.rawValue, privacy: .public)); waiting for the owner")
            pendingApproval = PendingApproval(approval: approval, kind: plan.kind)
            await refresh()
            return .failure(.needsApproval(approval))
        case .failed, .timedOut, .couldNotRun:
            await refresh()
            return .failure(.message(outcome.failureMessage ?? "Tailscale couldn’t finish the change."))
        }
        await refresh()
        guard let obs = observation else { return .failure(.message("Couldn’t read Tailscale’s settings after the change. Check again in a moment.")) }
        if obs.mode == plan.resultingMode { return .success(plan.resultingMode) }
        if plan.resultingMode != .publicFunnel, obs.anythingPublic {
            return .failure(.message("p2p.video is still reachable from the internet. Run `tailscale funnel status` and turn it off there."))
        }
        return .failure(.message("Tailscale accepted the change, but it doesn’t show up yet (\(obs.mode.map { "\($0)" } ?? "partial")). Check again in a moment."))
    }

    #if DEBUG
    /// Snapshot aid only: shows the approval step without running Tailscale.
    func showApprovalForSnapshot(_ approval: TailscaleApproval) {
        pendingApproval = PendingApproval(approval: approval, kind: .enablePublic)
    }
    #endif

    enum SetupError: Error, Equatable {
        case message(String)
        /// Tailscale printed an enable link and is waiting for the owner.
        case needsApproval(TailscaleApproval)
    }
}
