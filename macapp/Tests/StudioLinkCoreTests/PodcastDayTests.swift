import Foundation
import StudioLinkCore
import XCTest

// [SL-P2-2] onboarding, checklist, browser, room creation, updates, quit.

final class OnboardingFlowTests: XCTestCase {
    func testWalksForwardThroughEveryStepThenCompletes() {
        var flow = OnboardingFlow()
        XCTAssertTrue(flow.isFirst)
        XCTAssertEqual(flow.position, 1)
        var seen: [OnboardingStep] = [flow.step]
        while !flow.isLast { flow.next(); seen.append(flow.step) }
        XCTAssertEqual(seen, [.welcome, .tailscale, .guestAccess, .browser, .firstRoom, .done])
        XCTAssertEqual(flow.position, OnboardingFlow.count)
        XCTAssertFalse(flow.completed)
        flow.next()
        XCTAssertTrue(flow.completed)
        XCTAssertEqual(flow.step, .done)
    }

    func testBackStopsAtWelcomeAndForwardJumpsAreIgnored() {
        var flow = OnboardingFlow()
        flow.back()
        XCTAssertEqual(flow.step, .welcome)
        flow.go(to: .firstRoom)
        XCTAssertEqual(flow.step, .welcome, "cannot skip ahead from the sidebar")
        flow.next(); flow.next(); flow.next()
        XCTAssertEqual(flow.step, .browser)
        flow.go(to: .tailscale)
        XCTAssertEqual(flow.step, .tailscale)
        flow.back()
        XCTAssertEqual(flow.step, .welcome)
    }

    func testFinishMarksCompletedFromAnyStep() {
        var flow = OnboardingFlow(step: .tailscale)
        flow.finish()
        XCTAssertTrue(flow.completed)
        XCTAssertEqual(flow.step, .done)
    }

