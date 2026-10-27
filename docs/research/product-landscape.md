# StudioLink: Product & Competitor Landscape Research

> Comprehensive research on remote studio recording, local multicam, competitor capabilities, open-source alternatives, and professional NLE editor requirements.

---

## 1. Riverside.fm Feature Analysis (2025–2026)

### 1.1 Core Architecture & Recording Engine
Riverside (riverside.com) built its market dominance by solving the fundamental flaw of cloud video calls (Zoom, Teams, Meet): **network packet loss degrades recording quality**.

* **Local ISO Recording:** Every participant's browser records full-resolution video (up to 4K / 2160p at 30 fps) and uncompressed or high-bitrate audio (48 kHz 16/24-bit WAV) directly onto local storage using browser media APIs. Network hitches, video artifacts, and audio dropouts during the call never affect the recorded media.
* **Progressive Uploading:** Rather than waiting until the session ends to upload gigabytes of footage, Riverside uploads recorded media chunk-by-chunk in the background while the call is live.
  - When the host clicks "Stop", 80–95% of the file is typically already on the server.
  - A completion screen holds the guest in the tab until remaining chunks finish uploading, with a per-participant progress bar (0–100%).
  - If a guest closes the tab accidentally, reopening the studio link resumes the upload from the last uncommitted chunk.
* **Independent Stream Architecture:** The live WebRTC call is strictly a low-bitrate monitor feed (typically 360p–540p, ~500–800 kbps). Encoding parameters for the live WebRTC stream are decoupled from the local recorder encoder.

### 1.2 Full Feature Matrix & Tiering
Riverside segments features across Free, Pro, Grow, Webinar, and Business tiers:

| Category | Feature | Free ($0) | Pro ($24–$29/mo) | Business (Custom) | Our Priority |
|---|---|---|---|---|---|
| **Recording** | Local ISO tracks | 2 hrs one-off (720p) | 15 hrs/mo (4K) | Unlimited (4K) | **P0 (Must-Have)** |
| | Separate audio/video tracks | Watermarked | Clean WAV/MP4 | Clean WAV/MP4 | **P0 (Must-Have)** |
| | Progressive upload & crash resume | Basic | Included | Priority relay | **P0 (Must-Have)** |
| | Guest browser access (no install) | Chrome/Edge/Safari | Chrome/Edge/Safari | Chrome/Edge/Safari | **P0 (Must-Have)** |
| | Multi-camera / Phone as 2nd cam | Limited | Included | Included | **P1 (v1)** |
| | Producer mode (silent monitor) | No | Included | Advanced routing | **P1 (v1)** |
| | Screen share recording | Shared feed | Separate track | Separate track | **P1 (v1.1)** |
| **Post/Export**| Synchronized NLE export (XML/FCPXML) | No | Basic / Workaround | Full timeline export | **P0 (Must-Have)** |
| | Text-based editing | Limited | Included | Included | **P2 (Roadmap)** |
| | AI Magic Clips / Highlights | Watermarked | Included | Included | **P3 (Skip)** |
| | Transcription | Basic | 100+ languages | Priority model | **P2 (via Minutes)** |
| **Production** | Media board / Sound effects | No | Included | Included | **P2 (Skip v1)** |
| | Teleprompter | No | Included | Team shared | **P2 (Skip v1)** |
| **Live** | RTMP Multistreaming (YouTube, Twitch) | No | Grow ($34/mo)+ | Custom RTMP | **P2 (v3)** |

### 1.3 Key User Pain Points with Riverside
1. **Subscription Cost & Hour Caps:** Users pay $24–$39/month but remain bound to strict monthly caps on separate-track recording hours (e.g. 15 hours/month on Pro). Going over incurs steep fees.
2. **Variable Frame Rate (VFR) Drift:** Browser `MediaRecorder` outputs VFR video. Riverside's cloud processing does not always perfectly conform to Constant Frame Rate (CFR), leading to noticeable audio/video drift over 60+ minute takes in Premiere Pro and DaVinci Resolve.
3. **Data Sovereignty & Privacy:** Sensitive corporate interviews, legal depositions, and unreleased podcast episodes reside on third-party cloud buckets.
4. **Guest Upload Stalls:** Guests on asymmetrical home broadband (e.g. cable with 10 Mbps upload) experience very slow post-call uploads, often exiting before completion and stranding local files.

---

## 2. Apple Final Cut Camera + Final Cut Pro "Live Multicam"

