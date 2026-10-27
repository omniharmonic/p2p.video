# Post-take transcripts (SL-P4-1, SL-P4-3)

After a take is exported, **Transcribe** in the export panel turns each participant's
isolated microphone track into a transcript with exact speaker names. It writes
`transcript/transcript.{json,md,srt,vtt}` into the export package, refreshes the
clip-editor handoff (`handoff/clip-editor.json`, `handoff/ave/transcript.json`), and the
Parachute session note picks it up on the next send.

There are two engines. Both produce the same `studiolink.transcript/1` document through
the same merge, so everything downstream is identical.

| Engine | Where it runs | Needs |
|---|---|---|
| **Built-in (this Mac)**, the default | The p2p.video Mac app, Apple `SpeechAnalyzer` + `SpeechTranscriber` reading the WAV on device | The app running with **Settings › Transcripts › Transcribe recordings on this Mac** on (the default). macOS 26+. No Speech Recognition prompt on macOS 26. |
| **Minutes** (optional) | `minutes-cli transcribe-file` (see `MINUTES.md`) | A `minutes-cli` that offers `transcribe-file` (today only the unmerged Minutes branch `feat/transcribe-file-json`). It can be chosen only when detected. |

No audio or text leaves the Mac with either engine, and there is no hosted service.

## Using it

1. Export the take (**Prepare editor files**).
2. In **Transcript and handoffs › Transcript**, the **Transcription engine** picker shows
   Built-in (this Mac), and Minutes when a compatible `minutes-cli` is detected
   ("Minutes (not detected)" otherwise). The choice is host-wide and is remembered in
   `<data-dir>/integrations/transcription.json` (0600).
3. Press **Transcribe**. Each participant gets a row with a progress bar ("42%",
   "12 lines", "No speech", "Failed"). **Cancel** stops it; nothing is written to the
   package. Tracks that finished are kept, so **Retry remaining tracks** only redoes the
   rest. The kept tracks are tied to the engine and the exact `manifest.json`: after a
   re-export everything is transcribed again.
4. "Auto-transcribe after export in this session" starts the same job after every export.
   If the engine can't run at that moment, the Transcript row shows the error (it is never
   skipped silently).

When the selected engine isn't available, Transcribe is disabled and the row says what to
do. The panel checks again every 5 s, so opening the app is noticed.

| State | What the host sees |
|---|---|
| App not running / built-in turned off | "Built-in transcription runs in the p2p.video Mac app. Open p2p.video (and keep Settings › Transcripts › Transcribe recordings on this Mac on), then try again." |
| Speech Recognition not allowed (pre-macOS 26 engine only) | "Speech Recognition is not allowed for p2p.video. Open p2p.video › Settings › Transcripts and choose Allow, or turn it on in System Settings › Privacy & Security › Speech Recognition, then retry." Settings shows an **Allow Speech Recognition…** button. A job that hits this mid-run ends in the Error state with the same text. |
| On-device model missing for the language | The first job downloads Apple's model (a system asset, like a dictation language). If that fails: "The on-device speech model for en-US could not be installed." |
| App quits mid-job | "The p2p.video app disconnected while transcribing. Reopen it, then retry." |
| Recognizer silent for 3 min | "The on-device recognizer stopped responding. Retry, or restart the p2p.video app." |
| A track with no speech | That speaker is `status: "empty"` and the job succeeds. A take where every track is silent writes a valid transcript with no segments. |
| One track fails | The transcript is written from the others, the speaker is `status: "error"` with the reason, and the message suggests a retry. If every track fails, the job fails and nothing is written. |

Headless hosts (no app): run the same provider from the command line:

```sh
cd macapp && swift build --product studiolink-captions
.build/debug/studiolink-captions file-provider --port <daemon port> --data-dir <data dir>
.build/debug/studiolink-captions transcribe-file /abs/path/voice.wav     # one file → JSON lines
```

## How it works

```
 export package (published)            p2pvideod                              p2p.video.app
 audio/<participant>.wav ──path──▶ integrations ──job──▶ /api/v1/transcription/provider ──▶ AVAudioFile
                                   (one track at a time)  (loopback + desktop token)          SpeechAnalyzer
 transcript/*  ◀── merge on offsetSec ◀── segments, words, progress, done/error ◀──────────── SpeechTranscriber
```

* **Speakers are exact.** Each track belongs to one participant (`export.Manifest.SpeakerAudio`:
  the dedicated microphone WAV, else the audio of that participant's camera; never screen
  audio). The recognizer never names speakers. The daemon labels every segment from the
  manifest and shifts it by the track's `offsetSec` (`transcript.Merge`), exactly as for Minutes.