    func testPrimaryLabelsReflectWhetherTheStepIsDone() {
        let fresh = OnboardingContext()
        XCTAssertEqual(OnboardingFlow.primaryLabel(.welcome, fresh), "Get Started")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.tailscale, fresh), "Skip for Now")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.guestAccess, fresh), "Decide Later")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.firstRoom, fresh), "Skip")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.done, fresh), "Finish")
        let ready = OnboardingContext(daemonHealthy: true, tailscaleInstalled: true, tailscaleLoggedIn: true, httpsAvailable: true,
                                      funnelAllowed: true, guestAccess: .privateTailnet, chromeInstalled: true, roomCount: 1)
        XCTAssertEqual(OnboardingFlow.primaryLabel(.tailscale, ready), "Continue")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.guestAccess, ready), "Continue")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.firstRoom, ready), "Continue")
        XCTAssertEqual(OnboardingFlow.primaryLabel(.done, ready), "Open p2p.video")
        for step in OnboardingStep.allCases { XCTAssertTrue(OnboardingFlow.satisfied(step, ready), "\(step)") }
        XCTAssertFalse(OnboardingFlow.satisfied(.tailscale, fresh))
        XCTAssertFalse(OnboardingFlow.satisfied(.guestAccess, fresh))
    }

    func testGuestChoicesNeedASignedInTailscaleWithHTTPS() {
        var c = OnboardingContext()
        XCTAssertEqual(OnboardingFlow.availability(of: .privateTailnet, c), .unavailable("Connect Tailscale first (previous step)."))
        XCTAssertTrue(OnboardingFlow.availability(of: .later, c).selectable)
        c.tailscaleInstalled = true
        XCTAssertFalse(OnboardingFlow.availability(of: .privateTailnet, c).selectable)
        c.tailscaleLoggedIn = true
        XCTAssertFalse(OnboardingFlow.availability(of: .publicLink, c).selectable, "HTTPS certificates are required")
        c.httpsAvailable = true
        XCTAssertEqual(OnboardingFlow.availability(of: .privateTailnet, c), .available)
        if case .availableWithCaveat = OnboardingFlow.availability(of: .publicLink, c) {} else { XCTFail("Funnel caveat expected") }
        c.funnelAllowed = true
        XCTAssertEqual(OnboardingFlow.availability(of: .publicLink, c), .available)
        c.guestAccess = .publicFunnel
        XCTAssertEqual(OnboardingFlow.availability(of: .publicLink, c), .current)
        XCTAssertFalse(OnboardingFlow.availability(of: .publicLink, c).selectable)
        XCTAssertEqual(GuestChoice.publicLink.mode, .publicFunnel)
        XCTAssertNil(GuestChoice.later.mode)
    }

    func testSettingsPersistCompletionAndBrowserAndOlderFilesDefault() throws {
        let old = try JSONDecoder().decode(DesktopSettings.self, from: Data(#"{"guestAccess":"privateTailnet","publicSTUN":false}"#.utf8))
        XCTAssertFalse(old.onboardingCompleted)
        XCTAssertNil(old.dashboardBrowser)
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        var s = DesktopSettings()
        s.onboardingCompleted = true
        s.dashboardBrowser = .systemDefault
        let file = dir.appendingPathComponent("desktop.json")
        try s.save(to: file)
        let back = DesktopSettings.load(from: file)
        XCTAssertEqual(back, s)
    }
}

final class BrowserDetectionTests: XCTestCase {
    let home = URL(fileURLWithPath: "/Users/tester", isDirectory: true)

    func testFindsChromeInApplicationsThenUserApplications() {
        let system = URL(fileURLWithPath: "/Applications/Google Chrome.app", isDirectory: true)
        let user = home.appendingPathComponent("Applications/Google Chrome.app", isDirectory: true)
        XCTAssertEqual(BrowserDetection.locateChrome(home: home) { $0 == system }, system)
        XCTAssertEqual(BrowserDetection.locateChrome(home: home) { $0 == user }, user)
        XCTAssertEqual(BrowserDetection.locateChrome(home: home) { _ in true }, system)
        XCTAssertNil(BrowserDetection.locateChrome(home: home) { _ in false })
    }

    func testRecommendsChromeOnlyWhenInstalledAndFallsBack() {
        let chrome = URL(fileURLWithPath: "/Applications/Google Chrome.app")
        XCTAssertEqual(BrowserDetection.recommended(chromeInstalled: true), .chrome)
        XCTAssertEqual(BrowserDetection.recommended(chromeInstalled: false), .systemDefault)
        XCTAssertEqual(BrowserDetection.target(preference: nil, chrome: chrome), .application(chrome))
        XCTAssertEqual(BrowserDetection.target(preference: .chrome, chrome: nil), .systemDefault, "Chrome removed after choosing it")
        XCTAssertEqual(BrowserDetection.target(preference: .systemDefault, chrome: chrome), .systemDefault)
        XCTAssertEqual(BrowserDetection.effective(preference: nil, chromeInstalled: false), .systemDefault)
    }
}

final class CreateRoomRequestTests: XCTestCase {
    let base = URL(string: "http://127.0.0.1:7743")!

    func testRequestShape() throws {
        let req = try HostClient.createRoomRequest(baseURL: base, name: "  The Podcast \n", desktopToken: String(repeating: "t", count: 40))
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.url?.absoluteString, "http://127.0.0.1:7743/api/v1/rooms")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer " + String(repeating: "t", count: 40))
        XCTAssertEqual(req.value(forHTTPHeaderField: "Content-Type"), "application/json")
        let body = try XCTUnwrap(JSONSerialization.jsonObject(with: try XCTUnwrap(req.httpBody)) as? [String: Any])
        XCTAssertEqual(body.keys.sorted(), ["name"], "the daemon rejects unknown fields")
        XCTAssertEqual(body["name"] as? String, "The Podcast")
    }

    func testRejectsEmptyAndOverlongNamesBeforeSending() {
        XCTAssertThrowsError(try HostClient.createRoomRequest(baseURL: base, name: "   ", desktopToken: "x"))
        XCTAssertThrowsError(try HostClient.createRoomRequest(baseURL: base, name: String(repeating: "a", count: 81), desktopToken: "x"))
        XCTAssertNoThrow(try HostClient.createRoomRequest(baseURL: base, name: String(repeating: "é", count: 80), desktopToken: "x"))
    }

    func testDecodesTheDaemonsRoomView() throws {
        let json = #"{"id":"room_0a1b2c3d4e5f","name":"The Podcast","slug":"the-podcast","linkEnabled":true,"guestUrl":"https://mac.tail.ts.net/r/x#k=secret","guestUrlRemote":true,"live":false,"hostInCall":false,"producer":{}}"#
        let link = try JSONDecoder().decode(RoomLink.self, from: Data(json.utf8))
        XCTAssertEqual(link.id, "room_0a1b2c3d4e5f")
        XCTAssertTrue(link.guestUrlRemote)
    }
}

final class ChecklistTests: XCTestCase {
    private func power(_ fixture: String) throws -> PowerStatus {
        let obj = try XCTUnwrap(JSONSerialization.jsonObject(with: try Fixture.data(fixture)) as? [String: Any])
        return PowerStatus.parse(providingType: obj["providingType"] as? String, descriptions: obj["sources"] as? [[String: Any]] ?? [])
    }

