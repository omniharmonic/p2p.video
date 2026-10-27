# p2p.video roadmap: call-first studio, recording optional

Set 2026-10-25. Supersedes the v1/v2/v3 split in PRD §2 where they conflict.

## Product position

p2p.video is a **self-hosted video calling platform** that happens to be the best
tool for recording a podcast. It isn't a recording tool with a call attached.
A meeting works with nothing but a link; recording, livestreaming and transcription
are switches you turn on inside a call.

Competitors we measure against: **Google Meet** (call quality, zero-friction join,
group calls, chat, screen share, captions) and **Riverside.fm** (local ISO recording,
progressive upload, edit-ready export, livestream, clips).

Post-SaaS principle: no hosted service of ours. The host's Mac is the server.
Tailscale (the host's own account) supplies reachability. Guests need a browser.

## Ecosystem split

| Capability | Home | Interface |
|---|---|---|
| Calls, group rooms, chat, screen share, host controls | **p2p.video** | browser + host daemon |
| Local ISO recording, upload, editor export | **p2p.video** | export package (`manifest.json`) |
| Livestream (RTMP/RTMPS, multi-destination) | **p2p.video desktop host** | host compositor → ffmpeg |
| Live captions, post-take transcript, speaker naming | **Minutes** | local transcription bridge; per-speaker WAVs make diarization exact |
| Session notes, transcripts, show notes into the vault | **Parachute / Prism** | vault REST export after a take |
| Clip selection, social cuts | **agentic-video-editor** | reads export package + transcript |

p2p.video ships stable, documented outputs (export manifest, live audio bridge,
webhooks) and lets the other tools consume them. It doesn't absorb them.

## Phases

### P0: podcast-ready (now)
- [ ] **Guests without Tailscale.** Funnel-served guest page, ICE with policy-controlled
      public STUN for direct UDP, embedded TURN on loopback reached as `turns:` through
      Funnel :8443 as relay fallback. Dashboard/admin stay unreachable through Funnel.
      *Implemented and synthetically verified 2026-10-25 (SL-P0-1, `docs/PUBLIC_GUESTS.md`).
      Still open: a real Funnel :8443 run from an outside network.*
- [x] **Call bitrate discipline.** Recording captures full resolution; the call sender is
      capped (resolution + bitrate scaled to participant count) so the call and uploads
      don't fight. (720p/1.5 Mbps → 540p/900 kbps → 360p/500 kbps; Opus 64 kbps.)
- [x] **Media recovery.** Renegotiate on peer failure, not just WebSocket retry: the client
      re-offers from a new PeerConnection and the SFU replaces the participant's peer.
- [x] **Group calls verified.** 4-participant browser test (`web/tests/group.browser.mjs`):
      everyone decodes everyone, mic/camera indicators, recovery mid-take, leave removes tiles.
- [x] **Restart recovery.** Media back within ~10 s of a restarted daemon being healthy
      (3 s capped reconnect backoff; a peer closed mid-renegotiation no longer wedges
      signaling). A guest tab killed mid-take resumes its OPFS uploads when the invite is
      reopened and rejoins with one admission. *(SL-P0-7, 2026-10-25: 20/20 restart runs,
      synthetic only.)*
- [x] **Pre-flight.** One "ready to record" check: storage, devices, path, headphones.
      Green room + studio chip, host aggregate, confirm on red. *(SL-P0-5, synthetic only;
      label/battery checks are hints.)*
- [x] **Real-world networks.** Per-participant connection quality (signal bars on tiles and in
      the people panel, "your connection is unstable" toast), relay-aware and congestion-adaptive
      call caps with hysteresis, and upload pacing that backs off while the call is stressed and
      drains at 4× after Stop, with the host seeing each participant's upload backlog.
      *(SL-P0-8, 2026-10-25: verified with CDP throttling of a guest in `web/tests/netimpair.browser.mjs`;
      real lossy/cellular networks still to try.)*
- [x] **Fast delivery.** Originals (+ manifest/offsets) download as soon as uploads assemble; the conformed
      export runs as parallel jobs with live progress. H.264 is bounded by the Mac's single VideoToolbox H.264
      stream (≈12× realtime per camera); the ProRes Proxy option is ~4× faster for three cameras.
      *(SL-P0-9, 2026-10-25: synthetic benchmark, `TestExportBenchmark`.)*
- [ ] Long-session (60 min) soak with fake devices: memory, OPFS, upload backlog.
      *Harness done 2026-10-25 (SL-P0-6, `web/tests/soak.browser.mjs`); 3-min run flat. 30/60-min runs
      still to do on mains power.*

### P1: Meet parity
Persistent rooms (reusable link), participant list with mute/camera state, active
speaker, chat, raise hand, reactions, screen share (call + optional recorded source),
host controls (mute, remove, lock room), recording on/off with a visible consent banner,
device switching mid-call, keyboard shortcuts, speaker/grid/pinned views.

Progress (2026-10-25, SL-P1-1):
- [x] **Chat**: session-scoped over the signaling socket, server-stamped, 2000-char and 10/10 s limits, last 200 messages replayed on admission/resume, safe linkify, unread badge.
- [x] **Raise hand + reactions**: hand state in the roster with an ordered queue for the host; six allow-listed reactions floating on the sender's tile, rate-limited by the hub.
- [x] **Participant list**: name, role, mic/camera/hand per row. Host actions: mute (call only, recording continues, DECISIONS 2026-10-25), remove (deny/revoke path) and lock room. All enforced by role on the hub with negative tests. Connection quality per row: signal bars since SL-P0-8 (`net_stats`), plus upload backlog for the host.
- [x] **Recording consent banner**: persistent banner plus an assertive live announcement on start/stop for everyone in the call; the waiting room is told before admission.
- [x] **Keyboard shortcuts**: ⌘/Ctrl+D mic, ⌘/Ctrl+E camera, Ctrl+⌘+H (Ctrl+Alt+H elsewhere) hand, `?` help. Ignored while typing.
- [x] **Persistent rooms** (SL-P1-5, 2026-10-25): named rooms with one reusable guest link (≥128-bit, stored hashed, rotatable, revocable), a meeting per opening, guests waiting for the host and asking to join automatically, room defaults (auto-admit off by default, lock on start, recording preset), host End meeting, and a call-first home (New meeting, Rooms, Recent meetings). Verified synthetically by `web/tests/rooms.browser.mjs`. Chat still lives in hub memory per meeting, not per room.
- Verified synthetically by `web/tests/callfeatures.browser.mjs` (host + guests, fake devices). Real-device and screen-reader passes are still open.

Progress (2026-10-25, SL-P1-2/3/4):
- [x] **Screen share**: any admitted participant can present a screen, window or tab (with tab/system audio when the browser offers it). It is published on the existing PeerConnection by client renegotiation with glare handling, under sharpness-first sender caps. Receivers get Meet's presentation layout. One presenter at a time, and the host can stop anyone's share (enforced by the hub, negative-tested). The browser's native "Stop sharing" bar is handled.
- [x] **Recorded screen source**: while a take records, the presenter's screen is recorded locally as its own late-registered `screen` track. The export carries it as its own angle, with shared audio as a separate WAV. It never counts toward, or blocks, a participant's expected camera/mic recording.
- [x] **Mid-call device switching**: camera, mic and speaker from a Devices popover without rejoining (`replaceTrack`, `setSinkId`). Camera/mic are locked while a take records (DECISIONS 2026-10-25).
- [x] **Views**: Grid, Speaker (active speaker from remote audio levels with a 1.5 s hold), Focus, Switcher, local pin, presentation layout, hide self view. No horizontal overflow at 1280×720 or 390 px.
- Verified synthetically by `web/tests/screenshare.browser.mjs` (host + 2 guests, Chromium's fake screen and devices). Still open: real screens/windows across browsers (Safari/Firefox `getDisplayMedia` audio support differs), a real second camera, and long shares with window resizes on real hardware.

Progress (2026-10-25, SL-P1-6/7/8/9):
- [x] **Leave call for guests**: Leave (confirm while uploads are pending), a post-leave screen with live upload progress and Rejoin through the in-memory resume grant (no second admission). The host keeps End meeting only: there is no host-less meeting (no host transfer; waiting guests couldn't be admitted, a take couldn't be stopped), so no "leave and keep it running".
- [x] **Background blur / replace** for the call sender only (Off, Light/Strong blur, three generated backgrounds), on-device MediaPipe selfie segmentation served from our origin (`/mediapipe/`, CSP `'wasm-unsafe-eval'`). Insertable streams in Chrome, canvas fallback elsewhere; auto-off with a notice when fps collapses; persisted; green room preview. The recording keeps the untouched camera (not offered as an option).
- [x] **Call audio processing**: noise suppression, echo cancellation and automatic gain switches for the call mic. The capture (recording) mic now opens raw as ARCHITECTURE §4.2 always specified; the call gets its own processed capture. Host mute still works.
- [x] **Phone as an extra camera (PRD F3)**: single-use 10-minute capability minted for an admitted participant, QR code drawn locally, `role: subsource` admitted without a prompt, publish-only in the SFU, "<Name> · Phone" tile, late registration in a running take, upload under its own grant, exported as the participant's "Phone" angle.
- Verified synthetically by `web/tests/leave.browser.mjs`, `effects.browser.mjs`, `phonecam.browser.mjs`. Still open: a real person in front of a real camera for segmentation quality, a real phone over Funnel, real microphones for noise suppression, Safari/Firefox (canvas fallback and per-track audio processing untested).

### P2: desktop host app
Native Mac shell around the daemon: menu bar, native window, guided Tailscale
Serve/Funnel setup with explicit consent, notifications, login item, Sparkle updates
(same pattern as Minutes).

Progress (2026-10-25, SL-P2-1, `docs/DESKTOP_APP.md`, ADR-015):
- [x] **Daemon supervision**: `macapp/` Swift app is the bundle executable. It reuses a daemon only on an exact build match and never stops a foreign one, restarts after a crash with backoff, never restarts while recording/live, and quit stops the daemon after warning if work is in progress. `daemon.log` is rotated.
- [x] **Menu bar extra**: status, recording/live/waiting indicators, Start a Meeting, Copy Guest Invite Link, Open Dashboard, Settings, Quit.
- [x] **Native control center** window. The call stays in the default browser. The WKWebView spike passed every API with mock devices; real-device capture is unconfirmed, so the in-app studio is an opt-in beta.
- [x] **Guided Tailscale setup**: read-only checks; private (Serve) and public (Funnel + TURN :8443) links and "turn off public link", each showing the exact argv-built commands before an explicit click. The daemon restarts with the matching flags.
- [x] **Login item** (`SMAppService`) and **notifications** (guest waiting, take uploaded, take exported) from the loopback-only `GET /api/v1/desktop/state`.
- [x] **In-app studio window** (SL-P2-3, ADR-021): production behavior (permissions, downloads, links, lifecycle, App Nap, one window per meeting) and an automated end-to-end probe (`make macapp-probe`). Default stays the browser until the attended real-device run.
- [ ] Sparkle updates (needs an appcast host decision and an EdDSA key), app icon, a real run of the Serve/Funnel plans, an attended in-app capture run (real camera/mic/screen in the studio window), and Developer ID/notarized launch.

### P3: livestream
Host-side compositor (browser canvas, WYSIWYG layouts) → WebSocket → ffmpeg →
RTMP(S) to YouTube/Twitch/custom, several destinations at once. Layout presets,
lower thirds. Independent of the recording pipeline.

- [x] **Bitrate in the panel and status** (SL-P3-2, 2026-10-26): ffmpeg's tee output reports `N/A`, so the status shows the ingest rate over a 5 s sliding window (output rate from `total_size` when an ffmpeg reports it) with its source.
- [x] **Finished livestream** (SL-P3-3, 2026-10-26, ADR-019): ffmpeg only encodes (FLV to a pipe, no key in argv); the daemon publishes RTMP/RTMPS itself, one publisher per destination with its own queue, keyframe skip and reconnect backoff; RTMPS presets for YouTube, Twitch, Facebook, LinkedIn; per-destination status and encoder health warnings in the panel. Verified against local sinks only; real platforms are an owner test.

### P4: ecosystem bridges
Minutes live-caption bridge (per-participant PCM over a local socket, speaker = participant),
post-take transcript handoff, Parachute session note, clip-editor handoff manifest.

Progress (2026-10-25, SL-P4-1, post-take slice; `docs/integrations/`):
- [x] **Transcript handoff**: the export modal's "Transcribe with Minutes" runs `minutes-cli transcribe-file` (argv
      only) on each participant's isolated track, then merges the segments on the take timeline using manifest
      offsets. The package gets `transcript/transcript.{json,md,srt,vtt}`. "Auto-transcribe after export" is a
      per-session setting. Minutes needed one additive, stateless CLI command, now on the Minutes branch
      `feat/transcribe-file-json` (not merged). See `MINUTES.md`.
- [x] **Parachute session note**: "Send to Parachute" creates or updates one vault note per take in Minutes' meeting
      format (`Meetings/<date time> <title>`, tags `meeting podcast studiolink`, transcript inline). It uses an
      ephemeral token minted at send time; no secret is stored. Host-only and Funnel-denied. See `PARACHUTE.md`.
- [x] **Clip-editor handoff**: `handoff/clip-editor.json`, a reference `handoff/program.wav`, and `ave`-native
      `work/transcript.json`/`work/sync.json` shapes. See `CLIP_EDITOR.md`.
- [ ] Open: a real Apple Speech run. It is blocked by Speech Recognition permission: TCC attributes the CLI to the
      launching app, so p2p.video.app needs a usage string and a one-time grant (`MINUTES.md`). Also open: a real
      vault send, a real `ave` run on a p2p.video package, bleed handling, merging the Minutes branch, and the live
      caption bridge.
Progress (2026-10-26, SL-P4-4, `docs/integrations/CLIP_EDITOR.md`):
- [x] **Clip-editor handoff verified with `ave`**: a 150 s three-person p2p.video take ran through every deterministic
      `ave` stage to an exported short, on stock `ave` (with `--session s1`) and on `ave import-studiolink` (ave branch
      `feat/studiolink-import`, not merged). p2p.video now also writes the camera mapping (`handoff/ave/episode.json`)
      and `work/transcript.md` in ave's format, names speakers host-first, and `scripts/ave-import.sh` imports in one
      command. `TestHandoffLoadsInAve` loads a generated package with ave's own code when ave is installed.
- [ ] Open: merge the ave branch; a real-speech transcript; lip sync judged on real cameras; the LLM-driven clip picks.
Progress (2026-10-25, SL-P4-2, `docs/integrations/CAPTIONS.md`):
- [x] **Live captions, on device**: the host's page taps each participant's call audio and sends 16 kHz PCM to the daemon. The daemon relays it to a local caption provider (p2p.video.app, Apple SpeechAnalyzer/SpeechTranscriber; on-device SFSpeechRecognizer fallback before macOS 26) over a loopback, desktop-token socket, and fans results out as `caption` messages with exact speaker names. Meet-style overlay, CC button and `c` per viewer, a "Captions on" indicator for the call and the waiting room, host room switch and language, and a VTT/JSON transcript at meeting end. Captions are separate from recording (guardrail test).
- [x] Verified: Go/Swift/web unit tests; a fake-provider browser e2e (`web/tests/captions.browser.mjs`) proves per-participant routing; a real on-device run recognized `say` speech through the whole pipeline (browser → daemon → app provider → both pages).
- [ ] Minutes as a provider (the protocol is documented for it); a real-microphone multi-person session; non-English quality; handing the live transcript to Minutes/Parachute (the post-take handoff from ISO tracks is a separate task).
Progress (2026-10-26, SL-P4-3, `docs/integrations/TRANSCRIPTS.md`):
- [x] **Transcripts without Minutes**: a built-in engine, the default. The Mac app transcribes each exported ISO WAV
      on device (SpeechAnalyzer/SpeechTranscriber reading the file; chunked SFSpeechRecognizer fallback before macOS 26)
      behind a sibling loopback provider socket (`/api/v1/transcription/provider`, desktop token). Same merge and
      output as Minutes. Engine picker (Built-in / Minutes when detected), per-track progress, cancel, retry of the
      remaining tracks, explicit states for "app not running" and "Speech Recognition not allowed". Real recognition
      verified unattended on synthetic speech, including a 62-minute file in 92 s.
- [ ] Real call voices through the built-in engine; the pre-macOS 26 fallback with a real grant; non-English.

### P5: Riverside extras
Teleprompter, branded layouts, producer-only controls, async "record links".

Progress (2026-10-26, SL-P5-1 / SL-P5-2):
- [x] **Teleprompter** (SL-P5-1): one script per room (SQLite, migration 4) or instant meeting (memory). The host and producers edit (a producer not when the host keeps it "only me"); the host chooses who sees it. Reader overlay at the top centre, near the camera: auto-scroll with speed and size, Space / arrow keys, mirrored mode, follow-host scroll sync. Script never logged; rendered as text; 64 KB cap; role-enforced on the hub with negative tests.
- [x] **Branding and lower thirds** (SL-P5-2): per-room brand (show name, accent, PNG/JPEG logo ≤ 1 MB validated by magic bytes and decode, served same-origin with `nosniff`), per-person name + title by host/producer, tile labels, a branded variant of every livestream layout with logo bug and animated lower thirds (active speaker or on demand), titles in `manifest.json` `participants[]`. Recordings stay clean ISO.
- [ ] Open: a person reading the prompter on a real laptop camera (eye line), a real platform stream with branding, an FCPXML title track (manifest only today), carrying a person's title over to the next episode of a room.

Progress (2026-10-26, SL-P5-3, ADR-017):
- [x] **Async record links** (Riverside "record solo"): the host creates a link (title, optional script, per-take limit, expiry, how many recordings, storage limit, optional room) in the home's Recordings panel. A guest opens it with no call and no host: green room (camera, microphone, microphone sound, uncompressed WAV option, pre-flight), the script with auto-scroll, takes recorded locally with the call's OPFS recorder and progressive upload, review, delete and re-record, send. Reopening the link in the same browser resumes the upload. The host exports each take (editor package, originals ZIP, transcript/Parachute/clip-editor handoffs) exactly like a call take. Link secret like room links (256-bit, hashed, rotate/revoke/expire), per-browser submission key, no call access, caps enforced on the server, management Funnel-denied. Verified synthetically by `web/tests/recordlink.browser.mjs`.
- [ ] Open: a real guest on a real camera/microphone through a real Funnel link (phone browsers in particular: iOS Safari MediaRecorder/OPFS), a long (60 min) solo take, Safari/Firefox.

### P6: calendar
Meet-style calendar integration without a calendar API, OAuth or hosted service (ADR-018).

Progress (2026-10-26, SL-P6-1, `docs/integrations/CALENDAR.md`):
- [x] **Scheduling**: schedule a meeting in a room (title, start, length, agenda, guest emails, optional organizer email); Home → Upcoming with Start, Invite, Edit, Cancel, Delete; starting opens the room.
- [x] **Invites any calendar accepts**: RFC 5545 `.ics` (UTC, stable UID, SEQUENCE bump on edit, CANCEL on cancel, REQUEST when organizer + guests), Google Calendar and Outlook template links, `mailto:` and invite text with Chrome/headphones tips; only the room guest link inside.
- [x] **Host calendar feed**: rotatable secret, served on this Mac and via private Serve to tailnet devices, never Funnel (guardrail-tested).
- [x] **Mac app (EventKit)**: upcoming meetings with this host's room links from the calendars in macOS, reminders N minutes before with a Start meeting action, optional write-back into a chosen calendar; off by default, permission asked only from Settings.
- Verified synthetically: Go golden/authz/migration tests, `calendar.browser.mjs`, Swift unit tests with a fake calendar store; ICS parsed by python `icalendar`.
- [ ] Open: a real macOS calendar permission grant and real EventKit reads/writes; importing the `.ics` into real Google Calendar, Apple Calendar and Outlook (CANCEL handling without an organizer); subscribing in macOS Calendar over a real Serve URL; recurring meetings; recognizing Google Meet/Zoom links (future).

## Working rules
Every phase item lands with tests (Go unit, web unit, fake-device browser test where
media is involved) and a STATUS log line. Real-device and two-network acceptance are
recorded separately and never claimed from synthetic runs.
