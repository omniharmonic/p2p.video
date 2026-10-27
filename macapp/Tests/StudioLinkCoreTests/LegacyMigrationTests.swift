import Foundation
import StudioLinkCore
import XCTest

/// StudioLink → p2p.video migration [SL-REL-1]. Everything here runs in a
/// temporary directory: the real `~/Library/Application Support` is never touched.
final class LegacyMigrationTests: XCTestCase {
    private var root: URL!
    private var legacy: URL { root.appendingPathComponent("Library/Application Support/StudioLink", isDirectory: true) }
    private var target: URL { root.appendingPathComponent("Library/Application Support/p2p.video", isDirectory: true) }

    override func setUpWithError() throws {
        root = FileManager.default.temporaryDirectory.appendingPathComponent("p2pvideo-migration-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: root.appendingPathComponent("Library/Application Support"), withIntermediateDirectories: true)
    }

    override func tearDownWithError() throws {
        try? FileManager.default.removeItem(at: root)
    }

    private func decide(default usesDefault: Bool = true, running: String? = nil) -> LegacyMigration.Decision {
        LegacyMigration.decide(legacy: legacy, target: target, usesDefaultDataDirectory: usesDefault,
                               runningLegacyHost: running, state: LegacyMigration.diskState)
    }

    private func makeLegacy() throws {
        try FileManager.default.createDirectory(at: legacy.appendingPathComponent("sessions/s1"), withIntermediateDirectories: true)
        try Data("db".utf8).write(to: legacy.appendingPathComponent("studiolink.db"))
        try Data(#"{"guestAccess":"privateTailnet"}"#.utf8).write(to: legacy.appendingPathComponent("desktop.json"))
        try Data("chunk".utf8).write(to: legacy.appendingPathComponent("sessions/s1/take.webm"))
    }

    // MARK: decisions

    func testMigratesOnlyWhenOldExistsNewMissingAndNothingRuns() throws {
        XCTAssertEqual(decide(), .notNeeded("no StudioLink data folder"))
        try makeLegacy()
        XCTAssertEqual(decide(), .migrate(from: legacy, to: target))
    }

    func testCustomDataFolderNeverMigrates() throws {
        try makeLegacy()
        guard case .notNeeded = decide(default: false) else { return XCTFail("an environment override must not migrate") }
    }

    func testRunningStudioLinkDefersAndNamesIt() throws {
        try makeLegacy()
        guard case .deferred(let message) = decide(running: "The StudioLink app") else { return XCTFail("must defer") }
        XCTAssertTrue(message.contains("The StudioLink app is still running"), message)
        XCTAssertTrue(message.contains("quit StudioLink"), message)
        XCTAssertTrue(message.contains("Nothing was moved"), message)
    }

    func testBothFoldersExistLeavesBothUntouched() throws {
        try makeLegacy()
        try FileManager.default.createDirectory(at: target, withIntermediateDirectories: true)
        guard case .notNeeded(let reason) = decide(running: "x") else { return XCTFail("must not migrate over an existing folder") }
        XCTAssertTrue(reason.contains("both folders exist"), reason)
    }

    func testOldPathThatIsAFileIsIgnored() throws {
        try Data("x".utf8).write(to: legacy)
        guard case .notNeeded = decide() else { return XCTFail("a file is not a data folder") }
    }

    // MARK: the move

    func testMoveIsAtomicRenameAndWritesMarker() throws {
        try makeLegacy()
        let inode = try FileManager.default.attributesOfItem(atPath: legacy.appendingPathComponent("sessions/s1/take.webm").path)[.systemFileNumber] as? Int
        let when = Date(timeIntervalSince1970: 1_800_000_000)
        let result = try LegacyMigration.perform(from: legacy, to: target, now: when)

        XCTAssertFalse(FileManager.default.fileExists(atPath: legacy.path), "the old folder is moved, not copied")
        XCTAssertEqual(try String(contentsOf: target.appendingPathComponent("sessions/s1/take.webm"), encoding: .utf8), "chunk")
        XCTAssertEqual(try String(contentsOf: target.appendingPathComponent("studiolink.db"), encoding: .utf8), "db")
        let movedInode = try FileManager.default.attributesOfItem(atPath: target.appendingPathComponent("sessions/s1/take.webm").path)[.systemFileNumber] as? Int
        XCTAssertEqual(inode, movedInode, "rename(2) keeps the same files")

        XCTAssertTrue(result.markerWritten)
        let text = try String(contentsOf: target.appendingPathComponent(LegacyMigration.markerName), encoding: .utf8)
        let marker = try XCTUnwrap(LegacyMigration.Marker.decode(text))
        XCTAssertEqual(marker, result.marker)
        XCTAssertEqual(marker.migratedFrom, legacy.standardizedFileURL.path)
        XCTAssertEqual(marker.migratedTo, target.standardizedFileURL.path)
        XCTAssertEqual(marker.migratedAt, "2027-01-15T08:00:00Z")
        XCTAssertEqual(marker.fromBundleIdentifier, "com.omniharmonic.studiolink")
        XCTAssertEqual(marker.toBundleIdentifier, "video.p2p.app")
        let perms = try FileManager.default.attributesOfItem(atPath: target.appendingPathComponent(LegacyMigration.markerName).path)[.posixPermissions] as? Int
        XCTAssertEqual(perms, 0o600)

        // A second launch sees the new folder and does nothing.
        XCTAssertEqual(decide(), .notNeeded("no StudioLink data folder"))
    }

    func testMoveRefusesATargetThatAppearedAndKeepsTheOldFolder() throws {
        try makeLegacy()
        try FileManager.default.createDirectory(at: target, withIntermediateDirectories: true)
        try Data("new".utf8).write(to: target.appendingPathComponent("desktop.json"))
        XCTAssertThrowsError(try LegacyMigration.perform(from: legacy, to: target)) { error in
            XCTAssertEqual(error as? LegacyMigration.MoveError, .targetExists)
        }
        XCTAssertTrue(FileManager.default.fileExists(atPath: legacy.appendingPathComponent("sessions/s1/take.webm").path))
        XCTAssertEqual(try String(contentsOf: target.appendingPathComponent("desktop.json"), encoding: .utf8), "new")
    }

    func testMoveRefusesAnEmptyTargetFolderToo() throws {
        try makeLegacy()
        try FileManager.default.createDirectory(at: target, withIntermediateDirectories: true)
        XCTAssertThrowsError(try LegacyMigration.perform(from: legacy, to: target))
        XCTAssertTrue(FileManager.default.fileExists(atPath: legacy.appendingPathComponent("studiolink.db").path))
    }

    // MARK: configuration after a move

    func testConfigurationKeepsTheWebDataIdentityOfTheOldFolder() throws {
        try makeLegacy()
        try LegacyMigration.perform(from: legacy, to: target)
        let home = root!
        let resources = root.appendingPathComponent("Resources", isDirectory: true)
        try FileManager.default.createDirectory(at: resources, withIntermediateDirectories: true)
        try Data("B\n".utf8).write(to: resources.appendingPathComponent("build-id.txt"))

        let c = try HostConfiguration.resolve(environment: [:], resources: resources, home: home)
        XCTAssertTrue(c.usesDefaultDataDirectory)
        XCTAssertEqual(c.dataDirectory.lastPathComponent, "p2p.video")
        XCTAssertEqual(c.webDataIdentityDirectory.path, legacy.standardizedFileURL.path)
        XCTAssertEqual(StudioDataStoreIdentity.identifier(dataDirectory: c.webDataIdentityDirectory),
                       StudioDataStoreIdentity.identifier(dataDirectory: legacy),
                       "the studio window keeps finding pre-rename upload journals")
    }

    func testConfigurationWithoutMarkerUsesTheDataFolder() throws {
        let c = try configuration()
        XCTAssertTrue(c.usesDefaultDataDirectory)
        XCTAssertEqual(c.webDataIdentityDirectory, c.dataDirectory)
        XCTAssertEqual(HostConfiguration.legacyDataDirectory(home: testHome).path, "/Users/tester/Library/Application Support/StudioLink")
        XCTAssertFalse(try configuration(["STUDIOLINK_DATA_DIR": "/tmp/iso"]).usesDefaultDataDirectory)
    }

    func testP2PVideoEnvironmentAliases() throws {
        let c = try configuration(["P2PVIDEO_DATA_DIR": "/tmp/p2p", "P2PVIDEO_PORT": "7700", "STUDIOLINK_PORT": "7600"])
        XCTAssertEqual(c.dataDirectory.path, "/tmp/p2p")
        XCTAssertFalse(c.usesDefaultDataDirectory)
        XCTAssertEqual(c.port, 7700, "the P2PVIDEO_ value wins")
        XCTAssertEqual(try configuration(["STUDIOLINK_PORT": "7600"]).port, 7600)
    }

    // MARK: preferences

    func testDefaultsAreCopiedOnceAndNewValuesWin() throws {
        XCTAssertNil(LegacyMigration.mergedDefaults(legacy: nil, current: nil))
        XCTAssertNil(LegacyMigration.mergedDefaults(legacy: [:], current: ["a": 1]))
        let merged = try XCTUnwrap(LegacyMigration.mergedDefaults(legacy: ["SUEnableAutomaticChecks": true, "frame": "old"],
                                                                  current: ["frame": "new"]))
        XCTAssertEqual(merged as NSDictionary, ["SUEnableAutomaticChecks": true, "frame": "new",
                                                LegacyMigration.defaultsMigratedKey: true] as NSDictionary)
        XCTAssertNil(LegacyMigration.mergedDefaults(legacy: ["frame": "old"], current: merged), "copied only once")
    }

    // MARK: product facts (About / Settings)

    func testProductInfoOffersTheSource() {
        XCTAssertEqual(ProductInfo.name, "p2p.video")
        XCTAssertEqual(ProductInfo.sourceURL.absoluteString, "https://github.com/omniharmonic/p2p.video")
        XCTAssertEqual(ProductInfo.versionLine(shortVersion: "1.0.0", bundleVersion: "5", buildID: "B1"), "1.0.0 (5), build B1")
        XCTAssertEqual(ProductInfo.versionLine(shortVersion: nil, bundleVersion: nil, buildID: nil), "development")
        XCTAssertEqual(HostConfiguration.bundleIdentifier, "video.p2p.app")
    }

    // MARK: an old StudioLink still on the port

    func testOldStudioLinkOnThePortIsNamed() throws {
        let body = Data(#"{"status":"ok","product":"com.omniharmonic.studiolink","buildId":"studiolink-1.0.0-preview"}"#.utf8)
        let verdict = HealthCheck.classify(statusCode: 200, body: body, expectedBuild: "B1")
        XCTAssertEqual(verdict, .otherBuild("studiolink-1.0.0-preview"))
        let message = try XCTUnwrap(verdict.message(port: 7443))
        XCTAssertTrue(message.contains("StudioLink"), message)
        XCTAssertTrue(message.contains("7443"), message)
        XCTAssertTrue(message.contains("No running service was stopped"), message)
    }
}