    func testPowerSourceFixtures() throws {
        let ac = try power("power-laptop-ac.json")
        XCTAssertEqual(ac, PowerStatus(source: .ac, hasBattery: true, batteryPercent: 87, charging: true))
        XCTAssertEqual(ac.readiness.level, .ok)
        XCTAssertEqual(ac.readiness.detail, "Power adapter connected (87%).")

        let batt = try power("power-laptop-battery.json")
        XCTAssertEqual(batt.source, .battery)
        XCTAssertEqual(batt.batteryPercent, 54)
        XCTAssertEqual(batt.readiness.level, .warning)
        XCTAssertTrue(batt.readiness.detail.contains("Plug in"))

        let desk = try power("power-desktop.json")
        XCTAssertEqual(desk, PowerStatus(source: .ac, hasBattery: false))
        XCTAssertEqual(desk.readiness.detail, "On mains power.")

        let ups = try power("power-ups.json")
        XCTAssertEqual(ups.source, .ups)
        XCTAssertFalse(ups.hasBattery, "a UPS is not the internal battery")
        XCTAssertEqual(ups.readiness.level, .warning)

        let fallback = PowerStatus.parse(providingType: nil, descriptions: [["Type": "InternalBattery", "Power Source State": "Battery Power"]])
        XCTAssertEqual(fallback.source, .battery)
        XCTAssertEqual(PowerStatus.parse(providingType: nil, descriptions: []).readiness.level, .neutral)
    }

    func testDiskEstimateAgainstTenGBPerHour() {
        XCTAssertEqual(DiskEstimate.hours(freeBytes: 25_000_000_000), 2.5, accuracy: 1e-9)
        let plenty = DiskEstimate.evaluate(freeBytes: 250_000_000_000)
        XCTAssertEqual(plenty.level, .ok)
        XCTAssertTrue(plenty.detail.contains("about 25 hours"), plenty.detail)
        XCTAssertEqual(DiskEstimate.evaluate(freeBytes: 40_000_000_000).level, .ok)
        let tight = DiskEstimate.evaluate(freeBytes: 30_000_000_000)
        XCTAssertEqual(tight.level, .warning)
        XCTAssertTrue(tight.detail.contains("3.0 hours"), tight.detail)
        XCTAssertEqual(DiskEstimate.evaluate(freeBytes: 12_000_000_000).level, .problem)
        XCTAssertEqual(DiskEstimate.evaluate(freeBytes: -5).level, .problem)
        XCTAssertEqual(DiskEstimate.evaluate(freeBytes: nil).level, .neutral)
    }

    func testGuestLinkSelfCheckIsAReadOnlyHEADToHealth() throws {
        let req = try XCTUnwrap(GuestLinkCheck.request(publicURL: "https://studio.tail1234.ts.net/?x=1#frag"))
        XCTAssertEqual(req.httpMethod, "HEAD")
        XCTAssertEqual(req.url?.absoluteString, "https://studio.tail1234.ts.net/health")
        XCTAssertNil(req.value(forHTTPHeaderField: "Authorization"), "never sends a credential")
        XCTAssertNil(GuestLinkCheck.request(publicURL: "http://studio.tail1234.ts.net"))
        XCTAssertNil(GuestLinkCheck.request(publicURL: "not a url"))
        XCTAssertEqual(GuestLinkCheck.classify(statusCode: 200, error: nil), .reachable)
        XCTAssertEqual(GuestLinkCheck.classify(statusCode: 502, error: nil), .unexpected(502))
        XCTAssertEqual(GuestLinkCheck.classify(statusCode: nil, error: "timed out"), .unreachable("timed out"))
    }

    private func inputs(_ mutate: (inout ReadinessInputs) -> Void = { _ in }) -> ReadinessInputs {
        var i = ReadinessInputs(daemonHealthy: true, daemonDetail: "Host running", tailscale: .connected(dnsName: "studio.tail1234.ts.net"),
                                guestAccess: .publicFunnel, publicURL: "https://studio.tail1234.ts.net", linkCheck: .reachable,
                                freeBytes: 200_000_000_000, power: PowerStatus(source: .ac, hasBattery: false), browser: .chrome)
        mutate(&i)
        return i
    }

    func testAllGreenChecklist() {
        let items = PodcastChecklist.evaluate(inputs())
        XCTAssertEqual(items.map(\.id), ["host", "tailscale", "guest", "disk", "power", "devices"])
        XCTAssertEqual(items.filter { $0.id != "devices" }.map(\.level), Array(repeating: .ok, count: 5))
        XCTAssertEqual(items.last?.level, .neutral, "browser permissions can't be read; guidance only")
        XCTAssertTrue(items.last!.detail.contains("Chrome"))
        XCTAssertTrue(items[2].detail.contains("checked over the tailnet"))
    }

