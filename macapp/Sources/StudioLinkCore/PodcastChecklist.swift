import Foundation

/// How a readiness row looks.
public enum CheckLevel: Equatable, Sendable {
    case ok, warning, problem
    /// Not measured yet, or not applicable (shown neutral).
    case neutral
}

public struct ReadinessItem: Identifiable, Equatable, Sendable {
    public var id: String
    public var title: String
    public var detail: String
    public var level: CheckLevel

    public init(id: String, title: String, detail: String, level: CheckLevel) {
        self.id = id
        self.title = title
        self.detail = detail
        self.level = level
    }
}

// MARK: disk

/// Free space against the recording estimate (~10 GB per hour: every
/// participant's local 1080p video plus PCM, uploaded to this Mac, plus the
/// processed export).
public enum DiskEstimate {
    public static let bytesPerHour: Int64 = 10_000_000_000
    /// Below this many hours of headroom the row is a warning; below `minimumHours` a problem.
    public static let comfortableHours = 4.0
    public static let minimumHours = 2.0

    public static func hours(freeBytes: Int64) -> Double { Double(max(freeBytes, 0)) / Double(bytesPerHour) }

    public static func evaluate(freeBytes: Int64?) -> ReadinessItem {
        guard let free = freeBytes else {
            return ReadinessItem(id: "disk", title: "Disk space", detail: "Could not read free space for the data folder.", level: .neutral)
        }
        let h = hours(freeBytes: free)
        let gb = ByteCountFormatter.string(fromByteCount: free, countStyle: .decimal)
        let hoursText = h >= 10 ? "\(Int(h.rounded(.down))) hours" : String(format: "%.1f hours", h)
        let detail = "\(gb) free · about \(hoursText) of recording at ~10 GB/hour"
        let level: CheckLevel = h >= comfortableHours ? .ok : (h >= minimumHours ? .warning : .problem)
        let suffix = level == .ok ? "" : " Free up space before a long episode."
        return ReadinessItem(id: "disk", title: "Disk space", detail: detail + "." + suffix, level: level)
    }
}

// MARK: power

/// The parts of IOKit's power-source info the checklist needs
/// (`IOPSGetProvidingPowerSourceType` plus `IOPSGetPowerSourceDescription`).
public struct PowerStatus: Equatable, Sendable {
    public enum Source: Equatable, Sendable { case ac, battery, ups, unknown }
    public var source: Source
    public var hasBattery: Bool
    public var batteryPercent: Int?
    public var charging: Bool?

    public init(source: Source, hasBattery: Bool, batteryPercent: Int? = nil, charging: Bool? = nil) {
        self.source = source
        self.hasBattery = hasBattery
        self.batteryPercent = batteryPercent
        self.charging = charging
    }

    /// - Parameters:
    ///   - providingType: `IOPSGetProvidingPowerSourceType` (`"AC Power"`, `"Battery Power"`, `"UPS Power"`).
    ///   - descriptions: each source's description dictionary (keys as in IOPSKeys.h).
    public static func parse(providingType: String?, descriptions: [[String: Any]]) -> PowerStatus {
        let source: Source
        switch providingType {
        case "AC Power": source = .ac
        case "Battery Power": source = .battery
        case "UPS Power": source = .ups
        default: source = .unknown
        }
        let battery = descriptions.first { ($0["Type"] as? String) == "InternalBattery" }
        var percent: Int?
        if let b = battery, let cur = (b["Current Capacity"] as? NSNumber)?.doubleValue,
           let max = (b["Max Capacity"] as? NSNumber)?.doubleValue, max > 0 {
            percent = Int((cur / max * 100).rounded())
        }
        // A battery's own state wins when the providing type is missing.
        var resolved = source
        if resolved == .unknown, let state = battery?["Power Source State"] as? String {
            resolved = state == "AC Power" ? .ac : (state == "Battery Power" ? .battery : .unknown)
        }
        return PowerStatus(source: resolved, hasBattery: battery != nil, batteryPercent: percent,
                           charging: (battery?["Is Charging"] as? NSNumber)?.boolValue)
    }

    public var readiness: ReadinessItem {
        let pct = batteryPercent.map { " (\($0)%)" } ?? ""
        switch source {
        case .ac:
            let detail = hasBattery ? "Power adapter connected\(pct)." : "On mains power."
            return ReadinessItem(id: "power", title: "Power", detail: detail, level: .ok)
        case .ups:
            return ReadinessItem(id: "power", title: "Power", detail: "Running on a UPS. Restore mains power before recording.", level: .warning)
        case .battery:
            return ReadinessItem(id: "power", title: "Power", detail: "On battery\(pct). Plug in the power adapter before recording.", level: .warning)
        case .unknown:
            return ReadinessItem(id: "power", title: "Power", detail: "Could not read the power source.", level: .neutral)
        }
    }
}

// MARK: guest link reachability

/// A read-only self-check of the configured guest link: `HEAD <publicURL>/health`.
/// From this Mac the request goes over the tailnet to Tailscale Serve, so it
/// proves the route reaches this daemon; it does not prove the public internet
/// can reach Funnel. It never changes Serve or Funnel.
public enum GuestLinkCheck {
    public enum Result: Equatable, Sendable {
        case reachable
        /// Something answered, but not a healthy p2p.video.
        case unexpected(Int)
        case unreachable(String)
    }

