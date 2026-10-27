# p2p.video desktop host app (SL-P2-1)

`p2p.video.app` is a native Swift app that runs the host for you. It starts the
bundled `p2pvideod`, keeps it healthy, shows what is happening from the menu bar,
and walks you through Tailscale setup. The call can run in a p2p.video studio window
(WKWebView, SL-P2-3) or in your browser; the browser is still the default until the
real-device checks below are done on a real Mac (see "In-app studio window").

Source is in `macapp/`, a SwiftPM package for macOS 14+ on Apple Silicon. There is
no Xcode project.

```
macapp/
  Package.swift
  Sources/StudioLinkCore/   pure logic, unit-tested (no UI)
    HostConfiguration       ports, data dir, bundle paths, env overrides
    DaemonArguments         p2pvideod argv per guest-access mode
    Health                  /health → expected | otherBuild | foreign | unreachable
    RestartPolicy           crash backoff, no restart while recording/live/uploading
    TailscaleCommands       argv builders for every tailscale command + CLI locator
    TailscaleStatus         status --json, serve/funnel status --json, route observation
    HostState               GET /api/v1/desktop/state model + interruption warnings
    NotificationPlanner     state diff → guest-waiting / take uploaded / exported
    LogRotation, CommandRunner (Process with an argv array, timeout), HostClient
    Onboarding              setup-guide state machine + guest-choice availability (SL-P2-2)
    Browser                 Chrome detection, dashboard-browser preference → open target
    PodcastChecklist        disk estimate, IOKit power parsing, guest-link HEAD self-check, rows
    QuitWarning             quit/restart warning (recording, live, uploads, calls, waiting)
    UpdateConfiguration     Sparkle on/off from the bundle's SUFeedURL + SUPublicEDKey
    StudioWindowPolicy      studio window: origin + media permission, navigation/download routing,
                            download naming, data-store identity, launch-token hygiene,
                            close/quit/App Nap/open decision tables, page-bridge messages (SL-P2-3)
  Sources/StudioLinkArtwork/ the app icon, drawn with CoreGraphics (shared by app + studiolink-icon)
  Sources/StudioLink/       SwiftUI/AppKit shell (internal target name kept)
    StudioLinkApp           @main: control-center, Setup and Studio Windows, MenuBarExtra, Settings
    DaemonSupervisor        start/adopt/watch/restart/stop the daemon
    TailscaleSetup          read-only refresh + confirmed plans + verification
    AppModel                actions: start meeting, copy invite, create room, browser, login item
    OnboardingView          first-run setup guide
    ReadinessMonitor        disk/power/guest-link measurements for the checklist (30 s)
    AppUpdater              Sparkle 2 wrapper (compiled only in STUDIOLINK_SPARKLE=1 builds)
    StudioWindowController  the in-app studio window (one window, one meeting; SL-P2-3)
    StudioWebView           WKWebView factory, UI/navigation/download delegate, page bridge script
    StudioProbe             end-to-end studio probe (STUDIOLINK_STUDIO_PROBE, make macapp-probe)
    WebViewProbe/ProbePage  WKWebView capability spike (automated)
  Sources/studiolink-icon/  writes AppIcon.iconset (package-mac.sh → iconutil → AppIcon.icns)
  Tests/StudioLinkCoreTests/ + Fixtures/
```

## Build and test

```sh
cd macapp
swift build                       # debug
swift test                        # 111 unit tests
swift build -c release --arch arm64
```

`scripts/package-mac.sh` builds the web assets, the daemon and the Swift app. It puts
the app at `Contents/MacOS/p2p.video`, the daemon and LGPL ffmpeg/ffprobe in
`Contents/Resources/bin`, and the old zsh launcher in `Contents/Resources/launch-mac.sh`
as a headless fallback (`zsh p2p.video.app/Contents/Resources/launch-mac.sh`).
Signing and notarization are unchanged. Ad-hoc signing (`-`) is the default, and
`DEVELOPER_ID_APPLICATION` plus `NOTARYTOOL_PROFILE` produce a release build. The
existing `p2p.video.entitlements` already covers what the app needs: camera,
audio-input, network client/server and no sandbox. The Info.plist adds
`NSAppTransportSecurity/NSAllowsLocalNetworking` for loopback HTTP and updates the
camera and microphone usage strings.

```sh
STUDIOLINK_MEDIA_TOOLS=/path/to/build/media-tools scripts/package-mac.sh
```

The bundle is `p2p.video.app`, bundle identifier `video.p2p.app`, version 1.0.0
(`CFBundleVersion` is `BUILD_NUMBER` in the script; bump it for every release). Outputs:
`build/p2p.video-1.0.0-arm64.dmg`, `build/p2p.video-1.0.0-arm64.zip`, `build/SHA256SUMS.txt`.
`LICENSE.txt` (AGPL-3.0) and `THIRD_PARTY_NOTICES.md` are copied into `Contents/Resources`.

## Migrating from StudioLink [SL-REL-1]

