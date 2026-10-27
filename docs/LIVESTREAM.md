# Livestreaming (ROADMAP P3) [SL-P3-1, SL-P3-3]

p2p.video can send the call live to YouTube, Twitch, Facebook, LinkedIn, or any custom RTMP/RTMPS server. You can send to as many as four destinations at once. Each destination connects, fails and reconnects on its own. Livestreaming runs separately from recording: it never reads from or writes to the recording, upload or post-processing paths, and recording never depends on it (ADR-014, ADR-019).

## How it works

```
host browser                              host daemon (p2pvideod)                                       platforms
┌─────────────────────────────┐ binary WS ┌──────────────────────────────────────────────────────┐ RTMPS ┌─────────┐
│ #video-grid tiles ─► canvas │ (~1 s     │ livestream.Manager                                   │ ────► │ YouTube │
│ compositor (grid / speaker /│  webm     │  ─► ffmpeg stdin  (h264_videotoolbox + aac)          │       └─────────┘
│ side-by-side, name labels)  │  chunks)  │      stdout: FLV ─► FLV tag splitter ─┬─► publisher 1 ┤ RTMPS ┌─────────┐
│ Web Audio mix of all tracks │ ────────► │      fd 3: -progress ─► status        ├─► publisher 2 ┼─────► │ Twitch  │
│ ─► MediaRecorder (1 s slice)│ ◄─status─ │      stderr ─► redacted health        └─► publisher n ┤       └─────────┘
└─────────────────────────────┘ (GET poll)│  (each publisher: own queue, own RTMP(S) connection, │ ────► custom
                                          │   own reconnect backoff; keys never leave the daemon)│
                                          └──────────────────────────────────────────────────────┘
```

1. **The host's browser composites the call, so viewers see what the host sees.** `web/src/lib/compositor.ts` draws each participant's `<video>` element onto a 1280×720 or 1920×1080 canvas, using cover cropping and a name label on each tile. Three layouts are available:
   - **Grid:** near-square cells, with a short last row centred.
   - **Speaker:** the loudest participant fills the stage, chosen with a 1.5 s hold so it doesn't flicker, and everyone else appears in a filmstrip below.
   - **Side by side:** one row of 16:9 tiles.

   The audio is a Web Audio mix of every participant's track plus the host microphone. Nothing is played back locally, so the mix doesn't echo. A worker timer keeps drawing frames even when the host switches to another tab.
2. **The browser sends the composited stream to the daemon.** `canvas.captureStream(30)` and the mixed audio track feed a `MediaRecorder` with a 1 s timeslice, and each chunk goes to the daemon as a binary frame on `GET /api/v1/sessions/{id}/livestream/ingest`. If `ws.bufferedAmount` passes 4 MB, the panel shows "Network can't keep up". At 24 MB the stream stops with an error instead of buffering without limit.
3. **ffmpeg encodes once.** `host/internal/livestream` pipes the frames into one ffmpeg process per session. The argv comes only from `ffmpeg.BuildLivestreamEncodeArgs` (golden-tested) and is never run through a shell. It contains **no destination and no stream key**:
   - Input is read from `pipe:0`, with probing limited to 1 s.
   - Video is scaled and padded to the preset, converted to constant 30 fps, and encoded with **`h264_videotoolbox`** (never libx264). The preset bitrate is used as both target and maxrate, with a 2× buffer and a keyframe every 2 s (`-g 60` plus `force_key_frames`).
   - Audio is encoded with ffmpeg's native **`aac`** encoder at 160 kb/s, 48 kHz stereo.
   - Output is one FLV stream on `pipe:1` (`-flvflags no_duration_filesize -flush_packets 1`). `-progress` goes to file descriptor 3, a pipe the daemon passes in.
