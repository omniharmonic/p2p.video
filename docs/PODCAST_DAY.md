# Podcast day runbook

How to host a recorded episode with the `meet-parity` build. Written 2026-10-25.
Everything below was verified with fake devices on one machine unless it says
otherwise. Read **Not yet proven** before you rely on a feature for a real guest.

## 1. Before the day (once)

1. **Build the app** from `meet-parity`:
   ```sh
   STUDIOLINK_MEDIA_TOOLS="$PWD/build/media-tools" scripts/package-mac.sh
   ```
   This gives `build/p2p.video.app` (ad-hoc signed without Developer ID
   variables). Quit any older p2p.video first. The app refuses to reuse a
   different build on port 7443 and tells you what to stop; it never kills it.
   Coming from StudioLink? Finish any take, quit the StudioLink app (or stop
   its `studiolinkd`), then open p2p.video: on first launch it moves
   `~/Library/Application Support/StudioLink` to
   `~/Library/Application Support/p2p.video` once (docs/DESKTOP_APP.md).
2. **Follow the setup guide** that opens on first launch (or Settings → General
   → Show Setup Guide…). It covers steps 3–5 below in order: Tailscale status,
   guest link, browser (choose Google Chrome), first room, then opens the
   dashboard signed in. Since Oct 25 the host dashboard needs a per-boot sign-in
   that the app passes automatically; typing `http://127.0.0.1:7443` by hand
   won't give host controls. Afterwards open it from the menu bar → Open
   Dashboard / Start a Meeting, or from a terminal: `bin/p2pvideo dashboard --open`.
3. **Grant camera and microphone** to where you host from. Either the browser
   (Chrome, the default and longest-tested path; the guide sets "Open the
   dashboard in"), or the p2p.video studio window (Settings → General → Studio
   → Open meetings in: A p2p.video window). The window keeps the call in the
   app, asks before closing during a take, keeps the Mac awake while recording
   and saves downloads to ~/Downloads. If you use it, do one 2-minute test take
   in it before the day: macOS asks once for camera and microphone, and for
   Screen Recording the first time you Present. Real devices in the window are
   not yet proven (see "Not yet proven").
4. **Choose how guests reach you** (setup guide → Guest access, or app →
   Settings → Guest access; each action shows the exact Tailscale commands and
   runs only when you click):
   - **Private link**: guests are on your tailnet (or you shared the machine
     with them). Proven on real devices on Oct 15–16.
   - **Public guest link** (Funnel): anyone with the link, no Tailscale needed.
     Media goes direct over UDP when possible, else through the built-in relay
     on Funnel :8443. Host controls are refused through Funnel. Not yet run on a
     real outside network (see below). Turn it off from the same pane after the
     episode if you don't want the machine reachable.
5. **Make a room** in the setup guide's "First room" step or on the home
   screen (**New → Create a room**, or the **Rooms** tab; e.g. "The Podcast")
   and send guests its link (**Copy link**). The link stays the same every
   episode. Rotate it if it leaks.
6. **Schedule the episode and invite guests** (home → **New → Schedule for
   later**, or **Schedule** above the day in **Meetings**; see `docs/integrations/CALENDAR.md`). Pick the room (or name a
   new one), the date, start and length, add the agenda and the guests' emails.
   Fill in your own email if you want the invite to work as a real invitation
   (guests can accept it, and their RSVPs go to your mailbox). After you save,
   send the invite in one of these ways:
   - **Email guests** opens your mail app with the invite text. Attach the
     **.ics** (Download .ics) so it lands in any calendar, or
   - **Add to Google Calendar** (or **Outlook**) puts it in your calendar.
     Invite the guests from there and Google sends the email, or
   - **Copy invite text** and paste it anywhere.
   The invite holds the room's guest link plus "use Chrome, wear headphones"
   tips. Nothing else is in it. If you change the time, use **Edit** and send
   the new invite: calendars update it in place. If the episode is off, use
   **Cancel meeting** and send the cancellation. On the day, **Start** next to the
   meeting in **Meetings** opens Ready to join for the room. In the Mac app, Settings → Calendar can remind you
   a few minutes before with a **Start meeting** button and add scheduled
   episodes to your calendar (both off until you turn them on; macOS asks for
   calendar access once). Not yet tried with real Google/Apple/Outlook
   imports.
