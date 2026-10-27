# Live captions and the caption provider protocol (SL-P4-2)

p2p.video shows Meet-style live captions with exact speaker attribution, recognized
**on the host's Mac**. No cloud speech service is involved. The browser's Web Speech
API is never used, because Chrome sends that audio to Google.

This document specifies the **caption provider protocol** (version 1). The p2p.video
Mac app implements it with Apple's on-device speech. Any other local app (for example
Minutes) can implement the same protocol and replace it.

## How it works

```
 guest browsers ──WebRTC──▶ SFU ──WebRTC──▶ HOST browser (in the call)
                                              │ AudioWorklet tap per participant
                                              │ 16 kHz mono Int16, 100 ms frames
                                              ▼
                     p2pvideod  /api/v1/sessions/{id}/captions/ingest   (host token)
                                              │ opaque stream IDs, same frames
                                              ▼
       caption provider  ◀── /api/v1/captions/provider  (loopback + desktop token)
       (p2p.video.app: SpeechAnalyzer/SpeechTranscriber, one stream per participant)
                                              │ JSON results (partial/final, times)
                                              ▼
                     p2pvideod ── signaling `caption` ──▶ every admitted participant
                                 └─ final segments kept in memory → host VTT/JSON
```

* **Speaker attribution is exact.** Each participant's audio is a separate WebRTC track.
  The host's page taps each track separately and keys the frames by participant ID, and
  the daemon attaches the name from its admitted roster. The recognizer never decides
  who spoke and never learns names or participant IDs. It only sees opaque stream IDs.
* **Not the recording pipeline.** Captions are made from call (WebRTC-received) audio.
  They never write into takes, tracks, uploads or exports, and nothing in recording reads
  them. `host/internal/captions` has a guardrail test for imports in both directions.
  The post-take transcript built from recorded ISO tracks is a separate feature
  (`TRANSCRIPTS.md`) with its own provider socket; the two share no code path in the daemon.
* **Privacy.** When the host turns captions on, everyone in the call and in the waiting
  room sees a "Captions on" indicator. Audio and caption text are never logged.
  Transcripts live only in the daemon's memory until it restarts (at most the 20 most
  recently ended meetings, 20 000 final segments each). The host can download them as
  VTT or JSON.

## Using it

1. Run p2p.video.app with **Settings › Live captions › Recognize live captions on this
   Mac** turned on (the default). The first use of a language downloads Apple's on-device
   model once, which is a system asset like a dictation language. No audio is sent.
2. In a meeting, the host presses **CC** (or `c`) to turn captions on for everyone. The
   **▾** menu next to CC has the room switch, the spoken language (en-US by default), the
   recognizer status and transcript downloads.
3. Each viewer shows or hides captions with **CC** / `c`. This choice is per browser and
   stored in `localStorage`.
4. At **End meeting**, the host is offered the transcript (`.vtt` or `.json`).

Headless alternative to the app (also the test harness):

```sh
cd macapp && swift build
.build/debug/studiolink-captions provider --port 7443 --data-dir ~/Library/Application\ Support/p2p.video
.build/debug/studiolink-captions transcribe speech.wav --language en-US   # file → JSON results
```

## Endpoints

| Endpoint | Who | Auth |
|---|---|---|
| `GET /api/v1/sessions/{id}/captions/ingest` (WebSocket) | host page | host token (`?token=` or Bearer) **and** local management (`requireHost`). Never through Funnel. One feed per meeting (409 otherwise). |
| `GET /api/v1/sessions/{id}/captions/transcript?format=vtt\|json` | host | same as ingest |
| `GET /api/v1/captions/provider` (WebSocket) | provider | **direct loopback only** (loopback peer and `Host`, no `Origin`, no `X-Forwarded-*`/`Forwarded`, no `Tailscale-*` headers) **and** `Authorization: Bearer <data-dir>/.desktop_token`. One provider at a time. Same rules as `/api/v1/desktop/state`. |

The desktop token is a 0600 file next to the daemon's data. Only processes running as the
owner can read it.

## Audio frames (binary WebSocket messages)

Both the ingest and provider sockets use the same frame:

| Offset | Size | Field |
|---|---|---|
| 0 | 1 | version = `1` |
| 1 | 1 | N = stream key length, 1–64 |
| 2 | N | stream key, ASCII `[A-Za-z0-9_.:-]` |
| 2+N | rest | PCM, signed 16-bit little-endian, mono, 16 000 Hz; even length, 1 sample to 1 s |

On ingest, the key is the speaking participant's ID. Towards the provider, it is the
daemon's opaque stream ID (`s1`, `s2`, …). Golden vector (key `s1`, samples
`[0, 1, -1, 32767, -32768]`): `01 02 73 31 00 00 01 00 ff ff ff 7f 00 80`. It is checked
by the Go, Swift and web tests.

The host page skips frames that are pure digital silence (muted tracks).

