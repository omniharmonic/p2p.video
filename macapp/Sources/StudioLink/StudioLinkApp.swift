import AppKit
import StudioLinkCore
import SwiftUI

/// Quits through a run-loop perform instead of calling `terminate` inside a
/// main-queue block: `applicationShouldTerminate` answers `.terminateLater`
/// and finishes in a main-actor Task, which cannot run while the main dispatch
/// queue is still executing the caller (found when SIGTERM deadlocked).
@MainActor
func requestQuit() {
    NSApp.perform(#selector(NSApplication.terminate(_:)), with: nil, afterDelay: 0)
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let model: AppModel?
    let startupError: String?
    private var probe: WebViewProbe?
    private var studioProbe: StudioProbe?

    override init() {
        do {
            let resources = Bundle.main.resourceURL ?? URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent()
            let home = FileManager.default.homeDirectoryForCurrentUser
            let environment = ProcessInfo.processInfo.environment
            var cfg = try HostConfiguration.resolve(environment: environment, resources: resources, home: home)
            // One-time StudioLink → p2p.video data move [SL-REL-1], before
            // anything reads or writes the data folder.
            switch LegacyMigrator.run(configuration: cfg, home: home) {
            case .blocked(let message):
                model = nil
                startupError = message
                super.init()
                return
            case .migrated:
                cfg = try HostConfiguration.resolve(environment: environment, resources: resources, home: home)
            case .none:
                break
            }
            model = AppModel(configuration: cfg)
            startupError = nil
        } catch {
            model = nil
            startupError = "\(error)"
        }
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.regular)
        guard let model else { return }
        appLog.notice("p2p.video app launched; port \(model.configuration.port, privacy: .public) data \(model.configuration.dataDirectory.path, privacy: .public)")
        if let output = model.configuration.webViewProbeOutput {
            Task { await runProbe(model: model, output: output) }
            return
        }
        if let output = model.configuration.studioProbeOutput {
            let probe = StudioProbe(model: model, output: output)
            studioProbe = probe
            Task {
                let passed = await probe.run()
                await model.supervisor.stop()
                appLog.notice("Studio probe finished: \(passed ? "passed" : "FAILED", privacy: .public)")
                exit(passed ? 0 : 1)
            }
            return
        }
        installTerminationSignalHandler()
        let snapshotDir = ProcessInfo.processInfo.environment["STUDIOLINK_SNAPSHOT_DIR"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0, isDirectory: true) }
        Task {
            await model.launch()
            if let snapshotDir {
                for _ in 0..<50 where model.hostState == nil { try? await Task.sleep(for: .milliseconds(200)) }
                Snapshotter.captureAll(model: model, directory: snapshotDir)
            }
        }
        if let snapshotDir {
            // Development aid: `kill -USR1 <pid>` re-captures into <dir>/live.
            signal(SIGUSR1, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: SIGUSR1, queue: .main)
            source.setEventHandler {
                MainActor.assumeIsolated { Snapshotter.captureAll(model: model, directory: snapshotDir.appendingPathComponent("live")) }
            }
            source.resume()
            snapshotSource = source
        }
    }

    private var signalSource: DispatchSourceSignal?
    private var snapshotSource: DispatchSourceSignal?

    /// SIGTERM (e.g. `kill`, launchd) quits through the normal path so the
    /// daemon is stopped cleanly instead of being orphaned.
    private func installTerminationSignalHandler() {
        signal(SIGTERM, SIG_IGN)
        let source = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        source.setEventHandler { [weak self] in
            MainActor.assumeIsolated {
                appLog.notice("SIGTERM received; stopping the host and quitting")
                self?.terminatingFromSignal = true
                requestQuit()
            }
        }
        source.resume()
        signalSource = source
    }

    private var terminatingFromSignal = false

    private func runProbe(model: AppModel, output: URL) async {
        await model.supervisor.start()
        guard model.supervisor.isHealthy else {
            try? Data("{\"note\":\"daemon not healthy: \(model.problem ?? "unknown")\"}".utf8).write(to: output)
            requestQuit()
            return
        }
        // Pass 1: real devices through the permission delegate and macOS TCC.
        let real = WebViewProbe(port: model.configuration.port, output: output)
        self.probe = real
        real.run { [weak self] _ in
            // Pass 2: WebKit mock devices (TCC bypassed), same checks.
            let mockOutput = output.deletingPathExtension().appendingPathExtension("mock.json")
            let mock = WebViewProbe(port: model.configuration.port, output: mockOutput, mockDevices: true)
            self?.probe = mock
            mock.run { _ in requestQuit() }
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard let model else { return .terminateNow }
        let probing = model.configuration.webViewProbeOutput != nil || model.configuration.studioProbeOutput != nil
        if !probing, !terminatingFromSignal,
           let warning = StudioLifecycle.quitMessage(state: model.hostState, window: model.studioWindow.windowState) {
            let alert = NSAlert()
            alert.alertStyle = .warning
            alert.messageText = "Quit p2p.video and stop the host?"
            alert.informativeText = "\(warning) Quitting stops the host: guests are disconnected and uploads pause until you reopen p2p.video."
            alert.addButton(withTitle: "Keep Running")
            alert.addButton(withTitle: "Quit Anyway")
            if alert.runModal() == .alertFirstButtonReturn { return .terminateCancel }
        }
        model.studioWindow.closeForQuit()
        Task { @MainActor in
            await model.supervisor.stop()
            appLog.notice("p2p.video host stopped; app quitting")
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }
}

@main
struct StudioLinkApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        Window("p2p.video", id: "main") {
            if let model = delegate.model {
                ControlCenterView(model: model)
            } else {
                StartupErrorView(message: delegate.startupError ?? "Unknown error")
            }
        }
        .defaultSize(width: 760, height: 560)
        .windowResizability(.contentMinSize)

        Window("Set Up p2p.video", id: "onboarding") {
            if let model = delegate.model { OnboardingView(model: model) }
        }
        .defaultSize(width: 780, height: 560)
        .windowResizability(.contentSize)
        .defaultPosition(.center)

        MenuBarExtra {
            if let model = delegate.model {
                MenuBarContent(model: model)
            } else {
                Text("p2p.video could not start")
                Button("Quit") { requestQuit() }
            }
        } label: {
            MenuBarLabel(model: delegate.model)
        }

        Settings {
            if let model = delegate.model { SettingsView(model: model) }
        }
        .commands {
            CommandGroup(replacing: .appInfo) {
                Button("About \(ProductInfo.name)") { showAboutPanel(buildID: delegate.model?.configuration.expectedBuildID) }
            }
        }
    }
}

