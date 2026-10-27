# UX redesign: calls first, studio when you need it [SL-UX-2]

Owner direction (2026-10-27): p2p.video is now a full peer-to-peer video-call tool that is also great for recording.
The UX still reads like the recording rig it started as. Make it friendly and intuitive, Google Meet-like in flow,
**without removing any feature**. Appearance follows the system setting (light and dark both designed).

## Principles

1. **One job per screen.** Home = find or start a call. Ready to join = check yourself and join. Call = talk.
   Studio tools (record, uploads, export, livestream, teleprompter, branding) live in one Studio panel, opened on demand.
2. **Plain words, by what people do.** Name things by the user's goal, not the system. Technical detail moves behind
   "Recording options" / "Details" disclosures, never deleted.
3. **Calm by default, loud only when it matters.** Status the user must act on is shown; status that is fine is a
   single quiet "✓ Ready to record" line. Red only means "recording" or "leave/end".
4. **The live sound ring is the one signature.** The mic button (Ready to join + dock) and each tile show a soft ring
   (`--sl-ring`, `--sl-ring-soft`) that moves with the voice. Respect `prefers-reduced-motion` (show a static level instead).
5. **Same name for the same action everywhere.** A button "Start recording" produces "Recording started".

## Tokens (web: `web/src/tokens.css`)

Light default, dark under `@media (prefers-color-scheme: dark)`; same variable names. Feature CSS must use tokens only
(no new hex/rgba literals; migrate the ~30 that exist). Type: `--sl-font` Figtree (UI), `--sl-display` Bricolage
Grotesque (headings, meeting titles, names on the home). Fonts are bundled `@fontsource-variable/*` (no network).

| Role | Light | Dark |
|---|---|---|
| canvas / surface / stage | `#f4f6f5` / `#ffffff` / `#e4e9e7` | `#101418` / `#171c21` / `#0a0d10` |
| ink / secondary | `#17202b` / `#4a5663` | `#e9eef0` / `#aab5be` |
| accent (signal teal) | `#0f7b76` | `#5fcfc6` |
| chip (people/status) | `#e9e6f8` | `#2b2840` |
| recording / leave | `#e5484d`, fill `#c8353a` | `#f2555a`, fill `#c8353a` |

Primary actions are pill-shaped (`--r-pill`), teal. Secondary actions are quiet outlined pills. Avoid ALL-CAPS labels,
eyebrow labels, middle-dot meta strings and "→" suffixes (use real structure instead).

## Information architecture

```
HOME                                                     READY TO JOIN (its own step)
┌──────────────────────────────────────────────┐        ┌───────────────────────────────────────┐
│ ◎ p2p.video  [⌨ Paste a link or room name Join] [+ New ▾]  ⚙ ? │ │ ┌──────────────────┐   Ready to join?  │
│ ┌──────┐ Tuesday, Oct 27  🗓   ‹ S M T W T F S › │      │ │  camera preview   │   The Podcast      │
│ │Meetings│ ─────────────────────────────────── │       │ │  (🎙ring) (📷) (✨)│   [Your name     ]│
│ │ Rooms │ 4:30 PM Episode 12  The Podcast [Start] │      │ └──────────────────┘   [   Join now   ] │
│ │Recordings│  …or friendly empty state + New  │       │ 🎙 Mic ▾   📷 Camera ▾   🔈 Speaker ▾     │
│ └──────┘                                       │       │ ✓ Ready to record   ☐ I'm wearing headphones │
└──────────────────────────────────────────────┘        │ ▸ Recording options  (mic mode, uncompressed WAV, checks) │
                                                         └───────────────────────────────────────┘
```

- **Top bar**: brand, "Paste a link or room name" + Join (accepts a p2p.video guest/room URL or a room slug),
  **New ▾** menu: *Start a meeting now* · *Schedule for later* · *Create a room* (a reusable link) · *Ask someone to
  record on their own* (record link). Settings gear (opens existing settings), Help (setup guide).
- **Left rail** (icons + labels, Meet's Meetings/Calls style): **Meetings** (date strip + that day's scheduled meetings
  + live rooms; Recent meetings below), **Rooms** (room list with Copy link, Start, ⋯ menu: producer link, saved titles,
  replace/turn off link, rename, archive, delete), **Recordings** (past meetings' takes with export/originals/transcript,
  and solo record links + drafts). Feed / calendar settings live in Meetings.
- **Empty states** are invitations to act: "Nothing scheduled for Tuesday" + "New meeting" button; a small friendly
  illustration is allowed (inline SVG drawn with tokens; one per screen at most).
