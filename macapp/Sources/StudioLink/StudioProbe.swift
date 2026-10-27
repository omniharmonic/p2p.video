import AppKit
import StudioLinkCore
import WebKit

/// End-to-end studio probe [SL-P2-3]: the real embedded studio from a test
/// daemon, in the real studio window (`StudioWindowController`), with WebKit's
/// mock capture devices. It signs in with the launch token, joins, records a
/// take, verifies it server-side, exercises downloads, links, clipboard, the
/// page bridge and screen share, then closes the window mid-take, reopens it
/// and checks that the journaled upload resumes and the take completes.
///
/// Run with `STUDIOLINK_STUDIO_PROBE=<out.json>` (see `make macapp-probe`).
/// Probe-only private switches: mock capture devices (they bypass macOS TCC,
/// which can't be answered unattended) and window occlusion detection.
@MainActor
final class StudioProbe {
    struct Check: Codable {
        var name: String
        var ok: Bool
        var required: Bool
        var detail: String
    }

    private let model: AppModel
    private let output: URL
    private let downloads: URL
    private var checks: [Check] = []
    private var externalURLs: [URL] = []
    private var controller: StudioWindowController!
    private let started = Date()

    /// Hooks fetch: remembers the created session (for rejoin) and can drop
    /// chunk uploads so a take's tail exists only in OPFS.
    static let fetchHook = #"""
(() => {
  const probe = window.__slProbe = { session: null, dropChunks: false, dropped: 0 };
  const original = window.fetch.bind(window);
  window.fetch = async (input, init) => {
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    const method = String(init?.method || (typeof input === 'string' ? 'GET' : input.method) || 'GET').toUpperCase();
    if (probe.dropChunks && /\/api\/v1\/takes\/[^/]+\/tracks\/[^/]+\/chunks\//.test(url.pathname)) {
      probe.dropped++; throw new TypeError('probe: chunk upload dropped');
    }
    const response = await original(input, init);
    if (method === 'POST' && url.pathname === '/api/v1/sessions') { try { probe.session = await response.clone().json(); } catch (_) {} }
    return response;
  };
})();
"""#

    init(model: AppModel, output: URL) {
        self.model = model
        self.output = output
        downloads = output.deletingLastPathComponent().appendingPathComponent("studio-probe-downloads-\(getpid())", isDirectory: true)
    }

    private func record(_ name: String, _ ok: Bool, _ detail: String, required: Bool = true) {
        checks.append(Check(name: name, ok: ok, required: required, detail: detail))
        appLog.notice("Studio probe: \(ok ? "ok " : "FAIL", privacy: .public) \(name, privacy: .public): \(detail, privacy: .public)")
    }

    // MARK: run

    func run() async -> Bool {
        await model.supervisor.start()
        guard model.supervisor.isHealthy else {
            record("daemonHealthy", false, model.problem ?? "unknown")
            return finish()
        }
        record("daemonHealthy", true, "port \(model.configuration.port)")
        try? FileManager.default.removeItem(at: downloads)
        try? FileManager.default.createDirectory(at: downloads, withIntermediateDirectories: true)
        controller = StudioWindowController(model: model, options: .init(
            downloadsDirectory: downloads,
            openExternally: { [weak self] url in self?.externalURLs.append(url) },
            autoAnswerClose: true, probeMockCapture: true, probeUserScript: Self.fetchHook))
        do {
            try await steps()
        } catch {
            record("probeAborted", false, "\(error)")
        }
        controller.closeForQuit()
        try? await Task.sleep(for: .milliseconds(500))
        let id = StudioDataStoreIdentity.identifier(dataDirectory: model.configuration.webDataIdentityDirectory)
        do {
            try await WKWebsiteDataStore.remove(forIdentifier: id)
        } catch {
            appLog.notice("Studio probe: data store not removed: \(error.localizedDescription, privacy: .public)")
        }
        return finish()
    }

    struct Failure: Error, CustomStringConvertible { var description: String }

    private func steps() async throws {
        let launchToken = HostClient.readLaunchToken(model.configuration.launchTokenFile)
        let home = DashboardURLs.launch(DashboardURLs.home(model.configuration.baseURL), token: launchToken)

        // 1. Sign-in via the launch token, persistent named data store.
        controller.open(home)
        // Since SL-UX-2 the camera opens on Ready to join, not on the home:
        // New → Start a meeting now.
        guard try await waitJS("!!document.querySelector('[data-new=\"now\"]')", timeout: 30) else {
            throw Failure(description: "home never rendered: \(try await bodyText())")
        }
        _ = try await js("document.querySelector('[data-new=\"now\"]').click(); return true;")
        guard try await waitJS("document.querySelector('#preview-video')?.videoWidth > 0", timeout: 30) else {
            throw Failure(description: "green room camera preview never rendered: \(try await bodyText())")
        }
        record("greenRoomCamera", true, "mock camera preview rendering")
        // (No local strong reference to the web view: it must die with the window.)
        let list = try view.backForwardList
        let history = ([list.currentItem] + list.backList + list.forwardList).compactMap { $0?.url }
        let leaked = LaunchTokenHygiene.containsToken(try view.url) || history.contains { LaunchTokenHygiene.containsToken($0) }
        record("launchTokenNotRetained", !leaked, "page URL and \(history.count) history item(s) free of launch=")
        let rooms = try await js("const r = await fetch('/api/v1/rooms'); return r.status;") as? Int
        record("signedInViaLaunchToken", rooms == 200, "GET /api/v1/rooms from the page: HTTP \(rooms.map(String.init) ?? "?")")
        let store = try view.configuration.websiteDataStore
        let expectedID = StudioDataStoreIdentity.identifier(dataDirectory: model.configuration.webDataIdentityDirectory)
        record("persistentNamedDataStore", store.isPersistent && store.identifier == expectedID,
               "persistent=\(store.isPersistent) identifier=\(store.identifier?.uuidString ?? "default")")
        let storage = try await js("""
            const est = await navigator.storage.estimate();
            return JSON.stringify({persisted: await navigator.storage.persisted(), quotaGB: Math.round((est.quota || 0) / 1e9)});
            """) as? String ?? "?"
        record("storagePersistence", true, "\(storage) (persist() is not granted in WKWebView; see docs)", required: false)

        // 2. Devices: enumeration and switching.
        let devices = try await js("""
            const d = await navigator.mediaDevices.enumerateDevices();
            const pick = kind => d.filter(x => x.kind === kind && x.label);
            const out = {video: pick('videoinput').length, audio: pick('audioinput').length, output: pick('audiooutput').length, switched: []};
            for (const [kind, list] of [['video', pick('videoinput')], ['audio', pick('audioinput')]]) {
              const target = list[list.length - 1];
              if (!target) continue;
              const s = await navigator.mediaDevices.getUserMedia({[kind]: {deviceId: {exact: target.deviceId}}});
              const t = s.getTracks()[0];
              out.switched.push(kind + ':' + (t.getSettings().deviceId === target.deviceId));
              s.getTracks().forEach(x => x.stop());
            }
            return JSON.stringify(out);
            """) as? String ?? "{}"
        let dev = try JSONSerialization.jsonObject(with: Data(devices.utf8)) as? [String: Any] ?? [:]
        let switched = dev["switched"] as? [String] ?? []
        record("deviceEnumeration", (dev["video"] as? Int ?? 0) > 0 && (dev["audio"] as? Int ?? 0) > 0, devices)
        record("deviceSwitching", switched.count == 2 && switched.allSatisfy { $0.hasSuffix("true") }, switched.joined(separator: ", "))

        // 3. Join the call.
        _ = try await js("""
            const name = document.querySelector('#name-input');
            name.value = 'Probe Host'; name.dispatchEvent(new Event('input', {bubbles: true}));
            document.querySelector('#join-btn').click(); return true;
            """)
        guard try await waitJS("document.querySelector('#studio-view')?.getClientRects().length > 0", timeout: 30) else {
            throw Failure(description: "studio view never appeared: \(try await bodyText())")
        }
        record("joinCall", true, "host admitted, studio view visible")
        guard try await waitJS("!!window.__slProbe?.session?.hostToken", timeout: 5) else { throw Failure(description: "session not captured") }
        let sessionID = try await js("return window.__slProbe.session.id;") as? String ?? ""
        let hostToken = try await js("return window.__slProbe.session.hostToken;") as? String ?? ""
        let bridged = await waitNative(timeout: 5) { self.controller.inCall }
        record("bridgeReportsCall", bridged, "page bridge → inCall=\(controller.inCall)")
        record("appNapHeldInCall", controller.activity >= .call && controller.holdsActivityAssertion,
               "activity=\(controller.activity) assertion=\(controller.holdsActivityAssertion)")

        // 4. One window, one meeting: Start a Meeting again focuses the call.
        let before = ObjectIdentifier(try view)
        controller.open(home)
        try await Task.sleep(for: .seconds(1))
        let studioWindows = NSApp.windows.filter { $0.title == "p2p.video Studio" && $0.isVisible }.count
        let stillInCall = try await waitJS("document.querySelector('#studio-view')?.getClientRects().length > 0", timeout: 2)
        let same = controller.webView.map(ObjectIdentifier.init) == before
        record("singleWindowPerMeeting", same && studioWindows == 1 && stillInCall,
               "studio windows=\(studioWindows), same web view=\(same), still in call=\(stillInCall)")

        // 5. Page notifications and badge → Notifier and Dock.
        _ = try await js("new Notification('Probe notification', {body: 'from the studio page'}); await navigator.setAppBadge(3); return true;")
        let notified = await waitNative(timeout: 3) { self.controller.pageMessages.contains { if case .notify = $0 { return true } else { return false } } }
        let badged = await waitNative(timeout: 3) { NSApp.dockTile.badgeLabel == "3" }
        record("pageNotificationBridge", notified, "Notification() reached the app (posted via Notifier when the window isn't in front)")
        record("dockBadge", badged, "navigator.setAppBadge(3) → Dock badge \(NSApp.dockTile.badgeLabel ?? "none")")
        _ = try await js("await navigator.clearAppBadge(); return true;")

        // 6. Calendar invite (.ics download) and Google Calendar link (browser).
        let meeting = try await js("""
            const json = (method, body) => ({method, headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body)});
            const room = await (await fetch('/api/v1/rooms', json('POST', {name: 'Probe room'}))).json();
            const r = await fetch('/api/v1/schedule', json('POST', {roomId: room.id, title: 'Probe meeting',
              startsAt: new Date(Date.now() + 864e5).toISOString(), durationMin: 30, timeZone: 'UTC'}));
            if (!r.ok) return 'HTTP ' + r.status + ' ' + await r.text();
            const m = await r.json();
            return JSON.stringify({ics: m.invite.icsPath, google: m.invite.googleUrl});
            """) as? String ?? ""
        if let data = meeting.data(using: .utf8), let m = try? JSONSerialization.jsonObject(with: data) as? [String: String],
           let ics = m["ics"], let google = m["google"] {
            _ = try await js("""
                const a = document.createElement('a'); a.href = ics; a.download = ''; document.body.appendChild(a); a.click(); a.remove(); return true;
                """, ["ics": ics])
            let icsFile = await waitFile { $0.pathExtension == "ics" }
            record("icsDownload", icsFile != nil, icsFile.map { "saved \($0.lastPathComponent)" } ?? "no .ics in the downloads folder")
            try await injectButton(id: "probe-google", html: "<a id='probe-google' target='_blank' rel='noopener noreferrer'>Google</a>", attribute: ("href", google))
            let after = try await syntheticClick("#probe-google")
            let opened = await waitNative(timeout: 5) { self.externalURLs.contains { $0.host == "calendar.google.com" || $0.host == "www.google.com" } }
            record("targetBlankOpensBrowser", opened, "target=_blank → default browser: \(externalURLs.compactMap(\.host)); page still at \(after)")
            try await injectButton(id: "probe-open", html: "<button id='probe-open' onclick=\"window.open('https://calendar.google.com/calendar/render?action=TEMPLATE&text=probe', '_blank')\">open</button>")
            let count = externalURLs.count
            let after2 = try await syntheticClick("#probe-open")
            let windowOpened = await waitNative(timeout: 5) { self.externalURLs.count > count }
            // The studio itself only uses target=_blank links; window.open is
            // informational (WebKit's popup rules for script-run clicks differ).
            record("windowOpenOpensBrowser", windowOpened, "window.open from a click → default browser (no second web view); page still at \(after2)", required: false)
            let inCall = try await waitJS("document.querySelector('#studio-view')?.getClientRects().length > 0", timeout: 2)
            record("linksKeepCall", inCall, "the call is still on screen after opening links")
        } else {
            record("icsDownload", false, "could not schedule a meeting: \(meeting)")
        }

        // 7. Clipboard copy from a click (the user's clipboard is restored).
        let saved = NSPasteboard.general.pasteboardItems?.map { item in
            item.types.reduce(into: [NSPasteboard.PasteboardType: Data]()) { $0[$1] = item.data(forType: $1) }
        } ?? []
        let marker = "studiolink-probe-\(UUID().uuidString.prefix(8))"
        try await injectButton(id: "probe-copy", html: "<button id='probe-copy'>copy</button>")
        _ = try await js("""
            document.querySelector('#probe-copy').onclick = () => navigator.clipboard.writeText(marker)
              .then(() => window.__slProbe.clip = 'ok', e => window.__slProbe.clip = e.name + ': ' + e.message);
            return true;
            """, ["marker": marker])
        try await syntheticClick("#probe-copy")
        _ = try await waitJS("!!window.__slProbe.clip", timeout: 5)
        let clip = try await js("return window.__slProbe.clip || 'no result';") as? String ?? ""
        let pasted = NSPasteboard.general.string(forType: .string)
        record("clipboardWrite", pasted == marker, "navigator.clipboard.writeText → \(clip); pasteboard \(pasted == marker ? "matches" : "differs")")
        NSPasteboard.general.clearContents()
        if !saved.isEmpty {
            NSPasteboard.general.writeObjects(saved.map { types in
                let item = NSPasteboardItem()
                for (type, data) in types { item.setData(data, forType: type) }
                return item
            })
        }

        // 8. Screen share (getDisplayMedia from a click; mock display device).
        try await injectButton(id: "probe-share", html: "<button id='probe-share'>share</button>")
        _ = try await js("""
            document.querySelector('#probe-share').onclick = () => navigator.mediaDevices.getDisplayMedia({video: true})
              .then(s => { const t = s.getVideoTracks()[0]; const st = t.getSettings();
                           window.__slProbe.share = 'ok ' + (t.label || 'track') + ' ' + st.width + 'x' + st.height; s.getTracks().forEach(x => x.stop()); },
                    e => window.__slProbe.share = e.name + ': ' + e.message);
            return true;
            """)
        try await syntheticClick("#probe-share")
        _ = try await waitJS("!!window.__slProbe.share", timeout: 15)
        let share = try await js("return window.__slProbe.share || 'no answer within 15 s';") as? String ?? ""
        record("getDisplayMedia", share.hasPrefix("ok"), share, required: false)

        // 9. Caption transcript style download (a page-made blob).
        _ = try await js("""
            const a = document.createElement('a');
            a.href = URL.createObjectURL(new Blob(['WEBVTT\\n\\n00:00.000 --> 00:01.000\\nprobe\\n'], {type: 'text/vtt'}));
            a.download = 'captions-probe.vtt'; document.body.appendChild(a); a.click(); a.remove(); return true;
            """)
        let vtt = await waitFile { $0.lastPathComponent == "captions-probe.vtt" }
        record("blobDownload", vtt != nil, vtt.map { "saved \($0.lastPathComponent)" } ?? "captions-probe.vtt not saved")

        // 10. Take 1: record, stop, verify server-side.
        let take1 = try await recordTake(sessionID: sessionID, hostToken: hostToken, label: "take1")
        try await Task.sleep(for: .seconds(4))
        _ = try await js("document.querySelector('#rec-btn').click(); return true;")
        let complete1 = try await waitTakeAssembled(take1, hostToken: hostToken, timeout: 60)
        record("take1UploadedAndAssembled", complete1.ok, complete1.detail)
        let desktop = await waitNative(timeout: 8) {
            self.model.hostState?.takes.first { $0.id == take1 }.map { $0.uploadsComplete && $0.missingParticipants == 0 } ?? false
        }
        record("desktopStateTakeComplete", desktop, "GET /api/v1/desktop/state reports the take's uploads complete")

        // 11. Originals ZIP (Content-Disposition: attachment from the daemon),
        // once the host reports it ready (as the Transfers panel does).
        let readyDeadline = Date().addingTimeInterval(30)
        var originalsStatus = ""
        while Date() < readyDeadline {
            originalsStatus = try await js("""
                const r = await fetch(`/api/v1/takes/${encodeURIComponent(take)}/originals?token=${encodeURIComponent(token)}`);
                return r.ok ? 'ready' : 'HTTP ' + r.status + ' ' + (await r.text()).slice(0, 120);
                """, ["take": take1, "token": hostToken]) as? String ?? ""
            if originalsStatus == "ready" { break }
            try await Task.sleep(for: .seconds(1))
        }
        _ = try await js("""
            const a = document.createElement('a'); a.download = '';
            a.href = `/api/v1/takes/${encodeURIComponent(take)}/originals?token=${encodeURIComponent(token)}`;
            document.body.appendChild(a); a.click(); a.remove(); return true;
            """, ["take": take1, "token": hostToken])
        let zip = await waitFile(timeout: 30) { $0.pathExtension == "zip" }
        let listing = (try? FileManager.default.contentsOfDirectory(atPath: downloads.path)) ?? []
        let zipSize = zip.flatMap { try? FileManager.default.attributesOfItem(atPath: $0.path)[.size] as? Int } ?? 0
        record("originalsZipDownload", zip != nil && zipSize > 1000, zip.map { "saved \($0.lastPathComponent) (\(zipSize) bytes)" } ?? "no ZIP saved; downloads: \(listing) failures: \(controller.failedDownloads); fetch: \(originalsStatus)")

        // 12. Take 2: close the window mid-take with an unsent tail, reopen.
        let take2 = try await recordTake(sessionID: sessionID, hostToken: hostToken, label: "take2")
        guard try await waitJournal(minSeq: 1, timeout: 60) != nil else { throw Failure(description: "take 2 never journaled") }
        _ = try await js("window.__slProbe.dropChunks = true; return true;")
        let tail = try await waitJournal(minSeq: 4, timeout: 90)
        guard let tail, let trackID = tail["trackId"] as? String, let uploadToken = tail["uploadToken"] as? String,
              let lastSeq = tail["lastSeq"] as? Int else { throw Failure(description: "take 2 journal never grew") }
        let have = try await js("""
            const r = await fetch(`/api/v1/takes/${take}/tracks/${track}/status`, {headers: {Authorization: `Bearer ${token}`}});
            const s = await r.json(); return (s.have || []).reduce((n, [a, b]) => n + b - a + 1, 0);
            """, ["take": take2, "track": trackID, "token": uploadToken]) as? Int ?? -1
        record("unsentTailInOPFS", have >= 0 && have <= lastSeq, "OPFS journal has \(lastSeq + 1) fragments, host has \(have)")
        controller.performClose()
        try await Task.sleep(for: .milliseconds(500))
        let asked: Bool
        if case .confirm = controller.lastCloseDecision { asked = true } else { asked = false }
        record("closeConfirmMidTake", asked && !controller.isOpen, "close asked first: \(asked); window closed after confirming: \(!controller.isOpen)")
        // The desktop state is polled every 2 s; allow one poll after the close.
        let warned = await waitNative(timeout: 5) { StudioLifecycle.quitMessage(state: self.model.hostState, window: self.controller.windowState) != nil }
        record("quitWarnsDuringTake", warned, StudioLifecycle.quitMessage(state: model.hostState, window: controller.windowState) ?? "no warning")
        let held = await waitNative(timeout: 5) { self.controller.activity == .capture && self.controller.holdsActivityAssertion }
        record("sleepPreventedDuringTake", held, "after close, take still recording: activity=\(controller.activity)")
        try await Task.sleep(for: .seconds(2))

        controller.open(home)
        let resumed = try await waitJS(#"/1 local tracks received by host\. 0 remain incomplete|1 saved on the host\. 0 still to upload/.test(document.querySelector('#opfs-recovery-banner')?.textContent || '')"#, timeout: 90)
        let banner = try await js("return document.querySelector('#opfs-recovery-banner')?.innerText || 'no banner';") as? String ?? ""
        record("uploadResumedAfterReopen", resumed, banner.replacingOccurrences(of: "\n", with: " ").prefix(240).description)

        // Rejoin the meeting (the Recent meetings "Rejoin" link) and stop the take.
        var rejoin = URLComponents(url: model.configuration.baseURL, resolvingAgainstBaseURL: false)!
        rejoin.path = "/"
        rejoin.queryItems = [URLQueryItem(name: "session", value: sessionID), URLQueryItem(name: "token", value: hostToken)]
        controller.open(rejoin.url!)
        guard try await waitJS("location.search.includes('session=') && document.querySelector('#preview-video')?.videoWidth > 0", timeout: 30) else {
            throw Failure(description: "rejoin green room never rendered")
        }
        _ = try await js("document.querySelector('#join-btn').click(); return true;")
        let rejoined = try await waitJS("""
            document.querySelector('#studio-view')?.getClientRects().length > 0 && document.querySelector('#rec-btn')?.textContent === 'Stop recording'
            && !document.querySelector('#rec-btn').disabled
            """, timeout: 30)
        let recState = try await js("const b = document.querySelector('#rec-btn'); return `button '${b?.textContent}' disabled=${b?.disabled} indicator=${document.querySelector('#rec-indicator')?.style.display}`;") as? String ?? ""
        record("rejoinSeesRunningTake", rejoined, "rejoined host page shows the running take with Stop: \(recState)")
        _ = try await js("document.querySelector('#rec-btn').click(); return true;")
        let complete2 = try await waitTakeAssembled(take2, hostToken: hostToken, timeout: 90)
        record("take2CompletesAfterReopen", complete2.ok, complete2.detail)
    }

    // MARK: take helpers

    private func recordTake(sessionID: String, hostToken: String, label: String) async throws -> String {
        _ = try await js("document.querySelector('#rec-btn').click(); return true;")
        let started = try await waitJS("""
            (() => { const c = document.querySelector('#preflight-confirm-record'); if (c) c.click();
                     return document.querySelector('#rec-indicator')?.style.display === 'inline-flex'; })()
            """, timeout: 30)
        guard started else { throw Failure(description: "\(label): recording never started: \(try await bodyText())") }
        let take = try await js("""
            const r = await fetch(`/api/v1/sessions/${encodeURIComponent(sid)}`, {headers: {Authorization: `Bearer ${token}`}});
            const d = await r.json(); const t = d.takes.find(t => t.status === 'recording') || d.takes[d.takes.length - 1]; return t ? t.id : '';
            """, ["sid": sessionID, "token": hostToken]) as? String ?? ""
        let hostSees = await waitNative(timeout: 8) { self.model.hostState?.recording == true }
        record("\(label)Recording", !take.isEmpty && hostSees, "take recording; desktop state recording=\(hostSees)")
        if label == "take1" {
            let decision = StudioLifecycle.closeDecision(state: model.hostState, window: controller.windowState)
            if case .confirm(let message) = decision { record("closeAsksDuringTake", true, message) } else { record("closeAsksDuringTake", false, "would close silently") }
            // The controller follows host state through a main-run-loop sink, one
            // turn after `model.hostState` changes: wait for it rather than race it.
            let held = await waitNative(timeout: 5) { self.controller.activity == .capture && self.controller.holdsActivityAssertion }
            record("captureActivityDuringTake", held,
                   "activity=\(controller.activity) (userInitiated + idleSystemSleepDisabled)")
        }
        return take
    }

    private func waitTakeAssembled(_ take: String, hostToken: String, timeout: Double) async throws -> (ok: Bool, detail: String) {
        let deadline = Date().addingTimeInterval(timeout)
        var last = ""
        while Date() < deadline {
            last = try await js("""
                const r = await fetch(`/api/v1/takes/${encodeURIComponent(take)}`, {headers: {Authorization: `Bearer ${token}`}});
                const s = await r.json();
                return JSON.stringify({status: s.take?.status, tracks: (s.tracks || []).map(t => [t.status, t.totalBytes]), missing: (s.missingParticipants || []).length});
                """, ["take": take, "token": hostToken]) as? String ?? ""
            if let d = try? JSONSerialization.jsonObject(with: Data(last.utf8)) as? [String: Any],
               let status = d["status"] as? String, status != "recording",
               let tracks = d["tracks"] as? [[Any]], !tracks.isEmpty,
               tracks.allSatisfy({ ($0.first as? String) == "assembled" && (($0.last as? Int) ?? 0) > 1000 }),
               (d["missing"] as? Int) == 0 {
                return (true, last)
            }
            try await Task.sleep(for: .milliseconds(500))
        }
        return (false, last)
    }

    /// The open (unfinished) track journal in OPFS once it has `minSeq` fragments.
    private func waitJournal(minSeq: Int, timeout: Double) async throws -> [String: Any]? {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if let json = try await js("""
                const root = await navigator.storage.getDirectory(), takes = await root.getDirectoryHandle('takes', {create: true});
                for await (const [, take] of takes.entries()) {
                  const tracks = await take.getDirectoryHandle('tracks');
                  for await (const [, track] of tracks.entries()) {
                    const meta = JSON.parse(await (await (await track.getFileHandle('meta.json')).getFile()).text());
                    if (!meta.isComplete) return JSON.stringify({takeId: meta.takeId, trackId: meta.trackId, uploadToken: meta.uploadToken, lastSeq: meta.lastSeq});
                  }
                }
                return '';
                """) as? String, !json.isEmpty,
               let meta = try JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any],
               (meta["lastSeq"] as? Int ?? -1) >= minSeq {
                return meta
            }
            try await Task.sleep(for: .milliseconds(500))
        }
        return nil
    }

    // MARK: web view helpers

    private var view: WKWebView {
        get throws {
            guard let v = controller.webView else { throw Failure(description: "studio web view is gone") }
            return v
        }
    }

    private func js(_ body: String, _ arguments: [String: Any] = [:]) async throws -> Any? {
        try await view.callAsyncJavaScript(body, arguments: arguments, in: nil, contentWorld: .page)
    }

    private func waitJS(_ condition: String, timeout: Double) async throws -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if let ok = try? await js("return !!(\(condition));") as? Bool, ok { return true }
            try await Task.sleep(for: .milliseconds(250))
        }
        return false
    }

    private func waitNative(timeout: Double, _ condition: @escaping @MainActor () -> Bool) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return true }
            try? await Task.sleep(for: .milliseconds(200))
        }
        return condition()
    }

    private func waitFile(timeout: Double = 15, _ match: @escaping (URL) -> Bool) async -> URL? {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            let files = (try? FileManager.default.contentsOfDirectory(at: downloads, includingPropertiesForKeys: nil)) ?? []
            if let hit = files.first(where: match), controller.finishedDownloads.contains(where: { $0.lastPathComponent == hit.lastPathComponent }) {
                return hit
            }
            try? await Task.sleep(for: .milliseconds(250))
        }
        return nil
    }

    private func bodyText() async throws -> String {
        String((try await js("return document.body?.innerText || '';") as? String ?? "").prefix(300)).replacingOccurrences(of: "\n", with: " ")
    }

    /// Adds a fixed, clickable element at the top-left of the page.
    private func injectButton(id: String, html: String, attribute: (String, String)? = nil) async throws {
        _ = try await js("""
            document.getElementById(id)?.remove();
            const holder = document.createElement('div'); holder.innerHTML = html;
            const el = holder.firstElementChild;
            if (attr) el.setAttribute(attr[0], attr[1]);
            Object.assign(el.style, {position: 'fixed', left: '8px', top: '8px', zIndex: 2147483647, width: '80px', height: '28px', display: 'block', background: '#fff'});
            document.body.appendChild(el); return true;
            """, ["id": id, "html": html, "attr": attribute.map { [$0.0, $0.1] } as Any? ?? NSNull()])
    }

    /// Clicks an element from `evaluateJavaScript`, which WebKit runs with a
    /// user gesture (transient activation), so clipboard, window.open and
    /// getDisplayMedia behave as for a person's click. Never takes focus.
    @discardableResult
    private func syntheticClick(_ selector: String) async throws -> String {
        let source = "(() => { const el = document.querySelector(" + Self.jsString(selector) + "); if (!el) return 'missing'; el.click(); return 'clicked'; })()"
        let result = try await view.evaluateJavaScript(source) as? String ?? "?"
        guard result == "clicked" else { throw Failure(description: "\(selector): \(result)") }
        try await Task.sleep(for: .milliseconds(300))
        return (try? await js("return location.pathname + location.search.replace(/token=[^&]*/, 'token=…');") as? String) ?? "?"
    }

    private static func jsString(_ s: String) -> String {
        let data = try? JSONSerialization.data(withJSONObject: [s])
        let array = data.flatMap { String(data: $0, encoding: .utf8) } ?? "[\"\"]"
        return String(array.dropFirst().dropLast())
    }

    // MARK: report

    private func finish() -> Bool {
        let missing = checks.filter { $0.required && !$0.ok }.map(\.name)
        let passed = missing.isEmpty && !checks.isEmpty
        var doc: [String: Any] = [
            "passed": passed, "missingRequired": missing,
            "seconds": Int(Date().timeIntervalSince(started)),
            "macOS": ProcessInfo.processInfo.operatingSystemVersionString,
            "mockCaptureDevices": true,
            "note": "WebKit mock capture devices bypass macOS camera/microphone/screen permission (TCC); real-device capture is an owner test.",
        ]
        if let data = try? JSONEncoder().encode(checks), let obj = try? JSONSerialization.jsonObject(with: data) { doc["checks"] = obj }
        if let data = try? JSONSerialization.data(withJSONObject: doc, options: [.prettyPrinted, .sortedKeys]) {
            try? FileManager.default.createDirectory(at: output.deletingLastPathComponent(), withIntermediateDirectories: true)
            try? data.write(to: output, options: .atomic)
        }
        try? FileManager.default.removeItem(at: downloads)
        return passed
    }
}