7. **On the day**, the control center's **Podcast-day checklist** shows host,
   Tailscale, guest link (a read-only check that the link answers), disk space
   (~10 GB per hour) and power adapter. Fix anything orange or red, then press
   **Run a 2-Minute Test Call**. It can't read Chrome's camera/mic permission;
   the test call is how you find out.

## 2. Thirty minutes before

1. Plug the Mac into power. Close apps that use the camera.
2. Open the room (**Rooms → Start**), check yourself on **Ready to join**
   (the ring around the mic button moves when it hears you), then
   **Start The Podcast**.
3. Do a **2-minute test take** with a friend on a *different* network: both
   tiles show video, record, stop, and check Studio ✦ → **Uploads** says
   "All recordings saved on the host". This is the single most valuable check.
4. Ask guests to use **Chrome on a laptop**, plugged in, with **headphones**.
   Tell them: *if you wear headphones, tick "I'm wearing headphones" on Ready
   to join* — their recording mic is then **raw** (untouched, best for
   editing). Without headphones nothing to do: p2p.video records them
   **clean** automatically (echo cancellation + noise suppression, no
   automatic volume) so your voice from their speakers stays out of their
   track. The box is pre-ticked when the device name looks like headphones.
5. For an in-studio setup where everyone is on headphones or proper mics,
   set Settings → Recording microphone → **Always raw** (or **Always clean**
   for a speakers-only room). A guest's own explicit choice still wins.
6. Optional: **Invite a producer**: on the home, the room's ⋯ menu →
   **Copy producer link** (before the episode, like the guest link), or in
   the call's Studio ✦ → Invite producer. They get their own
   link (separate from the guest link; New link / Turn off in the same
   dialog; a room keeps the same producer link every episode). You admit
   them once. A producer is heard (camera off until they turn it on), is
   **never recorded** and not in exports or captions, and can admit or
   decline waiting guests, start/stop takes, mute and remove guests and see
   source health. They can't end the meeting, change settings or links,
   livestream, or act on you.

## 3. During the episode

- Guests who arrive early wait in a "waiting for host" screen and ask to join
  automatically when you start. Admit them from the banner, one at a time in
  the order they arrived; a waiting producer is offered to you first, and
  can then admit the guests.
- **Studio ✦ → Recording** says "Ready to record" or "Things to check before
  recording"; open it for each person's storage, devices and connection path.
  Red items ask "Record anyway?" before a take; read them.
- **Record** (in the dock) starts a recording on every device at the same moment.
  The top bar shows a red "Recording 12:04" pill and everyone sees a
  "Recording" note. Each person records their own camera and mic locally at full
  quality; the call itself is downscaled and never affects the recording.
- Turn on **Studio-quality audio** under **Recording options** on Ready to
  join (before joining) for separate uncompressed audio per guest. Off by default.