- **Ready to join** replaces the home green room: one big preview (rounded, mirrored self-view) with round mic / camera /
  background buttons overlaid, device pickers as compact dropdown chips below, name field, one "Join now" (host:
  "Start meeting"; room: "Join The Podcast"). Pre-flight collapses to one line ("✓ Ready to record" or "2 things to check
  before recording ▸"). Headphones checkbox stays visible (it decides raw vs clean). Session role, recording mic mode,
  uncompressed WAV and the full checklist move into "Recording options". Guests see the same screen with fewer options.
  Waiting-for-host / waiting-for-admission states use this screen's layout ("Asking to join…", "The host will let you in soon").
- **In call** (dark-ish stage in both appearances; tiles get the sound ring):
  top-left: time | meeting title (ⓘ opens details: link, room, recording info); top-right: participant avatars/count.
  "Your meeting's ready" card on first entry for the host (Add others / copy link / "People with the link need your
  permission to join"), dismissible. Dock: mic ▾, camera ▾, present, react, captions, raise hand, ⋯ more, **Record** (host
  and producer; red ring + timer while recording), **Leave** (red pill; host gets "End meeting for everyone" in its menu).
  Right cluster: Details ⓘ, People, Chat, **Studio** ✦ (recording status + uploads, export/originals/transcript/handoffs,
  livestream, teleprompter, branding & titles, captions download, phone camera, background, settings).
  Replace technical chips ("Pre-flight: Problem", "Remote video decoded", "No track uploaded yet", "Take 1") with: a
  REC pill ("Recording 12:04"), per-person health in People, and upload progress in Studio ("Uploading 2 of 3").

## Copy glossary (use these words)

| Old | New |
|---|---|
| Session / Untitled session | Meeting / "Meeting with Ben" (or the room name) |
| Take 1 / take | Recording 1 / recording |
| Start new meeting | Start meeting |
| Display name | Your name |
| Session role | (Recording options) "Join as" |
| Pre-flight / Not ready to record | "Ready to record" / "2 things to check before recording" |
| Persistent storage | "Keep recordings safe if the browser closes" |
| Uncompressed microphone track (Float32 WAV) | "Studio-quality audio (larger files)" + detail line |
| Recording microphone: Automatic / raw / clean | "Microphone for recording: Automatic · Untouched · Cleaned up" (detail explains) |
| Transfers / track uploaded | Uploads / "Your recording is saved on the host" |
| Prepare export / editor package | "Get files for editing" |
| Originals | "Download raw files" |
| Source health | "Connection & recording" (People panel) |
| Copy invite / Invite | Add others |
| Guest link | Meeting link (room link for rooms) |

## Constraints

- **No feature removed.** Every control still exists; many move. Keep element IDs that tests and other modules use
  (`#join-btn`, `#rec-btn`, `#studio-view`, `#name-input`, `#preview-video`, `#copy-invite-btn`, `#more-btn`, …) or
  update every reference and test in the same change. All 31 browser suites + unit tests must stay green.
- Accessibility: visible focus, 4.5:1 text contrast in both appearances, keyboard paths for menus/rail, reduced motion,
  works at 390 px (rail becomes a bottom tab bar on phones).
- Guardrails from AGENTS.md hold (no hosted services; fonts are bundled; nothing logs links/tokens).

## What shipped: Home, Ready to join, guest entry (SL-UX-2a, 2026-10-27)

Screens: `web/tests/ux-screens.browser.mjs` renders every screen below in light and dark at 1280×800 and 390×844
(`STUDIO_SCREENSHOT_DIR`), and fails on horizontal overflow or an invisible first focus ring.

- **Home** (`components/Home.ts`, `home.css`): its own top bar (`#home-join-input` "Paste a link or room name" + Join,
  `#home-new-btn` **New ▾** with the four actions, settings menu: Camera and microphone / Calendar feed / Private
  connection, help = setup guide). The app header is hidden on the home. Rail `#rail-meetings` / `#rail-rooms` /
  `#rail-recordings` (arrow keys move; the last view is kept per tab in sessionStorage), bottom tab bar ≤ 600 px.
  The Join box opens any p2p.video meeting, room or record link, or starts a room by name or slug.
- **Meetings** (`components/Upcoming.ts`, `upcoming.css`): day heading, Sunday-first week strip (arrows, ←/→ keys,
  dots on days with meetings), that day's meetings with a time column; on today also "Happening now" (live rooms) and
  "Coming up" (the next 5 later meetings); "Recent meetings" below. Empty day: illustration, "Nothing scheduled for
  today", "Schedule a meeting or enjoy the free time", **Start a meeting** + **Schedule**. Invite / edit / cancel /
  delete / .ics / Google / Outlook / email / calendar feed are unchanged.
- **Rooms** (`components/Rooms.ts`): same actions and ⋯ menu; "guest link" is now "room link" in all copy.
- **Recordings**: "From your meetings" (exported recordings as download pills, pending ones as chips; "Recording N",
  not "Take N") above the record-link panel (`#recordings-panel`, "Record on their own", drafts unchanged).
- **Ready to join** (`#green-room`, markup in `index.html`, styles in `home.css`): every way into a call on the host
  home (New, Start on a room or meeting, Join box, `?room=`) opens it first, with **Back to home**. Big rounded mirrored
  preview; round `#ready-mic-btn` (the live sound ring, a static ring above a threshold with reduced motion) /
  `#ready-cam-btn` (camera-off avatar) / `#ready-bg-btn` (background picker below the chips). Mic / camera / speaker
  chips (`#speaker-select` presets the call's output via `DevicePicker.presetSpeaker`). "Your name" (remembered in
  localStorage), one primary button ("Start meeting", "Start The Podcast", "Rejoin …", "Ask to join", "Join when Ben
  starts"), the readiness line (`Preflight` panel: "Ready to record" / "N things to check before recording", Details
  expands to the full list and Check again), "I'm wearing headphones" visible, and **Recording options**: Join as,
  Microphone for recording (Automatic / Untouched / Cleaned up), Studio-quality audio (larger files), camera details.
  Muting the mic or camera here carries into the call and the recording. Instant meetings are titled "Meeting with
  <name>".
- **Waiting** (`#waiting-room`): same layout; "Asking to join…" / "The host will let you in soon", waiting for the
  host, declined and locked keep their copy; the 10-second test clip and free space sit under "Check your picture and
  sound".
- **Solo record page** (`record.html`, `record.css`, `record.ts`): rounded preview, display type, "recording" instead
  of "take" in the guest's copy, Studio-quality audio and Microphone for recording labels; tokens only.
- **Phone camera page** (`phone.css`): over-video tokens so it reads in light and dark; "Recording" pill.
- **Setup guide** (`OnboardingWizard.ts`): step labels and copy in plain words ("Check camera and mic").
- Tokens added (none changed): over-video colours (`--sl-on-video`, `--sl-video-*`, `--sl-camera-off`,
  `--sl-on-rec`), QR paper/ink, `--sl-home-rail`, `--sl-preview-r`. Home, Ready, preflight, upcoming, record, phone
  and phone-QR CSS now have no colour literals.
- Tests: `readyToJoin(page)` and `openRecordingOptions(page)` in `tests/lib/harness.mjs`; host suites open Ready to
  join from the home; rooms/calendar/recordlink/rehearsal click the rail; the preflight suite expands the summary.

Deferred: the camera still starts when the home loads (a Ready-to-join-only camera needs the pre-flight and the Mac
app probe to start it there); the in-call header, dock and panels are SL-UX-2b.

## What shipped: in the call (SL-UX-2b, 2026-10-27)

Anatomy and IDs: `docs/design/call-screen.md`. Screens: `web/tests/ux-call-screens.browser.mjs`
(light and dark, 1280×800 and 390×844).

- **Top bar** inside the call: time | meeting title ⓘ (Meeting details: copy meeting link, Add others,
  how recording works, connection in plain words, reopen the ready card), REC pill "Recording 12:04",
  Live chip, Add others (host), avatars + count (opens People). The app header is hidden in a call.
  Removed from view: "Pre-flight: …", "Remote video decoded", take badge, "No track uploaded yet".
- **Your meeting's ready** card for the host on first entry (once per meeting per tab): Add others, Copy
  meeting link, the permission note; dismissible, reopened from Details. The link is created once per
  page and reused, so copying it twice doesn't invalidate the first copy.
- **Dock**: mic ▾ and camera ▾ split pills (caret opens device settings; camera ▾ also reaches
  Background effects), present, react, captions ▴, hand, ⋯, **Record** ("Record" → red ring "Stop
  recording 12:04"; the "Record anyway?" confirm is unchanged), red **Leave** pill. The host's pill is
  **End meeting for everyone** (the host still can't leave a running meeting; no host transfer).
- **Right cluster**: Details ⓘ, People, Chat, **Studio ✦**. Studio gathers Recording (status, pre-flight
  line, uploads in words, technical details, Uploads, Get files for editing, sync chirp), Show (Go live,
  Teleprompter, Branding & titles, Add phone camera, Download captions) and Meeting (Recording quality,
  Invite producer). More ⋯ keeps Layout, Devices, Background effects, Shortcuts, Setup guide.
- **People** gains "Connection & recording" (host and producer): per person connection, what reached
  the host, backlog and recording mic, in sentences instead of `RTT`/`TRACKS` chips.
- **Sound ring** on speaking tiles (receive-side levels) and on your own tile and dock mic (local
  analyser); a steady outline under reduced motion.
- **Copy** follows the glossary across toasts, uploads ("Uploads", "Saved on the host", "Download raw
  files", "Get files for editing"), the export modal ("Your files for editing are ready"), Add others
  dialog ("Meeting link"), recovery banner ("Finish uploading"), countdown ("Recording starts in"), and
  the post-call screens. Eyebrow kickers are gone from in-call panels and dialogs.
- **Tokens added** (no existing values changed): `--sl-call-bg`, `--sl-call-tile`, `--sl-call-ctl`,
  `--sl-call-ctl-hover`, `--sl-call-ctl-line`, `--sl-on-video`, `--sl-on-video-2`, `--sl-video-scrim`,
  `--sl-video-scrim-strong`, `--sl-toast-bg`, `--sl-toast-text`, `--sl-avatar-1..3` / `--sl-on-avatar-1..3`.
  In-call stylesheets have no colour literals left (masks excepted).

Deferred: the meeting-ended screen's copy and the End meeting confirm live in `Home.ts` (home owner);
this change restyles them only. A real "Leave, keep the meeting running" for hosts needs host transfer.
