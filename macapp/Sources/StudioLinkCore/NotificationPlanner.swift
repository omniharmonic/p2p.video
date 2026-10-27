import Foundation

/// A user-facing notification derived from host state changes.
public enum HostNotification: Equatable, Sendable {
    case guestWaiting(name: String, sessionTitle: String, participantId: String)
    case takeUploaded(number: Int, sessionId: String, takeId: String)
    case takeExported(number: Int, sessionId: String, takeId: String)

    /// Stable identifier so a repeated poll never re-posts the same event.
    public var identifier: String {
        switch self {
        case .guestWaiting(_, _, let p): return "guest-waiting-\(p)"
        case .takeUploaded(_, _, let t): return "take-uploaded-\(t)"
        case .takeExported(_, _, let t): return "take-exported-\(t)"
        }
    }

    public var title: String {
        switch self {
        case .guestWaiting(let name, _, _): return "\(name.isEmpty ? "A guest" : name) is waiting to join"
        case .takeUploaded(let n, _, _): return "Take \(n) finished uploading"
        case .takeExported(let n, _, _): return "Take \(n) is ready for your editor"
        }
    }

    public var body: String {
        switch self {
        case .guestWaiting(_, let title, _): return "Admit them in the studio\(title.isEmpty ? "" : " for “\(title)”")."
        case .takeUploaded: return "Every participant's recording arrived. You can process and export it now."
        case .takeExported: return "The editor package was exported."
        }
    }
}

/// Diffs consecutive host states into notifications. The first observation
/// only seeds state, so launching the app never replays old events.
public struct NotificationPlanner: Sendable {
    private var seeded = false
    private var waiting: Set<String> = []
    private var uploaded: Set<String> = []
    private var exported: Set<String> = []

    public init() {}

    public mutating func plan(_ state: HostState, guests: Bool, takes: Bool) -> [HostNotification] {
        var out: [HostNotification] = []
        var nowWaiting: Set<String> = []
        for s in state.sessions {
            for w in s.waiting {
                nowWaiting.insert(w.participantId)
                if seeded, guests, !waiting.contains(w.participantId) {
                    out.append(.guestWaiting(name: w.name, sessionTitle: s.title, participantId: w.participantId))
                }
            }
        }
        waiting = nowWaiting
        for t in state.takes {
            if t.uploadsComplete, !uploaded.contains(t.id) {
                if seeded, takes { out.append(.takeUploaded(number: t.number, sessionId: t.sessionId, takeId: t.id)) }
                uploaded.insert(t.id)
            }
            if t.exported, !exported.contains(t.id) {
                if seeded, takes { out.append(.takeExported(number: t.number, sessionId: t.sessionId, takeId: t.id)) }
                exported.insert(t.id)
            }
        }
        seeded = true
        return out
    }
}
