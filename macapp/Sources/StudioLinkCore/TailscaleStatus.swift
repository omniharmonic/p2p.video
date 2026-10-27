import Foundation

/// The parts of `tailscale status --json` the guided setup needs.
public struct TailscaleStatus: Equatable, Sendable {
    public var backendState: String
    /// `host.tailnet.ts.net` without the trailing dot.
    public var dnsName: String?
    public var online: Bool
    public var magicDNSEnabled: Bool
    public var magicDNSSuffix: String?
    public var certDomains: [String]
    public var capabilities: Set<String>

    public var loggedIn: Bool { backendState == "Running" }
    public var needsLogin: Bool { backendState == "NeedsLogin" || backendState == "NeedsMachineAuth" }
    /// HTTPS certificates are enabled for the tailnet and this node has a name.
    public var httpsAvailable: Bool { !certDomains.isEmpty || capabilities.contains("https") }

    /// Ports Funnel may use, from the node's `funnel-ports` capability; nil if
    /// the capability lists no explicit ports.
    public var funnelPorts: Set<Int>? {
        for cap in capabilities where cap.hasPrefix("https://tailscale.com/cap/funnel-ports") {
            guard let q = URLComponents(string: cap)?.queryItems?.first(where: { $0.name == "ports" })?.value else { continue }
            var ports = Set<Int>()
            for part in q.split(separator: ",") {
                let bounds = part.split(separator: "-").compactMap { Int($0) }
                if bounds.count == 1 { ports.insert(bounds[0]) }
                if bounds.count == 2, bounds[0] <= bounds[1] { ports.formUnion(bounds[0]...bounds[1]) }
            }
            return ports
        }
        return nil
    }

    /// The tailnet policy grants this node the Funnel attribute.
    public var funnelAllowed: Bool {
        capabilities.contains("funnel") || capabilities.contains("https://tailscale.com/cap/funnel")
    }

    /// Funnel is allowed on both ports the public mode uses (443 app, 8443 TURN).
    public var funnelReadyForPublicGuests: Bool {
        guard funnelAllowed else { return false }
        guard let ports = funnelPorts else { return true }
        return ports.contains(443) && ports.contains(TailscaleCommands.funnelTURNPort)
    }

    /// `https://host.tailnet.ts.net` for this node, when HTTPS is possible.
    public var httpsOrigin: String? {
        guard let name = dnsName, name.hasSuffix(".ts.net") else { return nil }
        return "https://\(name)"
    }

    public static func parse(_ data: Data) throws -> TailscaleStatus {
        struct Raw: Decodable {
            struct SelfNode: Decodable {
                var DNSName: String?
                var Online: Bool?
                var Capabilities: [String]?
                var CapMap: [String: AnyDecodable]?
            }
            struct Tailnet: Decodable {
                var MagicDNSEnabled: Bool?
                var MagicDNSSuffix: String?
            }
            var BackendState: String?
            var `Self`: SelfNode?
            var CurrentTailnet: Tailnet?
            var MagicDNSSuffix: String?
            var CertDomains: [String]?
        }
        let raw = try JSONDecoder().decode(Raw.self, from: data)
        var caps = Set(raw.Self?.Capabilities ?? [])
        caps.formUnion(raw.Self?.CapMap?.keys ?? [:].keys)
        var dns = raw.Self?.DNSName
        if let d = dns, d.hasSuffix(".") { dns = String(d.dropLast()) }
        if dns?.isEmpty == true { dns = nil }
        return TailscaleStatus(
            backendState: raw.BackendState ?? "Unknown",
            dnsName: dns,
            online: raw.Self?.Online ?? false,
            magicDNSEnabled: raw.CurrentTailnet?.MagicDNSEnabled ?? false,
            magicDNSSuffix: raw.CurrentTailnet?.MagicDNSSuffix ?? raw.MagicDNSSuffix,
            certDomains: raw.CertDomains ?? [],
            capabilities: caps
        )
    }
}

/// Accepts any JSON value; used where only keys matter.
struct AnyDecodable: Decodable {
    init(from decoder: Decoder) throws {
        // Consume and discard whatever value is present.
        if var unkeyed = try? decoder.unkeyedContainer() {
            while !unkeyed.isAtEnd { _ = try unkeyed.decode(AnyDecodable.self) }
        } else if let keyed = try? decoder.container(keyedBy: AnyKey.self) {
            for key in keyed.allKeys { _ = try keyed.decode(AnyDecodable.self, forKey: key) }
        } else {
            _ = try? decoder.singleValueContainer()
        }
    }
}

struct AnyKey: CodingKey {
    var stringValue: String
    var intValue: Int?
    init?(stringValue: String) { self.stringValue = stringValue }
    init?(intValue: Int) { self.stringValue = String(intValue); self.intValue = intValue }
}

