import Foundation

/// Mirror of `GET /api/v1/desktop/state` (host/internal/httpserver/desktop.go).
/// Contains no credentials.
public struct HostState: Decodable, Equatable, Sendable {
    public struct Waiting: Decodable, Equatable, Sendable {
        public var participantId: String
        public var name: String

        public init(participantId: String, name: String) {
            self.participantId = participantId
            self.name = name
        }
    }

    public struct Session: Decodable, Equatable, Sendable {
        public var id: String
        public var title: String
        public var connected: Int
        public var admitted: Int
        public var hostConnected: Bool
        public var waiting: [Waiting]
        public var recording: Bool
        public var takeNumber: Int?
        public var live: Bool
        public var uploadingTracks: Int
    }

    public struct Take: Decodable, Equatable, Sendable {
        public var id: String
        public var sessionId: String
        public var number: Int
        public var status: String
        public var tracks: Int
        public var tracksUploaded: Int
        public var missingParticipants: Int
        public var uploadsComplete: Bool
        public var exported: Bool
    }

    /// A persistent room (names and IDs only; the guest link is fetched on demand).
    public struct Room: Decodable, Equatable, Sendable {
        public var id: String
        public var name: String
        public var live: Bool

        public init(id: String, name: String, live: Bool) {
            self.id = id
            self.name = name
            self.live = live
        }
    }

    public var product: String
    public var buildId: String
    public var pid: Int32
    public var recording: Bool
    public var live: Bool
    public var uploadsPending: Int
    public var waitingCount: Int
    public var sessions: [Session]
    public var takes: [Take]
    /// Absent from daemons older than rooms [SL-P1-5].
    public var rooms: [Room]?

    /// Persistent rooms for quick-open menu entries.
    public var roomList: [Room] { rooms ?? [] }

    public static func decode(_ data: Data) throws -> HostState {
        try JSONDecoder().decode(HostState.self, from: data)
    }

    public var activity: DaemonActivity {
        DaemonActivity(recording: recording, live: live, uploadsPending: uploadsPending)
    }

    /// Sessions with a call in progress (someone admitted).
    public var activeCalls: [Session] { sessions.filter { $0.admitted > 0 } }
}

/// The work a restart or quit would interrupt.
public struct DaemonActivity: Equatable, Sendable {
    public var recording: Bool
    public var live: Bool
    public var uploadsPending: Int

    public init(recording: Bool = false, live: Bool = false, uploadsPending: Int = 0) {
        self.recording = recording
        self.live = live
        self.uploadsPending = uploadsPending
    }

    public static let idle = DaemonActivity()

    public var isIdle: Bool { !recording && !live && uploadsPending == 0 }

    /// A sentence for a quit/restart warning, or nil when idle.
    public var interruptionWarning: String? {
        var parts: [String] = []
        if recording { parts.append("a take is recording") }
        if live { parts.append("a livestream is running") }
        if uploadsPending > 0 { parts.append("\(uploadsPending) recording upload\(uploadsPending == 1 ? " is" : "s are") still pending") }
        guard !parts.isEmpty else { return nil }
        let joined = parts.count == 1 ? parts[0] : parts.dropLast().joined(separator: ", ") + " and " + parts.last!
        return joined.prefix(1).uppercased() + joined.dropFirst() + "."
    }
}