* **Read-only on the package.** Engines get an absolute path to a WAV inside the published
  package and only read it. The daemon writes `transcript/` (and refreshes `handoff/`) under
  the pipeline lock, and refuses if `manifest.json` changed while transcribing. Transcription
  never blocks or changes an export.
* **Not live captions, not recording.** `host/internal/filespeech` imports nothing from the
  module; captions, the recording pipeline and filespeech don't import each other
  (`filespeech_guardrail_test.go`, `captions_guardrail_test.go`). The app's file provider is a
  separate socket and a separate setting from the caption provider.
* **Long files.** `SpeechAnalyzer` has no per-request duration limit and reads the file
  directly, so a 60-minute track is one request (measured: 62 min of WAV in 92 s, below). The
  pre-macOS 26 fallback (`SFSpeechRecognizer`, on device only) cuts the audio into chunks of at
  most 50 s at the quietest 100 ms frame near each boundary (`ChunkPlanner`) and offsets the
  results.

## Provider protocol (version 1)

`GET /api/v1/transcription/provider` is a WebSocket with the caption provider's rules: **direct
loopback only** (loopback peer and `Host`, no `Origin`, no forwarding or `Tailscale-*` headers)
and `Authorization: Bearer <data-dir>/.desktop_token`. One provider at a time (a second one is
closed with 1008). It is never reachable through Serve or Funnel (Funnel guardrail test).

JSON text messages only, each with `"t"`. Shared fixture:
`macapp/Tests/StudioLinkCoreTests/Fixtures/transcription-provider-messages.json`, decoded by
the Go and Swift tests.

**Provider → daemon**

* `hello` (first, within 5 s): `{"t":"hello","protocol":1,"name":"p2p.video on-device transcription","engine":"apple-speechtranscriber","languages":["en-US",…],"state":"ready"}`.
  `state` is `ready`, `not_authorized` or `unavailable`, with a human `message` for the last two.
* `status`: `{"t":"status","state":"ready"}`, when availability changes (e.g. permission granted).
* `progress`: `{"t":"progress","jobId":"j1","processedSec":42.5}`, at most once per 2 s of audio.
* `segment`: `{"t":"segment","jobId":"j1","segment":{"start":0,"end":1.14,"text":"Welcome to the show.","confidence":0.995,"words":[{"word":"Welcome","start":0,"end":0.48,"confidence":0.997},…]}}`.
  Times are seconds from the start of the file; `confidence` is the mean word confidence and is
  omitted when there are no words. Final phrases only, in order.
* `done`: `{"t":"done","jobId":"j1","durationSec":9.5,"locale":"en-US","asr":"apple-speechtranscriber"}`.
* `error`: `{"t":"error","jobId":"j1","code":"not_authorized","message":"…"}`. Codes:
  `not_authorized`, `unsupported_language`, `model_unavailable`, `recognizer_unavailable`,
  `audio_unreadable`, `bad_job`, `busy`, `cancelled`, `recognizer_failed`.

**Daemon → provider**

* `ready`: `{"t":"ready","protocol":1}`.
* `job`: `{"t":"job","jobId":"j1","path":"/…/exports/…/audio/Ana_mic.wav","language":"en-US","durationSec":9.5}`.
  One job at a time; the daemon queues the rest.
* `cancel`: `{"t":"cancel","jobId":"j1"}`. The provider stops and answers `error` with `cancelled`.

**Limits the daemon enforces on provider input** (`filespeech`): 256 KiB per message, 50 000
segments per job, 500 words per segment, 2 000 characters of text; times must be finite,
non-negative, ordered and within the file (+1 s); text is folded to one line. An invalid result
fails the job (`bad_result`); a segment with no text is dropped. Recognized text, job paths and
tokens are never logged.

**Provider side** (`macapp/Sources/StudioLinkCore`, Foundation-only): `FileTranscriber` protocol
(Apple implementation `AppleFileTranscriber` in StudioLinkSpeech; tests use a fake),
`TranscriptionJobRunner` (validation, one job at a time, busy/cancel, throttled progress),
`FileSegmentBuilder` (runs → words, punctuation joined, times clamped, mean confidence),
`ChunkPlanner`, `TranscriptionProviderClient` (reconnects 1, 2, 4, 8, 16, then 30 s). The job
path must be absolute, without `..`, and a `.wav/.wave/.aif/.aiff/.caf` regular file.

## Host API

