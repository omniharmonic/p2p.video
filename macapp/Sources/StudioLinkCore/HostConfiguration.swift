import Foundation

/// Where the app finds its daemon and data, and which loopback ports it uses.
///
/// Defaults match the zsh launcher (`scripts/launch-mac.sh`). Environment
/// overrides exist so tests and development runs never collide with a real
/// host on 7443 or touch `~/Library/Application Support/p2p.video`.
/// `P2PVIDEO_*` variables are accepted as aliases of the `STUDIOLINK_*` ones.
public struct HostConfiguration: Equatable, Sendable {
    public static let defaultPort = 7443
    public static let defaultTURNListen = "127.0.0.1:3478"
    /// The daemon's `/health` product identity. An internal protocol
    /// identifier: it keeps its pre-rename value so the app, the daemon and
    /// older launchers keep recognizing each other across the rename.
    public static let product = "com.omniharmonic.studiolink"
    /// The app's bundle identifier (reverse DNS of p2p.video).
    public static let bundleIdentifier = "video.p2p.app"
    /// The bundle identifier the app shipped with as StudioLink.
    public static let legacyBundleIdentifier = "com.omniharmonic.studiolink"
    /// `~/Library/Application Support/<name>` holds the app's data.
    public static let dataFolderName = "p2p.video"
    /// The pre-rename data folder, migrated once (see `LegacyMigration`).
    public static let legacyDataFolderName = "StudioLink"

    public static func defaultDataDirectory(home: URL) -> URL {
        home.appendingPathComponent("Library/Application Support/\(dataFolderName)", isDirectory: true)
    }

    public static func legacyDataDirectory(home: URL) -> URL {
        home.appendingPathComponent("Library/Application Support/\(legacyDataFolderName)", isDirectory: true)
    }

    public var port: Int
    public var dataDirectory: URL
    /// True when `dataDirectory` is the default folder (no environment
    /// override). Only then may the legacy StudioLink folder be migrated.
    public var usesDefaultDataDirectory: Bool
    /// The path the studio window's WebKit store identity is derived from:
    /// the data directory, or the folder it was migrated from, so OPFS upload
    /// journals written before the rename are still found.
    public var webDataIdentityDirectory: URL
    /// The bundled daemon (`Contents/Resources/bin/p2pvideod`).
    public var daemonExecutable: URL
    /// Directory prepended to the daemon's PATH so it finds bundled ffmpeg.
    public var toolsDirectory: URL
    /// Exact build the bundle ships (`Contents/Resources/build-id.txt`).
    public var expectedBuildID: String
    /// Optional SFU UDP mux port override (`--udp-port`).
    public var udpPort: Int?
    /// Embedded TURN loopback listen address (`--turn-listen`).
    public var turnListen: String
    /// Open the studio in the browser when the app launches (launcher parity).
    public var openBrowserOnLaunch: Bool
    /// When set, run the WKWebView capability probe and write results here.
    public var webViewProbeOutput: URL?
    /// When set, run the end-to-end studio probe [SL-P2-3] and write results here.
    public var studioProbeOutput: URL?

    public var baseURL: URL { URL(string: "http://127.0.0.1:\(port)")! }
    public var daemonLog: URL { dataDirectory.appendingPathComponent("daemon.log") }
    public var desktopTokenFile: URL { dataDirectory.appendingPathComponent(".desktop_token") }
    /// Per-boot dashboard launch token (0600), rewritten at every daemon start.
    public var launchTokenFile: URL { dataDirectory.appendingPathComponent(".launch_token") }
    public var settingsFile: URL { dataDirectory.appendingPathComponent("desktop.json") }
    public var legacyPublicURLFile: URL { dataDirectory.appendingPathComponent("public-url.txt") }

    /// TCP port of the embedded TURN listener, used as the Funnel :8443 target.
    public var turnPort: Int? {
        guard let colon = turnListen.lastIndex(of: ":") else { return nil }
        return Int(turnListen[turnListen.index(after: colon)...])
    }

    public enum ResolveError: Error, Equatable, CustomStringConvertible {
        case invalidPort(String)
        case invalidUDPPort(String)
        case nonLoopbackTURN(String)
        case missingBuildID(URL)