/// The parts of `tailscale serve status --json` / `funnel status --json`
/// (Tailscale's ServeConfig) the setup inspects.
public struct ServeConfig: Equatable, Sendable {
    public struct TCPHandler: Equatable, Sendable {
        public var https: Bool
        public var tcpForward: String?
        public var terminateTLS: String?
    }

    /// Port → TCP handler.
    public var tcp: [Int: TCPHandler]
    /// "host:port" → path → proxy target.
    public var web: [String: [String: String]]
    /// "host:port" → Funnel enabled.
    public var allowFunnel: [String: Bool]

    public static let empty = ServeConfig(tcp: [:], web: [:], allowFunnel: [:])

    public static func parse(_ data: Data) throws -> ServeConfig {
        struct Raw: Decodable {
            struct TCP: Decodable { var HTTPS: Bool?; var TCPForward: String?; var TerminateTLS: String? }
            struct Handler: Decodable { var Proxy: String? }
            struct Web: Decodable { var Handlers: [String: Handler]? }
            var TCP: [String: TCP]?
            var Web: [String: Web]?
            var AllowFunnel: [String: Bool]?
        }
        let trimmed = String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty || trimmed == "{}" || trimmed == "null" { return .empty }
        let raw = try JSONDecoder().decode(Raw.self, from: data)
        var tcp: [Int: TCPHandler] = [:]
        for (k, v) in raw.TCP ?? [:] {
            if let p = Int(k) { tcp[p] = TCPHandler(https: v.HTTPS ?? false, tcpForward: v.TCPForward, terminateTLS: v.TerminateTLS) }
        }
        var web: [String: [String: String]] = [:]
        for (hostPort, w) in raw.Web ?? [:] {
            var handlers: [String: String] = [:]
            for (path, h) in w.Handlers ?? [:] { handlers[path] = h.Proxy ?? "" }
            web[hostPort] = handlers
        }
        return ServeConfig(tcp: tcp, web: web, allowFunnel: raw.AllowFunnel ?? [:])
    }
}

/// What the current Serve/Funnel configuration means for this p2p.video host.
public struct GuestAccessObservation: Equatable, Sendable {
    /// :443 "/" proxies to this daemon.
    public var appRouted: Bool
    /// :443 is Funnel-enabled.
    public var appFunneled: Bool
    /// :8443 forwards TLS-terminated TCP to the TURN port.
    public var turnRouted: Bool
    /// :8443 is Funnel-enabled.
    public var turnFunneled: Bool
    /// :443 "/" currently proxies to something else (would be replaced).
    public var conflictingAppTarget: String?
    /// :8443 forwards somewhere else (would be replaced).
    public var conflictingTURNTarget: String?
    /// The HTTPS origin of the route, when routed.
    public var origin: String?

    public var mode: GuestAccessMode? {
        if appRouted && appFunneled && turnRouted && turnFunneled { return .publicFunnel }
        if appRouted && !appFunneled && !(turnRouted && turnFunneled) { return .privateTailnet }
        if !appRouted && !turnRouted { return .off }
        return nil // partial / mixed
    }

    /// Anything of ours reachable from the public internet.
    public var anythingPublic: Bool { (appRouted && appFunneled) || (turnRouted && turnFunneled) }

    /// - Parameter dnsName: this node's MagicDNS name (`host.tailnet.ts.net`).
    public static func observe(_ config: ServeConfig, dnsName: String, port: Int, turnPort: Int?) -> GuestAccessObservation {
        var o = GuestAccessObservation(appRouted: false, appFunneled: false, turnRouted: false, turnFunneled: false)
        let appKey = "\(dnsName):443"
        let ourTargets: Set<String> = ["http://127.0.0.1:\(port)", "http://localhost:\(port)",
                                       "http://127.0.0.1:\(port)/", "http://localhost:\(port)/"]
        if let target = config.web[appKey]?["/"] {
            if ourTargets.contains(target) {
                o.appRouted = true
                o.origin = "https://\(dnsName)"
            } else {
                o.conflictingAppTarget = target
            }
        }
        o.appFunneled = config.allowFunnel[appKey] ?? false
        let turnPortKey = TailscaleCommands.funnelTURNPort
        if let h = config.tcp[turnPortKey], let fwd = h.tcpForward {
            if let tp = turnPort, fwd == "127.0.0.1:\(tp)" || fwd == "localhost:\(tp)", h.terminateTLS?.isEmpty == false {
                o.turnRouted = true
            } else {
                o.conflictingTURNTarget = fwd
            }
        }
        o.turnFunneled = config.allowFunnel["\(dnsName):\(turnPortKey)"] ?? false
        return o
    }
}