Introduced in 2024 for iPadOS and iOS, Apple's **Live Multicam** system provides the local equivalent of Riverside's studio workflow:

### 2.1 Technical Mechanism
* **Topology:** Up to 4 iPhones/iPads running the free **Final Cut Camera** app connect over local Wi-Fi / Bonjour to an iPad running **Final Cut Pro for iPad 2+**.
* **Dual-Stream Pipeline:**
  - **Monitoring:** Each iPhone encodes a low-latency, lightweight proxy preview stream transmitted over the local network to the master iPad director view.
  - **Recording:** Each iPhone records full-quality high-res ISO footage locally to internal storage (up to 4K ProRes 422 or HEVC, Log/HDR color spaces).
* **Remote Production Control:** The director iPad remotely inspects and adjusts camera parameters on any connected iPhone: ISO, shutter speed, white balance, manual focus/peaking, zoom, exposure compensation, and audio levels.
* **Synchronized Trigger:** Pressing "Record" on the iPad director app broadcasts a synchronized start command to all 4 cameras.
* **Footage Ingestion:** After recording stops, full-resolution files transfer over Wi-Fi in the background into the Final Cut Pro project bundle.
* **Instant Multicam Clip:** Final Cut automatically synchronizes all angles using timecode/capture epoch and generates a ready-to-edit Multicam Clip.

### 2.2 Lessons for StudioLink
1. **Decoupled Preview vs. Capture is King:** High-end production software *never* records the stream sent across the network.
2. **FCPXML Multicam is the Ultimate Deliverable:** Editors love opening an NLE project where clicking camera angles switches seamlessly during playback. Generating an `<mc-clip>` in FCPXML 1.10 provides immediate parity with Apple's $300 ecosystem.
3. **Local Remote Control (P2):** Browser APIs (`MediaStreamTrack.applyConstraints`) allow controlling exposure, white balance, and zoom on Chromium and mobile browsers where supported.

---

## 3. Open-Source & Competitor Landscape

### 3.1 VDO.Ninja (Steve Seguin)
* **License:** **AGPL-3.0** (SDK under MPL-2.0).
* **Architecture:** Serverless P2P WebRTC mesh. Uses a lightweight public or self-hosted signaling handshake (Node.js/WebSocket).
* **Recording Capabilities:**
  - `&record`: Adds an on-screen recording button. Sets target video recording bitrate in kbps (e.g. `&record=6000`).
  - `&record=0`: Audio-only lossless 32-bit PCM.
  - `&recordcodec=mp4`: Requests MP4 container where supported.
  - `&pcm`: Forces uncompressed PCM audio.
  - `&autorecordlocal` / `&autorecordremote`: Automates capture on connection.
  - `&splitrecording`: Splits output into 5-minute chunks to prevent memory exhaustion.
* **Sync Limitations:** VDO.Ninja has **no automated multi-track timeline synchronization**. It offers manual audio delay parameters (`&sync=X` in ms), but multi-guest recordings must be manually aligned in an editor using visual/audio claps.
* **Why We Don't Just Use VDO.Ninja:**
  1. No progressive chunk upload to the host; files save to the guest's local `Downloads/` directory and must be manually transferred.
  2. No automated clock-drift compensation or GCC-PHAT cross-correlation.
  3. No NLE timeline export (FCPXML/XML).
  4. AGPL-3.0 prevents embedding code into an MIT-licensed binary.

### 3.2 OpenStudio (`msitarzewski/openstudio`)
* **License:** **MIT** (135 stars, JavaScript).
* **Architecture:** Self-hosted virtual broadcast studio for community radio and podcasts. Connects participants over WebRTC mesh.
* **Relevant Features:** Implements mix-minus audio and records individual participant tracks as separate WAV files on the host, plus a mixed output.
* **Limitation:** Records audio *as received over the network* on the server, rather than high-res local ISO recording on the client.

### 3.3 SonoBus (`sonosaurus/sonosaurus`)
* **License:** **GPL-3.0** (C++).
* **Architecture:** Native peer-to-peer desktop/mobile audio streaming app for musicians.
* **Relevance:** Demonstrates high-quality peer-to-peer audio recording with per-track uncompressed PCM, but requires a native desktop/mobile client on all sides (no browser guest support).

