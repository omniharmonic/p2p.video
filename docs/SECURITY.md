# p2p.video security model [SL-SEC-1]

This document describes who p2p.video trusts, what each credential allows,
which controls enforce that, and the risks that remain. It covers the review
findings fixed in SL-SEC-1 (C1, H1, M1–M4, L1–L6, CSP). For the networking
setup see `PUBLIC_GUESTS.md`; for the Mac app see `DESKTOP_APP.md`.

## Reporting a vulnerability

Please report security problems privately through GitHub's private vulnerability
reporting: open https://github.com/omniharmonic/p2p.video/security/advisories/new
(the repository's **Security** tab → **Report a vulnerability**). Do not open a public
issue, pull request or discussion for a suspected vulnerability.

Include what you found, the affected version or commit (Settings → General → About in
the Mac app shows the build), steps to reproduce and the impact you expect. Please do
not include real recordings, invite links or tokens; redact them. We acknowledge
reports through the advisory, agree on a fix and disclosure timeline there, and credit
reporters who want it once a fix is released. Only the latest release is supported
with security fixes.

## Assets

- **Recordings**: raw chunks and assembled tracks under `<data-dir>/sessions/…`,
  exports under the export directory.
- **Host credentials**: host tokens (HMAC, 24 h; 2 h from the home screen),
  the daemon signing key (`.secret_key`), the desktop token (`.desktop_token`)
  and the per-boot launch token (`.launch_token`).
- **Guest credentials**: invite tokens (stored as SHA-256), room-link secrets
  (HMAC of a nonce, never stored), participant grants (HMAC, 12 h), TURN
  credentials (HMAC, 6 h).
- **Privacy**: names, rosters, chat, captions, the tailnet name and IPs.
- **Livestream keys** (memory only).

## Actors and trust boundaries

| Actor | Reaches the daemon via | Trusted with |
|---|---|---|
| Host on this Mac | Direct loopback (`127.0.0.1:7443`) | Everything, once signed in (launch cookie or desktop token) |
| Mac app, `p2pvideo`, launcher | Direct loopback | Desktop token, launch token (0600 files) |
| Owner over private Serve (opt-in `--owner-tailscale-login`) | Tailscale Serve → loopback, identity headers | Management, by Tailscale identity |
| Invited guest | Funnel or Serve → loopback | Waiting room only, until the host admits them |
| Admitted guest | Same | Their own media, uploads and display name |
| Anyone with a leaked link | Funnel (public internet) | Same as an invited guest: a join request |
| Other local users and processes | Loopback | **Nothing.** They can reach the port, but have no credential |
| Web pages in the host's browser | The browser | Nothing (CORS origin check, SameSite=Strict cookie, Host check) |

Funnel requests arrive from loopback with `Tailscale-Funnel-Request`; private
Serve requests carry `X-Forwarded-For` and identity headers. "Local" means a
direct loopback connection with a loopback `Host` header and none of those
headers (`localManagement`).

## Authorization levels (host)

- **managementAllowed**: network position. Local, or the configured owner
  over private Serve. Never Funnel. Host-token routes (`requireHost`) and
  host-token WebSockets require this **and** a valid host token. A host token
  presented from another tailnet device or through Funnel is refused (L3), so
  it must be both stolen and used from the host's own position.
- **adminAllowed**: credential. Routes that mint host tokens or reveal room
  links need it: `POST /api/v1/sessions`, `/api/v1/rooms*` (except the guest
  lobby), `/api/v1/home`, `/api/v1/doctor`, `/api/v1/tailscale/status`. On
  loopback that means the admin cookie or `Authorization: Bearer <desktop
  token>`. Over private Serve it is the owner identity (as before).

### Dashboard sign-in (launch token, M4)

Loopback alone is not a credential: any process or OS user on the Mac can open
a socket to `127.0.0.1:7443`. Before SL-SEC-1, `GET /api/v1/home` handed any of
them live host tokens and every room's guest link.

1. At every start the daemon writes 32 random bytes (hex) to
   `<data-dir>/.launch_token`, mode 0600, replacing the previous one. The data
   directory itself is 0700. The token is never logged.
2. The Mac app ("Open dashboard", "Start a meeting", room shortcuts, the in-app
   studio window), `scripts/launch-mac.sh`, `make dev` and `p2pvideo dashboard`
   read the file and open `http://127.0.0.1:<port>/?launch=<token>` (keeping
   any other query parameters).
3. On a GET navigation to a page (not `/api/`), from local management, with
   `Sec-Fetch-Mode` absent or `navigate`, and a matching token, the daemon sets
   `studiolink_admin_<port>`: a random per-boot value (not derived from the
   token), `HttpOnly`, `SameSite=Strict`, `Path=/`, session lifetime.
4. It always answers `303` to the same URL without `launch`, valid or not, so
   the token never stays in the address bar, history or a `Referer`.
5. Management requests from that browser carry the cookie automatically.
   The Mac app's own API calls use the desktop token instead.

A daemon restart invalidates both the launch token and all cookies. A page
opened without the token gets a 403 explaining how to sign in. Tests read the
token from their temporary data directory (`web/tests/lib/harness.mjs`
`launchURL`, `adminHeaders`).

Why a cookie and not a header: an HttpOnly cookie cannot be read by script, so
an injected script (if CSP were ever bypassed) cannot exfiltrate the admin
credential, only use it while the page is open. Why reusable for the boot
rather than one-time: several browsers/contexts (the in-app window, a browser,
tests) open the dashboard per run, and the only parties who can read the 0600
file are already the host user.

## Guest controls

- **Waiting room view only** (M2). `GET /api/v1/sessions/{id}`:
  invite/room-link token → `{session:{id,title}, settings}`; an admitted (or
  briefly disconnected) participant's grant → also `participants:[{id,name,role}]`;
  host token from a management position → full view with takes. Pending or
  denied grants and other sessions' grants get 401. `Session.InviteTokenHash`
  is never serialized (`json:"-"`).
- **Join rate limits** (M1), before the WebSocket upgrade and before any hub
  client or database row exists: token buckets per client address (burst 20,
  then 1 per 3 s) and per session (burst 60, then 1/s) → `429`. The client
  address is the first `X-Forwarded-For` entry on a loopback (Serve/Funnel)
  connection, otherwise the TCP peer.
- **Waiting-room cap** (M1): at most 20 pending join requests per session.
  The 21st is denied with reason `busy` and its socket closed, without a
  participant row or a join request to the host.
- **Hello timeout** (M1): a socket that has not sent `hello` within 10 s is
  closed and unregistered.
- **Link rotation drops the waiting room** (L4): each guest socket records which
  credential it joined with (room-link nonce or invite hash). Rotating or
  revoking a room link, or re-issuing a session invite, denies (`unavailable`)
  and closes every not-yet-admitted socket that used the old one. Admitted
  participants are unaffected.
- **Display names** (L5): Cc and Cf characters (including bidi overrides
  U+202A–202E and isolates U+2066–2069, zero-width characters, BOM) are
  removed, whitespace runs collapse to one space, the result is trimmed and
  capped at 80 runes. The same sanitizer covers host names and track labels;
  room names with Cf characters are rejected.
- **Recorder status** (C1, e3f6fe5): sanitized server-side and rendered as text.

## Uploads (H1)

- The chunk body is streamed to a private temp file and hashed **before** any
  lock is taken; only the commit (idempotency check, quota, rename, row) is
  serialized, per track. A slow upload no longer stalls anyone else's.
- Body deadline per request: 30 s + `Content-Length` at 128 kbit/s (half the
  client's own 256 kbit/s timeout assumption, so the client gives up first).
  Other request bodies get 30 s. WebSockets manage their own deadlines.
- `http.Server`: `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s,
  `MaxHeaderBytes` 64 KiB. No whole-request `ReadTimeout`/`WriteTimeout`
  (would cut slow uploads, WebSockets and large downloads).
- Limits, each with a clear message the client shows:
  - chunk size ≤ `MaxChunkSizeMB` (50 MB) → 413;
  - `seq` ≤ 1,000,000 → 413;
  - per-track quota = max(session video bitrate, 12 Mbit/s) × (take duration
    + 10 min) × 1.5 → 413 (a recording take's duration grows with the wall
    clock; a stopped take's is fixed);
  - ≤ 16 tracks per participant per take → 429 at registration.
- The client treats 413 as terminal, keeps the chunk in OPFS for recovery and
  shows the server's reason (`web/src/lib/upload.ts`).

## Branding logo and teleprompter script (SL-P5-1, SL-P5-2)

- **Logo upload is host management.** `POST/GET/DELETE /api/v1/sessions/{id}/branding/logo` need a host token from a management position (`requireHost`); the room variants need the admin credential (`adminAllowed`). Guest invites, room links, participant grants and producer links are refused, and every route is in the Funnel guardrail test.
- **No SVG.** An SVG is a document that can carry script. Only `image/png` and `image/jpeg` are accepted, and the declared type must match the file: the bytes must start with that format's signature (PNG `89 50 4E 47 0D 0A 1A 0A`, JPEG `FF D8 FF`) and Go's `image.DecodeConfig` must decode the header as that format at most 4096×4096. HTML, SVG or script behind a PNG signature fails the decode (415). At most 1 MB (`MaxBytesReader`, 413); a rejected upload leaves the stored logo unchanged.
- **Serving.** The stored type only (never sniffed), `X-Content-Type-Options: nosniff`, and a response CSP of `default-src 'none'; sandbox`, so even a mis-typed file opened directly can't run anything. The browser never puts the host token in a URL: the page fetches the logo with an `Authorization` header and shows it as a `blob:` URL (settings preview) or an `ImageBitmap` (livestream canvas), which the page CSP already allows.
- **Brand text** (show name, lower-third name/title) is sanitized like display names (invalid UTF-8, control and format characters such as bidi overrides removed, length-capped) and rendered with `textContent` or canvas `fillText`. The accent must be `#rrggbb` (validated on the server and again in the browser before it reaches CSS or the canvas).
- **Teleprompter script** is untrusted text: rendered with `textContent` only (a markup-looking line stays text; tested), capped at 64 KB, never logged (the daemon logs size and revision only; a unit test captures the logs). The hub sends the text only to those the host's visibility allows (host / host + producers / everyone) and clears it from a guest's page when visibility shrinks. Guests and phones can never edit (negative-tested over a forged socket message). Room scripts are stored in SQLite with the room (data directory 0700, database 0600); an instant meeting's script is memory-only.

## Async record links (SL-P5-3, ADR-017)

A record link lets anyone holding it record and send takes with no call. It reaches the daemon through Serve or Funnel like a guest invite.

- **Secret.** `HMAC-SHA256(daemon key, "studiolink/record-link/v1" ‖ link ID ‖ 128-bit nonce)`, 256 bits. The database keeps the nonce and SHA-256 of the secret only (`record_links`). Rotate replaces the nonce and refuses the old URL at once; revoke clears both; every link has an expiry (default 7 days, at most 90; the host can end it now). Every guest call checks the current secret in constant time. Any wrong, rotated, revoked or unknown link gets the same 401; only the correct secret of an expired link learns "expired" (410).
- **What it grants.** Only record-solo on that link: read the title, prompt and limits, start a submission, and work on that submission. A submission session refuses every WebSocket (link secret, participant grant and even a host token: `TestRecordLinkGuestFlowAndSecurity`), so a link can't join or create a call; the secret is not a credential for any meeting, room lobby or host route.
- **Guests can't see each other.** Starting a submission returns a 256-bit submission key (stored hashed, kept by that browser in `localStorage` per link). Reading, recording into, deleting from or submitting a submission needs the link secret **and** its key; anything else is 404. Uploads use that submission's participant grant (12 h), which the upload handler already binds to its own session, take and sources. The host view never contains key hashes or secret hashes.
- **Caps, on the server.** Submitted submissions ≤ `maxSubmissions` (one atomic SQL update); ≤ 5 drafts started per link per 24 h; ≤ 10 kept and ≤ 30 started takes per submission, one recording at a time; each take is stopped by the server at its limit + 2 s (timer, and a sweep after a restart), which fixes its per-track quota; total stored bytes per link ≤ `maxTotalBytes` (default 20 GB), checked in the upload handler under a per-link lock (413 with a reason the page shows). The per-track quota, chunk-size and sequence limits of calls still apply.
- **Rate limits.** Opening a link and starting a submission go through the join limiter (per address, per link), before any database write.
- **Management** (`/api/v1/recordlinks…`: create, edit, rotate, revoke, delete, delete a submission, list and clean up abandoned drafts) needs the admin credential, never Funnel; every route is in the Funnel guardrail test.
- **Abandoned drafts** (SL-P5-4) are deleted automatically after 14 days without activity (24 h on an expired or turned-off link), so a leaked link's unsent drafts don't keep media on disk forever. Deletion re-checks inside one transaction that the draft is still a draft with no take recording and no upload in the last hour; a submitted recording is never deleted automatically. Exports, originals and handoffs of a submission use the ordinary host-token take routes (2 h host tokens in the admin listing).
- **After rotation, revocation or expiry**, uploads of takes already recorded keep their grant and finish (so a guest's recording isn't lost); nothing new can be recorded or submitted. To stop a submission's uploads for good, delete the submission (its participant row goes, so its grant stops working).
- **The page.** The secret is removed from the address bar on load (kept in `sessionStorage` for reloads); prompt text is rendered with `textContent` only; no WebRTC runs on the page. Secrets and keys are never logged (the e2e test scans the daemon log).

Residual: anyone who obtains the link can record and send up to the caps until it expires or is replaced (as with a guest invite, the link is the credential); the submission key in `localStorage` is readable by script on the daemon's origin (the CSP forbids inline and third-party script).

## Calendar: invites and the host feed (SL-P6-1, ADR-018)

- **Scheduling and invites** (`/api/v1/schedule…`) are host management: the admin credential on loopback (launch cookie or desktop token) or the owner over Serve; never a host token alone, a guest link, another tailnet device or Funnel (`TestScheduleIsHostOnly`, Funnel guardrail test).
- **What an invite carries.** The room's guest link is the only credential in a `.ics`, the invite text, the Google/Outlook template links and `mailto:`. No host URL, host token, launch token or dashboard address (tested in Go and in `calendar.browser.mjs`). Opening "Add to Google Calendar" / "Add to Outlook" sends the title, time, agenda and guest link to that provider from the host's own browser; that is the host's choice, the same as pasting the link into Gmail. The links open with `rel="noopener noreferrer"`, and every page sends `Referrer-Policy: no-referrer`. Guest emails are checked against a conservative pattern so an address can't inject `mailto:` headers (`?bcc=`) or ICS parameters.
- **Feed secret.** `HMAC-SHA256(daemon key, "studiolink/calendar-feed/v1" ‖ 128-bit nonce)`, 256 bits; only the nonce and SHA-256 of the secret are stored (`calendar_feed`). Replace rotates the nonce (old subscriptions get 401 at once); Turn off clears it. If the daemon key changes, the view reports the address as lost. The key is in the URL because calendar apps can't send headers; it is compared in constant time and never logged (tested).
- **Feed position.** `/api/v1/calendar/feed.ics` is served only to a direct loopback request or to private Tailscale Serve with a tailnet `X-Forwarded-For` (100.64.0.0/10, fd7a:115c:a1e0::/48). A request carrying `Tailscale-Funnel-Request` gets 403 even with the right key; a LAN client or a public `X-Forwarded-For` gets 403 (`TestCalendarFeedTailnetOnlyRotatableNeverFunnel`). The feed lists every upcoming meeting with its guest link, so anyone holding the feed address can reach those rooms' waiting rooms. Treat it like the guest links themselves.
- **Mac app.** `/api/v1/desktop/calendar` needs direct loopback and the desktop token (no browser Origin, no Serve or Funnel headers) and returns room slugs, guest origins and meeting times only: no link secrets, no guest emails. EventKit matching reads only the slug and origin of a room link; the token is never stored. Write-back puts the invite text (with the guest link) into the calendar the user picked, so everyone who can see that calendar can reach the room's waiting room; it is off by default and the Settings text says so. The ledger `<data dir>/calendar-sync.json` is 0600 and holds event identifiers only.

Residual: a feed subscription on another tailnet device keeps the feed address in that device's calendar app; replace the address if a device is lost. Template links send meeting details to Google or Microsoft when the host clicks them.

## TURN (L1, L2)

- Removing, denying or ending the meeting calls `Hub.OnParticipantRevoked`,
  which closes that participant's live relay sockets immediately
  (`turn.Server.Revoke`, tracked by authenticated username). New allocations
  and refreshes are refused by `Authorize`, which now also refuses
  participants of ended meetings.
- Relayed datagrams may reach only the SFU's single loopback UDP mux port. If
  the mux could not bind a fixed port (busy, or `--udp-port 0`), TURN is not
  started or advertised, with a warning, instead of allowing any loopback port.

## Browser hardening

Every response carries:

```
Content-Security-Policy: default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self' blob:;
  connect-src 'self' ws: wss:; img-src 'self' data: blob:; media-src 'self' blob:;
  style-src 'self' 'unsafe-inline'; font-src 'self'; object-src 'none';
  frame-ancestors 'none'; base-uri 'none'; form-action 'self'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
Cross-Origin-Opener-Policy: same-origin
```

The UI is Vite output with one module script and no inline scripts, handlers
or `eval`. The AudioWorklet modules (`pcm-worklet.js`, `caption-worklet.js`)
are emitted as files (`assetsInlineLimit` excludes `.js`), so they load from
`'self'`; the compositor's timer worker is a `blob:` URL (`worker-src`).
`'unsafe-inline'` is for style attributes only. `'wasm-unsafe-eval'` (added
in SL-P1-7) is the one script-src extension: it lets the page compile
WebAssembly for on-device background blur (MediaPipe's runtime and model are
files under `/mediapipe/` on our own origin, never a CDN). It does not allow
`eval()`, `new Function()` or inline script; `'unsafe-eval'` stays off and
`TestCSPAndSecurityHeaders` pins script-src to exactly these two tokens. Checked: injected
`<img onerror>` and `<script>` do not run (CSP violations reported), and all
browser tests pass under the policy.

## Local data (L6)

- Data directory 0700 (created so and tightened on every start).
- Database created 0600 before SQLite opens it (so `-wal`/`-shm` inherit
  0600); an existing database is tightened.
- Chunk directories 0700, chunk and assembled files 0600, reference-audio
  directories 0700, sidecars 0600.
- Exports written to a user-chosen `--export-dir` keep normal permissions so
  Finder and editors can open them; the default `data-dir/exports` is inside
  the 0700 directory.

## Livestream keys

Stream keys are memory-only: never stored, logged, echoed in a response or
error, or passed to another process [SL-P3-3, ADR-019].

- **Not in `ps`.** ffmpeg only encodes; its argv
  (`ffmpeg.BuildLivestreamEncodeArgs`, golden-tested to contain no `://`) holds
  no destination or key. The daemon's own RTMP client sends the key inside the
  RTMP `releaseStream`/`FCPublish`/`publish` commands. Tested with
  `ps -o args=` on the running encoder (Go integration test) and on every
  daemon process plus the whole process list (`livestream.browser.mjs`).
- **Lifetime.** A key lives in the stream's destination list and in each
  publisher's target until the stream ends; `Stop`, auto-stop, "every
  destination failed" and daemon shutdown clear both. Keys are never written to
  SQLite, the data dir or the desktop state endpoint.
- **Redaction.** Status shows a destination as `scheme://host[:port]` only.
  Every failure reason (dial and TLS errors, server `_error`/`onStatus`
  descriptions, which some servers fill with the stream name, and ffmpeg
  stderr) passes through the stream's `Redactor` (key, escaped key, full
  publish URL, URL path and query) before it reaches status, logs or the UI.
- **Transport.** Presets use `rtmps://`. TLS is Go's `crypto/tls` with the
  system trust store (on macOS, the Security framework) and SNI/hostname
  verification; there is no option to skip verification. Plain `rtmp://`
  stays available for platforms and local servers that need it, and the panel
  labels it unencrypted. Facebook and LinkedIn presets refuse `rtmp://`.
- **Test-only trust.** `--livestream-test-ca <pem>` adds one extra root for
  rtmps destinations (end-to-end tests with a self-signed local ingest). It is
  added to the system roots, never replaces them, and the daemon logs a
  warning when it is set. It is not set by the app or the launcher.
- **Residual:** a process running as the host user can read the daemon's
  memory, like every other secret here (see "Same-user malware" below).

## Residual risks

- **Same-user malware** can read the 0600 tokens, the database and the
  recordings. p2p.video does not defend against code running as the host user.
- **Owner over private Serve** trusts Serve's identity headers on loopback.
  A local process can forge them; enabling `--owner-tailscale-login` therefore
  gives up the M4 protection against other local users. Off by default.
- **Cookie scope**: browsers share cookies across ports of `127.0.0.1`, so a
  page served by another local program on a different port would receive the
  admin cookie if the host visited it. It is per-boot and HttpOnly.
- **Rate limits are per process memory**: they reset on restart and a
  determined attacker with many source addresses can still send 60 join
  attempts per session per minute (each refused once 20 are waiting).
- **Quota floor** is generous (≥12 Mbit/s × (duration + 10 min) × 1.5 per
  track, 16 tracks): an admitted guest with bandwidth can still use disk up to
  that bound during a take.
- **TURN control connection**: revocation closes the relay sockets; the TCP
  control connection stays until the client closes it or its allocation
  expires. It can no longer carry media.
- **eval under CSP** cannot be exercised through Playwright (DevTools
  evaluation bypasses CSP); it is blocked by the policy (no `'unsafe-eval'`).

## Tests

Go (`go test ./...`):

| Finding | Tests |
|---|---|
| H1 | `TestH1SlowUploadDoesNotBlockOthers`, `TestH1ChunkAndSequenceLimits`, `TestH1TrackQuota`, `TestH1TracksPerParticipantCap` (upload); `TestH1HTTPServerBoundsSlowClients` (cmd/p2pvideod) |
| M1 | `TestM1JoinRateLimitsAndWaitingRoomCap`, `TestM1LimiterRefills`, `TestM1SilentSocketIsClosed` |
| M2 | `TestM2SessionDetailIsTrimmedPerCredential` |
| M3 | `TestM3TailscaleStatusHostOnlyAndCached` |
| M4 | `TestM4ManagementNeedsAdminCredential`, `TestLaunchTokenNeverLogged` (httpserver); `TestLaunchTokenIsOwnerOnlyAndPerBoot` (config) |
| L1 | `TestL1RevokeCutsLiveAllocation` (turn), `TestL1DenyRemoveAndEndRevokeRelayAccess` (signaling) |
| L2 | `TestRelayPeerFilterFailsClosed` (cmd/p2pvideod) |
| L3 | `TestL3HostTokenSocketNeedsManagementPosition`, `TestManagementAuthorization` |
| L4 | `TestL4RotatingLinksDeniesWaitingGuests`, `TestL4ReissuingInviteDeniesWaitingGuests` |
| L5 | `TestL5DisplayNamesLoseControlAndBidiCharacters` |
| L6 | `TestL6UploadFilesAreOwnerOnly` (upload), `TestL6DatabaseIsOwnerOnly` (store) |
| CSP | `TestCSPAndSecurityHeaders` |
| Calendar (SL-P6-1) | `TestScheduleInviteLifecycle`, `TestScheduleValidationAndRoomState`, `TestScheduleIsHostOnly`, `TestCalendarFeedTailnetOnlyRotatableNeverFunnel`, `TestDesktopCalendarMatchingDataHasNoSecrets`, `TestFunnelGuestJoinsWhileManagementStaysDenied` (httpserver); `TestMigration6AddsSchedulingToVersion5Fixture` (store); golden ICS tests (calendar) |
| Record links (SL-P5-3) | `TestRecordLinkGuestFlowAndSecurity`, `TestRecordLinkRotateRevokeExpire`, `TestRecordLinkDurationAndStorageCaps`, `TestRecordLinkDraftCapAndRateLimit`, `TestRecordLinkFunnel`, `TestFunnelGuestJoinsWhileManagementStaysDenied` (httpserver); `TestMigration5AddsRecordLinksToVersion4Fixture` (store) |
| Livestream keys (SL-P3-3) | `TestBuildLivestreamEncodeArgs1080pNoGPLAndNoDestinations` (ffmpeg); `TestLifecyclePipesBytesPublishesAndKeepsKeysOutOfArgv`, `TestDestinationsFailAfterMaxAttempts`, `TestRTMPPublishRejected`, `TestIntegrationRTMPSPublishKeyNotInArgv` (`ps -o args=`), `TestIntegrationRTMPSRejectsUntrustedCertificate` (system trust store, wrong host) (livestream); `TestLivestreamRoutesHostOnlyAndNeverEchoKeys` (httpserver); `web/tests/livestream.browser.mjs` (no process shows the key, logs and status key-free) |

Web: `recording.test.mjs` (413 reason surfaced, chunk kept), `collab.test.mjs`
(`busy` denial text). Mac: `testDashboardLaunchURLs`, `testReadLaunchToken`.
Launcher: `scripts/launch-mac.test.mjs` (signed-in URL, token not printed).
