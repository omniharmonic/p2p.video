# Feasibility report: a self-hosted Riverside.fm alternative (Mac host, browser guests, Tailscale)

**Short answer:** You can build this. The weak point is the network path for guests who don't have Tailscale. Tailscale Funnel only carries TLS-over-TCP, and it goes through Tailscale's relays, which have bandwidth limits Tailscale doesn't publish. So the live call and the uploads for those guests have to fit inside a "funnel, not a hose". Every other part (local recording, crash-safe chunked upload, sub-millisecond sync after the fact, building editor timelines) is solved engineering.

How to read the tags:
- **[HARD]** = a hard constraint.
- **[RISK]** = needs a prototype to confirm.
- **[UNVERIFIED]** = I could not confirm it from sources and it needs testing.

---

## 1. Tailscale options

### `tailscale serve` (tailnet-only HTTPS)
- Puts your service on `https://<node>.<tailnet>.ts.net` with automatic Let's Encrypt certificates. The tailnet needs MagicDNS and HTTPS turned on.
- This matters because `getUserMedia` only works in a secure context (HTTPS or localhost). A real certificate on a stable name means there is no self-signed-cert pain.
- Docs: https://tailscale.com/kb/1312/serve and https://tailscale.com/kb/1153/enabling-https
- It can also forward raw TCP (`--tcp`) or TLS-terminated TCP (`--tls-terminated-tcp`). It can add PROXY protocol v2 headers so your app sees the real client IP.

### `tailscale funnel` (public internet)
Docs: https://tailscale.com/kb/1223/funnel

- **[HARD] Ports:** public listeners are limited to 443, 8443 and 10000.
- **[HARD] TCP only, no UDP.**
- **[HARD] Routing is by TLS SNI.** Public DNS for the funnel name points at Tailscale's ingress relays. They read the SNI in the TLS ClientHello and forward over the tailnet to your node. So any protocol that doesn't start with a TLS ClientHello can't get through. Plain ICE-TCP from a browser is one of those. (This is my reading of how SNI routing works.)
- **[HARD] Bandwidth:** traffic goes through Tailscale's relays with limits that are not published and can't be configured. Tailscale staff describe it as "a funnel, not a hose" (HN discussion).
- **Funnel has to be allowed** by a `funnel` node attribute in the tailnet policy file. This is a one-time admin step, and the app should walk the user through it.
- **[RISK] DNS propagation:** the first time Funnel is turned on, the public name can take up to about 10 minutes to resolve.
- **[RISK] The hostname is public.** Certificates go into Certificate Transparency logs, so anyone can find `studio.tailXXXX.ts.net`. Treat the URL as known to the world. Every route needs app-level auth: unguessable per-session invite tokens, host approval of each guest, rate limits.

### Node sharing (guest has their own Tailscale account)
- In the admin console: Machines → Share → link or email. Docs: https://tailscale.com/kb/1084/sharing
- The guest's own tailnet sees only that one node, and the guest doesn't use one of your seats.
- Shared nodes start out quarantined: they can't open connections back to the guest. That's fine here, because the guest always connects to the host.
- **One URL serves both kinds of guest.** Inside the tailnet, MagicDNS resolves the same `*.ts.net` name to the 100.x address. On the public internet it resolves to Funnel. So one invite link picks the direct path automatically for Tailscale guests.
  - [UNVERIFIED] Check that the full ts.net name resolves for a shared-in node on the guest's side, rather than only the short name.
- Downside: the guest must install the Tailscale app and sign in. That's real friction for a podcast guest.

### tsnet (Tailscale embedded in a Go app)
Docs: https://tailscale.com/kb/1244/tsnet and https://pkg.go.dev/tailscale.com/tsnet