All host-only (`requireHost`: the session's host token on local management), denied through Funnel:

| Route | |
|---|---|
| `GET /api/v1/takes/{id}/integrations` | View: `transcription {engine, engines[{id,label,available,code,reason,detail}], language}`, `capabilities.transcript` (the selected engine), `canResume`, and `transcript.detail.tracks[{sourceId,speaker,state,progress,durationSec,segments,reused,error}]` while/after a run |
| `POST /api/v1/takes/{id}/transcript` | Start with the selected engine (202; 503 with the reason when unavailable; 409 when running) |
| `POST /api/v1/takes/{id}/transcript/cancel` | Cancel (200; 409 when nothing runs) |
| `PUT /api/v1/takes/{id}/transcript/engine` | `{"engine":"builtin"\|"minutes"}`, host-wide (400 unknown; 503 Minutes not detected) |

Job states: `idle`, `running`, `done`, `error`, `cancelled`.

## Output

Unchanged from SL-P4-1 (`MINUTES.md` › "Output in the export package"). `engine` in
`transcript.json` names what produced it: `{"name":"studiolink-builtin","version":"studiolink.file-transcription/1","asr":"apple-speechtranscriber","locale":"en-US"}`
or `{"name":"minutes-cli","version":"minutes.file-transcript/1",…}`. The Parachute note's
`transcript` metadata is `builtin` or `minutes`.

## Verification

* Go: `go test ./internal/filespeech ./internal/integrations ./internal/httpserver ./cmd/p2pvideod`
  (protocol fixture, hello/status, segment validation, serialization, cancel with
  acknowledgement, idle timeout, disconnect; default engine, persisted choice, Minutes only when
  detected, same document shape and merge as Minutes, silence, permission denied, per-track
  progress, cancel + resume of only the remaining tracks, kept tracks invalidated by a re-export,
  auto-transcribe error state; provider socket auth incl. Funnel/Serve/Origin/second provider,
  end to end over a real WebSocket, cancel and engine routes).
* Swift: `make macapp-test` (`FileTranscriptionTests`: fixture round trip, segment builder,
  chunk planner incl. 62 min, job validation, runner with a fake recognizer: segments, throttled
  progress, silence, error codes, busy, cancel).
* **Real recognition, observed on this Mac (macOS 26.6.2, 2026-10-26), unattended, no TCC
  prompt:** `STUDIOLINK_REAL_SPEECH=1 swift test --filter RealFileTranscription` (3 pass:
  `say -v Samantha` speech converted to 48 kHz/24-bit WAV is recognized with word timings and
  confidences; `say` silence gives no segments; a non-audio file is `audio_unreadable`). The CLI
  on the same speech returned "Welcome to the show." and "Today we are talking about recording
  podcasts on a Mac." with per-word times (e.g. "Welcome" 0.00–0.48 s, confidence 0.997). A
  62-minute WAV (the same sentence + 5 s silence, looped) took **92 s**, one request, 789
  segments, last segment at 3717.36 s; 30 s of digital silence returned `done` with no segments.
* **Real end to end (2026-10-26):** `studio.browser.mjs` with `STUDIO_TRANSCRIPTION_PROVIDER=macapp/.build/debug/studiolink-captions`
  (the real Swift provider, `file-provider` mode, over the real socket) and
  `STUDIO_FAKE_AUDIO_FILE=<say speech, 48 kHz WAV>` (Chromium uses it as both microphones): a two-person take was
  recorded, uploaded and exported, and the built-in engine produced
  `Guest 0.097 "Welcome to the show."`, `Host 0.122 "Welcome to the show."`, `Host 1.202 "Today we are talking about
  recording podcast."`, `Guest 1.597 "Today we are talking about recording."` (the take ends mid-sentence), engine
  `apple-speechtranscriber`, `en-US`.
* Web: `npm test` (`ecosystem.test.mjs`: engine picker, status, per-track rows, cancel/retry
  labels, permission error, polling). Browser: `studio.browser.mjs` runs a real two-person take,
  shows the "Open p2p.video" state, connects a fake built-in provider
  (`tests/lib/fake-transcription-provider.mjs`, the real protocol over the real socket), cancels
  a run (nothing written), retries to Done, checks the package, then switches to Minutes
  (stand-in CLI) and transcribes again. `rehearsal.browser.mjs` uses the built-in path and checks
  the Parachute note metadata.

**Not verified:** the SFSpeechRecognizer fallback with a real grant (pre-macOS 26 only; it needs
an attended Speech Recognition prompt from the app bundle, and fails closed with
`not_authorized` otherwise); human voices on real microphones in a real session (only synthetic
`say` speech so far); the provider inside a packaged, running p2p.video.app (the same
`TranscriptionProviderClient` + `AppleFileTranscriber` ran in the `studiolink-captions` harness);
non-English locales; bleed between microphones (attributed to the track owner, as with Minutes).
