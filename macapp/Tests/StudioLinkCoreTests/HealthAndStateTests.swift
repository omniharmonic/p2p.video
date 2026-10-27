import Foundation
import StudioLinkCore
import XCTest

final class HealthCheckTests: XCTestCase {
    private func body(_ json: String) -> Data { Data(json.utf8) }

    func testExactBuildIsReused() {
        let v = HealthCheck.classify(statusCode: 200, body: body(#"{"status":"ok","product":"com.omniharmonic.studiolink","buildId":"B1"}"#), expectedBuild: "B1")
        XCTAssertEqual(v, .expected)
        XCTAssertNil(v.message(port: 7443))
    }

    func testNothingListeningIsUnreachable() {
        XCTAssertEqual(HealthCheck.classify(statusCode: nil, body: nil, expectedBuild: "B1"), .unreachable)
    }

    // The launcher's three refusal cases (scripts/launch-mac.test.mjs).
    func testMismatchesAreNeverTreatedAsFree() {
        XCTAssertEqual(HealthCheck.classify(statusCode: 200, body: body(#"{"status":"ok"}"#), expectedBuild: "B1"), .foreign)
        XCTAssertEqual(HealthCheck.classify(statusCode: 200, body: body(#"{"status":"ok","product":"unrelated","buildId":"B1"}"#), expectedBuild: "B1"), .foreign)
        let older = HealthCheck.classify(statusCode: 200, body: body(#"{"status":"ok","product":"com.omniharmonic.studiolink","buildId":"older-build"}"#), expectedBuild: "B1")
        XCTAssertEqual(older, .otherBuild("older-build"))
        XCTAssertTrue(older.message(port: 7443)!.contains("Finish any active session"))
        XCTAssertTrue(older.message(port: 7443)!.contains("No running service was stopped"))
    }

    func testAnyOtherHTTPAnswerIsForeign() {
        XCTAssertEqual(HealthCheck.classify(statusCode: 404, body: body("not found"), expectedBuild: "B1"), .foreign)
        XCTAssertEqual(HealthCheck.classify(statusCode: 200, body: body("<html>"), expectedBuild: "B1"), .foreign)
        XCTAssertEqual(HealthCheck.classify(statusCode: 200, body: body(#"{"status":"degraded","product":"com.omniharmonic.studiolink","buildId":"B1"}"#), expectedBuild: "B1"), .foreign)
        XCTAssertTrue(HealthVerdict.foreign.message(port: 7643)!.contains("7643"))
    }
}

final class HostStateTests: XCTestCase {
    func testDecodesDaemonFixture() throws {
        let s = try HostState.decode(Fixture.data("desktop-state.json"))
        XCTAssertEqual(s.pid, 4242)
        XCTAssertTrue(s.recording)
        XCTAssertEqual(s.waitingCount, 1)
        XCTAssertEqual(s.sessions.first?.title, "Episode 12")
        XCTAssertEqual(s.sessions.first?.waiting.first?.name, "Ada")
        XCTAssertEqual(s.activeCalls.count, 1)
        XCTAssertEqual(s.takes.count, 2)
        XCTAssertEqual(s.activity, DaemonActivity(recording: true, live: false, uploadsPending: 2))
    }

    func testDecodesRoomsAndToleratesOlderDaemons() throws {
        let s = try HostState.decode(Fixture.data("desktop-state.json"))
        XCTAssertEqual(s.roomList, [
            .init(id: "room_0a1b2c3d4e5f", name: "The Podcast", live: true),
            .init(id: "room_5f4e3d2c1b0a", name: "Weekly sync", live: false),
        ])
        // A daemon from before rooms omits the field.
        let old = try HostState.decode(Data(#"{"product":"p","buildId":"b","pid":1,"recording":false,"live":false,"uploadsPending":0,"waitingCount":0,"sessions":[],"takes":[]}"#.utf8))
        XCTAssertEqual(old.roomList, [])
    }

    func testDashboardURLs() {
        let base = URL(string: "http://127.0.0.1:7443")!
        XCTAssertEqual(DashboardURLs.home(base).absoluteString, "http://127.0.0.1:7443")
        XCTAssertEqual(DashboardURLs.room(base, id: "room_0a1b").absoluteString, "http://127.0.0.1:7443/?room=room_0a1b")
        XCTAssertEqual(DashboardURLs.room(base, id: "a&b=c").absoluteString, "http://127.0.0.1:7443/?room=a%26b%3Dc")
    }

    /// SL-SEC-1: pages the app opens carry the per-boot launch token.
    func testDashboardLaunchURLs() {
        let base = URL(string: "http://127.0.0.1:7443")!
        XCTAssertEqual(DashboardURLs.launch(DashboardURLs.home(base), token: "t0k").absoluteString, "http://127.0.0.1:7443/?launch=t0k")
        XCTAssertEqual(DashboardURLs.launch(DashboardURLs.room(base, id: "room_1"), token: "t0k").absoluteString, "http://127.0.0.1:7443/?room=room_1&launch=t0k")
        // A stale token in the URL is replaced, never duplicated.
        let stale = URL(string: "http://127.0.0.1:7443/?launch=old&room=r")!
        XCTAssertEqual(DashboardURLs.launch(stale, token: "new").absoluteString, "http://127.0.0.1:7443/?room=r&launch=new")
        // No token (daemon not started yet): unchanged.
        XCTAssertEqual(DashboardURLs.launch(base, token: nil), base)
        XCTAssertEqual(DashboardURLs.launch(base, token: ""), base)
    }

    func testReadLaunchToken() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let file = dir.appendingPathComponent(".launch_token")
        XCTAssertNil(HostClient.readLaunchToken(file))
        try "short".write(to: file, atomically: true, encoding: .utf8)
        XCTAssertNil(HostClient.readLaunchToken(file))
        let token = String(repeating: "ab", count: 32)
        try (token + "\n").write(to: file, atomically: true, encoding: .utf8)
        XCTAssertEqual(HostClient.readLaunchToken(file), token)
    }

    func testRoomLinkDecodes() throws {
        let link = try JSONDecoder().decode(RoomLink.self, from: Data(#"{"id":"room_1","name":"The Podcast","slug":"the-podcast","guestUrl":"https://h.ts.net/join?room=the-podcast&token=x","guestUrlRemote":true,"live":false}"#.utf8))
        XCTAssertEqual(link.guestUrl, "https://h.ts.net/join?room=the-podcast&token=x")
        XCTAssertTrue(link.guestUrlRemote)
    }

    func testInterruptionWarning() {
        XCTAssertNil(DaemonActivity.idle.interruptionWarning)
        XCTAssertEqual(DaemonActivity(recording: true).interruptionWarning, "A take is recording.")
        XCTAssertEqual(DaemonActivity(uploadsPending: 1).interruptionWarning, "1 recording upload is still pending.")
        XCTAssertEqual(DaemonActivity(recording: true, live: true, uploadsPending: 3).interruptionWarning,
                       "A take is recording, a livestream is running and 3 recording uploads are still pending.")
    }
}

final class NotificationPlannerTests: XCTestCase {
    func testFirstObservationOnlySeeds() throws {
        var p = NotificationPlanner()
        let s = try HostState.decode(Fixture.data("desktop-state.json"))
        XCTAssertEqual(p.plan(s, guests: true, takes: true), [])
        XCTAssertEqual(p.plan(s, guests: true, takes: true), [], "repeated polls never re-post")
    }

    func testNewGuestUploadAndExportEvents() throws {
        var p = NotificationPlanner()
        var s = try HostState.decode(Fixture.data("desktop-state.json"))
        _ = p.plan(s, guests: true, takes: true)
        s.sessions[0].waiting.append(.init(participantId: "p_guest-3", name: "Grace"))
        s.takes[0].uploadsComplete = true
        s.takes[1].exported = true
        let events = p.plan(s, guests: true, takes: true)
        XCTAssertEqual(events, [
            .guestWaiting(name: "Grace", sessionTitle: "Episode 12", participantId: "p_guest-3"),
            .takeUploaded(number: 2, sessionId: "a1b2c3d4", takeId: "take_a1b2c3d4_02"),
            .takeExported(number: 1, sessionId: "a1b2c3d4", takeId: "take_a1b2c3d4_01"),
        ])
        XCTAssertEqual(events[0].title, "Grace is waiting to join")
    }

    func testPreferencesSuppressEvents() throws {
        var p = NotificationPlanner()
        var s = try HostState.decode(Fixture.data("desktop-state.json"))
        _ = p.plan(s, guests: false, takes: false)
        s.sessions[0].waiting.append(.init(participantId: "p_x", name: ""))
        s.takes[1].exported = true
        XCTAssertEqual(p.plan(s, guests: false, takes: false), [])
        // A guest who leaves and comes back is announced again.
        s.sessions[0].waiting.removeAll()
        _ = p.plan(s, guests: true, takes: true)
        s.sessions[0].waiting.append(.init(participantId: "p_x", name: ""))
        XCTAssertEqual(p.plan(s, guests: true, takes: true).map(\.title), ["A guest is waiting to join"])
    }
}

final class LogRotationTests: XCTestCase {
    func testRotatesBySizeAndKeepsBoundedHistory() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let log = dir.appendingPathComponent("daemon.log")
        try Data(count: 10).write(to: log)
        XCTAssertFalse(try LogRotation.rotateIfNeeded(log, maxBytes: 100, keep: 2))
        for i in 1...4 {
            try Data(repeating: UInt8(i), count: 200).write(to: log)
            XCTAssertTrue(try LogRotation.rotateIfNeeded(log, maxBytes: 100, keep: 2))
        }
        let names = try FileManager.default.contentsOfDirectory(atPath: dir.path).sorted()
        XCTAssertEqual(names, ["daemon.log.1", "daemon.log.2"])
        XCTAssertEqual(try Data(contentsOf: URL(fileURLWithPath: log.path + ".1")).first, 4)
        XCTAssertEqual(try Data(contentsOf: URL(fileURLWithPath: log.path + ".2")).first, 3)
        let handle = try LogRotation.openForAppend(log)
        try handle.write(contentsOf: Data("x".utf8))
        try handle.close()
        let perms = try FileManager.default.attributesOfItem(atPath: log.path)[.posixPermissions] as? Int
        XCTAssertEqual(perms, 0o600)
    }
}
