# Call screen anatomy

Set 2026-10-25 [SL-UX-1], redesigned 2026-10-27 [SL-UX-2]. Modelled on Google
Meet's call, with the recording tools gathered in one Studio panel. Styles:
`web/src/call.css` (shell, top bar, dock, panels), `web/src/studio.css` (tiles,
banners, modals). Arrangement and presentation: `web/src/components/CallChrome.ts`.

```
┌ top bar ───────────────────────────────────────────────────────────────────┐
│ 1:23 AM │ The Podcast ⓘ  (● Recording 12:04) (● Live)        ⁺👤 Add others  (B)(A)(G) 3 │
├────────────────────────────────────────────────────────────────────────────┤
│   ( Guest C wants to join   Deny  [Admit] )  (● Recording  Everyone is recorded…)  (CC Captions on…) │
│ ┌ stage ──────────────────────────────────────────┐ ┌ side panel ──────────┐ │
│ │  tiles, sized to the stage height; the live      │ │ People / Chat /      │ │
│ │  sound ring on whoever is speaking               │ │ Studio ✦             │ │
│ │ ┌ Your meeting's ready ┐ (host, first entry)     │ │                      │ │
│ └─┴──────────────────────┴─────────────────────────┘ └──────────────────────┘ │
├ dock ──────────────────────────────────────────────────────────────────────┤
│        (🎙▴)(📷▴)(⎚)(☺)(CC▴)(✋)(⋯) [● Record] [☎ red]           ⓘ 👥 💬 ✦ │
└────────────────────────────────────────────────────────────────────────────┘
```

## Appearance

The call is a stage one step darker than the home in both appearances
(`--sl-call-bg`; light `#dde3e1`, dark `#0a0d10`), with controls floating on it
(`--sl-call-ctl`). Text that sits on video (names, captions, badges) uses
`--sl-on-video` on `--sl-video-scrim` in both appearances. Red only means
recording or leaving/ending.

## Top bar (`.call-bar`, inside `#studio-view`)

- Left: local time (`#call-clock`), then the meeting title button
  (`#call-details-btn` holding `#display-session-title` + ⓘ). It opens
  **Meeting details** (`#call-details`): copy meeting link and Add others (host),
  how recording works, the connection in plain words (mirrors `#conn-text`),
  and "Show 'Your meeting's ready' again".
- The REC pill (`#rec-indicator`: "Recording 12:04", phones show the timer only)
  and the Live chip (`#live-chip`) sit next to the title.
- Right: **Add others** (`#copy-invite-btn`, host only) and the participant
  avatars + count (`#call-avatars`, opens People). `#connection-pill` stays in
  the DOM for the app and tests; the bar shows it only when the connection is
  lost (`data-state="lost"`).
- The app header (brand, Setup guide) is hidden during a call.

Gone from the bar: the pre-flight chip (now in Studio), the connection text,
the take badge (now `#take-status-badge` in Studio) and the dock's telemetry
footer (now Studio → Recording).

## "Your meeting's ready" (`#meeting-ready`)

Shown to the host on first entry to a meeting (once per meeting per tab,
`sessionStorage`), bottom-left of the stage: **Add others** (opens the invite
dialog), **Copy meeting link**, and "People who use this meeting link must get
your permission before they can join." Dismissible (× or Escape); reopen from
Meeting details. Guests never see it. The meeting link is created once per page
and reused (creating one replaces the previous link on the host).

## Notices (`.call-notices`)

