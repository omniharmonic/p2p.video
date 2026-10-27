import AppKit
import Foundation
import StudioLinkCore

/// Runs the one-time StudioLink → p2p.video move at launch [SL-REL-1]. The
/// decisions live in `LegacyMigration` (unit-tested); this only gathers the
/// facts (what is on disk, whether a StudioLink host is running) and acts.
@MainActor
enum LegacyMigrator {
    enum Outcome: Equatable {
        case none
        case migrated
        /// A StudioLink host is running; the app must not start. Message for the user.
        case blocked(String)
    }

    static func run(configuration: HostConfiguration, home: URL) -> Outcome {
        let legacy = HostConfiguration.legacyDataDirectory(home: home)
        // Only look for a running host when there is something to move.
        let candidate = configuration.usesDefaultDataDirectory && LegacyMigration.diskState(legacy) == .directory
            && LegacyMigration.diskState(configuration.dataDirectory) == .missing
        let running = candidate ? runningLegacyHost() : nil
        let decision = LegacyMigration.decide(legacy: legacy, target: configuration.dataDirectory,
                                              usesDefaultDataDirectory: configuration.usesDefaultDataDirectory,
                                              runningLegacyHost: running, state: LegacyMigration.diskState)
        switch decision {
        case .notNeeded(let reason):
            appLog.info("Data migration not needed: \(reason, privacy: .public)")
            migrateDefaultsIfNeeded(legacyHostRunning: legacyAppRunning)
            return .none
        case .deferred(let message):
            appLog.notice("Data migration deferred: \(running ?? "StudioLink", privacy: .public) is running")
            return .blocked(message)
        case .migrate(let from, let to):
            do {
                let result = try LegacyMigration.perform(from: from, to: to)
                appLog.notice("Moved \(from.path, privacy: .public) to \(to.path, privacy: .public) (marker written: \(result.markerWritten, privacy: .public))")
            } catch {
                appLog.error("Data migration from \(from.path, privacy: .public) failed: \(String(describing: error), privacy: .public)")
                return .blocked("p2p.video could not move your StudioLink data from \(from.path) to \(to.path): \(error). Nothing was deleted. Move the folder yourself (Finder: rename it to “\(HostConfiguration.dataFolderName)”), then reopen p2p.video.")
            }
            migrateWebKitStore(home: home)
            migrateDefaultsIfNeeded(legacyHostRunning: false)
            return .migrated
        }
    }

    /// The in-app studio window's WebKit data (OPFS upload journals) lives
    /// under `~/Library/WebKit/<bundle id>`; move it along with the data.
    private static func migrateWebKitStore(home: URL) {
        guard Bundle.main.bundleIdentifier == HostConfiguration.bundleIdentifier else { return }
        let base = home.appendingPathComponent("Library/WebKit", isDirectory: true)
        let legacy = base.appendingPathComponent(HostConfiguration.legacyBundleIdentifier, isDirectory: true)
        let target = base.appendingPathComponent(HostConfiguration.bundleIdentifier, isDirectory: true)
        guard LegacyMigration.diskState(legacy) == .directory, LegacyMigration.diskState(target) == .missing else { return }
        let rc = legacy.path.withCString { s in target.path.withCString { d in renamex_np(s, d, UInt32(RENAME_EXCL)) } }
        if rc == 0 {
            appLog.notice("Moved the studio window's web data to \(target.path, privacy: .public)")
        } else {
            appLog.error("Could not move the studio window's web data (errno \(errno, privacy: .public)); left in place")
        }
    }

    /// Copies the old app's preferences once (window frames, updater choices).
    private static func migrateDefaultsIfNeeded(legacyHostRunning: Bool) {
        guard !legacyHostRunning, Bundle.main.bundleIdentifier == HostConfiguration.bundleIdentifier else { return }
        let defaults = UserDefaults.standard
        guard let merged = LegacyMigration.mergedDefaults(legacy: defaults.persistentDomain(forName: HostConfiguration.legacyBundleIdentifier),
                                                          current: defaults.persistentDomain(forName: HostConfiguration.bundleIdentifier)) else { return }
        defaults.setPersistentDomain(merged, forName: HostConfiguration.bundleIdentifier)
        appLog.notice("Copied \(merged.count - 1, privacy: .public) preferences from \(HostConfiguration.legacyBundleIdentifier, privacy: .public)")
    }

    static var legacyAppRunning: Bool {
        !NSRunningApplication.runningApplications(withBundleIdentifier: HostConfiguration.legacyBundleIdentifier).isEmpty
    }

    /// A running StudioLink app or `studiolinkd` daemon, described for the user.
    static func runningLegacyHost() -> String? {
        if legacyAppRunning { return "The StudioLink app" }
        let pgrep = Process()
        pgrep.executableURL = URL(fileURLWithPath: "/usr/bin/pgrep")
        pgrep.arguments = ["-x", "studiolinkd"]
        pgrep.standardOutput = FileHandle.nullDevice
        pgrep.standardError = FileHandle.nullDevice
        do {
            try pgrep.run()
            pgrep.waitUntilExit()
            if pgrep.terminationStatus == 0 { return "A StudioLink host (studiolinkd)" }
        } catch {
            appLog.error("Could not check for a running StudioLink host: \(error.localizedDescription, privacy: .public)")
            return "A StudioLink host (could not check)"
        }
        return nil
    }
}