    func testProblemsAreFlagged() {
        let down = PodcastChecklist.evaluate(inputs { $0.daemonHealthy = false; $0.tailscale = .needsLogin; $0.linkCheck = .unreachable("timed out") })
        XCTAssertEqual(down[0].level, .problem)
        XCTAssertEqual(down[1].level, .problem, "a guest link mode needs Tailscale")
        XCTAssertEqual(down[2].level, .problem)
        let local = PodcastChecklist.evaluate(inputs { $0.guestAccess = .off; $0.tailscale = .notInstalled; $0.linkCheck = nil })
        XCTAssertEqual(local[1].level, .warning)
        XCTAssertEqual(local[2].level, .warning)
        XCTAssertTrue(local[2].detail.contains("This Mac only"))
        let wrong = PodcastChecklist.evaluate(inputs { $0.guestAccess = .privateTailnet; $0.linkCheck = .unexpected(404) })
        XCTAssertEqual(wrong[2].level, .problem)
        XCTAssertFalse(wrong[2].detail.contains("checked over the tailnet"))
        let pending = PodcastChecklist.evaluate(inputs { $0.linkCheck = nil; $0.power = nil })
        XCTAssertEqual(pending[2].level, .neutral)
        XCTAssertEqual(pending[4].level, .neutral)
    }
}

final class UpdateConfigurationTests: XCTestCase {
    let key = Data(repeating: 7, count: 32).base64EncodedString()

    func testDisabledUnlessLinkedWithHTTPSFeedAndRealKey() {
        let good: [String: Any] = ["SUFeedURL": "https://example.org/appcast.xml", "SUPublicEDKey": key]
        XCTAssertEqual(UpdateConfiguration.evaluate(info: good, updaterLinked: true), .enabled(feed: URL(string: "https://example.org/appcast.xml")!))
        XCTAssertFalse(UpdateConfiguration.evaluate(info: good, updaterLinked: false).isEnabled)
        XCTAssertFalse(UpdateConfiguration.evaluate(info: [:], updaterLinked: true).isEnabled)
        XCTAssertFalse(UpdateConfiguration.evaluate(info: ["SUFeedURL": "", "SUPublicEDKey": key], updaterLinked: true).isEnabled)
        XCTAssertFalse(UpdateConfiguration.evaluate(info: ["SUFeedURL": "http://example.org/appcast.xml", "SUPublicEDKey": key], updaterLinked: true).isEnabled)
        XCTAssertFalse(UpdateConfiguration.evaluate(info: ["SUFeedURL": "https://example.org/a.xml", "SUPublicEDKey": "REPLACE_WITH_EDDSA_PUBLIC_KEY"], updaterLinked: true).isEnabled)
        XCTAssertFalse(UpdateConfiguration.evaluate(info: ["SUFeedURL": "https://example.org/a.xml", "SUPublicEDKey": Data(count: 16).base64EncodedString()], updaterLinked: true).isEnabled)
    }
}

final class QuitWarningTests: XCTestCase {
    private func state(recording: Bool = false, live: Bool = false, uploads: Int = 0, admitted: Int = 0, waiting: Int = 0) throws -> HostState {
        let waitingJSON = (0..<waiting).map { #"{"participantId":"p\#($0)","name":"G\#($0)"}"# }.joined(separator: ",")
        let json = """
        {"product":"com.omniharmonic.studiolink","buildId":"B","pid":1,"recording":\(recording),"live":\(live),"uploadsPending":\(uploads),
         "waitingCount":\(waiting),"takes":[],
         "sessions":[{"id":"s1","title":"Ep","connected":\(admitted),"admitted":\(admitted),"hostConnected":true,"waiting":[\(waitingJSON)],
                      "recording":\(recording),"live":\(live),"uploadingTracks":0}]}
        """
        return try HostState.decode(Data(json.utf8))
    }

    func testIdleHasNoWarning() throws {
        XCTAssertNil(QuitWarning.message(nil))
        XCTAssertNil(QuitWarning.message(try state()))
    }

    func testCoversLivestreamUploadsAndCalls() throws {
        XCTAssertEqual(QuitWarning.message(try state(live: true)), "A livestream is running.")
        XCTAssertEqual(QuitWarning.message(try state(uploads: 2)), "2 recording uploads are still pending.")
        XCTAssertEqual(QuitWarning.message(try state(recording: true, live: true, uploads: 1, admitted: 2, waiting: 1)),
                       "A take is recording, a livestream is running, 1 recording upload is still pending, a call is in progress and 1 guest is waiting to be admitted.")
        XCTAssertEqual(QuitWarning.message(try state(admitted: 3)), "A call is in progress.")
    }
}