- Signal bars on each tile show connection quality. Under bad conditions the call
  lowers its own quality and uploads slow down so the conversation stays smooth;
  nothing recorded is dropped (it waits in the guest's browser storage).
- **Host mute** only mutes someone in the call; their isolated recording keeps
  going (they're told). Self-mute stops both.
- Camera/mic switching is locked while recording, on purpose.
- **Background blur / replace** (the sparkle button on the Ready to join preview,
  camera ▾ → Background effects, or ⋯ → Background effects): Light blur, Strong blur or a generated background. It changes only
  what others see in the call; **your recording keeps your real camera**. If a
  slow computer can't keep up the effect turns itself off and says so. The
  choice is remembered on that computer.
- **Recording microphone**: Studio ✦ → Recording and People → Connection &
  recording show each
  person's mode: "Raw mic (headphones)", "Clean mic (no headphones)",
  "… (host setting)" or "… (chosen)". It can change between takes only
  (mic ▾, or ⋯ → Devices → Recording microphone); a change you make in Settings during
  a take applies when it stops. The editor package's `manifest.json` says
  per audio track whether it was raw or clean (`micProcessing`).
- **Call audio** (mic ▾, or ⋯ → Devices): noise suppression, echo cancellation and
  automatic volume apply to what others hear. With a raw recording mic they
  are independent of the recording. With a clean recording mic, Chrome lets a
  microphone have only one processed capture, so the call uses the same
  clean processing and these switches are greyed out (wear headphones to set
  them separately).
- **Phone as an extra camera**: anyone in the call opens Studio ✦ → **Add phone
  camera** and scans the QR code with their phone (the link works once, for 10
  minutes; no second admission). The phone shows as "<Name> · Phone", records
  its camera and room mic during takes (also if added mid-take) and uploads
  under that person; the export has it as their extra "Phone" angle. Keep the
  phone page open until it says the upload finished. The QR code needs your
  Tailscale (Serve/Funnel) address: a code made on `localhost` can't be opened
  by a phone, and the dialog warns about that.
- **Guests can leave** with Leave call. If their recording is still uploading
  they are asked first; the upload keeps going while their tab stays open, and
  **Rejoin** brings them back without you admitting them again. The host's red
  button is **End meeting for everyone** (a meeting can't run without its host).
- **Live captions** (CC button or `c`): the host turns them on; speech is
  recognized on this Mac by the p2p.video app (Apple on-device speech), labelled
  per speaker, and never leaves the machine. Everyone sees a "Captions on"
  notice. Download the caption transcript (.vtt/.json) from Studio ✦ →
  Download captions, or at meeting end.
- **Teleprompter** (Studio ✦ → Teleprompter): write or paste your intro, ad reads
  or questions; `# Heading` and `## Sub-heading` lines stand out, line breaks
  are kept. It saves as you type; a room keeps its script for the next
  episode. **Who can see the script**: *Only me* (default), *Host +
  producers* (producers can then also edit) or *Everyone* (guests can read,
  never edit). **Open reader** puts it at the top of the screen, right under
  the camera, so your eyes stay near the lens. Click into the text, then
  **Space** plays/pauses and the **arrow keys** move a line; − / + set the
  speed, A− / A+ the size, **Mirror** is for beam-splitter glass. Turn on
  **Share scroll** and anyone who opened the reader follows your position
  (they can turn **Follow host** off).
- **Branding & titles** (Studio ✦ → Branding & titles): show name, accent colour and
  a logo (PNG or JPEG, up to 1 MB; SVG isn't accepted) for the room, and
  each person's name and title ("Ana Ruiz", "Founder, X"). A producer can
  edit the titles. **Name and title labels on call tiles** shows them to
  everyone in the call. In **Go live**, the **Branded layout** switch adds the
  background, show name and logo, and the names under **Lower thirds** show
  someone's lower third on demand (or automatically when they start
  speaking, if Branding says so). Recordings stay clean; the titles are in
  the editor package's `manifest.json` (`participants`). In a room, titles
  are remembered by the name people join with: next episode they're filled
  in automatically. **Clear** next to someone removes their title here and
  for future episodes; **Home → Rooms → ⋯ → Saved titles…** lists, edits,
  adds and forgets them between episodes.
- If someone's video freezes it recovers on its own (new connection, recording
  continues). If the host app restarts, everyone reconnects within seconds.

## 4. After the episode

1. **Stop recording**. Guests see "Uploading 1 of 2" in Studio ✦ (a
   percentage on the screen after Leave call); ask them to keep the tab open
   until People → Connection & recording says "Saved on the host" for them
   ("Still uploading, so keep the meeting open" lists anyone left).
2. If a guest closes the tab early, they reopen the same link later from the
   same browser and the rest uploads automatically.
3. Studio ✦ → **Get files for editing** (in Uploads) → **Download files for editing**: per-person video and WAV,
   FCP7 XML for Resolve/Premiere (native-tested in Resolve), experimental
   FCPXML, and `manifest.json`. The package README says, per person, whether
   their sync was **verified** or **corrected** against the host's copy of their
   microphone, or is clock-based only; check anyone marked "NOT verified" by ear. A 30-minute 3-person take takes ~7 min to export
   as H.264 on an M2 Max (the hardware encoder is the limit). Two faster options:
   **Download raw files** in Uploads gives everyone's raw files plus
   `offsets.json` as soon as uploads finish, while the export runs; or set
   Settings → Editor video format to **ProRes Proxy** (~4× faster with 3
   cameras, files 2–9× larger).
4. Optional: **Transcribe** (each person's own track, so every line has the
   right name), **Send to Parachute**, clip-editor handoff (see
   `docs/integrations/`). The default engine, **Built-in (this Mac)**, runs in
   the p2p.video app with Apple's on-device speech, so keep the app open and
   Settings › Transcripts on. A 60-minute track takes about 1.5 minutes. The
   panel shows each person's progress; **Cancel** writes nothing, and
   **Retry remaining tracks** keeps the ones that finished. Minutes can be
   picked instead if a compatible `minutes-cli` is installed.

### A guest who can't make the call: a recording link

For an intro, a testimonial or a segment someone records on their own time:
**Home → New → Ask someone to record on their own** (or **Recordings → New
record link**). Give it a title, paste a script if you want
one (they see it while recording), pick how long a take may be and how long
the link works, then send the copied link. The guest checks their camera and
mic, records as many times as they like, deletes the bad ones and presses
**Send**. Nothing is live and you don't need to be online. Their recording
appears under the link in Recordings; **Export for editor**, **Originals** and
**Transcript & handoffs** work as for a call take. If they close the tab
early, they reopen the same link in the same browser and the upload finishes.
Use **⋯ → Replace link** or **Turn off link** if it leaks; recordings already
sent stay.

Guests sometimes start and never send. Those drafts show up under
**Abandoned drafts** at the top of Recordings once they've had no activity
for a day (or their link expired or was turned off), with their size;
**Delete** removes one, **Clean up abandoned** removes them all. p2p.video
also deletes them by itself (at start and daily) after 14 days without
activity, or a day after their link expires or is turned off. A draft with an
upload or a take in the last hour is never deleted, and recordings that were
sent are never touched. Change the defaults with the daemon flags
`--record-draft-expiry-days` (0 turns automatic cleanup off),
`--record-draft-closed-link-hours` and `--record-draft-upload-guard-minutes`.

## Not yet proven (do not assume)

- Public link through real Funnel from an outside network, including the TURN
  relay on :8443. Do the test take in step 2.3 first.
- Livestreaming to a real platform. Since SL-P3-3 the daemon publishes
  RTMP/RTMPS itself (ffmpeg only encodes), and the presets use `rtmps://`
  for YouTube, Twitch, Facebook and LinkedIn. Verified only against local
  sinks (RTMPS through a local TLS proxy, reconnect after a sink restart).
  Do one private/unlisted test stream on each platform before announcing one.
- Built-in transcripts of human voices on real microphones: recognition was
  verified on synthetic `say` speech (a 62-minute file, and a two-person
  take recorded through the call with that speech as both mics). On
  macOS 14–25 the built-in engine needs Speech Recognition allowed once
  (Settings › Transcripts › Allow); that path is untested.
- The optional Minutes engine needs Speech Recognition permission granted once,
  and a `minutes-cli` built from the Minutes branch `feat/transcribe-file-json`.
- Clean vs raw recording mics on real laptop speakers and real headsets (how
  much bleed clean removes, and how it sounds in the edit), and the headphone
  label hint on real devices (Chromium fake devices only). The producer role
  with real people over Funnel.
- Background effects on a real person (synthetic patterns only), the phone
  camera on a real iPhone/Android over Funnel, and call noise suppression on
  real microphones (Chromium fake devices only).
- The p2p.video studio window with a real camera, microphone and screen share
  (its automated probe uses WebKit mock devices). Closing it mid-take and
  reopening resumes uploads in the probe; with real devices, still do the test take.
- Real-device acceptance of screen share, a second camera, and
  hour-long real-device sync. Alignment is clock-based with an
  acoustic check against the host's copy of each microphone (within ±20 ms,
  Chromium-calibrated); it is not sample-accurate against the physical capture.
- Safari and Firefox guests on real devices. Tested only synthetically
  (SL-P1-12): a WebKit 26.5 and a Firefox 151 guest join, see and hear
  everyone, chat, present-view, blur, record (Safari MP4, Firefox WebM) and
  export, and an iPhone-sized WebKit window lays out and records in a private
  window; but Safari's camera and microphone were a synthetic page stream, no
  real iPhone/iPad was used, and iOS autoplay was simulated. Firefox guests
  get no speaker picker unless Firefox lists output devices; Safari in a
  private window keeps no local copy (the green room says so). If a guest can
  use Chrome, prefer it for the first real episode.

## If something goes wrong

| Symptom | Do this |
|---|---|
| Guest stuck on "waiting" | Check the admission banner; check the room isn't locked (people panel) |
| Guest can't connect at all on public link | Confirm `tailscale funnel status` lists :443 and :8443; try the private link |
| App says another p2p.video is running | Stop that host yourself (finish any active take first), reopen the app |
| Export blocked | Studio ✦ → Uploads lists whose recording is missing; have them reopen the link from the same browser |
| Anything else | `~/Library/Application Support/p2p.video/daemon.log` |