/// The standard About panel plus the license and a source-code link (AGPL §13).
@MainActor
func showAboutPanel(buildID: String?) {
    let credits = NSMutableAttributedString(
        string: "Free software under the \(ProductInfo.license).\nSource code: ",
        attributes: [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor])
    credits.append(NSAttributedString(string: ProductInfo.sourceURL.absoluteString,
                                      attributes: [.font: NSFont.systemFont(ofSize: 11), .link: ProductInfo.sourceURL]))
    if let buildID, !buildID.isEmpty {
        credits.append(NSAttributedString(string: "\nBuild \(buildID)",
                                          attributes: [.font: NSFont.systemFont(ofSize: 11), .foregroundColor: NSColor.secondaryLabelColor]))
    }
    let paragraph = NSMutableParagraphStyle()
    paragraph.alignment = .center
    credits.addAttribute(.paragraphStyle, value: paragraph, range: NSRange(location: 0, length: credits.length))
    NSApp.orderFrontStandardAboutPanel(options: [.applicationName: ProductInfo.name, .credits: credits])
    NSApp.activate(ignoringOtherApps: true)
}

struct MenuBarLabel: View {
    let model: AppModel?
    var body: some View {
        if let model {
            MenuBarLabelContent(model: model)
        } else {
            Image(systemName: "video.slash")
        }
    }
}

private struct MenuBarLabelContent: View {
    @ObservedObject var model: AppModel
    var body: some View {
        Image(systemName: model.menuBarSymbol)
            .accessibilityLabel("p2p.video: \(model.statusLine)")
    }
}

struct StartupErrorView: View {
    let message: String
    var body: some View {
        VStack(spacing: 12) {
            Image(systemName: "exclamationmark.triangle.fill").font(.largeTitle).foregroundStyle(.orange)
            Text("p2p.video could not start").font(.title3.bold())
            Text(message).textSelection(.enabled).multilineTextAlignment(.center).foregroundStyle(.secondary)
            Button("Quit") { requestQuit() }.keyboardShortcut(.defaultAction)
        }
        .padding(32)
        .frame(minWidth: 420)
    }
}
