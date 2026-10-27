# p2p.video — PRD

> Working name. A free, self-hosted Riverside.fm alternative. The host runs it on a Mac, guests join in a browser, there is no SaaS server, and connectivity goes through Tailscale.

## 1. Problem

Riverside.fm (and Zencastr, SquadCast, etc.) solve one hard problem: **record every participant locally at full quality, then deliver synchronized, separate tracks to an editor**, so network glitches don't wreck the recording. They charge monthly for it, keep your media on their cloud, and lock features behind tiers.

Final Cut Camera + Final Cut Pro for iPad solve the **local** version of the same problem: up to 4 iPhones/iPads recording ISO angles that arrive in the editor as a synced multicam clip.

I want both in one free tool I control:
- **Remote:** guests anywhere, joining from a browser invite link.
- **Local:** extra cameras and phones in the same room, added as more sources.
- **Output:** a folder an editor can drop into Final Cut / Premiere / Resolve with everything already lined up.

## 2. Goals and non-goals

### Goals (v1)
1. **G1. Multi-source synchronized local recording.** Every source (each guest's camera and mic, plus extra local cameras and phones) records locally at full quality, independent of call quality.
2. **G2. Browser-only for guests.** No install for guests in the default path: open the link, grant camera/mic, done.
3. **G3. No hosted servers.** The host's Mac is the server. Connectivity comes from Tailscale (Serve for tailnet guests, Funnel for everyone else). No SaaS account other than the host's own Tailscale.
4. **G4. Crash-safe progressive upload.** Recordings stream to the host during the session and survive tab crashes or reloads.
5. **G5. Edit-ready output.** Per-source files at constant frame rate with aligned start points, timecode, an FCPXML multicam clip, a Premiere/Resolve XML, and a JSON manifest.
6. **G6. Good-enough live call.** Low-latency video and audio so people can talk naturally. The call is a monitor; recording quality never depends on it.

### Non-goals (v1)
- Live streaming to YouTube/Twitch (roadmap: v3).
- AI editing (Magic Clips, captions, filler-word removal). Transcription can later reuse the **Minutes** speech engine (roadmap: v2).
- Windows/Linux host (Go makes it possible later; not tested in v1).
- Native mobile apps (phones join through the browser).
- More than 8 simultaneous sources.
- Teleprompter, branded layouts, audience Q&A.

## 3. Users and personas

| Persona | Description | Key needs |
|---|---|---|
| **Host / Producer** (primary: me) | Runs the Mac app, invites guests, starts and stops recording, owns the files | One-click session, approve guests, see every source's recording/upload health, get an export folder |
| **Remote guest** | Non-technical podcast guest on a laptop | Click link, pick devices, see and hear the host, nothing to install, clear "you are recording / uploading, don't close the tab" signals |
| **Local source / camera op** | A phone or second camera in the room | Scan QR, phone becomes another angle, records locally, uploads afterwards |
| **Editor** (may be the host) | Gets the output folder | Separate ISO tracks, CFR, common timeline, open the XML and start cutting |

## 4. Competitive feature map (what we take and what we skip)

| Feature | Riverside | VDO.Ninja | Final Cut Camera / Live Multicam | **p2p.video v1** |
|---|---|---|---|---|
| Local ISO recording per participant | ✅ up to 4K, WAV audio | ✅ manual (`&record`) | ✅ (ProRes/HEVC on device) | ✅ |
| Automatic sync across sources | ✅ | ❌ (manual/clap) | ✅ (via the iPad host) | ✅ (clock sync + cross-correlation) |
| Progressive upload during the recording | ✅ | ❌ | ❌ (transfers after) | ✅ |
| Multiple sources per participant | Limited (2nd cam via phone) | ✅ (extra links) | ✅ 4 angles | ✅ (extra devices + phone QR) |
| Browser guests, no install | ✅ | ✅ | ❌ | ✅ |
| Self-hosted / no cloud | ❌ | ✅ (needs its signaling server) | ✅ (local only) | ✅ |
| Editor timeline export | XML for some NLEs | ❌ | Native FCP multicam | ✅ FCPXML + xmeml + manifest |
| Live streaming | ✅ | ✅ via OBS | ❌ | ❌ (v3) |
| Transcription / AI clips | ✅ | ❌ | ❌ | ❌ (v2: transcription only) |
| Producer mode (non-recorded producer) | ✅ | ✅ | ❌ | ✅ (role flag; cheap) |
| Screen share recording | ✅ | ✅ | ❌ | v1.1 |

> See [`research/product-landscape.md`](research/product-landscape.md) and [`research/webrtc-tailscale-feasibility.md`](research/webrtc-tailscale-feasibility.md).

## 5. Core user flows

### F1. Host creates a session
1. Host opens the p2p.video menu-bar app. On first run, an onboarding wizard checks Tailscale (installed, logged in, MagicDNS + HTTPS on, Funnel allowed) and ffmpeg.
2. Host clicks **New Session**, gives it a title, and picks a quality preset (1080p30 default; 720p30; 4K30 for capable machines).
3. The app shows an **invite link** (`https://studio.<tailnet>.ts.net/j/<token>`) and a QR code, and opens the studio in the host's browser.

### F2. Guest joins
1. Guest opens the link and lands in the **green room**: device pickers, a self preview, a mic level meter, a "use headphones" prompt, a browser compatibility check, and a storage check (`navigator.storage.estimate`).
2. Guest enters their name and clicks **Request to join**. The host sees the request and admits the guest (auto-admit can be configured per session).
3. The guest appears in the call grid. The host's dashboard shows a status row per source: connection path (direct / relayed), recorder state, local buffer, upload %.

### F3. Add an extra source
- **Same machine:** a guest or the host clicks "Add camera/mic" and picks another device. It becomes a new **source** under that participant.
- **Phone:** the participant clicks "Add phone", a QR code appears, the phone opens it and becomes a sub-source. It records locally, shows as a small tile, and uploads under that participant.

### F4. Record
1. Host clicks **Record**. The server schedules `start_at = now + 1.5 s` (host clock). Every source starts its local recorder at that moment, and a 3-2-1 countdown is shown.
2. During recording, chunks are written to OPFS and uploaded in the background at low priority (throttled to protect the call).
3. Optional **sync chirp**: the host can trigger a short tone that all clients play and record. It's a manual safety net.
4. Host clicks **Stop**. All recorders stop. Remaining chunks finish uploading. Guests see "Uploading — please keep this tab open (73%)". Closing early is safe: the next visit to the link resumes the upload.
5. Multiple **takes** per session are supported (Record/Stop repeatedly). Each take is a separate folder.

### F5. Get the output
1. When every source of a take is uploaded, the host app runs post-processing: concatenate, remux, CFR conversion, drift correction, alignment, timeline export.
2. The host gets a notification with an **Open folder** link:

```
~/Movies/StudioLink/2026-10-13 Episode 42/take-01/
  manifest.json
  Episode42_take01.fcpxml
  Episode42_take01_premiere.xml
  video/  01_Ben_cam1.mov  02_Ana_cam1.mov  03_Ana_phone.mov
  audio/  01_Ben_mic1.wav  02_Ana_mic1.wav
  reference/  mixdown.wav   (call-quality mix, for quick review)
  raw/  (original uploads, kept until the user deletes them)
```

## 6. Functional requirements

ID format `SL-FR-##`. Priority: **P0** = MVP, **P1** = v1, **P2** = later.

| ID | Requirement | Pri | Acceptance criteria |
|---|---|---|---|
| SL-FR-01 | Host app runs on macOS 14+ (Apple Silicon) as a menu-bar app with a web dashboard | P0 | Launches, shows status, creates a session |
| SL-FR-02 | Onboarding checks Tailscale state (logged in, MagicDNS, HTTPS certs, Funnel attribute) and ffmpeg, with fix-it instructions | P0 | Each failed check shows the exact fix; re-check button |
| SL-FR-03 | Sessions with unguessable invite tokens (≥128-bit), expiry, and revocation | P0 | Old or revoked tokens get a 403; a token isn't valid for a different session |
| SL-FR-04 | Green room with device selection, preview, level meter, and compatibility and storage checks | P0 | Unsupported browser shows a warning; low storage blocks recording with an explanation |
| SL-FR-05 | Host admits or denies join requests; optional auto-admit | P0 | An unadmitted client gets no media and no upload credentials |
| SL-FR-06 | Live A/V call through a host-run SFU, 2–6 participants | P0 | 4 participants for 60 min, glass-to-glass latency < 400 ms on tailnet/direct paths |
| SL-FR-07 | Guest connectivity: Tailscale-shared node (direct), Funnel + direct UDP, Funnel + TURN-TLS fallback | P0 (Funnel+TURN), P1 (direct UDP opts) | A guest behind symmetric NAT can still join through TURN-TLS on Funnel |
| SL-FR-08 | Local recording per source at the chosen preset, independent of call bitrate | P0 | The 1080p30 recording is 1080p even when the call stream is 360p |
| SL-FR-09 | Lossless or near-lossless local audio (PCM WAV 48 kHz via AudioWorklet; Opus 256k fallback) | P0 (Opus) / P1 (PCM) | Delivered audio is 48 kHz, no resampling artefacts |
| SL-FR-10 | Crash-safe chunk storage in OPFS (IndexedDB fallback) | P0 | Killing the tab mid-recording loses ≤ 2 s; reopening the link offers to resume the upload |
| SL-FR-11 | Progressive, resumable, idempotent upload of chunks to the host | P0 | Interrupting the network for 60 s → upload resumes with no duplicate or missing chunks (checksums verified) |
| SL-FR-12 | Synchronized start: scheduled start on the host clock, plus a per-source clock-sync sidecar | P0 | Start offset after the coarse sync is < 50 ms |
| SL-FR-13 | Fine alignment by cross-correlating each local track against the host-received reference, with drift correction | P1 | Final inter-source audio alignment error < 5 ms across a 60-min take (measured with the chirp test) |
| SL-FR-14 | Multiple sources per participant (extra local devices) | P1 | 2 cameras + 1 mic on one Mac all record and upload as separate tracks |
| SL-FR-15 | Phone as a sub-source via QR | P1 | An iPhone in Safari joins as a sub-source and its file arrives aligned |
| SL-FR-16 | Post-processing to CFR video, 48k WAV, aligned offsets, embedded timecode | P0 | Files import into FCP/Premiere/Resolve with no VFR warnings |
| SL-FR-17 | FCPXML export: a multicam clip with one angle per source | P0 | Opens in Final Cut Pro 11 as a synced multicam |
| SL-FR-18 | Premiere/Resolve export (FCP7 xmeml) with aligned tracks | P1 | Opens in Premiere and Resolve, tracks aligned |
| SL-FR-19 | Host dashboard: per-source recorder state, buffer size, upload progress, connection path, clock-sync quality | P0 | Live updates ≤ 1 s latency |
| SL-FR-20 | Multiple takes per session | P1 | Each take produces its own output folder |
| SL-FR-21 | Producer role (in the call, not recorded) | P1 | Producer has no recording tracks |
| SL-FR-22 | Sync chirp button | P1 | Chirp is visible in all waveforms within 1 frame |
| SL-FR-23 | Screen share as a recorded source | P2 | Recorded as its own track |
| SL-FR-24 | Transcripts via the Minutes speech engine | P2 | Per-speaker transcript sidecar in the output |
| SL-FR-25 | RTMP live output | P2 | — |

## 7. Non-functional requirements

| ID | Requirement |
|---|---|
| SL-NFR-01 **Privacy** | Media never leaves the guest's device except to the host. No third-party media relay (Tailscale's Funnel/DERP relays carry encrypted bytes only). |
| SL-NFR-02 **Security** | The public Funnel hostname is discoverable (CT logs), so every endpoint needs a session token, admission, rate limits, and short-lived TURN credentials. Upload endpoints accept only declared tracks and sizes. |
| SL-NFR-03 **Reliability** | No recording lost to a network drop, tab crash or host restart. The host persists session state in SQLite. |
| SL-NFR-04 **Performance** | Guest CPU: recording 1080p30 + call on an M1 MacBook Air with no dropped recorded frames. Host: 6 participants on an M-series Mac. |
| SL-NFR-05 **Browser support** | Chrome/Edge (primary, all features); Safari 18.4+ macOS (supported; multi-camera best-effort); Firefox (supported through the MediaRecorder WebM fallback); iOS Safari (as a phone sub-source). |
| SL-NFR-06 **Install** | Host: one signed and notarized `.app` plus Tailscale.app. ffmpeg is detected (Homebrew) or downloaded on first run. |
| SL-NFR-07 **Licensing** | Project is MIT. Don't redistribute GPL ffmpeg builds; use the system ffmpeg or an LGPL build with VideoToolbox/ProRes encoders. |

