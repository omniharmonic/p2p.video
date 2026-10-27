# Minutes: post-take transcript handoff

Status: **Minutes is optional** (SL-P4-3, 2026-10-26). Transcripts work without it: the default engine is
p2p.video's own on-device engine in the Mac app ("Built-in (this Mac)", `TRANSCRIPTS.md`), which produces the same
transcript through the same merge. Minutes is the second choice in **Transcript › Transcription engine** and can only
be chosen when a `minutes-cli` with `transcribe-file` is detected.

The Minutes path below is implemented on p2p.video's side (SL-P4-1, 2026-10-25) against the CLI contract below and
kept intact. The Minutes side is implemented on the Minutes branch `feat/transcribe-file-json` (commit `5d92c01`),
not merged. A real Apple Speech run through `minutes-cli` from p2p.video has **not** happened yet; see "Speech
permission" below. (Real recognition through the built-in engine has: `TRANSCRIPTS.md` › Verification.)

## Why isolated tracks

Google Meet gives Minutes no per-speaker audio, so Minutes has to diarize a mixed recording and guess who spoke.
A p2p.video export already contains one WAV per participant microphone (`audio/*.wav`, listed in `manifest.json`).
Each file holds one voice, so the speaker of every recognized word is known: it is the participant who owns the
track. p2p.video transcribes each track separately and names the segments itself. There is no diarization step to
get wrong. Bleed from another participant's voice into a microphone is the one remaining error source (see
"Known limits").

## What Minutes could already do, and the gap

`minutes-cli` (`MinutesKit/Sources/minutes-cli/main.swift` at Minutes `98817aa`) can:

- `import <audio>` → create a meeting in the library; `process <meeting-id>` → diarize + transcribe into the store;
  `assign <meeting-id> <cluster>=<name>` → name a cluster;
- `export <meeting-id> --target markdown|parachute` → Markdown/vault note;
- `show <meeting-id>` → a human summary with the first five utterances, truncated to 80 characters.

None of these gives a caller a machine-readable transcript of one file. Reaching one meant `import` + `process` +
`assign` per track, writing every p2p.video track into the user's Minutes library, then scraping Markdown. That
fails the requirement "never modify the user's real Minutes data" and loses word timings and confidences.

## The additive change (implemented on the Minutes branch)

```
minutes-cli transcribe-file <audio-path> [--speaker <name>] [--locale <id>] (--json | --output <file>)
```

- Runs the same pipeline as `process` (`PipelineOrchestrator`: FluidAudio speech regions + on-device Apple Speech)
  on one file.
- **Stateless.** It is dispatched before the library store is opened. It never reads or writes `minutes.db`, and
  `--data-dir` is accepted and ignored.
- `--speaker` names every segment. Without it, Minutes' cluster labels are kept.
- `--output <file>` writes the JSON atomically with mode 0600 and prints one summary line. `--json` prints the JSON to
  stdout instead. Exactly one of the two is required.
- Exit 0 on success. A file with no recognizable speech is a success with `segments: []`. Exit 1 with
  `Error: <message>` on stdout for any failure (existing CLI convention).
- Code: `MinutesSpeech/FileTranscript.swift` (`FileTranscriptDocument`), `minutes-cli/FileTranscriptCLI.swift`, one
  dispatch line plus a help line in `main.swift`, tests in `Tests/MinutesSpeechTests/FileTranscriptTests.swift`
  (pass), README section. No migrations, no changes to existing commands, no release or signing changes.

### Output: `minutes.file-transcript/1`

```json
{
  "schema": "minutes.file-transcript/1",
  "audio": "Ana_mic_t1.wav",
  "duration": 9.5, "speechDuration": 3, "uncoveredSpeechSeconds": 0,
  "speaker": "Ana", "clusters": 1,
  "engine": { "asr": "apple-speech-on-device", "diarization": "fluidaudio", "locale": "en-US" },
  "segments": [
    { "start": 0.5, "end": 2, "speaker": "Ana", "cluster": "c1", "text": "Welcome to the show.", "confidence": 0.8,
      "words": [ { "word": "Welcome", "start": 0.5, "end": 0.9, "confidence": 0.9 } ] }
  ]
}
```

