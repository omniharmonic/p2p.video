# Feature parity scorecard

Status of the `meet-parity` branch against Google Meet and Riverside.fm, as of
2026-10-26. "Synthetic" means verified with Chromium fake devices on one Mac;
"real" means verified on physical devices or networks. Details: `STATUS.md`.

Legend: ✅ built and tested · 🟡 built, real-world acceptance open · ⬜ not built

## Calling (Google Meet)

| Feature | p2p.video | Evidence / gap |
|---|---|---|
| Join from a link, no account, no install | 🟡 | Tailnet guests real (Oct 15–16). Public Funnel link + TURN relay synthetic only |
| Safari / Firefox guests | ✅ synthetic, 🟡 real | Chromium host + WebKit 26.5 guest + Firefox 151 guest in one call (Playwright 1.61.1, `crossbrowser.browser.mjs`): green room, admission, everyone decodes and hears everyone (all send VP8), chat, hands, reactions, background blur, the host's screen share, then a take: WebKit records MP4 (H.264 + AAC) through the OPFS worker path (Safari ≤ 18 without `createWritable`, simulated), Firefox WebM (VP8 + Opus); chunks acknowledged during the take; export ffprobes every guest's video and audio within 2 s of the take. iPhone 13 profile in WebKit (`iphone.browser.mjs`): 390 px layout, dock and 44 px controls on screen, playsinline, private-window memory recording, sound held by the (simulated) iOS autoplay rule until one tap. Synthetic: Firefox uses its fake devices; **WebKit's camera and mic are a page-level canvas + oscillator** (Playwright's macOS WebKit never resolves `getUserMedia`); iOS autoplay is simulated and the iPhone run is macOS WebKit with an iPhone viewport and user agent. Never run: a real iPhone/iPad, real Safari or Firefox with real cameras and mics, a Safari/Firefox guest presenting its own screen, Firefox’s real OpenH264, iOS MediaRecorder and storage quotas (SL-P1-12) |
| Group calls | ✅ synthetic | 4-person browser test; call caps scale 720p → 540p → 360p |
| Reusable meeting links (rooms) | ✅ synthetic | Rotate/revoke, waiting-for-host lobby |
| Waiting room / admit / lock | ✅ synthetic | Join flood limits, 20 waiting max |
| Chat, raise hand, reactions | ✅ synthetic | History for late joiners, XSS-safe |
| Participant list, host mute/remove | ✅ synthetic | Host mute keeps the ISO recording |
| Screen share | ✅ synthetic | With system/tab audio, presentation layout |
| Grid / speaker / pin / hide self | ✅ synthetic | |
| Live captions | 🟡 | On-device Apple Speech, exact speaker names; real `say` speech recognized; multi-person real mics open |
| Device switching mid-call | ✅ synthetic | Locked while recording, on purpose |
| Connection indicators, adaptive quality | ✅ synthetic | CDP-throttled test; real lossy networks open |
| Reconnect after drop / host restart | ✅ synthetic, 🟡 real | Phone reconnect real (Oct 16); 20/20 restart runs |
| Keyboard shortcuts | ✅ | Meet bindings |
| Background blur/replace, call noise suppression, guest Leave + Rejoin | ✅ synthetic | On-device MediaPipe blur, call sender only (30 fps, 7.5 ms/frame on M2 Max); recording mic raw (headphones) or clean + separate processed call mic (shared when clean); SL-P1-6/7/8/10 |
| Native app | 🟡 | Mac menu-bar host app supervising the daemon. The call can run in a p2p.video studio window (WKWebView, SL-P2-3, ADR-021): origin-scoped camera/mic, device switching, screen share, downloads to ~/Downloads with Show in Finder, links to the default browser, close/quit confirmations, App Nap/sleep held during calls and takes, one window per meeting, launch-token sign-in. End-to-end probe (join, record, upload, close mid-take, reopen, resume, stop) passes with WebKit mock devices; real camera/mic/screen permission prompts not yet run, so the browser (Chrome) stays the default |
| Breakout rooms, polls, Q&A, whiteboard | ⬜ | Not planned for v1 |
| Calendar integration | ✅ synthetic | Schedule in a room → Upcoming on the home; `.ics` invite (PUBLISH/REQUEST/CANCEL, SEQUENCE on edit), Add to Google Calendar / Outlook template links, email and copy text, all with the room's guest link only; host calendar feed on this Mac and the tailnet (never Funnel); Mac app reads macOS calendars (Google/iCloud/Exchange via EventKit) for room links, reminds with Start, optional write-back. No OAuth, no calendar API (SL-P6-1, ADR-018). Real calendar imports and a real EventKit permission grant not yet done; Meet-link detection not planned |

## Recording and production (Riverside.fm)