## 8. Success metrics
- I record a real 2–4 person episode with remote guests and import into FCP with **zero manual syncing**.
- Measured alignment error < 5 ms (audio) and < 1 frame (video) across 60 minutes.
- **0** lost recordings across 10 sessions, including deliberate tab-kill and network-drop tests.
- A guest goes from link to admitted in < 60 s without help.

## 9. Risks (summary; details in ARCHITECTURE §12)
1. Funnel bandwidth is undisclosed and throttled. That affects the call quality and upload speed of relayed guests. *Mitigation:* low call bitrate, uploads continue after the call, direct-UDP paths, Tailscale-shared tier.
2. Funnel is TCP/TLS-only, so media must use TURN-over-TLS. *Mitigation:* Spike S1 proves it early.
3. Browser differences (Safari multi-cam, Firefox no MP4/WebCodecs, iOS backgrounding). *Mitigation:* capability detection, a MediaRecorder fallback, clear UX warnings.
4. Two simultaneous encodes on weak guest machines. *Mitigation:* hardware H.264 and an adaptive call bitrate.

## 10. Open questions for the owner
1. **STUN policy.** Is a public STUN server (Google/Cloudflare; sees IPs, never media) acceptable for direct-UDP paths? Otherwise Funnel guests always go through TURN-TLS. *Default: allowed, configurable off.*
2. Default output codec: **ProRes 422** (huge, edit-friendly) or **H.264 high-bitrate** (smaller)? *Default: H.264 via VideoToolbox, ProRes as an option.*
3. Should the host's own camera also go through the browser (simplest), or be captured natively by the Mac app (best quality, Continuity Camera)? *Default: browser in v1.*
4. Final name.