        public var description: String {
            switch self {
            case .invalidPort(let v): return "STUDIOLINK_PORT \(v) is not a valid TCP port."
            case .invalidUDPPort(let v): return "STUDIOLINK_UDP_PORT \(v) is not a valid UDP port."
            case .nonLoopbackTURN(let v): return "STUDIOLINK_TURN_LISTEN \(v) must be a 127.0.0.1 address."
            case .missingBuildID(let url): return "This p2p.video bundle is incomplete: \(url.path) is missing."
            }
        }
    }

    /// Resolves configuration from the bundle and environment.
    /// - Parameters:
    ///   - environment: process environment (overrides).
    ///   - resources: the app bundle's `Contents/Resources`.
    ///   - home: the user's home directory.
    ///   - readFile: file reader (injected for tests).
    public static func resolve(
        environment: [String: String],
        resources: URL,
        home: URL,
        readFile: (URL) -> String? = { try? String(contentsOf: $0, encoding: .utf8) }
    ) throws -> HostConfiguration {
        let environment = EnvironmentAliases.apply(environment)
        var port = defaultPort
        if let raw = environment["STUDIOLINK_PORT"], !raw.isEmpty {
            guard let p = Int(raw), (1...65535).contains(p) else { throw ResolveError.invalidPort(raw) }
            port = p
        }
        var udp: Int?
        if let raw = environment["STUDIOLINK_UDP_PORT"], !raw.isEmpty {
            guard let p = Int(raw), (0...65535).contains(p) else { throw ResolveError.invalidUDPPort(raw) }
            udp = p
        }
        let turn = environment["STUDIOLINK_TURN_LISTEN"].flatMap { $0.isEmpty ? nil : $0 } ?? defaultTURNListen
        guard turn.hasPrefix("127.0.0.1:") || turn.hasPrefix("localhost:") else { throw ResolveError.nonLoopbackTURN(turn) }

        let dataDir: URL
        let usesDefault: Bool
        if let raw = environment["STUDIOLINK_DATA_DIR"], !raw.isEmpty {
            dataDir = URL(fileURLWithPath: raw, isDirectory: true)
            usesDefault = false
        } else {
            dataDir = defaultDataDirectory(home: home)
            usesDefault = true
        }
        let marker = readFile(dataDir.appendingPathComponent(LegacyMigration.markerName)).flatMap(LegacyMigration.Marker.decode)
        let webIdentity = marker.map { URL(fileURLWithPath: $0.migratedFrom, isDirectory: true) } ?? dataDir
        let bin = resources.appendingPathComponent("bin", isDirectory: true)
        let daemon = environment["STUDIOLINK_DAEMON"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0) }
            ?? bin.appendingPathComponent("p2pvideod")
        let buildFile = resources.appendingPathComponent("build-id.txt")
        let build: String
        if let override = environment["STUDIOLINK_BUILD_ID"], !override.isEmpty {
            build = override
        } else if let text = readFile(buildFile)?.trimmingCharacters(in: .whitespacesAndNewlines), !text.isEmpty {
            build = text
        } else {
            throw ResolveError.missingBuildID(buildFile)
        }
        return HostConfiguration(
            port: port,
            dataDirectory: dataDir,
            usesDefaultDataDirectory: usesDefault,
            webDataIdentityDirectory: webIdentity,
            daemonExecutable: daemon,
            toolsDirectory: daemon.deletingLastPathComponent(),
            expectedBuildID: build,
            udpPort: udp,
            turnListen: turn,
            openBrowserOnLaunch: environment["STUDIOLINK_NO_OPEN"] != "1",
            webViewProbeOutput: environment["STUDIOLINK_WEBVIEW_PROBE"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0) },
            studioProbeOutput: environment["STUDIOLINK_STUDIO_PROBE"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0) }
        )
    }
}

/// `P2PVIDEO_<NAME>` is an alias of `STUDIOLINK_<NAME>`; when both are set,
/// the `P2PVIDEO_` value wins. The rest of the app reads `STUDIOLINK_*`.
public enum EnvironmentAliases {
    public static let prefix = "P2PVIDEO_"
    public static let legacyPrefix = "STUDIOLINK_"

    public static func apply(_ environment: [String: String]) -> [String: String] {
        var out = environment
        for (key, value) in environment where key.hasPrefix(prefix) && key.count > prefix.count {
            out[legacyPrefix + key.dropFirst(prefix.count)] = value
        }
        return out
    }
}
