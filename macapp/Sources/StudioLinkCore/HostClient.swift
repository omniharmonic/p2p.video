import Foundation

/// A session created from the app. The host token and invite are kept in
/// memory only and never logged.
public struct CreatedSession: Decodable, Equatable, Sendable {
    public var id: String
    public var title: String
    public var hostToken: String
    public var hostUrl: String
}

public struct InviteResponse: Decodable, Equatable, Sendable {
    public var sessionId: String
    public var inviteUrl: String
}

/// A room's guest link as the local dashboard API returns it. Contains the
/// link secret: clipboard only, never logged.
public struct RoomLink: Decodable, Equatable, Sendable {
    public var id: String
    public var name: String
    public var guestUrl: String?
    public var guestUrlRemote: Bool
}

/// Dashboard URLs the app opens [SL-P1-5].
public enum DashboardURLs {
    /// The call-first home: New meeting, Rooms, Recent meetings.
    public static func home(_ base: URL) -> URL { base }

    /// The home with a room preselected; the host checks devices, then starts it.
    public static func room(_ base: URL, id: String) -> URL {
        var c = URLComponents(url: base, resolvingAgainstBaseURL: false)!
        c.path = "/"
        c.queryItems = [URLQueryItem(name: "room", value: id)]
        return c.url!
    }

    /// Adds the daemon's per-boot launch token so the opened page is signed
    /// in as the host [SL-SEC-1]. The daemon sets an HttpOnly cookie and
    /// redirects to the same URL without the token. Without a token the URL
    /// is returned unchanged (the page then asks to be opened from the app).
    public static func launch(_ url: URL, token: String?) -> URL {
        guard let token, !token.isEmpty,
              var c = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
        if c.path.isEmpty { c.path = "/" }
        var items = (c.queryItems ?? []).filter { $0.name != "launch" }
        items.append(URLQueryItem(name: "launch", value: token))
        c.queryItems = items
        return c.url ?? url
    }
}

public enum HostClientError: Error, Equatable, CustomStringConvertible {
    case http(Int, String)
    case missingDesktopToken

    public var description: String {
        switch self {
        case .http(let code, let message): return message.isEmpty ? "The host answered \(code)." : message
        case .missingDesktopToken: return "The host has not created its desktop token yet."
        }
    }
}

/// Talks to the local daemon over loopback only.
public struct HostClient: Sendable {
    public var baseURL: URL
    public var session: URLSession

    public init(baseURL: URL, session: URLSession = HostClient.makeSession()) {
        self.baseURL = baseURL
        self.session = session
    }

    public static func makeSession() -> URLSession {
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 2
        cfg.timeoutIntervalForResource = 5
        cfg.requestCachePolicy = .reloadIgnoringLocalCacheData
        cfg.connectionProxyDictionary = [:] // never route loopback through a proxy
        return URLSession(configuration: cfg)
    }

    /// Probes `/health` and classifies who is listening.
    public func health(expectedBuild: String) async -> HealthVerdict {
        var req = URLRequest(url: baseURL.appendingPathComponent("health"))
        req.timeoutInterval = 2
        do {
            let (data, resp) = try await session.data(for: req)
            return HealthCheck.classify(statusCode: (resp as? HTTPURLResponse)?.statusCode, body: data, expectedBuild: expectedBuild)
        } catch {
            return .unreachable
        }
    }

