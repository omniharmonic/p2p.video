import Darwin
import Foundation

/// One-time move from the StudioLink app to p2p.video [SL-REL-1].
///
/// The rename changed the bundle identifier (`com.omniharmonic.studiolink` →
/// `video.p2p.app`) and the data folder (`Application Support/StudioLink` →
/// `Application Support/p2p.video`). On first launch the app moves the old
/// folder into place so rooms, recordings, settings and tokens carry over.
///
/// Safety rules:
/// - only for the default data folder (an environment override never migrates);
/// - only when the old folder exists and the new one does not;
/// - never while a StudioLink host is running (it owns the old folder);
/// - one `rename(2)` (with `RENAME_EXCL`), so the folder is either moved
///   whole or not at all; a move across volumes is refused, never copied;
/// - a marker in the new folder records what happened.
public enum LegacyMigration {
    /// Written into the new data folder after a move.
    public static let markerName = ".migrated-from-studiolink.json"
    /// Set in the new defaults domain once old preferences were copied.
    public static let defaultsMigratedKey = "P2PVideoMigratedDefaultsFromStudioLink"

    public struct Marker: Codable, Equatable, Sendable {
        /// The old folder's absolute path (also the studio window's WebKit
        /// store identity, so pre-rename OPFS upload journals stay reachable).
        public var migratedFrom: String
        public var migratedTo: String
        /// ISO 8601 time of the move.
        public var migratedAt: String
        public var fromBundleIdentifier: String
        public var toBundleIdentifier: String

        public init(migratedFrom: String, migratedTo: String, migratedAt: String,
                    fromBundleIdentifier: String = HostConfiguration.legacyBundleIdentifier,
                    toBundleIdentifier: String = HostConfiguration.bundleIdentifier) {
            self.migratedFrom = migratedFrom
            self.migratedTo = migratedTo
            self.migratedAt = migratedAt
            self.fromBundleIdentifier = fromBundleIdentifier
            self.toBundleIdentifier = toBundleIdentifier
        }

        public static func decode(_ text: String) -> Marker? {
            guard let m = try? JSONDecoder().decode(Marker.self, from: Data(text.utf8)), !m.migratedFrom.isEmpty else { return nil }
            return m
        }
    }

    public enum Decision: Equatable, Sendable {
        /// Move `from` to `to` now.
        case migrate(from: URL, to: URL)
        /// Nothing to do; the reason is for the log.
        case notNeeded(String)
        /// A StudioLink host is running: do not start, show this message.
        case deferred(String)
    }

    /// What exists at a path (`lstat` semantics are not needed: a symlinked
    /// folder is moved as the link, which still points at the same data).
    public enum PathState: Equatable, Sendable {
        case missing
        case directory
        case file
    }

    /// Decides whether to migrate. Pure: every input is passed in.
    /// - Parameters:
    ///   - legacy: the old StudioLink data folder.
    ///   - target: the p2p.video data folder.
    ///   - usesDefaultDataDirectory: false when an environment override chose the folder.
    ///   - runningLegacyHost: a description of a running StudioLink app or daemon, if any.
    ///   - state: what exists at a path.
    public static func decide(legacy: URL, target: URL, usesDefaultDataDirectory: Bool,
                              runningLegacyHost: String?, state: (URL) -> PathState) -> Decision {
        guard usesDefaultDataDirectory else { return .notNeeded("a custom data folder is configured") }
        let legacyState = state(legacy)
        switch (legacyState, state(target)) {
        case (.missing, _):
            return .notNeeded("no StudioLink data folder")
        case (_, .directory), (_, .file):
            return .notNeeded(legacyState == .directory
                ? "both folders exist; using \(target.path) and leaving \(legacy.path) untouched"
                : "already set up")
        case (.file, .missing):
            return .notNeeded("\(legacy.path) is not a folder")
        case (.directory, .missing):
            if let running = runningLegacyHost {
                return .deferred("\(running) is still running and using your StudioLink data. Finish any meeting or upload there, quit StudioLink, then reopen p2p.video: it will move your rooms, recordings and settings over. Nothing was moved or stopped.")
            }
            return .migrate(from: legacy, to: target)
        }
    }

    public enum MoveError: Error, Equatable, CustomStringConvertible {
        case targetExists
        case differentVolume
        case failed(Int32)

        public var description: String {
            switch self {
            case .targetExists: return "the p2p.video folder appeared while moving; nothing was moved"
            case .differentVolume: return "the folders are on different volumes; nothing was moved (copy it yourself if needed)"
            case .failed(let e): return "rename failed: \(String(cString: strerror(e)))"
            }
        }
    }

    /// Moves the folder with one atomic `renamex_np(RENAME_EXCL)` and writes
    /// the marker. Never copies. Returns the marker (and whether it was
    /// written: a marker failure does not undo a completed move).
    @discardableResult
    public static func perform(from legacy: URL, to target: URL, now: Date = Date()) throws -> (marker: Marker, markerWritten: Bool) {
        try FileManager.default.createDirectory(at: target.deletingLastPathComponent(), withIntermediateDirectories: true)
        let rc = legacy.path.withCString { src in target.path.withCString { dst in renamex_np(src, dst, UInt32(RENAME_EXCL)) } }
        if rc != 0 {
            let e = errno
            switch e {
            case EEXIST, ENOTEMPTY: throw MoveError.targetExists
            case EXDEV: throw MoveError.differentVolume
            default: throw MoveError.failed(e)
            }
        }
        let formatter = ISO8601DateFormatter()
        let marker = Marker(migratedFrom: legacy.standardizedFileURL.path, migratedTo: target.standardizedFileURL.path,
                            migratedAt: formatter.string(from: now))
        var written = false
        if let data = try? JSONEncoder().encode(marker) {
            let file = target.appendingPathComponent(markerName)
            written = (try? data.write(to: file, options: [.atomic])) != nil
            if written { try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path) }
        }
        return (marker, written)
    }

    /// The new defaults domain after copying the old one, or nil when there is
    /// nothing to copy or it was already done. Values already set in the new
    /// domain win.
    public static func mergedDefaults(legacy: [String: Any]?, current: [String: Any]?) -> [String: Any]? {
        let current = current ?? [:]
        guard current[defaultsMigratedKey] == nil, let legacy, !legacy.isEmpty else { return nil }
        var merged = legacy
        for (k, v) in current { merged[k] = v }
        merged[defaultsMigratedKey] = true
        return merged
    }

    /// The state of a path on disk (follows symlinks).
    public static func diskState(_ url: URL) -> PathState {
        var isDir: ObjCBool = false
        guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir) else { return .missing }
        return isDir.boolValue ? .directory : .file
    }
}
