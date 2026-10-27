import Foundation

/// Size-based rotation for `daemon.log`, applied before each daemon start.
/// The daemon writes to an O_APPEND file handle it owns, so it keeps running
/// (and logging) even if the app quits unexpectedly.
public enum LogRotation {
    public static let defaultMaxBytes: UInt64 = 10 * 1024 * 1024
    public static let defaultKeep = 3

    /// Rotates `log` → `log.1` → … → `log.<keep>` when `log` exceeds maxBytes.
    /// Returns true when a rotation happened.
    @discardableResult
    public static func rotateIfNeeded(_ log: URL, maxBytes: UInt64 = defaultMaxBytes, keep: Int = defaultKeep,
                                      fileManager fm: FileManager = .default) throws -> Bool {
        guard let attrs = try? fm.attributesOfItem(atPath: log.path),
              let size = attrs[.size] as? UInt64, size > maxBytes else { return false }
        func rotated(_ n: Int) -> URL { URL(fileURLWithPath: log.path + ".\(n)") }
        if fm.fileExists(atPath: rotated(keep).path) { try fm.removeItem(at: rotated(keep)) }
        if keep > 1 {
            for n in stride(from: keep - 1, through: 1, by: -1) where fm.fileExists(atPath: rotated(n).path) {
                try fm.moveItem(at: rotated(n), to: rotated(n + 1))
            }
        }
        try fm.moveItem(at: log, to: rotated(1))
        return true
    }

    /// Opens (creating 0600 if needed) the log for appending.
    public static func openForAppend(_ log: URL, fileManager fm: FileManager = .default) throws -> FileHandle {
        try fm.createDirectory(at: log.deletingLastPathComponent(), withIntermediateDirectories: true)
        if !fm.fileExists(atPath: log.path) {
            guard fm.createFile(atPath: log.path, contents: nil, attributes: [.posixPermissions: 0o600]) else {
                throw CocoaError(.fileWriteUnknown, userInfo: [NSFilePathErrorKey: log.path])
            }
        }
        let handle = try FileHandle(forWritingTo: log)
        try handle.seekToEnd()
        return handle
    }
}
