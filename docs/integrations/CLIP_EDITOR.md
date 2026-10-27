# Clip editor: p2p.video export → agentic video editor (`ave`)

Status (SL-P4-4, 2026-10-26): **verified end to end on a real p2p.video export.** A 150 s, three-person take
(Chromium fake devices, `say`-voiced microphones) went through p2p.video's export, transcript and handoff, then
through every deterministic `ave` stage up to an exported vertical short. Both a stock `ave` (`main` at `cd9f8e7`)
and `ave` with the new `ave import-studiolink` (branch `feat/studiolink-import`, not merged) were run. What ran and
what didn't is listed under "Verified run". The transcript came from a stand-in Minutes CLI, because real Apple
Speech is still blocked by the Speech permission (`MINUTES.md`).

## What the editor expects

`ave` works on an *episode* folder: `raw/` (read-only footage and audio), `work/` (artifacts), `edits/`, `exports/`.
Its pipeline is `ingest → audio → transcribe → sync → map cameras → plan-cuts → assemble → cleanup → final → clips →
shorts`. Stages are "done" when their artifact exists on disk, so files p2p.video supplies count as done stages.

- `work/transcript.json`: `{source, engine, duration, speakers{S1:{talk_seconds,name}}, words[{w,start,end,conf,speaker}],
  utterances[{speaker,start,end,first_word,last_word,text}]}`. **Episode time** is the timeline of `source`.
- `work/transcript.md`: one line per utterance, `[HH:MM:SS.ss] S1: text`.
- `work/sync.json`: `{reference, reference_duration, files{<raw path>:{start_offset, drift_ppm, ...}}}`. A file's
  `t=0` sits at `start_offset` on the reference.
- `episode.json`: `cameras{key:{file, role, session}}`, `speakers{S1:{name, camera}}`, `audio_source`. Filled at the
  "map cameras" checkpoint, which the human confirms.

A p2p.video export already knows each speaker exactly (one isolated microphone per person) and its cameras carry no
audio (`-an`), so `ave transcribe` (diarization) and `ave sync` (audio correlation) are skipped. p2p.video supplies
those files in `ave`'s own shapes, plus the camera mapping.

## What p2p.video writes (`handoff/`)

Written by **Prepare clip-editor handoff** in the export modal, and refreshed after every transcript:

```
handoff/clip-editor.json      studiolink.clip-editor-handoff/1 (index of everything below)
handoff/program.wav           every isolated mic mixed on the take timeline (mono 48 kHz, unmastered): ave's reference
handoff/ave/sync.json         -> work/sync.json        reference raw/program.wav, offsets from manifest.json
handoff/ave/transcript.json   -> work/transcript.json  on raw/program.wav (only once transcribed)
handoff/ave/transcript.md     -> work/transcript.md    ave's format (only once transcribed)
handoff/ave/episode.json      cameras / speakers / audio_source to merge into episode.json
```

`clip-editor.json` lists:

- `program`: `handoff/program.wav` and its duration. Its `t=0` is take zero, so program time is take time.
- `participants`: the manifest participants (id, name, role host/guest, lower-third name and title).
- `speakers`: `S1`, `S2`… **host first**, then everyone else in manifest order. Each has the participant name, id and
  role, the isolated `audio` file, that participant's camera `video` files (webcam first, then a phone camera; screen
  shares are not speaker cameras), and `offsetSec`.
- `sources`: the manifest sources verbatim (file, kind, participant, offset, duration, alignment method).
- `transcript`: paths to `transcript/transcript.{json,md,srt,vtt}` and the segment count, or `null`.
- `editorTimelines`: the FCPXML and xmeml in the package.
- `ave`: the work files above, `raw` (where each package file goes in an episode: `raw/<package path>`, plus
  `raw/program.wav ← handoff/program.wav`), `episode`, and the `import` command.
- `alignment`: the method, and a note that offsets are clock-based (sidecar-coarse) and drift is already corrected in
  the exported files, hence `drift_ppm: 0`.

`handoff/ave/episode.json` maps speaker `S<n>` to camera `C<n>`, that participant's own webcam (or their phone camera
if they had no webcam), with `role` host/guest and `session: "s1"` on every camera. The session matters: `ave ingest`
groups cameras into sessions by creation time and duration, p2p.video's files have no creation time, and a late
joiner's shorter camera would otherwise become a second session. Other cameras and screen shares go to
`extra_angles`, so `ave cleanup`'s cutaways never land on a slide deck. `audio_source` is `raw/program.wav`.

`ave/transcript.json` is built from p2p.video's merged transcript. Words from every speaker are sorted by take time,
and utterances are rebuilt with `ave`'s rule (same speaker, gap ≤ 1.0 s). Segments without word timings get evenly
spaced word times, marked `"timing": "interpolated"` with `conf: 0`. `engine.asr` names the recognizer the transcript
really came from (`minutes-cli: <asr>`), not a fixed label.

