# iOS companion app (next release)

Status: planned. Version 1.0 supports iPhone and iPad through mobile Safari: guests join a meeting from the link,
record locally and upload, and anyone in a call can add their phone as an extra camera by scanning a QR code.

## Why the Mac stays the host

The host runs the meeting server, the media relay, Tailscale Serve/Funnel and post-processing. iOS suspends
background servers, does not let apps configure Tailscale Serve or Funnel, and limits long-running work, so a phone
cannot be a dependable host. The companion app is a better guest and a better camera, not a host.

## What the app adds over Safari

| Need | Safari today | iOS app |
|---|---|---|
| Upload after the screen locks or the app is switched away | Stops until the page is reopened | `URLSession` background transfers finish the upload |
| Recording quality | Browser `MediaRecorder` (H.264/AAC) | `AVCaptureSession` at the camera's native format, optional ProRes on supported iPhones, uncompressed audio |
| Storage | Private windows keep no local copy; browser storage can be evicted | App sandbox storage, kept until the host confirms receipt |
| Joining | Paste or tap a link | Universal links for meeting, room and phone-camera links open the app |
| Phone as a camera | QR code, page must stay open | Dedicated camera mode with orientation lock, framing guides and battery/thermal status |
| Notifications | None | "You've been admitted", "Recording started", upload finished |

## Shape

- SwiftUI app with a `WKWebView` for the call UI (the same web client, so features stay in step), plus native
  modules for capture, recording, the upload queue and background transfers, bridged to the page.
- Speaks the existing protocol and upload API (numbered chunks with SHA-256 idempotency), so the daemon needs no
  iOS-specific endpoints; the recorder declares itself in the sidecar so alignment and export treat it like any source.
- Same guardrail as the web client: the recording is captured independently of the call path.
- Distribution through TestFlight first, then the App Store; the web path keeps working for anyone without the app.

## Milestones

1. Universal links + WKWebView shell joining a meeting as a guest (parity with Safari).
2. Native recorder and background upload queue; recovery after the app is killed.
3. Phone-camera mode.
4. TestFlight beta, real-device sync and quality checks against Safari recordings.