### 3.4 GitHub "Riverside Clone" Projects
Several proof-of-concept projects exist on GitHub (e.g. MERN stack WebRTC local recorders):
* Common pattern: `getUserMedia` -> `MediaRecorder(timeslice=1000)` -> chunk array -> upload to S3/MinIO via HTTP POST or WebSocket.
* **Universal Missing Pieces in existing OSS:**
  1. No sub-frame audio synchronization or clock-drift correction (all suffer from 100–400 ms drift over an hour).
  2. No CFR transcoding pipeline with embedded timecode.
  3. No FCPXML/EDL/xmeml generator.
  4. Rely on cloud storage (AWS S3) and central servers rather than direct peer-to-host transfer over Tailscale.

---

## 4. Professional Video Editor Requirements

Video editors working in **Final Cut Pro**, **Adobe Premiere Pro**, and **DaVinci Resolve** have rigid format requirements:

### 4.1 Strict Constant Frame Rate (CFR)
* **The VFR Nightmare:** Browser `MediaRecorder` timestamps frames based on hardware delivery time. During CPU spikes, frame intervals fluctuate (e.g. 29.8 fps to 30.2 fps). When imported into Premiere Pro or DaVinci Resolve, audio slowly drifts out of sync with video over 30–60 minutes.
* **Requirement:** StudioLink host post-processing **must conform every video track to strict CFR** (e.g. exactly 30.000 or 29.970 fps) using ffmpeg `-fps_mode cfr -vf fps=30`.

### 4.2 Standard Audio Sampling
* **Requirement:** Audio tracks must be normalized to **48,000 Hz, 24-bit PCM WAV**. 44.1 kHz (CD standard) triggers real-time resampling in video NLEs, causing playback overhead and phase artifacts.

### 4.3 Multi-Camera Timeline Export Formats
1. **Apple FCPXML 1.10 / 1.11:**
   - The industry standard for Final Cut Pro 10.6+ and 11.
   - Requires an `<fcpxml version="1.10">` root containing `<resources>` (assets with file URLs) and a `<media id="…" name="Multicam">` element containing `<multicam>` with child `<mc-angle>` elements.
   - Each `<mc-angle>` contains `<asset-clip>` tags placed at specific rational timestamps (e.g. `offset="1001/30000s"`).
   - Resolve imports FCPXML natively.
2. **FCP7 XML (xmeml version 4):**
   - The universal interchange format supported natively by Adobe Premiere Pro and DaVinci Resolve.
   - A single `<xmeml version="4">` document containing a `<sequence>` with `<media><video><track>` and `<audio><track>` nodes, placing clips at sample-accurate `start` and `end` frame offsets.
3. **Embedded SMPTE Timecode:**
   - Every exported QuickTime/MP4 file should have an embedded QuickTime timecode track (`tmcd`) starting at the session wall-clock time-of-day (e.g. `14:32:05:00`). This allows editors to sync tracks inside Premiere/Resolve using the "Sync by Timecode" context menu option even without an XML file.

### 4.4 Standardized Directory Structure
Editors expect clean, predictable file structures:
```
Project Name - 2026-10-13/
  Take 01/
    manifest.json
    Take01_Multicam.fcpxml
    Take01_Sequence.xml
    video/
      01_Ben_cam1.mov
      02_Sarah_cam1.mov
      03_Sarah_phone.mov
    audio/
      01_Ben_mic.wav
      02_Sarah_mic.wav
    reference/
      rough_mix.wav
    raw/  (unmodified browser chunks, archived)
```

---

## 5. Corrections & Additions to Current StudioLink Docs

Review of `studiolink/docs/research/product-landscape.md` and `studiolink/docs/PRD.md`:

1. **VDO.Ninja Sync Capabilities:** Current docs stated VDO.Ninja has no sync. Clarification: VDO.Ninja provides manual latency compensation via `&sync=X` (ms), but has *no automated cross-source timeline alignment*. Our automated GCC-PHAT + drift correction is a major differentiator.
2. **Riverside NLE XML Support:** Current docs noted Riverside exports XML "for some NLEs". Verification: Riverside exports Premiere XML and FCPXML natively, but editors regularly complain about VFR drift inside Premiere. Conforming to CFR via ffmpeg on our host solves a known Riverside deficiency.
3. **Final Cut Camera Scale:** Confirmed exact limit of 4 cameras in Live Multicam. Remote camera control (white balance, focus, exposure) is a valuable feature for phone sub-sources to add to the P2 roadmap.
4. **License Constraints:** Confirmed that VDO.Ninja (AGPL-3.0) and SonoBus (GPL-3.0) cannot be source-forked into our MIT project; OpenStudio (MIT) and AudioCap (BSD-2-Clause) are permissively licensed.
