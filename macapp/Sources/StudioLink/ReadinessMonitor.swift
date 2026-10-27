import Foundation
import IOKit.ps
import StudioLinkCore

/// Measures what the podcast-day checklist needs [SL-P2-2]: free disk space,
/// the power source (IOKit) and a read-only HEAD of the guest link's
/// `/health`. It never changes Tailscale Serve or Funnel.
@MainActor
final class ReadinessMonitor: ObservableObject {
    @Published private(set) var freeBytes: Int64?
    @Published private(set) var power: PowerStatus?
    @Published private(set) var linkCheck: GuestLinkCheck.Result?
    /// The URL `linkCheck` belongs to; a change of mode or address resets it.
    private var checkedURL: String?
    private var loop: Task<Void, Never>?

    private let session: URLSession = {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 6
        cfg.timeoutIntervalForResource = 8
        cfg.requestCachePolicy = .reloadIgnoringLocalCacheData
        return URLSession(configuration: cfg)
    }()

    func start(every interval: Duration = .seconds(30), refresh: @escaping @MainActor () async -> Void) {
        loop?.cancel()
        loop = Task { @MainActor in
            while !Task.isCancelled {
                await refresh()
                try? await Task.sleep(for: interval)
            }
        }
    }

    func refresh(dataDirectory: URL, guestAccess: GuestAccessMode, publicURL: String?) async {
        freeBytes = Self.freeBytes(near: dataDirectory)
        power = Self.readPower()
        guard guestAccess != .off, let url = publicURL, let req = GuestLinkCheck.request(publicURL: url) else {
            linkCheck = nil
            checkedURL = nil
            return
        }
        if checkedURL != url { linkCheck = nil; checkedURL = url }
        do {
            let (_, resp) = try await session.data(for: req)
            linkCheck = GuestLinkCheck.classify(statusCode: (resp as? HTTPURLResponse)?.statusCode, error: nil)
        } catch {
            let reason = (error as? URLError).map(Self.describe) ?? "no answer"
            linkCheck = GuestLinkCheck.classify(statusCode: nil, error: reason)
        }
    }

    nonisolated static func describe(_ e: URLError) -> String {
        switch e.code {
        case .timedOut: return "timed out"
        case .cannotFindHost, .dnsLookupFailed: return "name not found"
        case .cannotConnectToHost: return "connection refused"
        case .notConnectedToInternet, .networkConnectionLost: return "offline"
        case .secureConnectionFailed, .serverCertificateUntrusted, .serverCertificateHasBadDate: return "certificate problem"
        default: return "error \(e.code.rawValue)"
        }
    }

    /// Free space on the data folder's volume (the folder may not exist yet).
    nonisolated static func freeBytes(near url: URL) -> Int64? {
        var dir = url
        while !FileManager.default.fileExists(atPath: dir.path), dir.path != "/" { dir.deleteLastPathComponent() }
        let values = try? dir.resourceValues(forKeys: [.volumeAvailableCapacityForImportantUsageKey, .volumeAvailableCapacityKey])
        if let v = values?.volumeAvailableCapacityForImportantUsage, v > 0 { return v }
        return values?.volumeAvailableCapacity.map(Int64.init)
    }

    nonisolated static func readPower() -> PowerStatus {
        guard let blob = IOPSCopyPowerSourcesInfo()?.takeRetainedValue() else {
            return PowerStatus.parse(providingType: nil, descriptions: [])
        }
        let list = (IOPSCopyPowerSourcesList(blob)?.takeRetainedValue() as? [CFTypeRef]) ?? []
        let descriptions = list.compactMap { IOPSGetPowerSourceDescription(blob, $0)?.takeUnretainedValue() as? [String: Any] }
        let providing = IOPSGetProvidingPowerSourceType(blob)?.takeUnretainedValue() as String?
        return PowerStatus.parse(providingType: providing, descriptions: descriptions)
    }
}