    public func state(desktopToken: String) async throws -> HostState {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/v1/desktop/state"))
        req.setValue("Bearer \(desktopToken)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try HostState.decode(data)
    }

    /// Management routes (create session, rooms) need the desktop token
    /// since any local process can reach loopback [SL-SEC-1].
    public func createSession(title: String, desktopToken: String) async throws -> CreatedSession {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/v1/sessions"))
        req.httpMethod = "POST"
        req.setValue("Bearer \(desktopToken)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: ["title": title, "autoAdmit": false])
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try JSONDecoder().decode(CreatedSession.self, from: data)
    }

    /// Longest room name the daemon accepts (`maxRoomNameRunes`).
    public static let maxRoomNameLength = 80

    /// `POST /api/v1/rooms {"name": …}` with the desktop token, which the
    /// daemon accepts as the host admin credential on direct loopback
    /// [SL-SEC-1]. The body carries only `name`: the daemon rejects unknown fields.
    public static func createRoomRequest(baseURL: URL, name: String, desktopToken: String) throws -> URLRequest {
        let clean = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !clean.isEmpty, clean.count <= maxRoomNameLength else {
            throw HostClientError.http(400, "Give the room a name of 1–\(maxRoomNameLength) characters.")
        }
        var req = URLRequest(url: baseURL.appendingPathComponent("api/v1/rooms"))
        req.httpMethod = "POST"
        req.setValue("Bearer \(desktopToken)", forHTTPHeaderField: "Authorization")
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONSerialization.data(withJSONObject: ["name": clean])
        return req
    }

    /// Creates a persistent room and returns its guest link (clipboard only, never logged).
    public func createRoom(name: String, desktopToken: String) async throws -> RoomLink {
        let req = try Self.createRoomRequest(baseURL: baseURL, name: name, desktopToken: desktopToken)
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try JSONDecoder().decode(RoomLink.self, from: data)
    }

    /// Reads a room's current guest link (loopback management API).
    public func roomLink(id: String, desktopToken: String) async throws -> RoomLink {
        let url = baseURL.appendingPathComponent("api/v1/rooms").appendingPathComponent(id)
        var req = URLRequest(url: url)
        req.setValue("Bearer \(desktopToken)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try JSONDecoder().decode(RoomLink.self, from: data)
    }

    /// Rooms, guest origins and meeting times for calendar matching
    /// (`GET /api/v1/desktop/calendar`, loopback + desktop token) [SL-P6-1].
    public func desktopCalendar(desktopToken: String) async throws -> DesktopCalendar {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/v1/desktop/calendar"))
        req.setValue("Bearer \(desktopToken)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try DesktopCalendar.decode(data)
    }

    /// Scheduled meetings with their invite text and guest link, for writing
    /// them into the user's chosen calendar. Never logged.
    public func scheduledMeetings(desktopToken: String) async throws -> [ScheduledMeetingInvite] {
        var req = URLRequest(url: baseURL.appendingPathComponent("api/v1/schedule"))
        req.setValue("Bearer \(desktopToken)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try ScheduledMeetingInvite.decodeList(data)
    }

    /// Mints a guest invite. This replaces any earlier invite for the session.
    public func invite(sessionID: String, hostToken: String) async throws -> InviteResponse {
        let url = baseURL.appendingPathComponent("api/v1/sessions").appendingPathComponent(sessionID).appendingPathComponent("invite")
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("Bearer \(hostToken)", forHTTPHeaderField: "Authorization")
        let (data, resp) = try await session.data(for: req)
        try Self.check(resp, data)
        return try JSONDecoder().decode(InviteResponse.self, from: data)
    }

    static func check(_ resp: URLResponse, _ data: Data) throws {
        guard let http = resp as? HTTPURLResponse else { throw HostClientError.http(0, "") }
        guard (200..<300).contains(http.statusCode) else {
            let text = String(decoding: data.prefix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw HostClientError.http(http.statusCode, text)
        }
    }

    /// Reads the data-directory desktop token written by the daemon.
    public static func readDesktopToken(_ file: URL) -> String? {
        guard let text = try? String(contentsOf: file, encoding: .utf8) else { return nil }
        let token = text.trimmingCharacters(in: .whitespacesAndNewlines)
        return token.count >= 32 ? token : nil
    }

    /// Reads the daemon's per-boot dashboard launch token (`.launch_token`).
    /// Read it fresh every time a page is opened: a daemon restart replaces it.
    public static func readLaunchToken(_ file: URL) -> String? {
        readDesktopToken(file)
    }
}