## Provider control messages (JSON text messages)

Every message has `"t"`. Fields that a type doesn't use are omitted. Shared fixture:
`macapp/Tests/StudioLinkCoreTests/Fixtures/caption-provider-messages.json`, decoded by
the Go and Swift tests.

**Provider → daemon**

* `hello` must be the first message, within 5 s:
  `{"t":"hello","protocol":1,"name":"p2p.video on-device speech","engine":"apple-speechtranscriber","languages":["en-US",…]}`.
  A wrong protocol, or another provider already connected, closes the socket with 1008.
* `result`:
  `{"t":"result","streamId":"s7","segmentId":"1","text":"Welcome to the","isFinal":false,"startMs":0,"endMs":1200}`
  * `segmentId` (≤ 64 chars) is stable while a segment is partial. Send the segment again
    with new text as recognition improves, then once with `"isFinal":true`. The next
    segment uses a new ID.
  * A final with empty `text` withdraws the partial.
  * `startMs`/`endMs` count milliseconds of audio from the stream's first sample. The
    daemon maps them to host wall-clock time and clamps them to the audio it actually sent.
  * The daemon trims text, folds control characters, caps it at 500 characters, sends at
    most one partial per 150 ms per stream, and accepts at most 40 results per second per
    stream.
* `error`: `{"t":"error","streamId":"s7","code":"unsupported_language","message":"…"}`. The
  daemon ends that stream and starts a new one on the next audio. It logs the `code`
  only, never the `message`.

**Daemon → provider**

* `ready`: `{"t":"ready","protocol":1}`, sent once the hello is accepted.
* `stream_start`: `{"t":"stream_start","streamId":"s7","language":"en-US","sampleRate":16000,"encoding":"pcm_s16le"}`.
  Binary frames for `s7` follow.
* `stream_end`: `{"t":"stream_end","streamId":"s7"}`. No more audio arrives for this
  stream. The provider should send its remaining finals soon after (the app allows 3 s).

A stream ends, and a new one starts on the next frame, when:
* no audio has arrived for the stream for 5 s (for example, the participant is muted)
* their audio fell more than 2 s behind wall time (a gap)
* a frame couldn't be queued to the provider (so timestamps never skew)
* the host changes the language or turns captions off
* the participant is no longer admitted

## Signaling (call participants)

The protocol has its own block in `host/internal/signaling/protocol.go`, mirrored in
`web/src/lib/protocol.ts` and `testdata/protocol/caption*.json`.

* `caption` (S→C, admitted only):
  `{id, seq, participantId, name, text, isFinal, startMs, endMs}`. The times are host Unix
  ms. Clients replace a line with the same `id` and ignore lower `seq`.
* `caption_state` (S→C, admitted **and** waiting room):
  `{enabled, language, provider, providerName?}`
* `caption_control` (host → S): `{enabled, language?}`. Only the host may send it; the
  hub replies `action_error` to anyone else.

## Limits

16 streams per meeting. Audio may run at most 2 s ahead of wall time; extra frames are
dropped. 1 s per frame. 512 queued messages to the provider.

## Implementing a provider (e.g. Minutes)

1. Read `<data-dir>/.desktop_token`. Connect to `ws://127.0.0.1:<port>/api/v1/captions/provider`
   with `Authorization: Bearer <token>` and no `Origin`.
2. Send `hello`, then wait for `ready`.
3. For each `stream_start`, create an on-device recognizer for `language`. Feed it every
   binary frame with that stream key. Report results as above, then finish on `stream_end`.
4. Reconnect with backoff (the app uses 1, 2, 4, 8, 16, then 30 s). On disconnect the
   daemon drops all streams, and a new connection starts fresh.

`macapp/Sources/StudioLinkCore` (`CaptionProtocol`, `CaptionSegmentAssembler`,
`CaptionStreamRouter`, `CaptionProviderClient`) is Foundation-only and can be reused as
is with any `CaptionRecognizerFactory`.

## Apple Speech engine

* **macOS 26+: `SpeechAnalyzer` + `SpeechTranscriber`** (default). It runs on device by
  design, with no server path. It has no per-request duration limit, gives volatile
  (partial) and final results with audio time ranges, and accepts the protocol's
  16 kHz Int16 mono natively (`bestAvailableAudioFormat`). It needs no Speech
  Recognition (TCC) prompt. On this Mac (macOS 26.6.2, SDK 27.0) it ran unattended.
* **macOS 14–25: `SFSpeechRecognizer`** with `requiresOnDeviceRecognition = true`. It
  fails closed if on-device recognition is unavailable, and rotates the task every 12 s,
  the same approach as Minutes' `LiveCaptionRecognizer`. It needs Speech Recognition
  permission, which only an app bundle can request; unattended, the CLI gets
  `not_authorized`. Force it for testing with `STUDIOLINK_CAPTIONS_ENGINE=sfspeech`.