Times are seconds from the start of the file. `audio` is the file name only; full paths are never echoed.
`confidence` is the mean word confidence and is omitted when the recognizer gave no words. The shared fixture is
`testdata/integrations/minutes.file-transcript.v1.json`; p2p.video's parser test and the Minutes encoder test use the
same shape. Additive fields keep the schema id, and a breaking change bumps it to `/2`. p2p.video rejects any other
schema.

## p2p.video's side

| Piece | Where |
|---|---|
| Discovery, argv builder, runner, parser | `host/internal/minutes` |
| Merge, JSON/Markdown/SRT/VTT | `host/internal/transcript` |
| Background job, package writes | `host/internal/integrations` |
| Routes (host-only) | `host/internal/httpserver/integrations.go` |
| UI | `web/src/components/EcosystemActions.ts` (export modal) |

**Discovery** (`minutes.Discover`): `--minutes-cli <abs path>` if given (it must exist, with no silent fallback), then
`$PATH`, then `/Applications/Minutes.app/Contents/{MacOS,Helpers}/minutes-cli`, `/opt/homebrew/bin`, `/usr/local/bin`,
`~/Applications/Minutes.app/...`, `~/.local/bin`, `~/bin`. The Minutes.app bundle doesn't ship the CLI today, so on
this machine the CLI has to be built (`swift build -c release --product minutes-cli`) and passed with
`--minutes-cli`. Before the first use p2p.video runs `minutes-cli help` and requires `transcribe-file` in the
output. An older CLI shows "minutes-cli does not support transcribe-file" in the UI.

**Invocation** (golden-tested in `TestTranscribeFileArgsGolden`), argv only through `os/exec`, never a shell:

```
<minutes-cli> transcribe-file <package>/audio/<file>.wav --speaker <participant> --locale en-US --output <private dir>/transcript.json
```

The output directory is a fresh 0700 folder under `<data-dir>/integrations/tmp`, removed after each file. The child
gets a minimal environment (HOME, USER, PATH, TMPDIR, LANG), so `STUDIOLINK_SECRET_KEY` never reaches it. The speaker
name is collapsed to one line, its leading dashes are stripped so it can't become a flag, and it is capped at 120
characters. The timeout is 5 min + 2 s per audio second.

**Which tracks** (`export.Manifest.SpeakerAudio`): one per participant. A dedicated microphone WAV is preferred over
audio extracted from that participant's camera recording. Screen-share audio is never a speaker. The manifest now
carries `participantId` per source (additive), so the transcript can name the participant.

**Merge** (`transcript.Merge`): each segment and word is shifted by its track's manifest `offsetSec` onto the take
timeline (zero is the same origin as every manifest offset). Results are clamped at zero and ordered by start, then
end, then track order, then segment order, so the output is deterministic. Overlapping speech stays overlapping. Tests:
`TestMergeAppliesOffsetsAndOrdersByTakeTime`, `TestMergeIsDeterministicAndClampsBeforeTakeZero`.

**Output in the export package:**

```
transcript/transcript.json   studiolink.transcript/1: session, take, speakers[{name, participantId, sourceId, file, offsetSec, segments, status ok|empty|error, error?}],
                             segments[{start, end, speaker, participantId, sourceId, text, confidence?, words[]}]
transcript/transcript.md     "# Title — Take N", "Speakers: …", "## Transcript", "**Name** (M:SS): text" (Minutes' convention)
transcript/transcript.srt    cues ≤ 84 characters and ≤ 7 s (≥ 0.5 s), split at word timings when present, "Name: text"
transcript/transcript.vtt    the same cues with <v Name> voice spans, escaped
```

The four files are staged and swapped in together under the pipeline lock. If `manifest.json` changed while Minutes
was running (a re-export), nothing is written and the job reports "run Transcribe again". The editor-package ZIP now
includes `transcript/` and `handoff/`. A failed track doesn't fail the job: the transcript is written from the other
tracks, that speaker is marked `status: "error"`, and the UI message names the failure. If every track fails, the job
fails.

**Auto-transcribe:** a per-session setting `auto_transcribe` (`PATCH /api/v1/sessions/{id}/settings`, host only, and a
checkbox in the export modal). When it's on, every successful export starts the transcript job.

