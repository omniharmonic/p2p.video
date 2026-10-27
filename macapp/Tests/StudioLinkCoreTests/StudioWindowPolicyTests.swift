import Foundation
import StudioLinkCore
import XCTest

/// In-app studio window policy [SL-P2-3].
final class StudioWindowPolicyTests: XCTestCase {
    let origin = DaemonOrigin(port: 7643)

    private func state(recording: Bool = false, live: Bool = false, uploads: Int = 0, admitted: Int = 0, waiting: Int = 0) throws -> HostState {
        let waitingJSON = (0..<waiting).map { #"{"participantId":"p_\#($0)","name":"G\#($0)"}"# }.joined(separator: ",")
        let sessions = admitted > 0 || recording || waiting > 0
            ? #"[{"id":"s1","title":"Show","connected":\#(admitted + waiting),"admitted":\#(admitted),"hostConnected":true,"waiting":[\#(waitingJSON)],"recording":\#(recording),"live":\#(live),"uploadingTracks":\#(uploads)}]"#
            : "[]"
        let json = #"{"product":"com.omniharmonic.studiolink","buildId":"B","pid":1,"recording":\#(recording),"live":\#(live),"uploadsPending":\#(uploads),"waitingCount":\#(waiting),"sessions":\#(sessions),"takes":[]}"#
        return try HostState.decode(Data(json.utf8))
    }

    // MARK: origin and media permission

    func testDaemonOriginIsLoopbackHTTPOnThePortOnly() {
        XCTAssertTrue(origin.matches(scheme: "http", host: "127.0.0.1", port: 7643))
        XCTAssertTrue(origin.matches(scheme: "HTTP", host: "localhost", port: 7643))
        XCTAssertFalse(origin.matches(scheme: "https", host: "127.0.0.1", port: 7643))
        XCTAssertFalse(origin.matches(scheme: "http", host: "127.0.0.1", port: 7443))
        XCTAssertFalse(origin.matches(scheme: "http", host: "studio.tail1234.ts.net", port: 7643))
        XCTAssertFalse(origin.matches(scheme: "http", host: "127.0.0.2", port: 7643))
        XCTAssertTrue(origin.contains(URL(string: "http://127.0.0.1:7643/?room=r1")!))
        XCTAssertFalse(origin.contains(URL(string: "http://127.0.0.1/")!))
        XCTAssertFalse(origin.contains(URL(string: "blob:http://127.0.0.1:7643/abc")!))
    }

    func testCameraAndMicrophoneOnlyForTheDaemonOriginAndFrame() {
        let page = URL(string: "http://127.0.0.1:7643/")!
        XCTAssertTrue(MediaCapturePolicy.allows(origin: origin, requestScheme: "http", requestHost: "127.0.0.1", requestPort: 7643, frameURL: page))
        XCTAssertTrue(MediaCapturePolicy.allows(origin: origin, requestScheme: "http", requestHost: "127.0.0.1", requestPort: 7643, frameURL: nil))
        XCTAssertTrue(MediaCapturePolicy.allows(origin: origin, requestScheme: "http", requestHost: "127.0.0.1", requestPort: 7643,
                                                frameURL: URL(string: "about:blank")!))
        // Another origin, another port, or a foreign frame on our origin: denied.
        XCTAssertFalse(MediaCapturePolicy.allows(origin: origin, requestScheme: "https", requestHost: "evil.example", requestPort: 0, frameURL: nil))
        XCTAssertFalse(MediaCapturePolicy.allows(origin: origin, requestScheme: "http", requestHost: "127.0.0.1", requestPort: 7443, frameURL: nil))
        XCTAssertFalse(MediaCapturePolicy.allows(origin: origin, requestScheme: "http", requestHost: "127.0.0.1", requestPort: 7643,
                                                 frameURL: URL(string: "https://evil.example/frame")!))
    }

    // MARK: navigation

    private func decide(_ s: String, main: Bool = true, newWindow: Bool = false, download: Bool = false) -> StudioNavigation {
        StudioNavigationPolicy.decide(URL(string: s)!, origin: origin, isMainFrame: main, opensNewWindow: newWindow, shouldDownload: download)
    }

    func testNavigationTable() {
        XCTAssertEqual(decide("http://127.0.0.1:7643/?session=s&token=t"), .allowInApp)
        XCTAssertEqual(decide("http://127.0.0.1:7643/join?session=s", newWindow: true), .openExternally)
        XCTAssertEqual(decide("https://calendar.google.com/calendar/render?action=TEMPLATE", newWindow: true), .openExternally)
        XCTAssertEqual(decide("https://tailscale.com/download"), .openExternally)
        XCTAssertEqual(decide("https://tailscale.com/download", main: false), .deny)
        XCTAssertEqual(decide("mailto:guest@example.com?subject=Invite", newWindow: true), .openExternally)
        XCTAssertEqual(decide("webcal://127.0.0.1:7643/cal.ics"), .openExternally)
        XCTAssertEqual(decide("about:blank", main: false), .allowInApp)
        XCTAssertEqual(decide("blob:http://127.0.0.1:7643/abc"), .allowInApp)
        XCTAssertEqual(decide("blob:http://127.0.0.1:7643/abc", newWindow: true), .deny)
        XCTAssertEqual(decide("file:///etc/passwd"), .deny)
        XCTAssertEqual(decide("javascript:alert(1)"), .deny)
        XCTAssertEqual(decide("x-custom://thing", newWindow: true), .deny)
    }

    func testDownloadsComeFromTheDaemonOrThePageOnly() {
        XCTAssertEqual(decide("http://127.0.0.1:7643/api/v1/takes/t/originals?token=x", download: true), .download)
        XCTAssertEqual(decide("http://127.0.0.1:7643/api/v1/schedule/m/invite.ics", download: true), .download)
        XCTAssertEqual(decide("blob:http://127.0.0.1:7643/1234", download: true), .download)
        XCTAssertEqual(decide("data:text/vtt,WEBVTT", download: true), .download)
        XCTAssertEqual(decide("https://evil.example/payload.zip", download: true), .deny)
    }

    // MARK: download naming

    func testDownloadNamesAreSafeSingleComponents() {
        XCTAssertEqual(DownloadNaming.sanitize("p2p.video-take-01-originals.zip"), "p2p.video-take-01-originals.zip")
        XCTAssertEqual(DownloadNaming.sanitize("../../.ssh/authorized_keys"), "-..-.ssh-authorized_keys")
        XCTAssertEqual(DownloadNaming.sanitize(".hidden"), "hidden")
        XCTAssertEqual(DownloadNaming.sanitize("a:b/c\u{0}d\n.vtt"), "a-b-cd.vtt")
        XCTAssertEqual(DownloadNaming.sanitize("   "), DownloadNaming.fallbackName)
        XCTAssertEqual(DownloadNaming.sanitize("..."), DownloadNaming.fallbackName)
        let long = String(repeating: "x", count: 400) + ".zip"
        let cut = DownloadNaming.sanitize(long)
        XCTAssertLessThanOrEqual(cut.count, 180)
        XCTAssertTrue(cut.hasSuffix(".zip"))
    }

    func testDownloadDestinationsNeverOverwrite() {
        let dir = URL(fileURLWithPath: "/Users/tester/Downloads", isDirectory: true)
        var taken: Set<String> = []
        let exists: (URL) -> Bool = { taken.contains($0.lastPathComponent) }
        XCTAssertEqual(DownloadNaming.destination(suggested: "invite.ics", in: dir, exists: exists).path, "/Users/tester/Downloads/invite.ics")
        taken = ["invite.ics", "invite (2).ics"]
        XCTAssertEqual(DownloadNaming.destination(suggested: "invite.ics", in: dir, exists: exists).lastPathComponent, "invite (3).ics")
        taken = ["README"]
        XCTAssertEqual(DownloadNaming.destination(suggested: "README", in: dir, exists: exists).lastPathComponent, "README (2)")
        XCTAssertEqual(DownloadNaming.destination(suggested: "../x.zip", in: dir, exists: { _ in false }).deletingLastPathComponent().path, dir.path)
    }

    // MARK: data store and launch token

    func testDataStoreIdentifierIsStablePerDataDirectory() {
        let a = StudioDataStoreIdentity.identifier(dataDirectory: URL(fileURLWithPath: "/Users/t/Library/Application Support/p2p.video", isDirectory: true))
        let a2 = StudioDataStoreIdentity.identifier(dataDirectory: URL(fileURLWithPath: "/Users/t/Library/Application Support/p2p.video/"))
        let b = StudioDataStoreIdentity.identifier(dataDirectory: URL(fileURLWithPath: "/tmp/sl-probe", isDirectory: true))
        XCTAssertEqual(a, a2)
        XCTAssertNotEqual(a, b)
        // RFC 4122 variant, name-based version nibble.
        XCTAssertEqual(a.uuidString[a.uuidString.index(a.uuidString.startIndex, offsetBy: 14)], "5")
        XCTAssertTrue("89AB".contains(a.uuidString[a.uuidString.index(a.uuidString.startIndex, offsetBy: 19)]))
    }

    func testLaunchTokenDetectionAndStripping() {
        let launched = DashboardURLs.launch(URL(string: "http://127.0.0.1:7643/?room=r1")!, token: "secret")
        XCTAssertTrue(LaunchTokenHygiene.containsToken(launched))
        let clean = LaunchTokenHygiene.stripped(launched)
        XCTAssertFalse(LaunchTokenHygiene.containsToken(clean))
        XCTAssertEqual(clean.absoluteString, "http://127.0.0.1:7643/?room=r1")
        XCTAssertEqual(LaunchTokenHygiene.stripped(URL(string: "http://127.0.0.1:7643/?launch=x")!).absoluteString, "http://127.0.0.1:7643/")
        XCTAssertFalse(LaunchTokenHygiene.containsToken(URL(string: "http://127.0.0.1:7643/?session=s&token=t")!))
        XCTAssertFalse(LaunchTokenHygiene.containsToken(nil))
    }

    // MARK: lifecycle decision tables

    func testCloseDecisionTable() throws {
        let closed = StudioLifecycle.WindowState(open: true, inCall: false)
        let inCall = StudioLifecycle.WindowState(open: true, inCall: true)
        XCTAssertEqual(StudioLifecycle.closeDecision(state: nil, window: closed), .close)
        XCTAssertEqual(StudioLifecycle.closeDecision(state: try state(), window: closed), .close)
        guard case .confirm(let call) = StudioLifecycle.closeDecision(state: try state(), window: inCall) else { return XCTFail("call") }
        XCTAssertTrue(call.hasPrefix("You are in a call."))
        guard case .confirm(let take) = StudioLifecycle.closeDecision(state: try state(recording: true, uploads: 1, admitted: 1), window: inCall) else {
            return XCTFail("take")
        }
        XCTAssertTrue(take.hasPrefix("A take is recording. 1 recording upload is still pending."))
        XCTAssertTrue(take.contains("finishes uploading when you open the studio again"))
        // Uploads alone (the take stopped, fragments still sending) also ask.
        guard case .confirm = StudioLifecycle.closeDecision(state: try state(uploads: 2), window: closed) else { return XCTFail("uploads") }
        guard case .confirm = StudioLifecycle.closeDecision(state: try state(live: true, admitted: 1), window: closed) else { return XCTFail("live") }
    }

    func testQuitMessageAddsTheStudioWindowsCall() throws {
        XCTAssertNil(StudioLifecycle.quitMessage(state: try state(), window: .closed))
        XCTAssertEqual(StudioLifecycle.quitMessage(state: nil, window: .init(open: true, inCall: true)), "You are in a call in the studio window.")
        XCTAssertEqual(StudioLifecycle.quitMessage(state: try state(recording: true, admitted: 1), window: .init(open: true, inCall: true)),
                       QuitWarning.message(try state(recording: true, admitted: 1)))
    }

    func testActivityTable() throws {
        let inCall = StudioLifecycle.WindowState(open: true, inCall: true)
        XCTAssertEqual(StudioLifecycle.activity(state: nil, window: .closed), StudioLifecycle.Activity.none)
        XCTAssertEqual(StudioLifecycle.activity(state: try state(), window: .init(open: true, inCall: false)), StudioLifecycle.Activity.none)
        XCTAssertEqual(StudioLifecycle.activity(state: try state(), window: inCall), .call)
        XCTAssertEqual(StudioLifecycle.activity(state: try state(recording: true, admitted: 1), window: inCall), .capture)
        // A take in the browser (window closed) still keeps the Mac awake.
        XCTAssertEqual(StudioLifecycle.activity(state: try state(recording: true, admitted: 1), window: .closed), .capture)
        XCTAssertEqual(StudioLifecycle.activity(state: try state(uploads: 1), window: .closed), .capture)
        XCTAssertEqual(StudioLifecycle.activity(state: try state(live: true, admitted: 1), window: .closed), .capture)
        XCTAssertLessThan(StudioLifecycle.Activity.none, .call)
        XCTAssertLessThan(StudioLifecycle.Activity.call, .capture)
    }

    func testOneWindowOneMeeting() {
        let home = URL(string: "http://127.0.0.1:7643/")!
        let homeLaunched = URL(string: "http://127.0.0.1:7643/?launch=abc")!
        let room = URL(string: "http://127.0.0.1:7643/?room=r1&launch=abc")!
        let open = StudioLifecycle.WindowState(open: true, inCall: false)
        let inCall = StudioLifecycle.WindowState(open: true, inCall: true)
        XCTAssertEqual(StudioLifecycle.openDecision(current: nil, requested: homeLaunched, window: .closed), .load)
        XCTAssertEqual(StudioLifecycle.openDecision(current: home, requested: homeLaunched, window: inCall), .focusExisting)
        XCTAssertEqual(StudioLifecycle.openDecision(current: home, requested: homeLaunched, window: open), .load)
        XCTAssertEqual(StudioLifecycle.openDecision(current: home, requested: room, window: open), .load)
        guard case .confirmReplace = StudioLifecycle.openDecision(current: home, requested: room, window: inCall) else { return XCTFail("replace") }
        XCTAssertEqual(StudioLifecycle.meetingKey(URL(string: "http://127.0.0.1:7643/?token=t&session=s")!),
                       StudioLifecycle.meetingKey(URL(string: "http://127.0.0.1:7643/?session=s&launch=x")!))
        XCTAssertEqual(StudioLifecycle.meetingKey(URL(string: "http://127.0.0.1:7643")!), "/")
    }

    // MARK: page bridge, badge

    func testPageMessagesParseStrictly() {
        XCTAssertEqual(StudioPageMessage.parse(["type": "call", "inCall": true]), .call(inCall: true))
        XCTAssertNil(StudioPageMessage.parse(["type": "call"]))
        XCTAssertEqual(StudioPageMessage.parse(["type": "notify", "title": "Guest waiting", "body": "Ada"]), .notify(title: "Guest waiting", body: "Ada"))
        XCTAssertNil(StudioPageMessage.parse(["type": "notify", "title": ""]))
        let long = StudioPageMessage.parse(["type": "notify", "title": String(repeating: "t", count: 500), "body": String(repeating: "b", count: 900)])
        if case .notify(let t, let b) = long { XCTAssertEqual(t.count, 200); XCTAssertEqual(b.count, 500) } else { XCTFail("long") }
        XCTAssertEqual(StudioPageMessage.parse(["type": "badge", "count": 3]), .badge(count: 3))
        XCTAssertEqual(StudioPageMessage.parse(["type": "badge", "count": -4]), .badge(count: 0))
        XCTAssertEqual(StudioPageMessage.parse(["type": "badge", "count": 100_000]), .badge(count: 999))
        XCTAssertNil(StudioPageMessage.parse(["type": "eval", "code": "x"]))
        XCTAssertNil(StudioPageMessage.parse("call"))
    }

    func testDockBadgeAndPageNotificationPolicy() {
        XCTAssertNil(DockBadge.label(pageBadge: 0, waitingGuests: 0))
        XCTAssertEqual(DockBadge.label(pageBadge: 0, waitingGuests: 2), "2")
        XCTAssertEqual(DockBadge.label(pageBadge: 5, waitingGuests: 2), "5")
        XCTAssertFalse(DockBadge.shouldPostPageNotification(windowIsKey: true, appActive: true))
        XCTAssertTrue(DockBadge.shouldPostPageNotification(windowIsKey: false, appActive: true))
        XCTAssertTrue(DockBadge.shouldPostPageNotification(windowIsKey: true, appActive: false))
    }
}
