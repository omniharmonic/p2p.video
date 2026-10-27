import Foundation

/// The daemon's `/health` body.
public struct HealthResponse: Decodable, Equatable, Sendable {
    public var status: String
    public var product: String
    public var buildId: String
}

/// What is listening on the configured port, mirroring `launch-mac.sh`:
/// only the exact product and build is ever reused; anything else is left
/// running untouched and reported with an actionable message.
public enum HealthVerdict: Equatable, Sendable {
    /// Nothing answered: the port is free to start our daemon.
    case unreachable
    /// Our exact build is healthy and can be reused.
    case expected
    /// A different build answered: another p2p.video, or the pre-rename StudioLink app (same product identity), older or newer.
    case otherBuild(String)
    /// Some other service answered, or the response was not p2p.video (or StudioLink) health.
    case foreign

    public func message(port: Int) -> String? {
        switch self {
        case .unreachable, .expected:
            return nil
        case .otherBuild(let build):
            return "Another p2p.video or StudioLink host (build \(build)) is running on port \(port). Finish any active session there, then quit the old StudioLink app (or stop that host yourself) and reopen p2p.video. No running service was stopped."
        case .foreign:
            return "Another service is using port \(port). Stop it or choose a different port, then reopen this app. No running service was stopped."
        }
    }
}

public enum HealthCheck {
    /// Classifies a health probe result.
    /// - Parameters:
    ///   - statusCode: HTTP status, or nil if no HTTP response arrived (refused/timeout).
    ///   - body: response body if any.
    ///   - expectedBuild: the bundle's exact build ID.
    public static func classify(statusCode: Int?, body: Data?, expectedBuild: String) -> HealthVerdict {
        guard let code = statusCode else { return .unreachable }
        // Something answered on the port: never treat it as free.
        guard code == 200, let body, let health = try? JSONDecoder().decode(HealthResponse.self, from: body) else {
            return .foreign
        }
        guard health.product == HostConfiguration.product else { return .foreign }
        guard health.status == "ok" else { return .foreign }
        return health.buildId == expectedBuild ? .expected : .otherBuild(health.buildId)
    }
}