4. **The daemon publishes to every destination itself [SL-P3-3].** `flv.go` splits ffmpeg's output into FLV tags and keeps the metadata and the H.264/AAC sequence headers of the current encoder run. Every media tag goes to each destination's **publisher** (`publisher.go`) without ever blocking:
   - A publisher connects only once the encoder has media, using the daemon's RTMP client (`rtmp.go`): simple handshake, `connect`, `releaseStream`, `FCPublish`, `createStream`, `publish` (the key is the publish name), then `@setDataFrame onMetaData`, both sequence headers, and media from the next keyframe with timestamps starting at 0. `rtmps://` is the same inside Go's `crypto/tls`, verified against the system trust store with SNI set to the host.
   - **Reconnect.** If the connection drops, a write times out (10 s) or the server refuses, that publisher alone retries after 1 s, 2 s, 4 s … up to 30 s between tries, and resumes at a keyframe with fresh headers. After 8 consecutive failures it is **failed**; a connection that stayed live for 30 s resets the count. When every destination has failed, the stream ends as failed.
   - **Isolation.** Each publisher has its own queue. If one falls more than 4 s behind (a slow uplink to that platform), it drops what it has queued and skips to the next keyframe, and the panel says so. The encoder and the other destinations never wait for it.
   - **Encoder restarts.** When the browser reconnects (a new MediaRecorder means a new encoder run), the platform connections stay up: each publisher sends the new sequence headers and continues one frame after the last timestamp it sent.
5. **The daemon reports health.** ffmpeg's `-progress` lines (frames, fps, speed, dup/drop, out_time) become `Status.Progress`. The daemon measures browser→daemon throughput (`ingestKbps`) and the encoder's FLV output (`outputKbps`) over a 5 s sliding window; `bitrateKbps` is the output rate once there is output (`bitrateSource: "output"`), else the ingest rate. Each destination reports its own state, sent rate (`kbps`), bytes, backlog, skipped frames, reconnect count and, while reconnecting, the attempt and seconds to the next try. `warnings` collects short, redacted notes: the encoder running below 0.95× real time for 3 s, 15 or more dropped frames in 5 s, a destination reconnecting, failed, behind or skipping. ffmpeg's stderr goes through a `Redactor` before it is logged.
6. **Lifecycle:**
   - `POST` validates the request and registers the stream (`waiting_ingest`).
   - ffmpeg starts when the ingest socket connects (`starting`), and the stream becomes `live` at the first progress block with frames. Destinations go `pending` → `connecting` → `live`.
   - If the ingest connection drops, the stream goes to `reconnecting`. The browser makes up to 3 attempts, each with a fresh MediaRecorder, and each reconnect starts a new ffmpeg process. If nothing reconnects within the 15 s grace period, the stream stops automatically.
   - A stream that never receives ingest stops after 60 s.
   - `DELETE`, or daemon shutdown, closes ingest, gives ffmpeg 5 s to flush, and closes every RTMP connection (`deleteStream`, then disconnect).

## HTTP API (host management only)

| Method | Path | Body / result |
|---|---|---|
| GET | `/api/v1/sessions/{id}/livestream` | `Status`: state, preset, bytesIn, ingestKbps, outputKbps, bitrateKbps, bitrateSource, progress, warnings[], destinations[{name, server, state, error, reconnects, attempt, maxAttempts, retryInSec, kbps, bytesOut, backlogMs, droppedFrames, behind}], lastError |
| POST | `/api/v1/sessions/{id}/livestream` | `{"preset":"720p30"\|"1080p30","destinations":[{"name","url","key"}]}`. Errors: 400 invalid, 409 already active, 503 unavailable |
| DELETE | `/api/v1/sessions/{id}/livestream` | Final `Status`; 404 if nothing is active |
| GET (WS) | `/api/v1/sessions/{id}/livestream/ingest?token=` | Binary MediaRecorder frames; 409 if not started or another ingest is connected |

