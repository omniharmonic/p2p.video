import Foundation
import StudioLinkCore
import XCTest

final class HostConfigurationTests: XCTestCase {
    func testDefaultsMatchTheLauncher() throws {
        let c = try configuration()
        XCTAssertEqual(c.port, 7443)
        XCTAssertEqual(c.dataDirectory.path, "/Users/tester/Library/Application Support/p2p.video")
        XCTAssertEqual(c.daemonExecutable.path, "/Applications/p2p.video.app/Contents/Resources/bin/p2pvideod")
        XCTAssertEqual(c.toolsDirectory.path, "/Applications/p2p.video.app/Contents/Resources/bin")
        XCTAssertEqual(c.expectedBuildID, "BUILD-1")
        XCTAssertEqual(c.turnListen, "127.0.0.1:3478")
        XCTAssertEqual(c.turnPort, 3478)
        XCTAssertNil(c.udpPort)
        XCTAssertTrue(c.openBrowserOnLaunch)
        XCTAssertNil(c.webViewProbeOutput)
        XCTAssertEqual(c.daemonLog.lastPathComponent, "daemon.log")
        XCTAssertEqual(c.baseURL.absoluteString, "http://127.0.0.1:7443")
    }

    func testEnvironmentOverridesIsolateTestRuns() throws {
        let c = try configuration([
            "STUDIOLINK_PORT": "7643", "STUDIOLINK_DATA_DIR": "/tmp/iso", "STUDIOLINK_UDP_PORT": "50143",
            "STUDIOLINK_TURN_LISTEN": "127.0.0.1:3578", "STUDIOLINK_NO_OPEN": "1", "STUDIOLINK_WEBVIEW_PROBE": "/tmp/p.json",
        ])
        XCTAssertEqual(c.port, 7643)
        XCTAssertEqual(c.dataDirectory.path, "/tmp/iso")
        XCTAssertEqual(c.udpPort, 50143)
        XCTAssertEqual(c.turnPort, 3578)
        XCTAssertFalse(c.openBrowserOnLaunch)
        XCTAssertEqual(c.webViewProbeOutput?.path, "/tmp/p.json")
    }

    func testRejectsInvalidOverridesAndMissingBuild() {
        XCTAssertThrowsError(try configuration(["STUDIOLINK_PORT": "70000"]))
        XCTAssertThrowsError(try configuration(["STUDIOLINK_PORT": "abc"]))
        XCTAssertThrowsError(try configuration(["STUDIOLINK_UDP_PORT": "-1"]))
        XCTAssertThrowsError(try configuration(["STUDIOLINK_TURN_LISTEN": "0.0.0.0:3478"]))
        XCTAssertThrowsError(try HostConfiguration.resolve(environment: [:], resources: testResources, home: testHome) { _ in nil }) { error in
            XCTAssertEqual(error as? HostConfiguration.ResolveError, .missingBuildID(testResources.appendingPathComponent("build-id.txt")))
        }
    }
}

final class DaemonArgumentsTests: XCTestCase {
    // Golden argv: the exact flags the launcher used, plus explicit TURN listen.
    func testOffModeMatchesLauncher() throws {
        let args = try DaemonArguments.build(configuration: configuration(), settings: DesktopSettings())
        XCTAssertEqual(args, ["--port", "7443", "--data-dir", "/Users/tester/Library/Application Support/p2p.video",
                              "--turn-listen", "127.0.0.1:3478", "--no-funnel"])
    }

    func testOffModeHonorsLegacyPublicURL() throws {
        let s = DesktopSettings(guestAccess: .off, publicURL: "https://studio-mac.tail1234.ts.net/")
        let args = try DaemonArguments.build(configuration: configuration(), settings: s)
        XCTAssertEqual(Array(args.suffix(3)), ["--no-funnel", "--public-url", "https://studio-mac.tail1234.ts.net"])
    }

    func testPrivateMode() throws {
        let s = DesktopSettings(guestAccess: .privateTailnet, publicURL: "https://studio-mac.tail1234.ts.net")
        let args = try DaemonArguments.build(configuration: configuration(["STUDIOLINK_UDP_PORT": "50100"]), settings: s)
        XCTAssertEqual(args, ["--port", "7443", "--data-dir", "/Users/tester/Library/Application Support/p2p.video",
                              "--turn-listen", "127.0.0.1:3478", "--udp-port", "50100",
                              "--no-funnel", "--public-url", "https://studio-mac.tail1234.ts.net"])
    }

