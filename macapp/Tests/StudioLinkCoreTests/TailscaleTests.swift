import Foundation
import StudioLinkCore
import XCTest

final class TailscaleCommandTests: XCTestCase {
    // Golden argv for every command the app can run.
    func testReadOnlyCommands() {
        XCTAssertEqual(TailscaleCommands.status.arguments, ["status", "--json"])
        XCTAssertEqual(TailscaleCommands.serveStatus.arguments, ["serve", "status", "--json"])
        XCTAssertEqual(TailscaleCommands.funnelStatus.arguments, ["funnel", "status", "--json"])
        XCTAssertFalse(TailscaleCommands.status.mutating || TailscaleCommands.serveStatus.mutating || TailscaleCommands.funnelStatus.mutating)
    }

    func testPrivatePlan() {
        let plan = TailscaleCommands.enablePrivate(port: 7443)
        XCTAssertEqual(plan.map(\.arguments), [["serve", "--bg", "--https=443", "http://127.0.0.1:7443"]])
        XCTAssertEqual(plan.map(\.display), ["tailscale serve --bg --https=443 http://127.0.0.1:7443"])
        XCTAssertTrue(plan.allSatisfy(\.mutating))
        XCTAssertFalse(plan.flatMap(\.arguments).contains("funnel"), "private mode never touches Funnel")
    }

    func testPublicPlan() {
        let plan = TailscaleCommands.enablePublic(port: 7643, turnPort: 3578)
        XCTAssertEqual(plan.map(\.arguments), [
            ["funnel", "--bg", "--https=443", "http://127.0.0.1:7643"],
            ["serve", "--bg", "--tls-terminated-tcp=8443", "tcp://127.0.0.1:3578"],
            ["funnel", "--bg", "--tls-terminated-tcp=8443", "tcp://127.0.0.1:3578"],
        ])
        XCTAssertTrue(plan.allSatisfy(\.mutating))
    }

    func testTurnOffPlans() {
        XCTAssertEqual(TailscaleCommands.disablePublic(port: 7443).map(\.arguments), [
            ["serve", "--bg", "--https=443", "http://127.0.0.1:7443"],
            ["serve", "--tls-terminated-tcp=8443", "off"],
        ])
        XCTAssertEqual(TailscaleCommands.disablePrivate().map(\.arguments), [["serve", "--https=443", "off"]])
    }

    func testNoPlanUsesResetOrShellSyntax() {
        let all = TailscaleCommands.enablePrivate(port: 1) + TailscaleCommands.enablePublic(port: 1, turnPort: 2)
            + TailscaleCommands.disablePublic(port: 1) + TailscaleCommands.disablePrivate()
        for c in all {
            XCTAssertFalse(c.arguments.contains("reset"), "never reset unrelated Serve routes")
            for a in c.arguments { XCTAssertNil(a.rangeOfCharacter(from: CharacterSet(charactersIn: " ;|&$`'\"<>")), a) }
        }
    }

    func testDisplayQuotingIsShellSafe() {
        XCTAssertEqual(TailscaleCommand(["serve", "a b", "it's"], mutating: true).display, #"tailscale serve 'a b' 'it'\''s'"#)
    }

    func testCLIEnvironmentForcesCLIMode() {
        XCTAssertEqual(TailscaleCommands.environment(base: ["PATH": "/bin"])["TAILSCALE_BE_CLI"], "1")
    }

    func testLocatorPrefersPATHThenMacApp() {
        let home = URL(fileURLWithPath: "/Users/t")
        let c = TailscaleCLILocator.candidates(path: "/custom/bin:/usr/local/bin", home: home).map(\.path)
        XCTAssertEqual(c, ["/custom/bin/tailscale", "/usr/local/bin/tailscale", "/opt/homebrew/bin/tailscale",
                           "/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/Users/t/Applications/Tailscale.app/Contents/MacOS/Tailscale"])
        let found = TailscaleCLILocator.locate(path: "/usr/bin", home: home) { $0.path.hasPrefix("/Applications/") }
        XCTAssertEqual(found?.path, "/Applications/Tailscale.app/Contents/MacOS/Tailscale")
        XCTAssertNil(TailscaleCLILocator.locate(path: nil, home: home) { _ in false })
    }
}

final class TailscaleStatusTests: XCTestCase {
    func testRunningWithHTTPS() throws {
        let s = try TailscaleStatus.parse(Fixture.data("tailscale-status-running.json"))
        XCTAssertTrue(s.loggedIn)
        XCTAssertFalse(s.needsLogin)
        XCTAssertEqual(s.dnsName, "studio-mac.tail1234.ts.net")
        XCTAssertEqual(s.httpsOrigin, "https://studio-mac.tail1234.ts.net")
        XCTAssertTrue(s.magicDNSEnabled)
        XCTAssertEqual(s.magicDNSSuffix, "tail1234.ts.net")
        XCTAssertTrue(s.httpsAvailable)
        XCTAssertFalse(s.funnelAllowed)
        XCTAssertFalse(s.funnelReadyForPublicGuests)
    }

