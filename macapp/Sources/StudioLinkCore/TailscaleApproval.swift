import Foundation

/// A one-time approval Tailscale asks the tailnet owner for, found in the CLI's
/// output. `tailscale funnel` / `serve` print, for example:
///
///     Funnel is not enabled on your tailnet.
///     To enable, visit:
///
///              https://login.tailscale.com/f/funnel?node=<id>
///
/// and then wait until it is approved. p2p.video stops waiting, shows the
/// link as an "Open Tailscale" button, and offers "Try again" [SL-UX-2].
/// The URL is not a secret, but it is kept out of logs anyway.
public struct TailscaleApproval: Equatable, Sendable {
    public enum Feature: String, Equatable, Sendable {
        /// Public links (Tailscale Funnel).
        case funnel
        /// HTTPS links inside the tailnet (Tailscale Serve / HTTPS certificates).
        case serve
    }

    public var feature: Feature
    /// The page in the Tailscale admin console that approves it.
    public var url: URL

    public init(feature: Feature, url: URL) {
        self.feature = feature
        self.url = url
    }

    /// The step's heading, in the user's words.
    public var title: String {
        switch feature {
        case .funnel: return "Allow public links in Tailscale"
        case .serve: return "Allow secure links in Tailscale"
        }
    }

    /// One plain sentence on what approving does.
    public var explanation: String {
        switch feature {
        case .funnel:
            return "Tailscale asks the owner of your network to approve public links once. Approving lets this Mac share a page on the internet; p2p.video shares only the guest page, and you still let each guest in."
        case .serve:
            return "Tailscale asks the owner of your network to approve secure links once. Approving lets this Mac share pages with devices on your Tailscale network, with an HTTPS certificate."
        }
    }

    /// Looks for an approval prompt in (partial) CLI output. Returns nil when
    /// the output has no enable link.
    public static func find(in output: String) -> TailscaleApproval? {
        let lines = output.components(separatedBy: .newlines)
        for (index, line) in lines.enumerated() {
            for url in urls(in: line) where isApprovalURL(url, after: lines[..<index]) {
                let context = lines[max(0, index - 4)...index].joined(separator: "\n")
                return TailscaleApproval(feature: feature(of: url, context: context), url: url)
            }
        }
        return nil
    }

    // MARK: parsing

    static func urls(in line: String) -> [URL] {
        var found: [URL] = []
        var rest = Substring(line)
        while let range = rest.range(of: "https://") {
            let tail = rest[range.lowerBound...]
            let end = tail.firstIndex(where: { $0.isWhitespace || "\"'<>()[]{}`".contains($0) }) ?? tail.endIndex
            var candidate = String(tail[..<end])
            while let last = candidate.last, ".,;:!?".contains(last) { candidate.removeLast() }
            if let url = URL(string: candidate), url.host != nil { found.append(url) }
            rest = tail[end...]
        }
        return found
    }

    /// A Tailscale login-server link, or any HTTPS link printed right after
    /// "To enable, visit:" (custom control servers).
    static func isApprovalURL(_ url: URL, after previous: ArraySlice<String>) -> Bool {
        guard url.scheme == "https", let host = url.host?.lowercased() else { return false }
        if host == "login.tailscale.com" {
            // Docs and KB links are not approval pages.
            return !url.path.hasPrefix("/kb") && url.path != "/" && !url.path.isEmpty
        }
        let recent = previous.suffix(3).joined(separator: "\n").lowercased()
        return recent.contains("to enable, visit")
    }

    static func feature(of url: URL, context: String) -> Feature {
        let path = url.path.lowercased()
        if path.contains("funnel") { return .funnel }
        if path.contains("serve") || path.contains("https") { return .serve }
        let text = context.lowercased()
        if text.contains("funnel") { return .funnel }
        return .serve
    }
}
