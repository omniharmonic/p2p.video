import Foundation
import StudioLinkCore
import XCTest

final class CommandRunnerTests: XCTestCase {
    func testArgumentsAreNeverShellInterpreted() async throws {
        let r = try await ProcessCommandRunner().run(URL(fileURLWithPath: "/usr/bin/printf"), arguments: ["%s|", "a b", "$(whoami)", "; echo pwned"],
                                                     environment: [:], timeout: 5)
        XCTAssertTrue(r.succeeded)
        XCTAssertEqual(r.stdoutText, "a b|$(whoami)|; echo pwned|")
    }

    func testCapturesStatusAndStderr() async throws {
        let r = try await ProcessCommandRunner().run(URL(fileURLWithPath: "/bin/ls"), arguments: ["/definitely/not/here"], environment: [:], timeout: 5)
        XCTAssertFalse(r.succeeded)
        XCTAssertNotEqual(r.status, 0)
        XCTAssertFalse(r.stderrText.isEmpty)
    }

    func testTimeoutTerminates() async throws {
        let start = Date()
        let r = try await ProcessCommandRunner().run(URL(fileURLWithPath: "/bin/sleep"), arguments: ["5"], environment: [:], timeout: 0.3)
        XCTAssertTrue(r.timedOut)
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
    }
}
