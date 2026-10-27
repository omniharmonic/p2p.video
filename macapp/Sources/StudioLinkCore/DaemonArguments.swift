import Foundation

/// Builds the `p2pvideod` argv. Arguments are passed to `Process` as an
/// array; nothing here is ever joined into a shell string.
public enum DaemonArguments {
    public enum BuildError: Error, Equatable, CustomStringConvertible {
        case publicURLRequired(GuestAccessMode)
        case invalidPublicURL(String)
        case turnRequiredForPublic

        public var description: String {
            switch self {
            case .publicURLRequired:
                return "Guest access needs this Mac's Tailscale HTTPS address. Set it up again in Settings → Guest access."
            case .invalidPublicURL(let u):
                return "\(u) is not a Tailscale HTTPS address (https://<host>.<tailnet>.ts.net)."
            case .turnRequiredForPublic:
                return "Public guest links need the embedded TURN relay on a loopback port."
            }
        }
    }

    /// Validates a Tailscale HTTPS origin: https, `*.ts.net`, no path/query/userinfo.
    public static func isTailscaleHTTPSOrigin(_ raw: String) -> Bool {
        guard let u = URLComponents(string: raw), u.scheme == "https", let host = u.host, host.hasSuffix(".ts.net"),
              host.count > ".ts.net".count, u.user == nil, u.password == nil, u.query == nil, u.fragment == nil,
              u.path.isEmpty || u.path == "/" else { return false }
        return true
    }

    public static func build(configuration c: HostConfiguration, settings s: DesktopSettings) throws -> [String] {
        var args = ["--port", String(c.port), "--data-dir", c.dataDirectory.path, "--turn-listen", c.turnListen]
        if let udp = c.udpPort { args += ["--udp-port", String(udp)] }
        switch s.guestAccess {
        case .off:
            args.append("--no-funnel")
            // Launcher parity: a hand-written public URL still applies (it is
            // validated by the daemon's invite path, never by Funnel).
            if let url = s.publicURL, !url.isEmpty {
                guard isTailscaleHTTPSOrigin(url) else { throw BuildError.invalidPublicURL(url) }
                args += ["--public-url", trimmed(url)]
            }
        case .privateTailnet:
            guard let url = s.publicURL, !url.isEmpty else { throw BuildError.publicURLRequired(.privateTailnet) }
            guard isTailscaleHTTPSOrigin(url) else { throw BuildError.invalidPublicURL(url) }
            args += ["--no-funnel", "--public-url", trimmed(url)]
        case .publicFunnel:
            guard let url = s.publicURL, !url.isEmpty else { throw BuildError.publicURLRequired(.publicFunnel) }
            guard isTailscaleHTTPSOrigin(url) else { throw BuildError.invalidPublicURL(url) }
            guard c.turnPort != nil else { throw BuildError.turnRequiredForPublic }
            args += ["--public-url", trimmed(url), "--funnel-turn"]
        }
        if !s.publicSTUN { args.append("--no-public-stun") }
        return args
    }

    /// Environment for the daemon: bundled tools first on PATH (ffmpeg/ffprobe).
    public static func environment(configuration c: HostConfiguration, base: [String: String]) -> [String: String] {
        var env = base
        let inherited = base["PATH"] ?? "/usr/bin:/bin:/usr/sbin:/sbin"
        env["PATH"] = "\(c.toolsDirectory.path):/opt/homebrew/bin:/usr/local/bin:\(inherited)"
        return env
    }

    private static func trimmed(_ url: String) -> String {
        url.hasSuffix("/") ? String(url.dropLast()) : url
    }
}