## Speech permission (the open blocker for a real Minutes run)

This blocks only the Minutes engine. The built-in engine uses `SpeechAnalyzer`/`SpeechTranscriber` on macOS 26, which
needs no Speech Recognition grant and ran unattended (`TRANSCRIPTS.md`). p2p.video.app now declares
`NSSpeechRecognitionUsageDescription` and has an **Allow Speech Recognition…** button (shown when its own pre-macOS 26
fallback needs it); whether a granted p2p.video.app also covers a `minutes-cli` launched by its daemon is still
unverified (attended TCC run).

Minutes' batch path never prompts: `ASRService.performRecognition` requires `SFSpeechRecognizer.authorizationStatus()
== .authorized` for the **responsible process**. TCC attributes a CLI to the app that launched it, so:

- run from Terminal or an agent shell, the check uses that terminal's Speech permission (denied here, so the real run
  below failed);
- run by `p2pvideod` under the p2p.video Mac app, it uses **p2p.video.app's** permission. The app has no
  `NSSpeechRecognitionUsageDescription` and has never asked.

The integration works end to end with a stand-in CLI. With the real CLI, the user has to grant Speech Recognition to
whichever app launches the daemon. Two follow-ups, and neither is done:

1. **p2p.video.app** adds `NSSpeechRecognitionUsageDescription` and an explicit "Allow transcription" button that
   calls `SFSpeechRecognizer.requestAuthorization` from the app (LaunchServices-launched, so the prompt is allowed).
   The daemon's `minutes-cli` children then inherit the permission. Small, but it needs an attended TCC run to verify.
2. Or, a larger Minutes change: hand the job to the running Minutes.app (which already holds Speech permission) via a
   URL scheme or XPC instead of an in-process CLI. Not proposed for now.

## Verification

- `cd host && go test ./internal/minutes ./internal/transcript ./internal/integrations ./internal/httpserver`: argv
  golden, discovery order, private output folder, CLI error line surfaced, schema/time validation, a stand-in CLI
  through real `os/exec`, merge ordering with offsets, SRT/VTT golden files, cue limits, atomic writes, partial and
  total failure, a manifest changed mid-run, one job per kind, auto-transcribe, host-only routes, Funnel denial.
- `web/tests/studio.browser.mjs` runs a real two-person take, exports, transcribes with the built-in engine (fake
  provider), then selects **Minutes** and transcribes again against a stand-in `minutes-cli` that honours this
  contract, and checks `transcript/*`, `handoff/*` and the decoded program mix.
- Minutes: `swift test --filter FileTranscriptTests` (3 pass). `swift build -c release --product minutes-cli` builds,
  and the argument errors behave as documented.
- **Real run (2026-10-25):** two synthetic TTS voices (`say -v Samantha`, `say -v Daniel` → 48 kHz/24-bit WAV), run
  through the branch build. `minutes-cli transcribe-file ana.wav --speaker Ana --output ana.json` exited 1 with
  `Speech recognition permission denied by user or system policy`, because the agent shell has no Speech permission.
  The opt-in Go test (`STUDIOLINK_MINUTES_CLI=… STUDIOLINK_MINUTES_AUDIO=… go test ./internal/minutes -run Real`)
  reached the real CLI and surfaced the same message. FluidAudio models are installed (`models status`: "Speaker
  recognition is ready"). **No real transcript has been produced yet.**

To run it yourself in a Terminal that has Speech Recognition permission:

```sh
cd /path/to/minutes && git switch feat/transcribe-file-json
swift build -c release --package-path MinutesKit --product minutes-cli
MinutesKit/.build/release/minutes-cli transcribe-file /abs/path/voice.wav --speaker "Ana" --json
```

## Known limits

- Bleed: a participant's microphone can pick up another participant's voice (speakers, no headphones). That speech is
  attributed to the track owner. Minutes reports `clusters` and a per-segment `cluster`, so a later pass can drop
  low-energy secondary clusters. Not done yet.
- English (`en-US`) only from p2p.video today. The CLI accepts `--locale`.
- Alignment is p2p.video's clock alignment (sidecar-coarse), so transcript times inherit its uncertainty.
