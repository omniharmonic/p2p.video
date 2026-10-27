import Foundation

/// The sentence shown before quitting stops the host [SL-P2-2]. Covers what
/// a restart would interrupt (recording, livestream, pending uploads) plus a
/// call in progress or guests waiting, which quitting also disconnects.
public enum QuitWarning {
    public static func message(_ state: HostState?) -> String? {
        guard let state else { return nil }
        var parts: [String] = []
        if state.recording { parts.append("a take is recording") }
        if state.live { parts.append("a livestream is running") }
        if state.uploadsPending > 0 {
            parts.append("\(state.uploadsPending) recording upload\(state.uploadsPending == 1 ? " is" : "s are") still pending")
        }
        let calls = state.activeCalls.count
        if calls > 0 { parts.append(calls == 1 ? "a call is in progress" : "\(calls) calls are in progress") }
        if state.waitingCount > 0 {
            parts.append("\(state.waitingCount) guest\(state.waitingCount == 1 ? " is" : "s are") waiting to be admitted")
        }
        guard !parts.isEmpty else { return nil }
        let joined = parts.count == 1 ? parts[0] : parts.dropLast().joined(separator: ", ") + " and " + parts.last!
        return joined.prefix(1).uppercased() + joined.dropFirst() + "."
    }
}