The program mix uses the argv builder `ffmpeg.BuildProgramMixArgs` (`adelay` for positive offsets, `atrim` for
negative ones, `amix normalize=0`, `alimiter`), golden-tested, with a synthetic-onset test. Without ffmpeg the handoff
has no `program`, no sync and no transcript for `ave` (they need it as the reference); `episode.json` is still written.

## Import into ave: one command

```sh
scripts/ave-import.sh <take folder or editor ZIP> <new episode folder> [ave checkout]
```

The ave checkout defaults to `$STUDIOLINK_AVE_DIR`, then `~/dev/agentic-video-editor`, and needs its venv
(`./scripts/setup.sh` there). The script runs `ave import-studiolink` when the `ave` has it, and the manual recipe
below otherwise. A take folder is `<data dir>/exports/<Session>_<id>/take_NN_<takeId>`; the editor ZIP holds the same
files at its root.

**`ave import-studiolink <package> <episode>`** (ave branch `feat/studiolink-import`): copies the package into `raw/`
(never moves; a different existing raw file stops the import, raw is read-only), places `work/sync.json`,
`work/transcript.json` and `work/transcript.md` (existing different work files are kept unless `--force-work`),
merges `cameras`, `speakers` and `audio_source` into `episode.json` (keys already set are kept; p2p.video's mapping is
also stored under `studiolink.mapping`), records the `transcribe` and `sync` stages as coming from p2p.video, sets a
review note for the camera-mapping checkpoint, then runs `ave ingest` + `ave audio` and checks that every path the work
files name was probed. Exit 2 lists problems. A numbered-layout episode gets the paths rebased onto `02_FOOTAGE/`.

Then, after confirming the camera mapping with stills (skill `ave-map-cameras`, `ave frame`):

```sh
ave plan-cuts <ep> && ave assemble <ep> && ave cleanup <ep> && ave final <ep>
ave clips prepare <ep>     # the agent writes picks.json, then: ave clips propose <ep> --picks picks.json
ave shorts build <ep> --clip <approved clip> && ave shorts export <ep> --clip <approved clip>
```

**With a stock ave** (no `import-studiolink`), the script follows the manual recipe: `ave init-episode`, copy each
`ave.raw` entry, copy the three work files into `work/`, merge `handoff/ave/episode.json` into `episode.json`, then
`ave ingest` and `ave audio`. Stock `ave plan-cuts` fails on a p2p.video take with "no camera covers 0.00-…s":
p2p.video's cameras start 20–50 ms after take zero and the stock planner wants a camera at episode time 0. Plan the
named session instead: `ave plan-cuts <ep> --session s1`, `ave assemble <ep> --session s1`, then `--name s1` for
cleanup, final, clips and shorts. With that, the opening shot falls back to whichever camera rolled first until the
next speaker change (the stock planner checks whole shots). The `feat/studiolink-import` branch fixes the planner: a
single-session cut list starts once every speaker camera rolls (when that is within `late_start_max_s`, 1 s) and the
lead-in is `excluded`. Episodes with a camera at 0 are unchanged.

## Verified run (2026-10-26)

Take: host + two guests, three browsers with `say` voices as fake microphones (each person speaks in their own 21 s
slot of a 63 s loop, so the take is a real three-way exchange), uncompressed microphone on, 150 s, then Export,
Transcribe and the handoff, all through the UI on the real daemon. The transcript came from a stand-in `minutes-cli`
that knows each speaker's `say` script and finds it in the recorded track by envelope correlation (agreement > 0.99),
so word times follow the real recorded audio. Package: three 1080p30 H.264 `.mov` cameras (no audio), three stereo
float WAV microphones, `program.wav` 150.18 s, 8 utterances / 336 words.

Independent checks of the time base:

- Utterance bounds in `ave/transcript.json` against speech onsets/offsets measured in `program.wav`: within 16 ms
  (the partial sentences at the take's edges are left out by the stand-in by design).
- FluidAudio Parakeet (ave's own ASR, run locally on `program.wav`) recognised the same sentences; its word starts sit
  a consistent 290–460 ms earlier than the measured onsets, which is the recogniser's timing, not p2p.video's.

| ave stage | Stock ave `cd9f8e7` (manual recipe) | ave `feat/studiolink-import` |
|---|---|---|
| import | by hand (copy, work files, mapping) | `ave import-studiolink`: 7 files, 3 work files, mapping applied, 0 problems |
| ingest / audio | ok: 7 files, one session `s1`, 4 analysis WAVs | ok (run by the import) |
| transcribe / sync | skipped, p2p.video's files used | skipped, p2p.video's files used |
| map cameras | written by hand from `clip-editor.json` | from `handoff/ave/episode.json` (human confirms) |
| plan-cuts | **fails** without flags; `--session s1` works: 7 shots, opening on the wrong camera | 8 shots, region 0.043–150.14 s, each speaker on their own camera from the first frame |
| assemble | `--session s1`: 150.17 s, 7 segments | 150.10 s, 8 segments |
| cleanup | `--name s1`: 150.2 → 113.4 s, 30 dead-air cuts, masks 22 cutaway + 8 camera change | 150.1 → 113.3 s, same |
| render (proxy) | not run | rough cut rendered |
| final (proxy) | `--name s1`: 113.4 s, -16 LUFS, captions 37 cues, re-transcribed SRT (FluidAudio) | 113.3 s, -16 LUFS, re-transcribed SRT (whisper, the worktree had no FluidAudio build) |
| clips | `prepare` 31 sentences / 3 sections; `propose` with a hand-written `picks.json` (2 picks, both complete statements) | same |
| decide / build | `decide --verdict keep` (recorded `--by sl-p4-4-test`, not an editorial decision), `build` ok | same |
| shorts | `build` (crop, title overlay, karaoke captions from p2p.video's word times) and `export --platforms youtube` (-14 LUFS, limits ok) | same |

Nothing in the run needed a paid service or an LLM: `plan-cuts` is deterministic, and the one agent judgement (clip
picks) was a hand-written `picks.json`.

### Mismatches found and fixed

| Mismatch | Effect | Fix |
|---|---|---|
| No camera at take zero (cameras start 17–44 ms late) | stock `ave plan-cuts` errors; session mode opens on the wrong camera | ave branch: `late_start_max_s` planner rule + test; stock recipe documents `--session s1` |
| No camera mapping in ave's shape, no roles | every import needed a hand-written `episode.json` | `handoff/ave/episode.json` (cameras with `session: "s1"`, roles, speakers, `audio_source`); `participants` in `clip-editor.json` |
| Cameras of different lengths split into ave sessions (no `creation_time`) | a late joiner would make a second session | every mapped camera carries `session: "s1"` |
| Recipe copied p2p.video's Minutes-style `transcript.md` into `work/` | `work/transcript.md` not in ave's format | `handoff/ave/transcript.md` in ave's `[HH:MM:SS.ss] S1: text` format |
| `engine.asr` hard-coded "minutes-cli (Apple Speech, on device)" | wrong when another recognizer made the transcript | taken from the transcript's engine |
| Speaker ids followed manifest audio order (the host was S3) | ids depended on upload order | host first, then manifest order |
| Screen shares and phone cameras could become cutaways | ave's cleanup cuts away to any non-wide camera | only each speaker's own camera is mapped; others are `extra_angles` |

### Automated checks

- `host/internal/handoff`: Go unit tests for the clip-editor index, speaker order, the episode mapping, the transcript,
  markdown and timestamp format, the engine label, and stale-file cleanup.
- `TestHandoffLoadsInAve` writes a real package (three late-starting cameras, three mics, the program mix from the
  production builder, a transcript) and loads it with ave's own code: `ave.studiolink.import_package` when available,
  the manual recipe otherwise, then ave's ingest, planner and assembly, and `editdoc.validate`. It runs when ffmpeg and
  an ave checkout with its venv exist (`$STUDIOLINK_AVE_DIR` or `~/dev/agentic-video-editor`;
  `STUDIOLINK_AVE_SRC=<ave worktree>/src` tests a worktree with that venv) and skips otherwise, and in `-short` mode.
- ave branch: `tests/test_studiolink_import.py` (6 tests) builds the same package shape and checks the import, re-import,
  kept work files and mapping, ZIP + older-package + numbered-layout imports, schema and path refusals, and the planner
  rule.

## Caveats (open)

- Offsets are clock-based (sidecar-coarse). The fake cameras show Chromium's test pattern, so lip sync between picture
  and voice was not judged here; spot-check it on the assembly (`ave render --stage assembly --preset proxy`).
- The transcript came from the stand-in CLI. A real Apple Speech transcript through p2p.video is still blocked by the
  Speech permission; its word timings will be less exact than the stand-in's.
- Fake microphones are digitally silent between lines, so `ave cleanup` found no room tone and `ave clips build` joined
  selects without gaps. Real rooms have a noise floor.
- The isolated microphone WAVs are stereo float (both channels the same); ave classifies them as `stereo_mix` and uses
  the average. Harmless, but twice the size of mono.
- The program mix is a reference for analysis and listening; the final mix in ave uses it as `audio_source`. For a
  mastered mix, the isolated WAVs in `raw/audio/` are there.
- `ave import-studiolink` lives on ave's `feat/studiolink-import` branch until it is merged. The stock-ave path works
  with the `--session s1` / `--name s1` flags.
- Not run: `ave handoff` (DaVinci Resolve bridge), music, motion graphics, `ave package`, publishing.