    public static func request(publicURL: String, timeout: TimeInterval = 6) -> URLRequest? {
        guard var c = URLComponents(string: publicURL), c.scheme == "https", let host = c.host, !host.isEmpty else { return nil }
        c.path = "/health"
        c.query = nil
        c.fragment = nil
        guard let url = c.url else { return nil }
        var req = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: timeout)
        req.httpMethod = "HEAD"
        return req
    }

    public static func classify(statusCode: Int?, error: String?) -> Result {
        if let code = statusCode { return (200..<300).contains(code) ? .reachable : .unexpected(code) }
        return .unreachable(error ?? "No answer")
    }
}

// MARK: Tailscale summary for the checklist

public enum TailscaleReadiness: Equatable, Sendable {
    case checking
    case notInstalled
    case notRunning
    case needsLogin
    case connected(dnsName: String?)
}

// MARK: checklist

public struct ReadinessInputs: Equatable, Sendable {
    public var daemonHealthy: Bool
    public var daemonDetail: String
    public var tailscale: TailscaleReadiness
    public var guestAccess: GuestAccessMode
    public var publicURL: String?
    public var linkCheck: GuestLinkCheck.Result?
    public var freeBytes: Int64?
    public var power: PowerStatus?
    public var browser: DashboardBrowser

    public init(daemonHealthy: Bool, daemonDetail: String, tailscale: TailscaleReadiness, guestAccess: GuestAccessMode,
                publicURL: String?, linkCheck: GuestLinkCheck.Result?, freeBytes: Int64?, power: PowerStatus?, browser: DashboardBrowser) {
        self.daemonHealthy = daemonHealthy
        self.daemonDetail = daemonDetail
        self.tailscale = tailscale
        self.guestAccess = guestAccess
        self.publicURL = publicURL
        self.linkCheck = linkCheck
        self.freeBytes = freeBytes
        self.power = power
        self.browser = browser
    }
}

/// The podcast-day checklist rows in the control center.
public enum PodcastChecklist {
    public static func evaluate(_ i: ReadinessInputs) -> [ReadinessItem] {
        var items: [ReadinessItem] = []
        items.append(ReadinessItem(id: "host", title: "p2p.video", detail: i.daemonDetail, level: i.daemonHealthy ? .ok : .problem))

        let needsTailscale = i.guestAccess != .off
        switch i.tailscale {
        case .checking:
            items.append(ReadinessItem(id: "tailscale", title: "Tailscale", detail: "Checking…", level: .neutral))
        case .notInstalled:
            items.append(ReadinessItem(id: "tailscale", title: "Tailscale", detail: "Not installed. Guests joining from other devices need it on this Mac.", level: needsTailscale ? .problem : .warning))
        case .notRunning:
            items.append(ReadinessItem(id: "tailscale", title: "Tailscale", detail: "Not running. Open Tailscale.", level: needsTailscale ? .problem : .warning))
        case .needsLogin:
            items.append(ReadinessItem(id: "tailscale", title: "Tailscale", detail: "Signed out. Open Tailscale and sign in.", level: needsTailscale ? .problem : .warning))
        case .connected(let name):
            items.append(ReadinessItem(id: "tailscale", title: "Tailscale", detail: "Connected\(name.map { " as \($0)" } ?? "").", level: .ok))
        }

        items.append(guestLink(i))
        items.append(DiskEstimate.evaluate(freeBytes: i.freeBytes))
        items.append(i.power?.readiness ?? ReadinessItem(id: "power", title: "Power", detail: "Checking…", level: .neutral))
        items.append(ReadinessItem(
            id: "devices", title: "Camera and microphone",
            detail: "macOS doesn't let p2p.video read \(i.browser == .chrome ? "Chrome" : "your browser")'s permissions. Run the test call: if the preview stays black, allow it in System Settings → Privacy & Security → Camera / Microphone.",
            level: .neutral))
        return items
    }

    static func guestLink(_ i: ReadinessInputs) -> ReadinessItem {
        let title = "Guest links"
        switch i.guestAccess {
        case .off:
            return ReadinessItem(id: "guest", title: title, detail: "This Mac only: guests can't join yet. Choose how guests join in Settings → Guest Access.", level: .warning)
        case .privateTailnet, .publicFunnel:
            let mode = i.guestAccess == .publicFunnel ? "Guests can join from anywhere" : "Guests on your Tailscale network can join"
            guard let url = i.publicURL, !url.isEmpty else {
                return ReadinessItem(id: "guest", title: title, detail: "\(mode), but no address is recorded. Check Settings → Guest Access.", level: .warning)
            }
            switch i.linkCheck {
            case nil:
                return ReadinessItem(id: "guest", title: title, detail: "\(mode). Checking \(url)…", level: .neutral)
            case .reachable?:
                let note = i.guestAccess == .publicFunnel ? " (checked over the tailnet; the public internet path is tested only by a guest outside it)" : ""
                return ReadinessItem(id: "guest", title: title, detail: "\(mode). \(url) answers\(note).", level: .ok)
            case .unexpected(let code)?:
                return ReadinessItem(id: "guest", title: title, detail: "\(mode). \(url) answered \(code), not p2p.video. Check Settings → Guest Access.", level: .problem)
            case .unreachable(let why)?:
                return ReadinessItem(id: "guest", title: title, detail: "\(mode). \(url) did not answer (\(why)). Check Tailscale and Settings → Guest Access.", level: .problem)
            }
        }
    }
}
