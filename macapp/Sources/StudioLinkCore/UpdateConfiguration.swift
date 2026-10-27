import Foundation

/// Whether in-app updates (Sparkle 2) are active for this copy [SL-P2-2].
///
/// Updates are opt-in at build time: the updater is linked only when the
/// package is built with `STUDIOLINK_SPARKLE=1`, and it starts only when the
/// bundle's Info.plist carries an https `SUFeedURL` and a real EdDSA
/// `SUPublicEDKey`. Anything else leaves updates off, so a build can never
/// install an update it cannot verify.
public enum UpdateConfiguration {
    public enum Status: Equatable, Sendable {
        case enabled(feed: URL)
        case disabled(String)

        public var isEnabled: Bool { if case .enabled = self { return true } else { return false } }
    }

    public static func evaluate(info: [String: Any], updaterLinked: Bool) -> Status {
        guard updaterLinked else { return .disabled("This copy was built without the updater. Download new versions manually.") }
        guard let raw = (info["SUFeedURL"] as? String)?.trimmingCharacters(in: .whitespaces), !raw.isEmpty else {
            return .disabled("No update feed is configured for this build.")
        }
        guard let feed = URL(string: raw), feed.scheme == "https", feed.host?.isEmpty == false else {
            return .disabled("The update feed must be an https URL.")
        }
        guard let key = info["SUPublicEDKey"] as? String, isEdDSAPublicKey(key) else {
            return .disabled("No update signing key is configured for this build.")
        }
        return .enabled(feed: feed)
    }

    /// A base64 Ed25519 public key (32 bytes), as `generate_keys` prints it.
    public static func isEdDSAPublicKey(_ s: String) -> Bool {
        Data(base64Encoded: s.trimmingCharacters(in: .whitespaces))?.count == 32
    }
}
