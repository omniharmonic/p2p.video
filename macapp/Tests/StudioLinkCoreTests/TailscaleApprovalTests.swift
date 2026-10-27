import Foundation
import StudioLinkCore
import XCTest

/// The exact text the owner saw on 2026-10-27 (node id redacted to a sample).
private let funnelPrompt = """
Funnel is not enabled on your tailnet.
To enable, visit:

         https://login.tailscale.com/f/funnel?node=nXyZ123CNTRL

"""

final class TailscaleApprovalTests: XCTestCase {
    func testFindsTheFunnelEnableLink() throws {
        let a = try XCTUnwrap(TailscaleApproval.find(in: funnelPrompt))
        XCTAssertEqual(a.feature, .funnel)
        XCTAssertEqual(a.url.absoluteString, "https://login.tailscale.com/f/funnel?node=nXyZ123CNTRL")
        XCTAssertEqual(a.title, "Allow public links in Tailscale")
        XCTAssertFalse(a.explanation.isEmpty)
    }

    func testDifferentNodeIDs() throws {
        for node in ["n1", "nABCdef9876CNTRL", "kH7s-2_x"] {
            let text = funnelPrompt.replacingOccurrences(of: "nXyZ123CNTRL", with: node)
            XCTAssertEqual(TailscaleApproval.find(in: text)?.url.absoluteString, "https://login.tailscale.com/f/funnel?node=\(node)")
        }
    }

    func testNoLinkMeansNoApproval() {
        XCTAssertNil(TailscaleApproval.find(in: ""))
        XCTAssertNil(TailscaleApproval.find(in: "error: failed to connect to local Tailscale daemon"))
        // A successful funnel run prints the public address, which is not an approval page.
        XCTAssertNil(TailscaleApproval.find(in: """
        Available on the internet:

        https://studio.tail1234.ts.net/
        |-- proxy http://127.0.0.1:7443

        Serve started and running in the background.
        """))
        // Docs links are not approval pages either.
        XCTAssertNil(TailscaleApproval.find(in: "See https://tailscale.com/kb/1223/funnel for details."))
        XCTAssertNil(TailscaleApproval.find(in: "See https://login.tailscale.com/kb/1223 for details."))
    }

    func testServeVariant() throws {
        let a = try XCTUnwrap(TailscaleApproval.find(in: """
        Serve is not enabled on your tailnet.
        To enable, visit:

                 https://login.tailscale.com/f/serve?node=nQ8R7CNTRL
        """))
        XCTAssertEqual(a.feature, .serve)
        XCTAssertEqual(a.url.absoluteString, "https://login.tailscale.com/f/serve?node=nQ8R7CNTRL")
        XCTAssertEqual(a.title, "Allow secure links in Tailscale")
    }

    func testHTTPSCertificateVariant() throws {
        let a = try XCTUnwrap(TailscaleApproval.find(in: """
        HTTPS is not enabled on your tailnet.
        To enable, visit:

                 https://login.tailscale.com/f/https?node=nQ8R7CNTRL.
        """))
        XCTAssertEqual(a.feature, .serve)
        XCTAssertEqual(a.url.absoluteString, "https://login.tailscale.com/f/https?node=nQ8R7CNTRL", "trailing punctuation is trimmed")
    }

    func testCustomControlServerAfterToEnable() throws {
        let a = try XCTUnwrap(TailscaleApproval.find(in: """
        Funnel is not enabled on your tailnet.
        To enable, visit:

                 https://control.example.org/enable/xyz
        """))
        XCTAssertEqual(a.feature, .funnel, "feature comes from the surrounding text")
        XCTAssertEqual(a.url.host, "control.example.org")
    }

    func testPartialOutputWithoutTheLinkYet() {
        XCTAssertNil(TailscaleApproval.find(in: "Funnel is not enabled on your tailnet.\nTo enable, visit:\n\n"))
        XCTAssertNil(TailscaleApproval.find(in: "Funnel is not enabled on your tailnet.\nTo enable, visit:\n\n         https://"))
    }
}

/// A scripted runner: one canned result per call, recording the argv it saw.
private final class FakeRunner: CommandRunning, @unchecked Sendable {
    private let lock = NSLock()
    private var results: [CommandResult]
    private var seen: [[String]] = []
    var calls: [[String]] { lock.withLock { seen } }

    init(_ results: [CommandResult]) { self.results = results }