    func testPublicModeAdvertisesFunnelTURNAndNeverPassesNoFunnel() throws {
        let s = DesktopSettings(guestAccess: .publicFunnel, publicURL: "https://studio-mac.tail1234.ts.net", publicSTUN: false)
        let args = try DaemonArguments.build(configuration: configuration(), settings: s)
        XCTAssertEqual(args, ["--port", "7443", "--data-dir", "/Users/tester/Library/Application Support/p2p.video",
                              "--turn-listen", "127.0.0.1:3478",
                              "--public-url", "https://studio-mac.tail1234.ts.net", "--funnel-turn", "--no-public-stun"])
        XCTAssertFalse(args.contains("--no-funnel"), "--funnel-turn conflicts with --no-funnel in the daemon")
    }

    func testGuestModesRequireATailscaleHTTPSOrigin() throws {
        let c = try configuration()
        XCTAssertThrowsError(try DaemonArguments.build(configuration: c, settings: DesktopSettings(guestAccess: .privateTailnet)))
        for bad in ["http://studio-mac.tail1234.ts.net", "https://example.com", "https://ts.net", "https://a.ts.net/path",
                    "https://a.ts.net?x=1", "https://user@a.ts.net"] {
            XCTAssertThrowsError(try DaemonArguments.build(configuration: c, settings: DesktopSettings(guestAccess: .publicFunnel, publicURL: bad)), bad)
        }
    }

    func testArgumentsWithSpacesStayOneArgument() throws {
        let c = try configuration(["STUDIOLINK_DATA_DIR": "/tmp/a dir; rm -rf ~"])
        let args = try DaemonArguments.build(configuration: c, settings: DesktopSettings())
        XCTAssertEqual(args[3], "/tmp/a dir; rm -rf ~")
    }

    func testEnvironmentPutsBundledToolsFirst() throws {
        let env = DaemonArguments.environment(configuration: try configuration(), base: ["PATH": "/usr/bin:/bin", "HOME": "/Users/tester"])
        XCTAssertEqual(env["PATH"], "/Applications/p2p.video.app/Contents/Resources/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin")
        XCTAssertEqual(env["HOME"], "/Users/tester")
    }
}

final class DesktopSettingsTests: XCTestCase {
    func testRoundTripWithOwnerOnlyPermissions() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let file = dir.appendingPathComponent("desktop.json")
        let s = DesktopSettings(guestAccess: .publicFunnel, publicURL: "https://a.tail1.ts.net", publicSTUN: false, notifyGuestWaiting: false)
        try s.save(to: file)
        XCTAssertEqual(DesktopSettings.load(from: file), s)
        let perms = try FileManager.default.attributesOfItem(atPath: file.path)[.posixPermissions] as? Int
        XCTAssertEqual(perms, 0o600)
    }

    func testMissingFileUsesDefaultsAndLegacyPublicURL() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let legacy = dir.appendingPathComponent("public-url.txt")
        try "https://old.tail1.ts.net\n".write(to: legacy, atomically: true, encoding: .utf8)
        let s = DesktopSettings.load(from: dir.appendingPathComponent("desktop.json"), legacyPublicURL: legacy)
        XCTAssertEqual(s.guestAccess, .off)
        XCTAssertEqual(s.publicURL, "https://old.tail1.ts.net")
        XCTAssertTrue(s.publicSTUN)
        XCTAssertFalse(s.studioInApp, "the call stays in the browser by default (ADR SL-P2-1)")
    }

    func testTolerantDecodingOfOlderOrUnknownValues() throws {
        let s = try JSONDecoder().decode(DesktopSettings.self, from: Data(#"{"guestAccess":"somethingNew","publicSTUN":false}"#.utf8))
        XCTAssertEqual(s.guestAccess, .off)
        XCTAssertFalse(s.publicSTUN)
        XCTAssertTrue(s.notifyGuestWaiting)
        XCTAssertTrue(s.captionProvider, "older settings get the on-device caption provider [SL-P4-2]")
        let off = try JSONDecoder().decode(DesktopSettings.self, from: Data(#"{"captionProvider":false}"#.utf8))
        XCTAssertFalse(off.captionProvider)
    }
}