- `tsnet.Server{Hostname:"studio", Ephemeral:…}` gives the app its own node and name, separate from the Mac's own tailscaled. It provides:
  - `ListenTLS` (tailnet HTTPS with certificates handled for you)
  - `ListenFunnel("tcp", ":443")` (public)
  - `ListenPacket` (UDP on the tailnet IP)
  - `LocalClient()` for certificates and status
- Auth is through a login URL the app shows, or through an auth key.
- **[RISK] tsnet runs its own userspace network stack.** Pion opens ordinary OS sockets. To carry WebRTC UDP over tsnet you would have to plug `tsnet.ListenPacket` into Pion through `SettingEngine.SetNet` / a custom `transport.Net`, or a UDPMux. That's doable but fiddly.
- **Simpler alternative:** require Tailscale.app on the host and control serve/funnel through the LocalAPI (`tailscale.com/client/local`). Pion then simply binds the OS `utun` interface (100.x). I'd use this for the MVP and move to tsnet later if you want a single install with no dependencies.

### DERP and NAT traversal
- Tailnet guests get WireGuard over UDP with hole-punching. When that fails they fall back to DERP, which relays over TCP/HTTPS. Either way, WebRTC UDP inside WireGuard works well. Docs: https://tailscale.com/kb/1232/derp-servers
- DERP is also rate-limited and is a fallback, so expect some degradation in the rare case a guest is forced onto it.

### Recommended tiers
1. **Tier A (best quality): guest with Tailscale through node sharing.** End-to-end UDP P2P with real IPs. Fast uploads, often on a direct path.
2. **Tier B (default, zero install): browser only, through the Funnel URL.**
   - Page, signaling WebSocket and uploads go over Funnel on 443.
   - Media tries direct UDP ICE to the host's public address first (see §2).
   - If that fails, it uses TURN-over-TLS through Funnel on 8443.
   - Show a "connection quality / relayed" badge.
3. **Tier C: same LAN.** Plain LAN, still on the ts.net HTTPS name (on the tailnet) for the secure context.

---

## 2. Signaling and media without a cloud server

### Signaling
- Run a WebSocket on the host (Go: `coder/websocket` or `gorilla/websocket`), served on the same origin as the page, through serve/funnel on 443.
- WSS over Funnel works fine, since it's ordinary HTTPS that gets upgraded.
- Use the same socket for the control plane: record start/stop, clock-sync pings, upload status.