The app was called StudioLink (`com.omniharmonic.studiolink`) before the rename. Because
the bundle identifier changed, the first launch of p2p.video moves the old data once
(`LegacyMigration`, unit-tested with temporary folders; `LegacyMigrator` gathers facts):

- `~/Library/Application Support/StudioLink` is moved to
  `~/Library/Application Support/p2p.video` with one `renamex_np(RENAME_EXCL)` call, so it
  is moved whole or not at all. Nothing is copied, and a move across volumes is refused.
- Only when the default data folder is in use (no `STUDIOLINK_DATA_DIR`/`P2PVIDEO_DATA_DIR`),
  the old folder exists and the new one does not.
- Never while StudioLink runs: if the StudioLink app (by bundle ID) or a `studiolinkd`
  process is running, p2p.video does not start the host and says to quit StudioLink first.
- A marker, `.migrated-from-studiolink.json` (0600), records the old and new paths, the time
  and both bundle IDs. The studio window keeps deriving its WebKit store identity from the
  old path recorded there, so pre-rename OPFS upload journals are still found, and
  `~/Library/WebKit/com.omniharmonic.studiolink` is moved to `~/Library/WebKit/video.p2p.app`
  if the latter does not exist yet.
- Preferences from the old defaults domain are copied once into `video.p2p.app` (new values
  win; key `P2PVideoMigratedDefaultsFromStudioLink` marks it done).
- The daemon's `/health` product stays `com.omniharmonic.studiolink` (an internal protocol
  identifier), so an old StudioLink host still on port 7443 is reported as "Another p2p.video
  or StudioLink host (build …)" and is never stopped.

The zsh launcher never moves data; it keeps using the StudioLink folder until the app has
migrated it. Environment variables accept a `P2PVIDEO_` prefix as an alias of `STUDIOLINK_`.

## Daemon supervision

The app keeps the zsh launcher's safety rules:

1. Before starting anything, it probes `http://127.0.0.1:<port>/health`.
   - If the response is exactly product `com.omniharmonic.studiolink` (kept from StudioLink), this bundle's
     `build-id.txt` and `status: ok`, the app **reuses** that daemon ("Ready for meetings (already running)").
   - If any other HTTP answer comes back (a different p2p.video or StudioLink build, another product,
     an error page), the app is **blocked**. It shows the actionable message ("Finish any
     active session, stop that host yourself, then reopen this app. No running service
     was stopped.") and never sends a signal.
   - If nothing answers, the port is free and the app starts its own daemon.
2. The app starts `Contents/Resources/bin/p2pvideod` as a `Process` with an argv array.
   It passes `--port`, `--data-dir`, `--turn-listen`, `--udp-port` (only when overridden)
   and the guest-access flags below. The bundled tools directory is first on `PATH`.
   The app then waits up to 10 s for this exact build to answer.
3. stdout and stderr are appended to `<data-dir>/daemon.log` through a file descriptor the
   daemon owns. If the app crashes, the daemon keeps running and logging, and the next
   launch adopts it. Before each start the log is rotated if it exceeds 10 MB
   (`daemon.log.1` … `.3`).
4. **Restart policy** (`RestartPolicy`, unit-tested):
   - If the daemon exits unexpectedly, it is restarted after 1, 2, 4, 8, 16 s (capped at
     30 s). After more than 5 crashes in 2 minutes the app gives up and points to
     `daemon.log`. A process that has exited is no longer recording, so a crash restart
     is always allowed. It lets participants' journaled uploads resume.
   - If the daemon is alive but `/health` fails 3 times in a row, it is killed and restarted
     only if the last known state was idle. If a take was recording, a stream was live,
     or the state is unknown, the app **does not restart it** and shows a warning instead.
   - Restarts for new settings (guest-access mode, public STUN) wait until no take is
     recording, no stream is live and no uploads are pending.
5. **Quit** stops a daemon the app started (SIGTERM, then SIGKILL after 10 s). It also stops
   an adopted daemon, but only one whose `/health` and token-protected state both report
   this exact build. That second check gives the PID. If a take is recording, a stream
   is live, uploads are pending, a call is in progress or a guest is waiting, Quit (and
   Restart Host) asks first (`QuitWarning`). SIGTERM sent to the app takes the same
   path without the dialog.

### Configuration overrides (testing and development)

| Variable | Default | Purpose |
|---|---|---|
| `STUDIOLINK_PORT` | 7443 | HTTP port |
| `STUDIOLINK_DATA_DIR` | `~/Library/Application Support/p2p.video` | data, settings, logs |
| `STUDIOLINK_UDP_PORT` | daemon default (50000) | SFU UDP mux port |
| `STUDIOLINK_TURN_LISTEN` | `127.0.0.1:3478` | embedded TURN (loopback only) |
| `STUDIOLINK_NO_OPEN` | unset | `1` = don't open the dashboard at launch |
| `STUDIOLINK_DAEMON`, `STUDIOLINK_BUILD_ID` | bundle | run against a dev daemon |
| `STUDIOLINK_WEBVIEW_PROBE` | unset | run the WKWebView probe, write JSON, quit |
| `STUDIOLINK_STUDIO_PROBE` | unset | run the end-to-end studio probe, write JSON, exit 0/1 |
| `STUDIOLINK_SNAPSHOT_DIR` | unset | render windows to PNG (`kill -USR1` re-captures to `live/`); outlined pills look clipped at their ends in these `cacheDisplay` renders (a renderer artifact), so the approval card and confirm sheet are also written as `*-swiftui.png` via `ImageRenderer` |

To pass them, use `open -n --env VAR=value p2p.video.app`. With `open`, macOS attributes
camera and microphone permission to p2p.video itself. Launching the binary from a
terminal attributes it to the terminal. Settings live in `<data-dir>/desktop.json`
(mode 0600), so a temporary data directory isolates a test run completely.

## Host state endpoint (Go)

The app polls `GET /api/v1/desktop/state` every 2 s. Polling is simpler than a host
WebSocket, which would need a per-session host token that the app doesn't have for
sessions created in the browser. The endpoint:

- is served only for a **direct loopback** request: loopback peer, loopback `Host`, no
  `Origin`, no `X-Forwarded-*`, no `Tailscale-*` Serve/Funnel headers. Funnel and Serve
  traffic gets 403 even with the token, and the Funnel guardrail test covers this.
- requires `Authorization: Bearer <data-dir/.desktop_token>`. The daemon creates that
  0600 file next to `.secret_key`, the comparison is constant-time, and the endpoint is
  disabled (503) if the file can't be created.
- returns product, build, pid, recording, live and pending-upload state, waiting guests
  (participant ID and name), live sessions and the last 10 takes with upload/export
  progress. It **never** returns session, invite, host or TURN credentials, and a test
  asserts this. The fixture
  `macapp/Tests/StudioLinkCoreTests/Fixtures/desktop-state.json` is decoded by both the
  Swift tests and a Go test (`DisallowUnknownFields`) so the contract can't drift.

## Menu bar and control center

The menu bar extra says what is happening in words ("Ready for meetings", "Recording",
"2 waiting to join", "Uploading 3 recording files") and how guests join ("Guests can join
from anywhere", "Guests on your Tailscale network can join", "Only this Mac can join"). It
offers **Start a Meeting** (⌘N), **Open p2p.video** (⌘O, the control center window),
**Upcoming meetings** (the next five calendar/scheduled meetings, inline), **Rooms** (one
entry per room: Start Meeting/Rejoin, Copy Room Link), **Open Home in Browser**,
**Settings…** and **Quit**. While Tailscale waits for an approval, **Tailscale Needs Your
Approval…** opens Settings. The icon changes while recording, while live, and while a
guest is waiting.

**Copy and look [SL-UX-2].** The app follows `docs/design/UX_REDESIGN.md`: plain words by
what the user is doing (Meeting, Recording, Room link, "Let guests join from anywhere"),
technical detail (ports, commands, build, data folder, daemon flags) behind **Show
details** disclosures, never removed. It follows the system appearance; the teal accent
(`#0F7B76` light, `#5FCFC6` dark, `Theme.accent`) tints every window, primary actions are
pill buttons (`PillButtonStyle`), icons are SF Symbols. The control center header reads
"Ready for meetings" + guest status, the cards are Now, Guests, Before you record and
Recent recordings, and its menu has Restart p2p.video / Show Log / Show Data Folder.

**Start a Meeting** opens the call-first home (SL-P1-5): device check, then New meeting or
one of the rooms. **Rooms** lists room names from `/api/v1/desktop/state` (names and IDs
only). Start Meeting goes to `/?room=<id>` with the room preselected; **Copy Room Link** reads
the room's reusable link from the loopback rooms API at click time and puts it on the
clipboard only; it is never stored or logged. The control center's **Copy Room Link**
menu does the same.

### Signed-in dashboard (launch token, SL-SEC-1)

Loopback is not a credential: any local process or OS user can reach
`127.0.0.1:7443`. Management routes (create meeting, rooms, home, doctor,
Tailscale status) therefore need an admin credential (see `docs/SECURITY.md`):

- **Pages the app opens** (Open Dashboard, Start a Meeting, room shortcuts, the
  in-app studio window, the launch-time dashboard) go through
  `DashboardURLs.launch(_:token:)`, which appends `launch=<token>` read fresh from
  `<data-dir>/.launch_token` (0600, rewritten at every daemon start). The daemon sets
  an HttpOnly, SameSite=Strict admin cookie for that browser or web view and
  redirects to the same URL without the token.
- **The app's own API calls** (`HostClient.createSession`, `roomLink`) send
  `Authorization: Bearer <desktop token>`, like `/api/v1/desktop/state`.
- `scripts/launch-mac.sh` opens the same signed-in URL; `p2pvideo dashboard [--open]
  [--data-dir D] [--port N]` prints or opens it for development (`make dev` opens it
  automatically). A browser tab opened without the token is told to open the dashboard
  from the app. A daemon restart signs browsers out (new token, new cookie secret).

## Guided Tailscale setup (Settings → Guest Access)

Read-only checks run automatically, through the CLI from `PATH`, `/usr/local/bin`,
`/opt/homebrew/bin` or `/Applications/Tailscale.app/Contents/MacOS/Tailscale` with
`TAILSCALE_BE_CLI=1`:

- `tailscale status --json`: installed, signed in, MagicDNS, HTTPS certificates, Funnel
  node attribute and ports
- `tailscale serve status --json`: the current route, and conflicts on :443/:8443

The pane leads with the current state in words and two options, **Guests from anywhere**
and **Guests on your Tailscale network**, each with Turn On… / Turn Off…. Changes happen
only after the user picks one, reads a sheet that explains it in one plain paragraph, and
clicks **Turn On** / **Turn Off**. The sheet's **Show details** lists the technical
description and the exact commands (and, after a run, Tailscale's output):

| Action (sheet title) | Commands | Daemon flags afterwards |
|---|---|---|
| Let guests on your Tailscale network join | `tailscale serve --bg --https=443 http://127.0.0.1:<port>` | `--no-funnel --public-url https://<host>.<tailnet>.ts.net` |
| Let guests join from anywhere | `tailscale funnel --bg --https=443 http://127.0.0.1:<port>`; `tailscale serve --bg --tls-terminated-tcp=8443 tcp://127.0.0.1:<turn>`; `tailscale funnel --bg --tls-terminated-tcp=8443 tcp://127.0.0.1:<turn>` | `--public-url https://… --funnel-turn` |
| Stop letting guests join from anywhere | `tailscale serve --bg --https=443 http://127.0.0.1:<port>`; `tailscale serve --tls-terminated-tcp=8443 off` | back to private |
| Turn off guest links | `tailscale serve --https=443 off` | `--no-funnel` |

**Approval links (SL-UX-2).** When Funnel (or Serve/HTTPS) isn't enabled for the tailnet,
`tailscale funnel` prints `Funnel is not enabled on your tailnet. To enable, visit:
https://login.tailscale.com/f/funnel?node=<id>` and then *waits*. The app used to wait
45 s and report a timeout. Now `ProcessCommandRunner.run(…, stopWhen:)` streams stdout and
stderr while the command runs; `TailscaleApproval.find(in:)` recognises the enable link
(any `login.tailscale.com` page except `/kb`, or any HTTPS link right after "To enable,
visit:"; Funnel vs Serve from the path or surrounding text), and `TailscalePlanRunner`
stops the waiting CLI (it has changed nothing yet) and skips the remaining commands. The
sheet then shows **Allow public links in Tailscale** (or *Allow secure links* for Serve)
with one sentence on what approving does, **Open Tailscale** (opens the link in the
default browser) and **Try Again** (re-runs the same confirmed plan). If the sheet is
closed first, the same step stays in Settings → Guest Access and the setup guide
(`TailscaleSetup.pendingApproval`), the control center's Guests card says Tailscale is
waiting, and the menu bar offers it. The link is shown in the UI only; logs record the
commands and "approval needed (funnel)", never the output or the link. Tests use a
scripted `CommandRunning` fake and a `/bin/sh` stand-in; no real `tailscale` mutation runs.

The setup stops at the first failing command, then re-reads `serve status` and checks
the result. If anything of p2p.video's is still public after turning the public link
off, it says so. If :443 or :8443 already route somewhere else, the sheet warns that the
route will be replaced. Nothing ever runs `tailscale serve reset` or `funnel reset`. The
chosen mode and HTTPS origin are saved in `desktop.json`. If the new mode needs different
daemon flags, the daemon restarts under the restart policy, so not while recording.
**Guardrail:** the dashboard and admin endpoints stay unreachable through Funnel. That
is enforced by the daemon, not the app.

**Not verified here:** none of the mutating commands were run during development. The
Funnel CLI semantics (`serve` on a funneled port clearing its Funnel flag, `off` removing
the :8443 forwarder) follow Tailscale 1.102 behavior and the post-run verification. A
real run on a tailnet with the Funnel attribute is still open.

## Live captions provider (SL-P4-2)

With **Settings › Live captions › Recognize live captions on this Mac** on (the default),
the app connects to the daemon as its caption provider: `ws://127.0.0.1:<port>/api/v1/captions/provider`
with the desktop token. It reconnects with backoff across daemon restarts. When a host turns
captions on in a meeting, the app recognizes each participant's audio with Apple's
on-device `SpeechAnalyzer`/`SpeechTranscriber` (macOS 26+, falling back to on-device
`SFSpeechRecognizer`) and returns partial/final results. Protocol logic lives in
`StudioLinkCore` (`CaptionProtocol`, `CaptionStreamRouter`, `CaptionProviderClient`) and
Apple Speech in the `StudioLinkSpeech` target. `studiolink-captions` is a CLI harness
(`transcribe <file>` / `provider`). Spec: `docs/integrations/CAPTIONS.md`.

## Transcription provider (SL-P4-3)

With **Settings › Transcripts › Transcribe recordings on this Mac** on (the default), the app
also connects as the daemon's *transcription provider* (`/api/v1/transcription/provider`,
desktop token, separate from captions). When the host presses Transcribe on an exported take,
the daemon sends one isolated WAV path at a time; the app reads it with `AVAudioFile` and
`SpeechAnalyzer`/`SpeechTranscriber` and returns word-timed segments, progress and done/error
(`TranscriptionProviderClient`, `TranscriptionJobRunner`, `AppleFileTranscriber`). The pre-macOS
26 fallback needs Speech Recognition; Settings then shows **Allow Speech Recognition…** (the
bundle declares `NSSpeechRecognitionUsageDescription`). Harness: `studiolink-captions
transcribe-file <wav>` / `file-provider`. Spec: `docs/integrations/TRANSCRIPTS.md`.

## Notifications and login item

- **Guest waiting** is posted when a new participant ID appears in a session's waiting
  list. **Take uploaded** is posted when every expected participant's tracks are
  assembled. **Take exported** is posted when post-processing marks the take complete.
  The first poll after launch only records the current state, so old events are never
  replayed. Each event can be turned off in Settings → General.
- **Open p2p.video at login** uses `SMAppService.mainApp`. Notifications and the login
  item need the app bundle and are skipped for a bare `swift run` binary.
- **Meeting reminders** (SL-P6-1): N minutes before a scheduled meeting or a
  calendar event holding one of this host's room links, with a **Start meeting**
  action that opens the room. Notifications now also show while the app is
  frontmost (`willPresent` returns banner + sound).

## Calendar (SL-P6-1, ADR-018)

Settings → Calendar, everything off by default. "Show upcoming meetings and
remind me" reads the calendars already in macOS through EventKit (Google,
iCloud and Exchange accounts added in System Settings) and finds events whose
URL, location or notes hold `<this host's origin>/join?room=<slug>`. Matching
data comes from the loopback, desktop-token `GET /api/v1/desktop/calendar`,
which carries no secrets. "Add meetings I schedule in p2p.video to <calendar>"
mirrors scheduled meetings into one writable calendar. Edits update the event,
cancelling removes it, and a 0600 ledger `calendar-sync.json` in the data
directory tracks what the app wrote. Upcoming meetings appear in the menu bar
under **Upcoming**. macOS full calendar access is requested only when a toggle
is turned on. Without it, scheduled meetings from the daemon are still listed
and reminded. The bundle declares `NSCalendarsFullAccessUsageDescription` and
the `com.apple.security.personal-information.calendars` entitlement. Pure
logic is in `StudioLinkCore/CalendarIntegration.swift`, tested with a fake
`CalendarProvider`. The real permission prompt and real EventKit writes are
not yet verified. Details: `docs/integrations/CALENDAR.md`.

## Setup guide, podcast-day checklist, icon and updates (SL-P2-2)

**Setup guide.** On a launch where `desktop.json` has no `onboardingCompleted: true`, the
control center opens the *Set Up p2p.video* window (once per launch) and the dashboard is
not auto-opened. Steps: Welcome → Connect Tailscale (read-only: Install, Sign in, Turn on
secure links (HTTPS); Download / Open Tailscale / admin console buttons) → How guests join
("Let guests join from anywhere" (recommended) or "Only guests on my Tailscale network",
each through the same confirm sheet as Settings, including the Allow public links step;
"Decide Later") → Browser (Chrome detected at `/Applications` or `~/Applications`; "Open
p2p.video in" Chrome or the default browser; camera/mic guidance) → First room
(`POST /api/v1/rooms {"name"}` with `Authorization: Bearer <desktop token>`, then Copy Room
Link to the clipboard only) → All set (summary; **Open p2p.video** opens the home and
marks the guide complete). Every step can be skipped,
**Skip Setup** completes it without opening anything, and Settings → General → **Show
Setup Guide…** reopens it. `OnboardingFlow` only allows jumping back, never ahead.

**Browser.** `dashboardBrowser` (`chrome` | `systemDefault`, unset = Chrome when installed)
applies to every page the app opens (Open Dashboard, Start a Meeting, rooms). If Chrome is
chosen but missing, or fails to open, the default browser is used.

**Podcast-day checklist** (control center, refreshed every 30 s and on Check Again):
host health; Tailscale state; guest link mode plus a read-only `HEAD <publicURL>/health`
(from this Mac it travels over the tailnet to Serve, so for Funnel it proves the route,
not the public internet path); free space on the data volume against ~10 GB/hour (≥ 4 h
ok, ≥ 2 h warning, else problem); power source from IOKit (`IOPSGetProvidingPowerSourceType`
+ battery description; battery or UPS = warning); camera/microphone guidance (macOS does
not expose another app's TCC grants). **Run a 2-Minute Test Call** opens the call-first
home in the chosen browser. The checklist never runs a mutating Tailscale command.

**App icon.** Two woven rings (host and guest) on a graphite tile, with the record-red
lens where they overlap; colours are the web tokens. `StudioLinkArtwork` draws it with
CoreGraphics; `package-mac.sh` runs `studiolink-icon build/AppIcon.iconset` and `iconutil`
to produce `Contents/Resources/AppIcon.icns` (`CFBundleIconFile`). The control center and
setup guide render the same artwork. The menu bar keeps template SF Symbols.

**Updates (Sparkle 2, off by default).** See DECISIONS 2026-10-26. `STUDIOLINK_SPARKLE=1
scripts/package-mac.sh` links Sparkle 2.10.0 (separate `macapp/.build-sparkle` scratch
path), embeds `Contents/Frameworks/Sparkle.framework`, signs its helpers inside out and
signs the app without `--deep`. `SUFeedURL`/`SUPublicEDKey` are written only from
`STUDIOLINK_APPCAST_URL` (https) and `STUDIOLINK_SPARKLE_PUBLIC_KEY`; with either missing,
or a key that is not a 32-byte base64 Ed25519 key, the updater never starts and Settings
→ General → Updates says why. No private key is generated, read or stored by the repo,
and nothing is published.

## WKWebView spike (2026-10-25, macOS 26.6.2, WebKit 605.1.15)

The probe page (`ProbePage.swift`) runs on the daemon's own loopback origin
(`http://127.0.0.1:<port>`) inside a `WKWebView` with the persistent data store and the
same `LoopbackMediaUIDelegate` the studio window uses. Run it with:

```sh
open -n -W --env STUDIOLINK_PORT=7643 --env STUDIOLINK_DATA_DIR=/tmp/sl-probe \
  --env STUDIOLINK_UDP_PORT=50143 --env STUDIOLINK_TURN_LISTEN=127.0.0.1:3578 \
  --env STUDIOLINK_NO_OPEN=1 --env STUDIOLINK_WEBVIEW_PROBE=/tmp/webview-probe.json build/p2p.video.app
```

There are two passes. Pass 1 uses real devices. Pass 2 (`*.mock.json`) turns on WebKit's
private mock capture devices, which skip macOS camera/microphone (TCC) authorization. The
run was unattended with the screen locked, so the probe also turns off WebKit's window
occlusion detection to keep the page "visible". Both switches are private API and are
used only by the probe. Results: `docs/research/2026-10-25-wkwebview-probe*.json`.

| Capability (studio requirement) | Real devices | Mock devices |
|---|---|---|
| Secure context on `http://127.0.0.1` | yes | yes |
| `getUserMedia` (camera + mic) | **no answer**: timed out with TCC `notDetermined`; the prompt could not be answered with the screen locked, and the media delegate was never reached | yes (delegate called, 1280×720 + audio) |
| MediaRecorder with the studio's containers | yes (`video/webm;codecs=vp9,opus`, `video/webm`, `video/mp4`, `audio/webm;codecs=opus`, `audio/mp4`) | yes (279 KB WebM from the camera stream) |
| OPFS `navigator.storage.getDirectory` | yes | yes |
| OPFS `createWritable` write and read back | yes | yes |
| AudioWorklet (`addModule` + node) | yes | yes |
| WebSocket host session against the daemon | yes (admitted) | yes |
| RTCPeerConnection (loopback, data channel + media) | yes | yes (remote video + audio tracks) |
| `navigator.storage.persist()` | **no** (OPFS journals are best-effort storage) | no |
| `getDisplayMedia` present | yes (not exercised) | yes |
| `canvas.captureStream` (livestream compositor) | yes | yes |

**Decision (ADR-015):** every web platform API the studio needs works in WKWebView. One
leg is unproven: the first-run camera/microphone TCC grant followed by real-device
capture. `persist()` is also refused, which matters for OPFS upload journals under
storage pressure. So the **call stays in the default browser** by default, with Chrome
recommended as the tested path. The main window is a native control center. Settings →
General → "Open meetings in a p2p.video window (beta)" switches Start a Meeting to the
in-app `WKWebView` studio. Before making that the default, run the probe attended,
answer the TCC prompt, and check that pass 1 reports `getUserMedia=yes`. Then run a full
take with uploads in the in-app window. (2026-10-26: the in-app window is now complete and
covered by an end-to-end probe; only the attended real-device legs remain. See "In-app
studio window" and ADR-021.)

## In-app studio window (SL-P2-3, 2026-10-26, ADR-021)

Settings → General → Studio → **Open meetings in: A p2p.video window / The browser
chosen above**. With the window chosen, Start a Meeting, room shortcuts and the
meeting reminder's Start open the host studio in one AppKit window
(`StudioWindowController`) holding a `WKWebView` on `http://127.0.0.1:<port>`.

| Area | Behavior |
|---|---|
| Camera and microphone | `requestMediaCapturePermissionFor` grants only the daemon's own origin (http, loopback, this port) and only when the requesting frame is that origin (`MediaCapturePolicy`). macOS asks once for the app (TCC; `NSCameraUsageDescription`, `NSMicrophoneUsageDescription`, entitlements `device.camera` and `device.audio-input`). Devices enumerate with labels and switch by `deviceId` (probe: 2 cameras, 4 microphones, both switched). |
| Screen share | `getDisplayMedia` works in WKWebView on macOS 26 (probe: "Mock screen device 1", 1920×1080, from a click). With real screens macOS asks for Screen & System Audio Recording permission for p2p.video. No ScreenCaptureKit fallback was needed. |
| Recorder and journal | Same code as the browser: MediaRecorder (`video/webm;codecs=vp9,opus` in WebKit; WebCodecs is not used) with the OPFS journal and live chunk upload to the loopback daemon. |
| Data store | A persistent `WKWebsiteDataStore(forIdentifier:)` whose UUID is derived from the data directory (`StudioDataStoreIdentity`), never `.nonPersistent()`. Closing the window or a crash keeps the journal; a test data directory gets its own store. `navigator.storage.persist()` stays `false` in WKWebView (quota reported 21 GB). Risk: WebKit may evict best-effort data under storage pressure. For the host it is small because fragments upload to the local daemon within seconds of being journaled (probe: everything but the deliberately dropped tail was on the host). The app never clears website data. |
| Close and reopen | Closing during a call, take, livestream or pending upload asks first ("Keep Window Open" / "Close Window"). Closing unloads the page (about:blank, then the view is released), which ends the call and the Web Locks. Reopening the studio resumes the journaled upload automatically; rejoining the meeting shows the take still running, and Record stops it (web fix: `onRoomState` for a host whose page isn't recording). The daemon now reports a take that is recording with no socket connected (`DesktopSnapshot`), so quit and restart still warn. |
| Quit | Same warning as before plus "You are in a call in the studio window." Quitting closes the studio window without a second question. |
| App Nap, sleep | While the window is in a call: `beginActivity(.userInitiatedAllowingIdleSystemSleep, .idleDisplaySleepDisabled, .latencyCritical)`. While a take records, a stream is live or uploads are pending (window open or not): `.userInitiated + .idleSystemSleepDisabled + .idleDisplaySleepDisabled`. Released when idle. |
| Window | One studio window (no tabs, no second window). Start a Meeting while in a call focuses it; another meeting while in a call asks "Leave the current call?". Size and position are remembered (`setFrameAutosaveName`); full screen via the green button. |
| Sign-in | The window opens `DashboardURLs.launch` (`?launch=<token>`, read fresh from `.launch_token`). The daemon sets the admin cookie and redirects to the clean URL, so the token is never in the page URL or the back/forward list (probe checks both). If it ever stayed, the window reloads the stripped URL. URLs are never logged. |
| Downloads | `<a download>`, `Content-Disposition: attachment` and page blobs go through `WKDownload` to `~/Downloads` (`NSDownloadsFolderUsageDescription`, `files.downloads.read-write`). Names are sanitized and never overwrite (`name (2).ext`). An HTTP error is reported, not saved as a file. A notification "Downloaded <name>" has **Show in Finder**; the Dock's Downloads stack bounces. Probe: `.ics` invite, originals ZIP, captions `.vtt` blob. |
| Links | `target=_blank` (Google Calendar / Outlook templates, Tailscale help, chat links), `mailto:`, `webcal:` and any non-daemon page open in the default browser or app. Third-party frames and other schemes are refused. |
| Clipboard | `navigator.clipboard.writeText` works from a click (probe compared the pasteboard). |
| Notifications, badge | A bridge script maps the page's `Notification` (WKWebView shows none) to the app's notifier when the window isn't in front, and `navigator.setAppBadge` to the Dock badge; otherwise the Dock shows the number of guests waiting. Today the studio page itself posts no Web Notifications; guest-waiting and take notifications come from the app as before. |

### End-to-end probe

`make macapp-probe` (or `scripts/macapp-studio-probe.sh [report.json]` with a built
daemon and `swift build`) runs the debug app with `STUDIOLINK_STUDIO_PROBE` against a test
daemon on free ports with a temporary data directory. It never uses port 7443 or
`~/Library/Application Support/p2p.video`. To probe a packaged bundle, set
`STUDIOLINK_APP_BIN`, `STUDIOLINK_DAEMON` and `STUDIOLINK_BUILD_ID` to the bundle's
executable, daemon and `build-id.txt`. The probe drives the real `StudioWindowController`
with WebKit's mock capture devices and occlusion detection off (both private, probe-only;
the shipping window uses no private WebKit API). Clicks are run from `evaluateJavaScript`,
which WebKit treats as a user gesture, so the probe never takes focus. It signs in, checks
the token is gone, enumerates and switches devices, joins, checks the bridge, App Nap
assertion, one-window rule, notification and badge mapping, schedules a meeting and
downloads its `.ics`, opens the Google Calendar link (recorded, not launched), copies to
the clipboard (the previous clipboard is restored), shares a mock screen, downloads a blob
`.vtt`, records take 1 and checks it assembled on the daemon and in
`/api/v1/desktop/state`, downloads the originals ZIP. It then records take 2, drops chunk
uploads so the tail exists only in OPFS, closes the window (confirm path), reopens it,
waits for the recovery banner to report the upload received, rejoins, stops the take from
the rejoined page, and checks take 2 assembled with no missing participant. The temporary
WebKit store is removed at the end.

Results (2026-10-26, macOS 26.6.2, M2 Max): debug build 34/35 checks, packaged ad-hoc
bundle 34/35, all required checks pass (about 30–40 s each). The one informational miss
is `window.open` from a script-run click, which WebKit's popup rule blocks; the studio
uses only `target=_blank` links, which pass.

Bugs the probe found and fixed:
- WebKit clamps `performance.now()` to whole milliseconds, so a loopback clock ping
  measured 0 µs, `rttP10Ms` was 0 and the daemon refused to align the take ("clock fit has
  no measured timing samples"): no originals and no export. `ClockSync` now reports at
  least 1 µs (`web/tests/clock.test.mjs`). This also affects Safari guests.
- A host page reopened mid-take could not stop the take (Record sent Start, which the hub
  ignores). Fixed in `onRoomState`; `web/tests/hostrejoin.browser.mjs` covers it in
  Chromium.
- The desktop state dropped a session once its last socket closed, even with a take
  recording, so quitting would not warn. Fixed and tested in Go.

### Default: still the browser

The default (`studioInApp` false) did not switch. Every automated check passes, but the
two legs that depend on macOS permission prompts cannot be answered unattended and are
unverified: (1) a real camera and microphone after the first TCC prompt, recorded for a
full take in the window, and (2) a real screen share after the Screen Recording prompt.
Those are the owner test below. Once they pass on a real Mac, flip the default in
`DesktopSettings` (new installs only; an existing `desktop.json` keeps its choice).

Owner test (about 10 minutes, attended): pick "A p2p.video window", Start a Meeting,
allow camera and microphone, record a 2-minute take with a guest on another device,
Present a window (allow Screen Recording, relaunch if macOS asks), stop, download the
originals and the editor ZIP, close the window during a second take and reopen it.

## Verified (2026-10-25)

- `swift build`, `swift test` (51 tests): argv golden tests (daemon and Tailscale),
  status/serve fixture parsing, health classification (the launcher's three refusal
  cases), restart policy, notifications, settings and log rotation.
- `cd host && go vet ./... && go test ./...` passes, including new desktop-state
  authorization, contents and fixture-contract tests, and the extended Funnel guardrail.
- `scripts/package-mac.sh` (ad-hoc) built and verified the bundle (`codesign --verify --deep --strict`).
- Packaged app on port 7643 with a temporary data dir, UDP 50143 and TURN 3578, next to a
  real host on 7443 that was left untouched:
  - The daemon started healthy with the expected argv. The menu bar extra is present
    (System Events: `p2p.video: Host running`, role "status menu").
  - `kill -9` of the daemon led to an automatic restart about 1 s later with a new PID.
  - With a take recording, the daemon was `SIGSTOP`ped for 16–18 s. The app logged
    "unresponsive but not restarted", kept the same PID, and the daemon recovered on
    `SIGCONT`.
  - A waiting test guest produced the guest-waiting notification event (app log).
  - Quit through AppleScript, and later through SIGTERM, stopped the daemon ("Shutting down").
    `kill -9` of the app left the daemon running. The next launch reused it ("Host
    running (reused)"), and SIGTERM to that launch stopped the adopted daemon.
  - Launching on 7443 (a different build was running there) showed the blocked message.
    The daemon on 7443 kept running, and `~/Library/Application Support/p2p.video` was
    not modified.
- Screenshots are rendered offscreen, because screen capture was unavailable with the
  screen locked.

## Open

- Real-device in-app capture (attended TCC grant) and real screen share in the studio
  window; the automated in-app take, upload, close/reopen and resume pass with mock devices.
- A real run of the Tailscale Serve/Funnel plans (they were deliberately not run in
  development), including the "turn off" verification on a Funnel-enabled tailnet.
- Sparkle updates (the Minutes pattern). They need an appcast host decision and an
  EdDSA key, both deferred.
- An app icon (`.icns`). The bundle has no icon yet.
- Clicking Start a Meeting from the menu was not automated: it would open the user's
  browser. The same create-session and host WebSocket path is exercised by the probe and
  the Go tests.
- Developer ID signing, notarization and hardened-runtime launch with these entitlements
  (no credentials in this environment).
