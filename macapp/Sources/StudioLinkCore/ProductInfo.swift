import Foundation

/// Product facts shown in About and Settings [SL-REL-1].
public enum ProductInfo {
    public static let name = "p2p.video"
    /// AGPL-3.0 §13: users (including remote guests) are offered the source.
    public static let sourceURL = URL(string: "https://github.com/omniharmonic/p2p.video")!
    public static let license = "GNU Affero General Public License v3"

    /// "1.0.0 (5), build <id>" from the bundle's Info.plist and build ID.
    public static func versionLine(shortVersion: String?, bundleVersion: String?, buildID: String?) -> String {
        var line = shortVersion ?? "development"
        if let b = bundleVersion, !b.isEmpty { line += " (\(b))" }
        if let id = buildID, !id.isEmpty { line += ", build \(id)" }
        return line
    }
}