Destination `state` is `pending` (waiting for the encoder's first media), `connecting`, `live`, `reconnecting`, `failed` or `stopped`.

Every route uses `requireHost`, the same check as settings PATCH, invite and post-processing. That means a host token for this session, plus `managementAllowed` (local loopback dashboard or the owner's private Serve identity). Requests through Funnel are always denied, and `funnel_guest_test.go` covers all four routes.

## Stream keys are secrets

- Keys are held only in the daemon's memory for the life of one stream. They are never written to SQLite or the data dir, and they are cleared when the stream ends.
- **Keys never reach another process, so they never appear in `ps`** [SL-P3-3]. ffmpeg's argv has no destination at all; the daemon's RTMP client sends the key inside the RTMP publish commands. The Go integration test reads the running encoder's argv with `ps -o args=`, and the browser suite checks every daemon process and the whole process list.
- Responses and status never contain a key or a full URL. Destinations are shown as `scheme://host[:port]` only.
- Validation errors never echo the URL or key. Server error descriptions (some servers repeat the stream name), connection errors and ffmpeg stderr are redacted before they are logged, stored or shown. Redaction covers the key, its escaped forms, the full publish URL, and the URL path and query.
- URLs and keys must be printable ASCII without spaces; only `rtmp://` and `rtmps://` are accepted, and a server URL needs an application path.
- **Verification is never skipped.** `rtmps://` uses the system trust store and checks the host name. Tests trust a self-signed local ingest only through `Options.TLS` (Go) or the daemon's test-only `--livestream-test-ca <pem>` flag (browser suite), which adds one root to the system roots and logs a warning.

## Presets and limits

| Preset | Video | Audio | Recommended upload |
|---|---|---|---|
| 720p30 (default) | 1280×720, 3 Mb/s, keyframe every 2 s | AAC 160 kb/s 48 kHz | ≥ 5 Mb/s per destination |
| 1080p30 | 1920×1080, 4.5 Mb/s, keyframe every 2 s | AAC 160 kb/s 48 kHz | ≥ 7 Mb/s per destination |

The browser→daemon hop is loopback, so congestion warnings there point to the host Mac being overloaded. The uplink to each platform is the publisher's job: a destination that can't keep up shows "N s behind" and then "skipping frames" in the panel. Watch the encoder **Speed** too: if it stays below about 0.95×, the panel warns that the Mac can't encode in real time.

Other limits:
- **Destinations.** At most 4, sharing one encode, each with its own connection and queue.
- **Layout.** The compositor uses the on-screen tile order. Branding and lower thirds are below (SL-P5-2); there is no free-form layout editor.
- **Container.** The browser sends WebM (H.264/Opus, else VP8/Opus). MP4 is used only where WebM recording isn't available (Safari). Chromium's fragmented-MP4 output reached ffmpeg in bursts, which added about 5 s of latency.
- **Compositor lifetime.** The host's browser tab must stay open, because it is the compositor. A worker timer keeps it drawing when the tab is in the background, but a sleeping machine stops it.
- **RTMP features.** H.264/AAC in classic FLV only (what the encoder produces); no enhanced RTMP (HEVC/AV1), no RTMPT/RTMPE, and only the simple RTMP handshake, which the major ingests accept from OBS-style encoders (unverified here; see Testing).

## Branding and lower thirds (SL-P5-2)

The livestream panel has a **Branded layout** switch and a **Lower thirds** row. Both can change while live.

- **Branded layout** is a variant of every layout preset (grid, speaker, side by side). The program gets a background gradient from the brand accent, a top band (10% of the height) with the show name and an accent tick on the left and the logo on the right, and the tiles are laid out in the stage below the band with a small side and bottom inset. Unbranded, tiles fill the canvas as before.
- **Lower thirds** are drawn inside the participant's tile, bottom left: an accent strip, the name and the title, sliding and fading in over 0.45 s, held 6 s, then out. The brand's mode decides when: **speaker** (default) shows a person's lower third when they become the active speaker (the same 1.5 s hysteresis as the speaker layout, in every layout) and not again for 45 s; **manual** only when the host presses a name in the panel; **off** never. Pressing a name always shows it at once. While a tile's lower third is on air its plain name label is hidden.
- Text and look come from **More → Branding & titles** (`docs/ARCHITECTURE.md` §5.2): show name, accent, logo, mode and each person's name and title. The panel's brand updates live; nothing about branding is configured inside the livestream request, and the daemon/ffmpeg side is unchanged.
- **The logo stays same-origin.** The host's page fetches `GET /api/v1/sessions/{id}/branding/logo` with the host token in the `Authorization` header and decodes the bytes into an `ImageBitmap` for the canvas, so the page CSP (`img-src 'self' data: blob:`) is unchanged and the canvas stays untainted (`captureStream` keeps working). Only PNG and JPEG are accepted.
- **Recordings stay clean.** Branding is only in the livestream program and on call tiles. The ISO recordings are never branded; the titles go to the take's `manifest.json` (`participants[].title`) for the editor.
- Code: `web/src/lib/branding.ts` (pure layout/animation math, `LowerThirdDirector`), `web/src/lib/programbrand.ts` (the compositor's `ProgramDecorator`), `web/src/lib/compositor.ts` (decorator hook; still import-free).

## ffmpeg build requirement

Livestreaming needs `h264_videotoolbox`, the `aac` encoder and the `flv` muxer. The daemon checks for VideoToolbox before starting a stream (`livestream.ExecPreflight`) and returns 503 with the reason if it is missing. Since SL-P3-3 ffmpeg does **no networking** for livestreams: it writes to a pipe, and the daemon publishes. The RTMP/RTMPS/TLS output protocols that `scripts/build-ffmpeg-mac.sh` enabled for SL-P3-1 (`--enable-securetransport --enable-zlib`, without `--disable-network`) are no longer used by p2p.video, and the 1.0.0 preview's `--disable-network` media tools can run the encoder (checked: `build/media-tools.preview1/bin/ffmpeg` with the encoder argv produced H.264 1280×720 + AAC FLV). The script still builds with them, which keeps ffmpeg useful as a local RTMP test sink; GPL, nonfree and version3 stay disabled.

History: with the SL-P3-1 design (ffmpeg's `tee` publishing), an `rtmps://` publish through a local TLS proxy stalled after the TLS handshake with both the LGPL build and Homebrew ffmpeg 8 (Secure Transport). That path no longer exists; RTMPS is now Go's TLS and is verified end to end against a local TLS ingest (below).

## Platforms and getting a stream key

The presets default to **RTMPS**. Sources were checked on 2026-10-26; platforms change ingest hosts occasionally, so if a platform shows a different server URL, paste that one.

| Platform | Preset server URL | Notes and source |
|---|---|---|
| YouTube | `rtmps://a.rtmps.youtube.com:443/live2` | RTMPS must use port 443 and send the host name via SNI; the server name contains "rtmps" and the path is the RTMP application (`live2`). [YouTube Live API: Delivering live content via RTMPS](https://developers.google.com/youtube/v3/live/guides/rtmps-ingestion). Plain RTMP (`rtmp://a.rtmp.youtube.com/live2`) still works as a custom URL. |
| Twitch | `rtmps://ingest.global-contribute.live-video.net/app` | Twitch's ingest list publishes `url_template_secure` RTMPS endpoints for every region; "Default" is the global anycast host. [Twitch ingest endpoints](https://ingest.twitch.tv/ingests), URL format from [Twitch Video Broadcast docs](https://dev.twitch.tv/docs/video-broadcast/) (`rtmp://<ingest-server>/app/<stream-key>`). |
| Facebook | `rtmps://live-api-s.facebook.com:443/rtmp/` | Facebook requires RTMPS; the preset refuses `rtmp://`. The Live Video API returns a `secure_stream_url` (its docs show `rtmps://rtmp-api.facebook.com/rtmp/` as the server part); Live Producer shows `live-api-s.facebook.com:443`. Keys contain a query string (`FB-…?s_bl=…`), which is passed through verbatim. [Facebook Live Video API: Getting started](https://developers.facebook.com/docs/live-video-api/getting-started). |
| LinkedIn | none (issued per event) | LinkedIn Live Studio issues a Stream URL and Stream key per event: Manage streams → Prepare to go live → choose a region → Get URL. The preset requires `rtmps://`. [LinkedIn Help: Go live using a custom stream (RTMP)](https://www.linkedin.com/help/linkedin/answer/128901). |
| Custom | none | Owncast, a restream relay, nginx-rtmp, etc. The key can be separate or the last segment of the URL (`rtmp://host/app/KEY`). |

**YouTube.** In YouTube Studio choose **Create → Go live** (the first time, enabling live streaming can take up to 24 h). Under **Stream**, choose **Streaming software** and copy the **Stream key**. In p2p.video choose **YouTube** and paste the key, then **Go live**. YouTube shows the preview after a few seconds; click **Go live** in YouTube Studio if auto-start is off.

**Twitch.** Creator Dashboard → **Settings → Stream** → copy the **Primary Stream key**. In p2p.video choose **Twitch** and paste it.

**Facebook.** Live Producer → **Streaming software** → copy the stream key (and the server URL if it differs from the preset).

**LinkedIn.** You must be approved to go live. In Live Studio, **Prepare to go live** becomes available one hour before the scheduled event (two for verified Pages); choose a region, click **Get URL**, and paste the **Stream URL** and **Stream key**. When testing, don't click **Go live** on LinkedIn, or the event starts for your audience.

Never share or screenshot a stream key. Anyone who has it can broadcast on your channel. If one leaks, reset it on the platform.

## Testing

- **Go:**
  - Golden encoder argv, 1080p values, no GPL encoders and no `rtmp`/`://`/`tee` in the argv (`internal/ffmpeg/livestream_test.go`).
  - Unit (`internal/livestream`, fake encoder and fake RTMP connections): bytes piped, nothing dials before media, headers + keyframe at 0 per connection, one destination's broken connection reconnects with a redacted reason while the other keeps streaming, all destinations failing after `MaxAttempts` fails the stream, a stalled destination skips to keyframes without blocking the others, an encoder restart keeps connections up with monotonic timestamps, encoder speed/drop warnings, backoff schedule, ingest grace and auto-stop, redaction. RTMP client against a scripted server: handshake (C2 echoes S1), ping response, connect/createStream/publish, `@setDataFrame`, a `BadName` rejection; chunk round trip with messages larger than the chunk size and extended timestamps; AMF0, FLV and target parsing (YouTube, Twitch, Facebook keys with a query, LinkedIn, inline keys).
  - Integration (`integration_test.go`, real ffmpeg, skips without one; `STUDIO_FFMPEG_DIR` selects a build): the real encoder publishing over `rtmps://` to a TLS-terminating proxy (self-signed root trusted only in the test) in front of an ffmpeg RTMP listener, with `ps -o args=` on the running encoder showing no key, no `rtmp` and no port; the production trust store refusing that certificate, and a trusted root refused for the wrong host name; two destinations where one sink is killed and restarted: it reconnects and receives a valid stream while the other never reconnects.
  - Host-only auth, no key echo, encoder argv without destinations, a single ingest at a time, and Funnel-negative routes (`internal/httpserver`).
- **Web unit:** layout math, cover crop, speaker hysteresis, MediaRecorder type preference, backpressure thresholds, RTMPS presets, URL/key validation (Facebook/LinkedIn require RTMPS, inline keys), destination status lines (`web/tests/livestream.test.mjs`).
- **Branding:** `web/tests/branding.test.mjs`; `web/tests/branding.browser.mjs` streams to a local RTMP sink unbranded, then branded with an on-demand lower third, and checks extracted frames.
- **End-to-end:** `web/tests/livestream.browser.mjs`. A host and one admitted guest use Chromium fake devices. The panel's presets are checked (RTMPS by default; LinkedIn refuses `rtmp://`). The host goes live to two **local** destinations: `rtmps://` through a Node TLS proxy with an `openssl`-generated self-signed certificate (trusted via `--livestream-test-ca`) in front of an ffmpeg RTMP listener, and plain `rtmp://` to a second listener. The test checks that no process shows the key (`ps`), then kills the second sink: that destination shows **Reconnecting · attempt n of 8** with a warning while the first stays live; a restarted sink gets it back. Both received FLVs are probed (H.264 1280×720, AAC 48 kHz, the uninterrupted one ≥ 15 s) and decoded. Logs and status never contain the key. No test publishes to an external service.
- **Owner test (not automated):** a real YouTube, Twitch, Facebook and LinkedIn stream with the presets above.
