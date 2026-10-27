p2p.video — Getting Started
===========================

Host: Apple Silicon Mac, macOS 14 or later.
Guests: a current Safari or Chrome browser with camera/microphone access.

INSTALL
1. Open the p2p.video disk image and drag p2p.video to Applications.
2. Open p2p.video from Applications. It starts the host, adds a p2p.video
   icon to the menu bar and shows the p2p.video control center.
3. The first time, the "Set Up p2p.video" guide opens. It checks Tailscale,
   lets you pick a private or public guest link (it shows the exact Tailscale
   commands and runs them only when you click), picks the browser for the
   dashboard (Google Chrome recommended), creates your first room, then opens
   the dashboard. Every step can be skipped; reopen it any time from
   Settings → General → "Show Setup Guide…".
4. Follow the device check. Do not start recording until everyone is ready.
5. Before each episode, look at the control center's "Podcast-day checklist"
   (host, Tailscale, guest link, disk space, power) and run its 2-minute test
   call.
6. Optional: Settings → General → "Open p2p.video at login".

FFmpeg and ffprobe are included. No Homebrew, Node.js or Go installation is
needed. The app and disk image are Developer ID signed and notarized when
built for release. Media tools' source, build instructions and licenses are
inside p2p.video.app → Show Package Contents → Contents → Resources.

MEETINGS AND ROOMS
The dashboard opens on Home. "Start new meeting" starts an instant call.
Create a room (for example "The Podcast") to get one guest link that works
every time you open that room; guests can bookmark it. Early guests wait
until you start, then ask to join automatically. Copy a room's link from
Home or from the menu bar → Rooms. "Replace guest link" retires the old link
immediately. Recording is a button inside the call; End meeting closes it.

CONNECT A REMOTE GUEST
1. Install Tailscale from https://tailscale.com/download on the host and guest
   devices. Each person signs into their own account.
2. On the host Mac, open p2p.video Settings → Guest Access. It checks that
   Tailscale is installed, signed in, and has MagicDNS and HTTPS certificates.
   Choose "Private link for tailnet guests". p2p.video shows the exact
   Tailscale command and runs it only when you click Run These Commands.
   This is private Serve, not public Funnel.
   Guests without Tailscale: "Public guest link (anyone with the link)" uses
   Tailscale Funnel instead (see PUBLIC_GUESTS.md). Your tailnet policy must
   allow Funnel. You can turn it off again from the same pane.
3. In Tailscale's admin console, share the host Mac with the guest. The guest
   accepts that machine share under their account and connects Tailscale.
4. The guest first opens the host's HTTPS server check. If Safari cannot find
   the server, enable Tailscale DNS on their device and MagicDNS on their
   account's tailnet, reconnect, and retry. A studio invitation cannot grant
   private-network access.
5. Copy a studio invitation from the dashboard. Send it to your guest. They
   check their devices and request admission; you approve them in the studio.
   Keep the local host dashboard private. Invitations contain session access;
   do not post them publicly.

RECORD AND EXPORT
Before joining, select the desired camera resolution and optionally enable
Uncompressed microphone track. The preview reports the negotiated resolution.
Record a short test, stop, and wait for each participant's upload receipt.
Keep the phone's Safari page foreground and its site data until receipt.
Process the take, then download its editor package. Preserve original media.
Import timeline.fcpxml in Resolve, relinking media to the package's media folder
when prompted. Camera angles and isolated microphones remain separately
editable. Resolve multicam import was tested; Final Cut/Premiere import still
needs independent verification. Review synchronization before important work.

YOUR DATA AND RUNNING HOST
Recordings and logs live in ~/Library/Application Support/p2p.video.
Coming from StudioLink (the app's previous name)? Finish any take and quit
StudioLink first. On its first launch p2p.video moves the StudioLink folder
to the p2p.video folder once (a single rename, nothing copied) and keeps your
rooms, recordings and settings. While StudioLink is still running, p2p.video
does not start and tells you to quit it. You can then delete StudioLink.app.
Closing the browser does not stop the host; it keeps uploads available.
The menu bar icon shows whether the host is running, recording, live, or has
a guest waiting. If the host stops unexpectedly, p2p.video restarts it, but
never while a take is recording.
Quit p2p.video from its menu bar icon to stop the host. If a take is
recording, a livestream is running, or uploads are pending, it asks first.
Reopening the same app reuses its host. An old host on port 7443 is never
silently replaced: p2p.video tells you, and you finish active sessions and
stop it yourself before updating.
Headless fallback: zsh /Applications/p2p.video.app/Contents/Resources/launch-mac.sh
starts the host with the previous launcher.

VALIDATION LIMITS
Separate-account iPhone joining/reconnection, Full HD video, isolated WAV
upload and Resolve multicam import passed. Short real-device checks and
synthetic two-minute drift tests do not guarantee hour-long synchronization.
Installation on an unrelated clean Mac remains to be checked.