    func testFunnelCapabilityAndPorts() throws {
        let s = try TailscaleStatus.parse(Fixture.data("tailscale-status-funnel.json"))
        XCTAssertTrue(s.funnelAllowed)
        XCTAssertEqual(s.funnelPorts, [443, 8443, 10000])
        XCTAssertTrue(s.funnelReadyForPublicGuests)
        let only443 = try TailscaleStatus.parse(Fixture.data("tailscale-status-funnel-443-only.json"))
        XCTAssertTrue(only443.funnelAllowed)
        XCTAssertFalse(only443.funnelReadyForPublicGuests, "TURN needs Funnel on 8443 too")
    }

    func testNeedsLogin() throws {
        let s = try TailscaleStatus.parse(Fixture.data("tailscale-status-needs-login.json"))
        XCTAssertFalse(s.loggedIn)
        XCTAssertTrue(s.needsLogin)
        XCTAssertNil(s.dnsName)
        XCTAssertNil(s.httpsOrigin)
        XCTAssertFalse(s.httpsAvailable)
    }
}

final class ServeObservationTests: XCTestCase {
    let dnsName = "studio-mac.tail1234.ts.net"

    func observe(_ fixture: String?, port: Int = 7443, turn: Int? = 3478) throws -> GuestAccessObservation {
        let cfg = try fixture.map { try ServeConfig.parse(Fixture.data($0)) } ?? ServeConfig.parse(Data("{}\n".utf8))
        return GuestAccessObservation.observe(cfg, dnsName: dnsName, port: port, turnPort: turn)
    }

    func testEmptyIsOff() throws {
        let o = try observe(nil)
        XCTAssertEqual(o.mode, .off)
        XCTAssertFalse(o.anythingPublic)
        XCTAssertEqual(try ServeConfig.parse(Data()), .empty)
    }

    func testPrivate() throws {
        let o = try observe("serve-private.json")
        XCTAssertEqual(o.mode, .privateTailnet)
        XCTAssertEqual(o.origin, "https://studio-mac.tail1234.ts.net")
        XCTAssertFalse(o.anythingPublic)
    }

    func testPublic() throws {
        let o = try observe("serve-public.json")
        XCTAssertEqual(o.mode, .publicFunnel)
        XCTAssertTrue(o.anythingPublic)
    }

    func testRoutesToAnotherPortAreNotOurs() throws {
        let o = try observe("serve-public.json", port: 7643, turn: 3578)
        XCTAssertEqual(o.mode, .off)
        XCTAssertEqual(o.conflictingAppTarget, "http://127.0.0.1:7443")
        XCTAssertEqual(o.conflictingTURNTarget, "127.0.0.1:3478")
        XCTAssertFalse(o.anythingPublic)
    }

    func testConflictsAreReported() throws {
        let o = try observe("serve-conflict.json")
        XCTAssertEqual(o.conflictingAppTarget, "http://127.0.0.1:3000")
        XCTAssertEqual(o.conflictingTURNTarget, "127.0.0.1:9000")
        XCTAssertFalse(o.appRouted)
        XCTAssertFalse(o.anythingPublic)
    }

    func testPartialPublicIsNotMistakenForPrivate() throws {
        let o = try observe("serve-partial-public.json")
        XCTAssertNil(o.mode)
        XCTAssertTrue(o.anythingPublic)
    }
}

final class WebViewCapabilityTests: XCTestCase {
    func testInAppStudioNeedsEveryRequiredCapability() {
        let all = WebViewCapabilityReport.requiredNames.map { WebViewCapabilityReport.Check(name: $0, ok: true, required: true, detail: "") }
        var r = WebViewCapabilityReport(checks: all, userAgent: "", origin: "http://127.0.0.1:7443", mediaPermissionDelegateCalled: true, finishedAt: "")
        XCTAssertTrue(r.supportsInAppStudio)
        r.checks[1].ok = false
        XCTAssertEqual(r.missingRequired, ["getUserMedia"])
        XCTAssertFalse(r.supportsInAppStudio)
        r.checks.removeLast()
        XCTAssertEqual(r.missingRequired, ["getUserMedia", "rtcPeerConnection"])
    }
}
