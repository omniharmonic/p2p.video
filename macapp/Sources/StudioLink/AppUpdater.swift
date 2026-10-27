import Foundation
import StudioLinkCore
#if canImport(Sparkle)
import Sparkle
#endif

/// In-app updates [SL-P2-2], mirroring Minutes: Sparkle 2 with an EdDSA-signed
/// appcast. Linked only in `STUDIOLINK_SPARKLE=1` builds, and started only when
/// the bundle carries an https `SUFeedURL` and a real `SUPublicEDKey`
/// (`UpdateConfiguration`). Sparkle asks the person on second launch before
/// checking automatically, and verifies every download against the key.
@MainActor
final class AppUpdater: ObservableObject {
    let status: UpdateConfiguration.Status
    @Published private(set) var canCheckForUpdates = false

    #if canImport(Sparkle)
    private var controller: SPUStandardUpdaterController?
    private var observation: NSKeyValueObservation?
    static let linked = true
    #else
    static let linked = false
    #endif

    init(info: [String: Any] = Bundle.main.infoDictionary ?? [:]) {
        status = UpdateConfiguration.evaluate(info: info, updaterLinked: Self.linked)
        #if canImport(Sparkle)
        guard status.isEnabled else { return }
        let controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
        self.controller = controller
        observation = controller.updater.observe(\.canCheckForUpdates, options: [.initial, .new]) { [weak self] updater, _ in
            let value = updater.canCheckForUpdates
            Task { @MainActor in self?.canCheckForUpdates = value }
        }
        #endif
    }

    var statusText: String {
        switch status {
        case .enabled(let feed): return "Updates are checked from \(feed.host ?? "the release feed") and verified before installing."
        case .disabled(let reason): return reason
        }
    }

    func checkForUpdates() {
        #if canImport(Sparkle)
        controller?.checkForUpdates(nil)
        #endif
    }
}