    func run(_ executable: URL, arguments: [String], environment: [String: String], timeout: TimeInterval) async throws -> CommandResult {
        lock.withLock {
            seen.append(arguments)
            return results.isEmpty ? CommandResult(status: 0) : results.removeFirst()
        }
    }
}

final class TailscalePlanRunnerTests: XCTestCase {
    private let cli = URL(fileURLWithPath: "/usr/local/bin/tailscale")
    private let plan = TailscaleCommands.enablePublic(port: 7443, turnPort: 3478)

    func testStopsAtTheApprovalPromptAndRunsNothingAfter() async throws {
        let fake = FakeRunner([CommandResult(status: -1, stdout: Data(funnelPrompt.utf8), timedOut: false)])
        let (outcome, transcript) = await TailscalePlanRunner(cli: cli, runner: fake, environment: [:]).run(plan)
        guard case .needsApproval(let approval, let command) = outcome else { return XCTFail("got \(outcome)") }
        XCTAssertEqual(approval.feature, .funnel)
        XCTAssertEqual(command, plan[0])
        XCTAssertEqual(fake.calls.count, 1, "the TURN commands must not run before approval")
        XCTAssertEqual(transcript.first, "$ \(plan[0].display)")
        XCTAssertNil(outcome.failureMessage)
    }

    func testTimedOutWithThePromptIsStillAnApproval() async {
        let fake = FakeRunner([CommandResult(status: -1, stderr: Data(funnelPrompt.utf8), timedOut: true)])
        let outcome = await TailscalePlanRunner(cli: cli, runner: fake, environment: [:]).run(plan).outcome
        guard case .needsApproval = outcome else { return XCTFail("got \(outcome)") }
    }

    func testAllCommandsSucceed() async {
        let fake = FakeRunner([CommandResult(status: 0), CommandResult(status: 0), CommandResult(status: 0)])
        let outcome = await TailscalePlanRunner(cli: cli, runner: fake, environment: [:]).run(plan).outcome
        XCTAssertEqual(outcome, .completed)
        XCTAssertEqual(fake.calls, plan.map(\.arguments))
    }

    func testFailureAndTimeoutWithoutALink() async {
        let failing = FakeRunner([CommandResult(status: 0), CommandResult(status: 1, stderr: Data("boom".utf8))])
        let failed = await TailscalePlanRunner(cli: cli, runner: failing, environment: [:]).run(plan).outcome
        XCTAssertEqual(failed, .failed(command: plan[1], status: 1))
        XCTAssertNotNil(failed.failureMessage)

        let slow = FakeRunner([CommandResult(status: -1, timedOut: true)])
        let timedOut = await TailscalePlanRunner(cli: cli, runner: slow, environment: [:]).run(plan).outcome
        XCTAssertEqual(timedOut, .timedOut(command: plan[0]))
        XCTAssertFalse(timedOut.failureMessage?.contains("login.tailscale.com") ?? true)
    }
}

final class StreamingCommandRunnerTests: XCTestCase {
    /// A real process that prints the prompt and then waits, like Tailscale:
    /// the runner must stop it as soon as the link appears, not at the timeout.
    func testStopsAWaitingProcessAsSoonAsTheLinkIsPrinted() async throws {
        let script = "printf 'Funnel is not enabled on your tailnet.\\nTo enable, visit:\\n\\n         https://login.tailscale.com/f/funnel?node=n42\\n'; sleep 20"
        let start = Date()
        let r = try await ProcessCommandRunner().run(URL(fileURLWithPath: "/bin/sh"), arguments: ["-c", script], environment: [:], timeout: 15) {
            TailscaleApproval.find(in: $0) != nil
        }
        XCTAssertTrue(r.stoppedEarly)
        XCTAssertFalse(r.timedOut)
        XCTAssertFalse(r.succeeded)
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
        XCTAssertEqual(TailscaleApproval.find(in: r.stdoutText)?.url.absoluteString, "https://login.tailscale.com/f/funnel?node=n42")
    }

    func testNoMatchRunsToCompletion() async throws {
        let r = try await ProcessCommandRunner().run(URL(fileURLWithPath: "/usr/bin/printf"), arguments: ["ok"], environment: [:], timeout: 5) { _ in false }
        XCTAssertTrue(r.succeeded)
        XCTAssertFalse(r.stoppedEarly)
        XCTAssertEqual(r.stdoutText, "ok")
    }
}
