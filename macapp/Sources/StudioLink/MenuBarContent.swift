import AppKit
import StudioLinkCore
import SwiftUI

/// The menu bar extra's menu: what's happening, then Start a Meeting, Open
/// p2p.video, upcoming meetings and rooms [SL-UX-2].
struct MenuBarContent: View {
    @ObservedObject var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        if let state = model.hostState, model.activityLine != nil {
            if state.recording { Text("Recording") }
            if state.live { Text("Livestream is live") }
            if !state.activeCalls.isEmpty {
                Text(state.activeCalls.count == 1 ? "In a meeting" : "\(state.activeCalls.count) meetings in progress")
            }
            if state.waitingCount > 0 { Text("\(state.waitingCount) waiting to join") }
        } else {
            Text(model.statusLine)
        }
        if let state = model.hostState, state.uploadsPending > 0 {
            Text("Uploading \(state.uploadsPending) recording file\(state.uploadsPending == 1 ? "" : "s")")
        }
        Text(model.tailscale.summary)
        if model.tailscale.pendingApproval != nil {
            Button("Tailscale Needs Your Approval…") { showSettings() }
        }
        if let problem = model.problem { Text(problem).lineLimit(3) }
        Divider()
        Button("Start a Meeting") {
            Task { await model.startMeeting() }
        }
        .keyboardShortcut("n")
        .disabled(!model.supervisor.isHealthy)
        Button("Open p2p.video") { showMainWindow() }
            .keyboardShortcut("o")
        if !model.calendar.upcoming.isEmpty {
            Section("Upcoming meetings") {
                ForEach(model.calendar.upcoming.prefix(5)) { m in
                    Button("\(m.start.formatted(date: .omitted, time: .shortened))  \(m.title)") { model.openRoom(id: m.roomId) }
                        .disabled(!model.supervisor.isHealthy)
                }
            }
        }
        if !model.rooms.isEmpty {
            Menu("Rooms") {
                ForEach(model.rooms, id: \.id) { room in
                    Menu(room.live ? "\(room.name) (live now)" : room.name) {
                        Button(room.live ? "Rejoin" : "Start Meeting") { model.openRoom(room) }
                        Button("Copy Room Link") { Task { await model.copyRoomLink(room) } }
                    }
                }
            }
            .disabled(!model.supervisor.isHealthy)
        }
        Divider()
        Button("Open Home in Browser") { model.openDashboard() }
            .disabled(!model.supervisor.isHealthy)
        SettingsLink { Text("Settings…") }
            .keyboardShortcut(",")
        Divider()
        Button("Quit p2p.video") { requestQuit() }
            .keyboardShortcut("q")
    }

    private func showMainWindow() {
        openWindow(id: "main")
        NSApp.activate(ignoringOtherApps: true)
    }

    private func showSettings() {
        openSettings()
        NSApp.activate(ignoringOtherApps: true)
    }
}