One centred row: the admission request ("Guest C wants to join · Deny ·
Admit"), the recording note ("Recording. Everyone is recorded on their own
device.") and the captions privacy note.

## Stage and tiles

Tiles size from the stage height (`--stage-h: 100cqh`): one tile fills it,
two sit side by side, three or four make a 2×2, five or six a 3×2; speaker view
and presenting put a filmstrip on the right. Names are white chips on a scrim.
A tile's connection bars show only when it's fair or poor. Hand raised is an
amber badge; reactions float up from the sender's tile.

**The live sound ring** is the one signature: remote tiles get `--level` from
`LayoutSwitcher`'s receive-side audio levels, your own tile and the dock mic
from a local analyser in `CallChrome` (it only reads the capture stream). Under
`prefers-reduced-motion` the ring is a steady outline.

## Dock

| Zone | Controls (DOM and Tab order) |
|---|---|
| Centre | mic ▾ (`#toggle-mic-btn` + `#mic-options-btn`), camera ▾ (`#toggle-cam-btn` + `#cam-options-btn`), present, react, captions (+ host caption settings ▴), raise hand, **More ⋯**, **Record** (host, producer), **Leave** / **End meeting for everyone** |
| Right | Meeting details ⓘ (`#details-btn`), People, Chat, **Studio ✦** (`#studio-btn`) |

- Controls are 44px circles; mic and camera are split pills whose caret opens
  the device settings (camera ▾ also reaches Background effects). Off is a
  muted red; toggles that are on (captions, presenting) are teal; a raised hand
  is amber; an open panel's button is tinted.
- **Record**: idle, a white pill "Record" with a red dot. Recording, a red ring
  with "Stop recording 12:04" (`data-label`; the owner's text "Start
  recording"/"Stop recording" stays as the accessible name). Red pre-flight
  items still ask "Record anyway?" first.
- **Leave** (`#leave-btn`, guests) and **End meeting for everyone**
  (`#end-meeting-btn`, host) are the red pill. The host can't leave a meeting
  running (no host transfer), so the host's pill ends it, after a confirmation.
- Group wrappers are `display: contents`; each control is ordered explicitly.

## Studio ✦ (`#studio-panel`)

A side panel like People and Chat (they replace each other). Sections:

- **Recording**: status card ("Not recording" / "Recording 12:04" and
  `#take-status-badge`), the pre-flight line (`#preflight-chip`: "Ready to
  record" / "Things to check before recording", opens the checklist inline),
  uploads in plain words (`#studio-upload-summary`: "Sending to the host as you
  record", "Uploading 2 of 3", "All recordings saved on the host"),
  `#telemetry-status`, and Technical details (`#telemetry-chunks`,
  `#telemetry-storage`, `#resolution-spec`). Then **Uploads**
  (`#dock-transfers-btn`), **Get files for editing** (`#postproc-btn`), **Play
  sync chirp** (`#chirp-btn`).
- **Show**: Go live (`#livestream-btn`), Teleprompter, Branding & titles, Add
  phone camera, Download captions (host, when captions exist).
- **Meeting**: Recording quality (`#settings-btn`), Invite producer.

Sections with nothing visible for this role hide themselves. The Studio button
gets a red dot while recording.

## People

Raised hands, then everyone with mic/camera state and host actions. Host and
producer also get **Connection & recording** (`#toggle-health-btn` toggles
`#session-health-aside`): per person, connection, what of their recording is on
the host, upload backlog and recording mic.

## More options (⋯)

Layout (Grid, Speaker, Focus, Switcher, Hide self), Devices, Background effects,
Keyboard shortcuts, Setup guide; on phones also captions, raise hand, react and
present. Escape or a click outside closes it and focus returns to ⋯.

## After the call

"You left the call" (guests, with Rejoin and upload progress) and "Meeting
ended" share one centred layout (`leave.css`).

## Narrow screens

- ≤1100px: the dock centres; Meeting details leaves the dock (the title opens it).
- ≤900px: Record becomes an icon circle; Add others shows its icon only.
- ≤820px: side panels become full-height sheets.
- ≤640px (phones): no clock; carets fold into ⋯ → Devices; captions, hand,
  react and present fold into More; People is reached from the avatars. The
  bar is mic, camera, More, Record, Leave, Chat, Studio.

## Rules

- Element IDs are contracts (tests and `web/src/app/` use them; static lookups
  are in `app/dom.ts`). Move controls with `CallChrome`, never rename them.
- Browser tests that use a control behind a panel call `revealControl()` in
  `web/tests/lib/harness.mjs`: it opens Studio, People or More as needed.
- Nothing here changes behaviour: every control keeps its owner, listener and
  state; `CallChrome` only re-parents and sets presentation attributes
  (`data-state`, `data-tip`, `data-label`, `--icon`, `--level`).
- Screens: `web/tests/ux-call-screens.browser.mjs` renders every state in
  light and dark at 1280×800 and 390×844.
