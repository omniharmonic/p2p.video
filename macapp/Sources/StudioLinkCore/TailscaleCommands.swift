import Foundation

/// One Tailscale CLI invocation: an argv array (never a shell string) plus a
/// display form shown to the user before anything that changes configuration.
public struct TailscaleCommand: Equatable, Sendable {
    public var arguments: [String]
    /// True for commands that change Serve/Funnel; these require an explicit click.
    public var mutating: Bool

    public init(_ arguments: [String], mutating: Bool) {
        self.arguments = arguments
        self.mutating = mutating
    }

    /// `tailscale serve --bg …` as the user would type it. Display only.
    public var display: String {
        (["tailscale"] + arguments.map(Self.quoteForDisplay)).joined(separator: " ")
    }

    static func quoteForDisplay(_ arg: String) -> String {
        let safe = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-_=:/.,@%+"))
        if !arg.isEmpty, arg.unicodeScalars.allSatisfy({ safe.contains($0) }) { return arg }
        return "'" + arg.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

/// Builders for every Tailscale command the app runs. Read-only commands may
/// run automatically; mutating plans run only after the user confirms the exact
/// commands. Golden tests pin each argv.
public enum TailscaleCommands {
    public static let funnelTURNPort = 8443

    // MARK: read-only
    public static let status = TailscaleCommand(["status", "--json"], mutating: false)
    public static let serveStatus = TailscaleCommand(["serve", "status", "--json"], mutating: false)
    public static let funnelStatus = TailscaleCommand(["funnel", "status", "--json"], mutating: false)

    // MARK: mutating plans

    /// (a) Private link for tailnet guests: HTTPS Serve on :443 → studio.
    public static func enablePrivate(port: Int) -> [TailscaleCommand] {
        [TailscaleCommand(["serve", "--bg", "--https=443", "http://127.0.0.1:\(port)"], mutating: true)]
    }

    /// (b) Public guest link: Funnel :443 → studio, and TLS-terminated TCP on
    /// :8443 → embedded TURN, also through Funnel (PUBLIC_GUESTS.md).
    public static func enablePublic(port: Int, turnPort: Int) -> [TailscaleCommand] {
        [
            TailscaleCommand(["funnel", "--bg", "--https=443", "http://127.0.0.1:\(port)"], mutating: true),
            TailscaleCommand(["serve", "--bg", "--tls-terminated-tcp=\(funnelTURNPort)", "tcp://127.0.0.1:\(turnPort)"], mutating: true),
            TailscaleCommand(["funnel", "--bg", "--tls-terminated-tcp=\(funnelTURNPort)", "tcp://127.0.0.1:\(turnPort)"], mutating: true),
        ]
    }

    /// Turn off the public link, keeping the private tailnet link: re-serving
    /// :443 with `serve` clears its Funnel flag, and the :8443 TURN forwarder is
    /// removed. The result is re-read and verified afterwards.
    public static func disablePublic(port: Int) -> [TailscaleCommand] {
        [
            TailscaleCommand(["serve", "--bg", "--https=443", "http://127.0.0.1:\(port)"], mutating: true),
            TailscaleCommand(["serve", "--tls-terminated-tcp=\(funnelTURNPort)", "off"], mutating: true),
        ]
    }

    /// Remove the private :443 route as well (guest access off).
    public static func disablePrivate() -> [TailscaleCommand] {
        [TailscaleCommand(["serve", "--https=443", "off"], mutating: true)]
    }

    /// Environment for the CLI: the Mac app binary must run in CLI mode.
    public static func environment(base: [String: String]) -> [String: String] {
        var env = base
        env["TAILSCALE_BE_CLI"] = "1"
        return env
    }
}

/// Finds the Tailscale CLI: PATH first, then the Mac app's bundled binary.
public enum TailscaleCLILocator {
    public static func candidates(path: String?, home: URL) -> [URL] {
        var urls: [URL] = []
        for dir in (path ?? "").split(separator: ":") where !dir.isEmpty {
            urls.append(URL(fileURLWithPath: String(dir)).appendingPathComponent("tailscale"))
        }
        // Finder-launched apps get a minimal PATH; include the usual installs.
        for dir in ["/usr/local/bin", "/opt/homebrew/bin"] {
            let u = URL(fileURLWithPath: dir).appendingPathComponent("tailscale")
            if !urls.contains(u) { urls.append(u) }
        }
        urls.append(URL(fileURLWithPath: "/Applications/Tailscale.app/Contents/MacOS/Tailscale"))
        urls.append(home.appendingPathComponent("Applications/Tailscale.app/Contents/MacOS/Tailscale"))
        return urls
    }

    public static func locate(path: String?, home: URL, isExecutable: (URL) -> Bool) -> URL? {
        candidates(path: path, home: home).first(where: isExecutable)
    }
}
