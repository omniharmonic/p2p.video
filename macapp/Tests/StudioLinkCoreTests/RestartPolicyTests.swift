import Foundation
import StudioLinkCore
import XCTest

final class RestartPolicyTests: XCTestCase {
    let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    func testCrashBackoffIsExponentialAndCapped() {
        let p = RestartPolicy()
        XCTAssertEqual((0...7).map { p.backoff(attempt: $0) }, [0, 1, 2, 4, 8, 16, 30, 30])
    }

    func testCrashesRestartThenGiveUpWithinWindow() {
        var p = RestartPolicy()
        var decisions: [RestartDecision] = []
        for i in 0..<6 {
            decisions.append(p.decide(.exited(status: 2), lastKnown: .idle, now: t0.addingTimeInterval(Double(i) * 5)))
        }
        XCTAssertEqual(Array(decisions.prefix(5)), [.restart(after: 1), .restart(after: 2), .restart(after: 4), .restart(after: 8), .restart(after: 16)])
        guard case .giveUp(let reason) = decisions[5] else { return XCTFail("expected give up, got \(decisions[5])") }
        XCTAssertTrue(reason.contains("daemon.log"))
    }

    func testCrashesOutsideTheWindowDoNotAccumulate() {
        var p = RestartPolicy()
        for i in 0..<10 {
            let d = p.decide(.exited(status: 1), lastKnown: nil, now: t0.addingTimeInterval(Double(i) * 200))
            XCTAssertEqual(d, .restart(after: 1))
        }
    }

    func testStableRunResetsBackoff() {
        var p = RestartPolicy()
        _ = p.decide(.exited(status: 1), lastKnown: nil, now: t0)
        _ = p.decide(.exited(status: 1), lastKnown: nil, now: t0.addingTimeInterval(1))
        p.noteHealthy(since: t0.addingTimeInterval(2), now: t0.addingTimeInterval(70))
        XCTAssertEqual(p.decide(.exited(status: 1), lastKnown: nil, now: t0.addingTimeInterval(71)), .restart(after: 1))
    }

    // A process that has exited cannot be recording; restarting lets journaled
    // uploads resume, so a crash restart is allowed even mid-take.
    func testCrashDuringRecordingStillRestarts() {
        var p = RestartPolicy()
        XCTAssertEqual(p.decide(.exited(status: 9), lastKnown: DaemonActivity(recording: true), now: t0), .restart(after: 1))
    }

    func testUnresponsiveDaemonIsNeverKilledWhileRecordingOrLive() {
        var p = RestartPolicy()
        XCTAssertEqual(p.decide(.unresponsive(consecutiveFailures: 2), lastKnown: .idle, now: t0), .none)
        for a in [DaemonActivity(recording: true), DaemonActivity(live: true), DaemonActivity(recording: true, uploadsPending: 4)] {
            guard case .deferUntilIdle = p.decide(.unresponsive(consecutiveFailures: 5), lastKnown: a, now: t0) else {
                return XCTFail("restarted an unresponsive daemon during \(a)")
            }
        }
        guard case .deferUntilIdle = p.decide(.unresponsive(consecutiveFailures: 5), lastKnown: nil, now: t0) else {
            return XCTFail("unknown recording state must not restart")
        }
        XCTAssertEqual(p.decide(.unresponsive(consecutiveFailures: 3), lastKnown: .idle, now: t0), .restart(after: 1))
    }

    func testConfigurationRestartWaitsForRecordingLiveAndUploads() {
        var p = RestartPolicy()
        for a in [DaemonActivity(recording: true), DaemonActivity(live: true), DaemonActivity(uploadsPending: 1)] {
            guard case .deferUntilIdle(let reason) = p.decide(.configurationChanged, lastKnown: a, now: t0) else {
                return XCTFail("config restart during \(a)")
            }
            XCTAssertFalse(reason.isEmpty)
        }
        guard case .deferUntilIdle = p.decide(.configurationChanged, lastKnown: nil, now: t0) else {
            return XCTFail("config restart with unknown state")
        }
        XCTAssertEqual(p.decide(.configurationChanged, lastKnown: .idle, now: t0), .restart(after: 0))
    }
}