### STUN and TURN
**The host can and should run its own TURN server.** Use `pion/turn` in-process (https://github.com/pion/turn), or coturn if you prefer.

**Why TURN-TLS is the only media path through Funnel:** a browser can't send UDP through Funnel, and plain ICE-TCP has no SNI for Funnel to route on. Browsers do support `turns:` (TURN over TLS on TCP), and that does carry SNI. So:
- `tailscale serve --tls-terminated-tcp=8443 tcp://127.0.0.1:3478`, with Funnel enabled on 8443. Tailscale terminates TLS and Pion TURN sees plain TCP.
- ICE config: `{urls:'turns:studio.tailX.ts.net:8443?transport=tcp', username, credential}`, with short-lived HMAC credentials issued over the WebSocket.
- Set the TURN relay address generator to a loopback/LAN address. The relay then hands packets straight to the host's own Pion peer on the same machine.

**Sharing 443 instead of 8443:** with `tsnet.ListenFunnel` you terminate TLS yourself. You can then demultiplex HTTP and TURN on 443 by peeking at the first bytes (STUN messages carry the magic cookie `0x2112A442`). That's worth it because some corporate firewalls block 8443.

**[RISK] Performance of the TURN-TLS fallback:**
- It's TCP inside TCP, through a relay with undisclosed limits. Expect head-of-line blocking and added latency.
- That's acceptable because the call stream is only a monitor. Recording quality doesn't depend on it.
- Keep call video at about 360–540p, roughly 0.5–1 Mbps per stream.

**Direct UDP for Funnel guests:**
- Both sides need server-reflexive candidates, which means a STUN server. Pure "no third party" options:
  - (a) The host does UPnP/NAT-PMP/PCP port mapping on the router (Go libraries: `huin/goupnp`, `jackpal/go-nat-pmp`) and advertises the mapped public IP:port as a host candidate. Pion supports this through `SettingEngine.SetNAT1To1IPs` and a fixed UDP port via `SetICEUDPMux`.
  - (b) Use Tailscale's DERP servers as STUN. They answer STUN on 3478, which is how `tailscale netcheck` works, but this use is undocumented.
  - (c) Accept a stateless public STUN server (Google or Cloudflare). It never sees media.
- Flag this choice to the user. With direct UDP working, most guests avoid the relay entirely. Hole-punching guest→host works unless both sides have symmetric NAT.

### Mesh vs host-as-SFU (2–6 people)
**Recommendation: host as SFU.** The topology is already a star, since all files end at the host and Funnel/TURN only reach the host. A mesh would make each guest upload N−1 copies and need guest↔guest relay paths, which for Funnel guests means hairpinning through the host's TURN anyway.

**Host egress:** about N×(N−1)×bitrate. For 6 people at 1 Mbps that's about 30 Mbps, which is fine on a home connection. Keep simulcast off or limited to 2 layers.

**SFU options:**

| Option | Notes | Fit |
|---|---|---|
| **Pion custom SFU** (https://github.com/pion/webrtc, see `example-webrtc-applications/sfu-ws`) | About 1–2k lines of Go. Full control. Embeds in the same binary. Can grab each guest's received audio for sync (§4). | **Best** |
| **LiveKit OSS** (single Go binary, Apache-2.0) | Mature: simulcast, reconnects, client SDKs. Built-in TURN with `turn.external_tls: true` fits Tailscale doing the TLS termination. Runs as a sidecar process. Docs: https://docs.livekit.io/home/self-hosting/ports-firewall/ | Strong option if you'd rather not write the SFU |
| mediasoup | Node with a C++ worker. Heavier to package. | Skip |

---

## 3. Local browser recording

### MediaRecorder codec support (as of late 2025)

| Browser | Containers / codecs | Notes |
|---|---|---|
| Chrome / Edge | WebM (VP8/VP9/AV1 + Opus), MKV (`x-matroska`); **MP4 with H.264/AAC (and Opus) since Chrome 126** | MP4 output is fragmented MP4 (chromestatus; https://blog.addpipe.com) |
| Safari | MP4 (H.264/AAC, also HEVC) since 14.1; **WebM/Opus/VP8/VP9 since Safari 18.4** (early 2025) | Detect at runtime with `MediaRecorder.isTypeSupported` |
| Firefox | WebM (VP8/Opus) only, **no MP4** | VP9 encode in MediaRecorder is limited. Fine; convert on the host. |

### Known problems
- **Variable frame rate.** Timestamps follow capture time, and frames drop under load.
- **Chrome WebM has no duration and no Cues index,** so seeking is broken.
- **Only the first chunk carries the init segment / EBML header.** Later chunks can't be decoded on their own: you must concatenate in order.
- Bitrate control is rough (`videoBitsPerSecond`).
- **Fix:** always remux or transcode on the host with ffmpeg (§6).

### WebCodecs path (better quality and control)
- Use `MediaStreamTrackProcessor` to get `VideoFrame`/`AudioData`, then `VideoEncoder`/`AudioEncoder` (with `hardwareAcceleration:'prefer-hardware'`, `latencyMode:'quality'`, explicit bitrate and keyframe interval), then a muxer.
- **Muxer:** use **Mediabunny** (https://mediabunny.dev). It supersedes the now-deprecated `mp4-muxer`/`webm-muxer` (https://github.com/Vanilagy/mp4-muxer). It writes fragmented MP4/MOV/WebM/MKV, has backpressure, and can write to custom stream targets, such as OPFS or an upload sink.
- **[RISK] Support for `MediaStreamTrackProcessor` differs by browser.** Chrome has it. Safari added it recently, in workers. Firefox has no shipped version. So you need a MediaRecorder fallback anyway.
  - Plan: WebCodecs on Chromium (and Safari if detection passes), MediaRecorder everywhere else.
  - Raw frame timestamps from WebCodecs give you exact per-frame capture times, which helps sync.

### Recording at higher resolution than the call
- **Use a single capture.** Call `getUserMedia({video:{width:1920,height:1080,frameRate:30}})` (or 4K). Record that track at full resolution. For the call, send the same track with `RTCRtpSender.setParameters({encodings:[{scaleResolutionDownBy:2–3, maxBitrate:800_000}]})`.
- **Alternative:** `track.clone()` plus `applyConstraints` on the clone. Chrome downscales per track. **Avoid** two `getUserMedia` calls on the same camera at different resolutions: it's unreliable, especially on Safari.
- **[RISK] CPU load.** You'll have two encodes running at once (WebRTC plus the recorder). Prefer hardware H.264 for the recording on Macs (VideoToolbox). Software VP9 at 1080p+ drops frames on mid-range laptops.

### Audio
- **Best:** record raw PCM in an `AudioWorklet` (counted sample by sample, 48 kHz float/int24), write WAV/FLAC chunks. This gives sample-accurate sync (§4).
- **Fallback:** Opus at 256 kbps through MediaRecorder.
- **Processing constraints:** the recording track wants `echoCancellation/noiseSuppression/autoGainControl: false`. The call wants AEC on unless the guest wears headphones.
  - [RISK] Browsers may apply one setting to every track from the same device.
  - Practical default: tell guests to wear headphones and turn processing off for both. Or offer a toggle.

### Crash safety
- Write every chunk (MediaRecorder `timeslice` of 1–2 s, or muxer fragments) to **OPFS** with `createSyncAccessHandle` in a worker. That's fast and works in Chrome, Safari 16.4+ and Firefox 111+ (https://developer.mozilla.org/en-US/docs/Web/API/File_System_API/Origin_private_file_system). IndexedDB is the fallback.
- Call `navigator.storage.persist()` and check `navigator.storage.estimate()`. One hour at 1080p and 8 Mbps is about 3.6 GB.
- [RISK] Safari quotas and eviction rules are stricter.
- On reload, a recovery screen lists unsent sessions and resumes uploading them.

### Progressive upload
- Upload chunks during the call at low priority, throttled to leave room for the call, and finish after the call.
- **Option 1: numbered segments.** `PUT /upload/{session}/{participant}/{track}/{seq}` with SHA-256 for idempotency. The host concatenates in order. Simple, and resumable by nature.
- **Option 2: the tus protocol** (https://tus.io). `tusd` embeds in Go as a handler (`github.com/tus/tusd/v2/pkg/handler`), and `tus-js-client` handles resume. The concatenation extension handles segments.
- **[HARD] Upload limits:** Funnel guests upload through the relays. Multi-GB files may be slow. Tier A guests avoid this.
- Mitigations:
  - Record at a sensible bitrate (1080p30 H.264 at about 6–8 Mbps).
  - Record audio losslessly but keep video reasonable.
  - Allow "finish upload later" (OPFS keeps the data).
  - Show per-file progress to the host.

---

## 4. Clock synchronization

### Layer 1: live clock offset (NTP-style)
- Over the data channel (lower latency than the WebSocket), send about 10 Hz pings: `t0 = performance.timeOrigin + performance.now()` on the client, host replies with `t1, t2` (host monotonic clock), client records `t3`.
- `offset = ((t1−t0)+(t2−t3))/2`, `rtt = (t3−t0)−(t2−t1)`. Keep the samples in the lowest-RTT ~10% and fit a line (offset + drift) over the session.
- Use `performance.now()` (monotonic), not `Date.now()`. `timeOrigin` only anchors it. Docs: https://developer.mozilla.org/en-US/docs/Web/API/Performance/timeOrigin
- Over Funnel/TCP the RTT is asymmetric and has jitter. Expect about ±5–20 ms. That's good enough for a rough start position, not for sample accuracy.

### Layer 2: recording markers
- **Start:** the host broadcasts `start_at = hostNow + 1500 ms`. Each client converts to local time and starts. Each client also logs the *actual* local time of its first audio sample and first video frame:
  - WebCodecs `VideoFrame.timestamp` / `AudioData.timestamp`.
  - In the AudioWorklet, `currentFrame` mapped to performance time with `AudioContext.getOutputTimestamp()` / `outputLatency`. For input, also note `baseLatency`.
- **Sidecar file:** every 5 s, log `(localSampleIndex, localPerfTime, hostTime)` to a sidecar JSON.
- **Why drift matters [HARD]:** audio hardware clocks run 20–100 ppm apart. That's **about 70–360 ms of drift per hour.** One start offset is not enough; you have to correct the rate. Fit `hostTime = a + b·sampleIndex`, then resample by the ratio `b` (ffmpeg `aresample=resampler=soxr` with an exact `asetrate`/`atempo` factor, or rubberband).

### Layer 3: cross-correlation (the precise layer; recommended as the main method, not just a fallback)
- The host SFU already receives every participant's live Opus audio, timed by the host's clock. Record those streams on the host as low-quality references.
- After the call, cross-correlate each participant's **local** high-quality track against **their own** received reference. It's the same signal, so the correlation peak is strong. Use GCC-PHAT on about 10 s windows every 30–60 s.
- That gives offset over time, so both offset and drift. Fit a line, reject outliers, then trim/pad and resample. The result is sub-millisecond, effectively sample-accurate.
- Correct for network delay using the median `jitterBufferDelay`/RTT from `getStats`, or simply align everything to the host-clock reference. The host's own local track goes through the same process.
- Also keep an optional **clap/sync tone**: the host triggers a short chirp played on every client and recorded locally. It's a manual safety net, and an editor can see it.
- **Video** follows audio through its in-file timestamps once converted to CFR (§6).

### How others do it
- **Riverside:** documents local recording, progressive upload and automatic sync, but not how. Server-side alignment like the method above is the likely approach. (https://riverside.fm)
- **VDO.Ninja:** has remote-triggered local recording (`&record`, `&autorecordlocal`), but **no automatic multi-track sync**. Users line tracks up by hand or with claps, or record centrally in OBS. (https://docs.vdo.ninja)
- So this design is a real step up over VDO.Ninja.

---

## 5. Multiple cameras/mics; iPhone as a camera

### Several devices on one machine
- `enumerateDevices()`. Labels only appear after permission is granted, so call `getUserMedia` first.
- Then call `getUserMedia({video:{deviceId:{exact:id}}})` once per device. Each track gets its own recorder and upload stream (`track` id in the upload path).
- Chrome on macOS handles several cameras and mics at once.
- **[RISK]** Safari has historically limited concurrent captures, and iOS Safari allows only one active camera (opening a second mutes the first). Test on macOS Safari, or treat Chrome as the supported "multi-source" browser.
- USB bandwidth: two 1080p webcams on one hub may not get enough bandwidth.

### Continuity Camera
- On macOS Ventura+, a nearby iPhone appears as a normal video device (and mic) in every browser, with no extra code.
- It's the easiest high-quality option for the host and for Mac-using guests.

### Phone as an extra source via QR
- The participant's page shows a QR code: `https://studio…ts.net/join/{session}?source_of={participantId}&token=…`. The phone joins as a *sub-source*: it doesn't appear in the call grid (or shows as a small preview), records locally and uploads under that participant.
- iOS Safari: MediaRecorder MP4 or WebM (18.4+), OPFS supported.
- **[RISK] iOS:**
  - Capture stops when the tab goes to the background or the screen locks. Use the Screen Wake Lock API (Safari 16.4+) and an on-screen warning.
  - Thermal throttling during long 4K/1080p sessions.
  - Storage quota.
- **Phone sync:** same NTP and sidecar method. The phone also sends a low-bitrate live preview through the SFU, so the cross-correlation reference exists for it too. Mic audio is required for this, even if it isn't used.

---

## 6. Host app packaging and post-processing

### Recommendation: a single Go binary
It combines tsnet (or LocalAPI control of Tailscale.app), Pion WebRTC (SFU), Pion TURN, the tusd handler, the WebSocket signaling server, and the web UI embedded with `embed.FS`. Wrap it as a signed, notarized `.app` with a menu-bar UI (e.g. `fyne.io/systray`, or Wails if you want a richer host dashboard). Bundle or detect ffmpeg.

Why Go:
- tsnet, Pion, pion/turn, tusd and LiveKit are all Go-native. That makes the Tailscale-embedded, single-process, no-dependency goal possible. Only Go has that whole stack.
- It cross-compiles, and doesn't need native Node addons or C++ build workers.

Alternatives:

| Option | Assessment |
|---|---|
| **Node/Bun** | No tsnet; you'd have to shell out to the `tailscale` CLI. mediasoup needs a C++ worker; node-webrtc/werift are weaker. Fine for the web UI, weak for the core. |
| **Tauri** | A nice native shell, but WebRTC on the Rust side (webrtc-rs) is less mature. A sensible pattern is a Tauri shell with the Go core as a sidecar. More moving parts. |
| **Native Swift** | Best Mac polish. But you'd need Google's WebRTC framework and libtailscale/TailscaleKit (C bindings, newer and less battle-tested), and no tusd. More code. Mac-only, which is fine but not needed. |

**Host UI:** the host joins the call from their own browser at the same URL. Their local recording runs like anyone else's, with no upload needed (it uses localhost).

### ffmpeg pipeline (run per track on the host after upload)
1. **Concatenate** chunks in order (byte concatenation for fMP4/WebM segments from one recorder).
2. **Fix headers and remux:** `ffmpeg -i raw.webm -c copy fixed.mkv`.
3. **Convert to CFR video for editors:** `ffmpeg -i fixed.mkv -vf "fps=30" -fps_mode cfr -c:v libx264 -crf 16 -preset slow -g 30 -pix_fmt yuv420p -an video.mp4`. Alternatively, an edit-friendly intermediate: `-c:v prores_ks -profile:v 2` or `h264_videotoolbox`.
4. **Audio:** decode to 48 kHz, 24-bit WAV and apply the drift correction:
   `-af "aresample=48000:resampler=soxr:precision=28,asetrate=48000*R,aresample=48000"`, where R is the fitted ratio, or `atempo=R` for small ratios.
5. **Alignment:** compute each track's offset relative to the session's zero point on the host clock.
   - Pad the start with `adelay` / `tpad=start_duration=` (or `-itsoffset`), or trim with `-ss`. Pad or trim the end to a common length.
   - Alternatively, leave media untouched and put the offsets in the timeline files (less transcoding, and what editors prefer).
6. **Timeline exports** (generated in Go from aligned offsets, durations, frame rate and timecode):
   - **FCPXML** (1.9–1.11) with an `<mc-clip>`/`<media><multicam>` holding one `<mc-angle>` per participant/camera, each `asset-clip` placed at its offset. Or a synchronized compound clip. Spec: https://developer.apple.com/documentation/professional-video-applications/fcpxml-reference
   - **Premiere / Resolve:** FCP7 XML (xmeml v4) sequence with one video and one audio track per source. Both Premiere and DaVinci Resolve import it. Resolve also reads FCPXML.
   - **EDL (CMX3600):** limited to one video track per EDL, so produce one EDL per track, or skip it. Mostly useful as a fallback.
   - Also stamp **timecode** into each file (`-timecode HH:MM:SS:FF` derived from the session start) so editors can sync by timecode even without the XML.

**[RISK] ffmpeg licensing:** bundle an LGPL build (no libx264, so use `h264_videotoolbox` or ProRes) or use the user's Homebrew ffmpeg. A GPL build with x264 has distribution implications.

---

## Hard constraints and risks
1. **[HARD] Funnel is TCP/TLS-only, routed by SNI, on 443/8443/10000.** Funnel guests can only get media through TURN-TLS via Funnel (no ICE-TCP, no UDP), unless direct UDP to the host works through STUN/port mapping.
2. **[HARD] Funnel bandwidth is undisclosed and throttled.** The live call has to stay low-bitrate. Multi-GB uploads from Funnel guests may be slow, so design for "upload continues after the call".
3. **[HARD] "No third-party server" vs STUN.** Direct UDP for Funnel guests needs a STUN server (Tailscale DERP, used in an undocumented way, or a public one) or router port mapping. If none is allowed, Funnel guests are always relayed.
4. **[HARD] Secure context:** guests must use the `https://*.ts.net` URL. Raw IPs won't do.
5. **[HARD] Audio clock drift (about 70–360 ms/hour)** needs rate correction, not just an offset. Use the server-side cross-correlation design.
6. **[RISK] Public Funnel URL** (it appears in CT logs) needs strong per-session tokens and host-approved joins.
7. **[RISK] Browser gaps:** Firefox has no MP4 and no WebCodecs track processor. Safari limits multiple cameras. iOS stops capture when backgrounded. Expect to support Chromium best, Safari well, Firefox through the fallback path.
8. **[RISK] Two simultaneous encodes** (call + recording) on weak laptops. Prefer hardware H.264 and let the call quality adapt.
9. **[RISK] Tailscale setup friction for the host:** a tailnet account, HTTPS/MagicDNS on, the Funnel ACL attribute, and first-time DNS propagation. Put it in an onboarding wizard.
10. **[RISK] tsnet + Pion UDP integration is non-trivial.** For the MVP, use system Tailscale.app plus the LocalAPI.

**Suggested MVP order:**
1. Go host: system Tailscale serve/funnel + Pion SFU + WebSocket signaling.
2. Chrome MediaRecorder MP4 + OPFS + segment uploads.
3. NTP sidecar + cross-correlation alignment.
4. ffmpeg CFR + FCPXML.
5. Then TURN-TLS on 8443, Safari/Firefox fallbacks, phone sources and WebCodecs.

**Sources:**
- Tailscale: https://tailscale.com/kb/1223/funnel, https://tailscale.com/kb/1312/serve, https://tailscale.com/kb/1153/enabling-https, https://tailscale.com/kb/1084/sharing, https://tailscale.com/kb/1244/tsnet, https://pkg.go.dev/tailscale.com/tsnet, https://tailscale.com/kb/1232/derp-servers
- WebRTC / SFU: https://github.com/pion/webrtc, https://github.com/pion/turn, https://docs.livekit.io/home/self-hosting/ports-firewall/
- Recording: https://mediabunny.dev, https://github.com/Vanilagy/mp4-muxer, https://blog.addpipe.com (Chrome 126 MP4), https://developer.mozilla.org/en-US/docs/Web/API/File_System_API/Origin_private_file_system
- Upload: https://tus.io, https://github.com/tus/tusd
- Clock: https://developer.mozilla.org/en-US/docs/Web/API/Performance/timeOrigin
- Prior art: https://docs.vdo.ninja, https://riverside.fm
- Timelines: https://developer.apple.com/documentation/professional-video-applications/fcpxml-reference

(The search tool returned redirect links, so the URLs above are the canonical doc pages. Details marked [UNVERIFIED] or [RISK] should be confirmed by prototyping.)