| Feature | p2p.video | Evidence / gap |
|---|---|---|
| Local ISO recording per person, independent of call quality | ✅ real | Physical Mac + iPhone takes (Oct 15–16) |
| Uncompressed WAV per person | ✅ real | iPhone 48 kHz float PCM |
| Progressive upload, crash/tab-kill recovery | ✅ synthetic, 🟡 real | Interrupted upload real (Oct 15); tab-kill synthetic |
| Separate screen-share track | ✅ synthetic | Exported as its own angle |
| Phone as extra camera | ✅ synthetic | QR single-use link, joins as the participant's subsource, late source in a running take, "<Name> Phone" export angle (SL-P1-9); real phone over Funnel not yet run |
| Pre-flight checks | ✅ synthetic | Storage, devices, path, headphones |
| Editor export (Resolve/Premiere XML, FCPXML) | 🟡 | FCPXML 1.10 valid against Apple's DTD (xmllint, 24 variants) and a transcribed DTD + Final Cut timing check in CI; FCP7 XML checked against Premiere's structure; negative offsets, over-long durations and half-frame mic placement fixed (SL-P0-10). Resolve imported earlier versions natively (Oct 16); this version's Resolve run waits on `scripts/resolve-acceptance.py` (Resolve must be open with scripting allowed). Final Cut and Premiere not installed: never opened |
| Sync accuracy | 🟡 | Placement by each device's clock fit (+ PCM mic matched to its own camera). Every person is then checked acoustically against the host's received copy of their mic (GCC-PHAT): verified within ±20 ms, corrected only beyond that (≤250 ms), result per person in `manifest.json` and README. Synthetic: injected errors recovered within 0.1 ms. Fake-device 3-min rehearsal: two people verified at +0.65 / −1.85 ms, measured delays 2.6 ms apart (relative cross-device bound, same browser). Absolute capture accuracy is not established (send path modelled, Chromium-calibrated); raw-mic guests can't be checked with fake devices; real devices, other browsers and 60-min drift unmeasured |
| Fast delivery | ✅ synthetic | Originals ZIP right after upload; ProRes Proxy ~4× faster export |
| Transcripts with speaker labels | ✅ synthetic | Built-in on-device engine in the Mac app (Apple SpeechTranscriber, no Minutes needed): one isolated track per participant, so names are exact; per-track progress, cancel, retry of remaining tracks; real recognition verified unattended on `say` speech, incl. a recorded two-person take end to end and a 62-min WAV in 92 s (SL-P4-3). Minutes optional when detected. Human voices on real mics not yet run |
| Livestream (YouTube/Twitch/Facebook/LinkedIn/RTMP) | 🟡 | Verified against **local** sinks only (SL-P3-3, ADR-019): RTMPS through a local TLS proxy with certificate verification (self-signed root trusted only in tests; the production trust store refuses it), two destinations at once, one sink killed mid-stream reconnects with backoff while the other stays live, per-destination state/reason/rate and encoder health warnings in the panel, stream key absent from every process's argv (`ps`), output bitrate measured. Presets default to RTMPS with ingest URLs checked against platform docs. **Not verified: any real YouTube, Twitch, Facebook or LinkedIn stream** (owner test; the RTMP client is new code, tested against ffmpeg's RTMP listener and a scripted server) |
| Magic clips / AI editing | 🟡 | Clip-editor handoff verified with `ave` on a 150 s three-person p2p.video take (fake devices, `say` voices, stand-in transcript): import, ingest, audio, plan-cuts, assemble, cleanup, final (proxy render, re-transcribed SRT), clips prepare/propose/build, shorts build/export all ran; one command (`scripts/ave-import.sh` → `ave import-studiolink`, ave branch not merged). Clip picks were hand-written (no LLM run), lip sync not judged, no real-speech transcript (SL-P4-4) |
| Teleprompter | ✅ synthetic | Script per room (saved) or meeting (memory); host + producers edit, host picks who sees it (only me / host + producers / everyone); reader at the top centre near the camera with play/pause (Space), arrow nudge, speed, size, mirror; follow-host scroll sync (SL-P5-1). Real-camera eye line not judged by a person yet |
| Branded layouts, lower thirds | ✅ synthetic | Show name, accent, PNG/JPEG logo (validated, same-origin), per-person name + title by host/producer; labels on call tiles; branded livestream variant with logo bug and animated lower thirds (on active speaker or on demand), verified in frames from a local RTMP sink; titles exported in `manifest.json`, recordings stay clean (SL-P5-2); a room remembers each person's title by display name for the next episode, editable and clearable in the call or Home → Rooms → Saved titles (SL-P5-4). Not on real YouTube/Twitch yet |
| Async recording links ("record solo") | ✅ synthetic | Expiring, rotatable link with an optional script; guest records alone in a green room (devices, pre-flight, mic sound, uncompressed WAV), reviews, deletes and re-records, sends; same OPFS + progressive upload, resumes after a closed tab; host exports, downloads originals and hands off like any take; server-side caps (submissions, take length, storage) (SL-P5-3); abandoned drafts listed with sizes, deleted by hand or automatically (14 days idle, 24 h after the link closes; never with an upload in the last hour, never a sent recording) (SL-P5-4). Real devices and a real Funnel link not yet run |
| Producer (in the call, not recorded) | ✅ synthetic | Separate rotatable producer link (per meeting or room); host admits; heard, camera optional, badged; admits guests, starts/stops takes, mutes/removes guests, sees source health; never recorded, exported or captioned; host-only actions refused on the hub and every host route (SL-P1-11) |
| Raw vs clean ISO mic per guest | ✅ synthetic | Headphones → raw, otherwise clean (EC + NS, no AGC); host default; measured `getSettings()` in pre-flight, host view and manifest; clean shares the call's capture in Chromium (SL-P1-10). Real mics/speakers not yet heard |

## Ownership (where we're ahead)

| | p2p.video | Meet | Riverside |
|---|---|---|---|
| Media stays on your machines | ✅ | ❌ | ❌ |
| No account / subscription | ✅ | ❌ | ❌ |
| On-device captions | ✅ | ❌ | ❌ |
| Isolated tracks into your own notes vault (Parachute) | ✅ synthetic | ❌ | ❌ |
| Open source (AGPL-3.0) | ✅ | ❌ | ❌ |

## Biggest remaining risks before a public-guest episode

1. One real Funnel test from an outside network (page, call and TURN relay).
2. One real 3-person, 60-minute take on real hardware, imported into your editor.
3. Notarized build of the current branch for guests' trust and Gatekeeper.
4. Before announcing a livestream: one real stream to each platform you'll use (YouTube, Twitch, Facebook, LinkedIn) with the RTMPS presets, since p2p.video now publishes with its own RTMP client.
