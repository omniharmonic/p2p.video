# Optional uncompressed microphone capture

Each participant can enable Uncompressed microphone before joining. It is off by default. The call and valid MediaRecorder A/V file remain unchanged. A separate local microphone request asks for 48 kHz, up to two channels, and disabled echo cancellation, noise suppression and automatic gain control. AudioWorklet records the actual Web Audio Float32 samples into IEEE Float32 WAV; the sidecar includes actual rate, channel count and source track settings. Browser/OS/device processing is still possible, so this does not promise bit-perfect hardware capture.

## Durability and recovery

Sample blocks use immutable sequences 1..N. Every block and its digest/metadata journal close before network upload. Sequence 0 is reserved for the exact 58-byte WAV header, generated only after the worklet flush and durable sample writes finish. Host assembly requires all sequences including 0; it never treats a headerless stream as a completed file. Write buffering is bounded to 64 MiB. The conservative stereo RIFF32 limit stops capture visibly before size fields overflow; RF64/automatic file rotation is not implemented.

A reopened browser can reconstruct a header from the hash-verified committed journal. That recovered prefix has `captureComplete:false`; host state becomes `partial`, the UI describes it as partial, and export remains blocked. Unjournaled tail data is not assumed to be recorded. Missing/corrupt fragments fail recovery. Unfinished A/V without its final sidecar remains available for review rather than falsely finalized.

## Ownership, timing and deliverables

`replacesAudioOfTrackId` pairs isolated microphone audio to one A/V track. Post-processing validates same take, participant ID ownership, canonical WAV header geometry, frame count and complete capture. Multiple audio replacements for one video are rejected. Both raw originals remain intact. The editor export uses the isolated PCM source once and omits the paired embedded audio; unrelated camera/audio sources are unaffected. Existing delivery normalization produces 48 kHz/24-bit WAV; it cannot restore fidelity lost before Float32 capture or by the optional A/V-only path.

Both recorders use the same measured clock-fit snapshot. PCM's origin is estimated after the worklet observes its first real render block; the sidecar explicitly disclaims calibrated input latency. Alignment remains `sidecar-coarse`, not sample-accurate capture synchronization.

An opted-in PCM startup/runtime failure stays visible and stops that device; host capture failure stops the shared take. There is no silent fallback to compressed-only recording for an opted-in take.

## Verification and limits

The real Chromium oscillator test decodes an actual OPFS-backed WAV and checks rate, stereo channels, duration, exact RIFF sizes and independent 440/880 Hz channel frequencies. Two-context daemon tests verify four playable originals and exactly two video/two audio editor outputs, default-off compatibility, page-close prefix recovery, PCM microphone-denial abort, and the existing OPFS exhaustion abort. Unit/Go tests cover buffer/header ordering, RIFF overflow, missing-header refusal, partial state, ownership and replacement conflicts.

These are short, same-machine synthetic checks. Real audio interfaces, other browsers, long recordings, physical input quality, remote Tailscale sessions and editor-import timing still require acceptance testing. See the evidence manifest in `audit-2026-10-14/pcm-evidence` beside the repository.
