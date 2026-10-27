# Parachute: session notes in the vault

Status: implemented (SL-P4-1, 2026-10-25) and tested only against an `httptest` vault. Nothing has been sent to a
real vault.

## What it does

**Send to Parachute** in the export modal creates one vault note per exported take. Sending again updates the same
note. The note follows Minutes' meeting-note conventions (`MinutesExport/ParachuteVault.swift`,
`ParachuteMeetingNote`), so p2p.video sessions sit next to Minutes meetings:

| | Minutes | p2p.video |
|---|---|---|
| Path | `<folder>/<yyyy-MM-dd HHmm> <title>` | same; takes after the first get ` (Take N)` |
| Folder | `Meetings` | `Meetings` (`--parachute-folder`) |
| Body | Markdown, no front matter | same |
| Transcript | `## Transcript` + `**Name** (M:SS): text` | same, from `transcript/transcript.json` |
| Tags | `meeting` | `meeting`, `podcast`, `studiolink` |
| Metadata (strings) | `source`, `minutes_meeting_id`, `date`, `attendees` | `source: studiolink`, `studiolink_session_id`, `studiolink_take_id`, `take`, `date` (RFC 3339 UTC), `attendees`, `duration_seconds`, `export_path`, `transcript: minutes` |
| Re-send | PATCH with `if_updated_at`; 409 means edited in the vault and left alone; 404 recreates; POST 409 keeps both | same |

The body lists the date, participants as `[[wikilinks]]`, duration, "Recorded with p2p.video, one isolated track per
participant", a `file://` link and the host path of the export package, and the key files (manifest, FCPXML/xmeml,
transcript files, clip-editor handoff). If there's no transcript yet, the body says so. Transcribe, then send again,
to update the note in place.

## REST calls

```
POST  <hub>/vault/<vault>/api/notes            {path, content, tags, metadata}
PATCH <hub>/vault/<vault>/api/notes/<id>       {content, metadata, path, if_updated_at | force}
Authorization: Bearer <JWT>
```

Responses may be `{note: {...}}` or the bare note. `id`, `updatedAt`/`updated_at` and `path` are read. Receipts
(`id`, `updatedAt`, `path`, keyed by vault URL + take id) are stored in `<data-dir>/integrations/parachute-receipts.json`
(0600, in a 0700 folder). They hold no credentials.

## Credentials

Minutes uses OAuth 2.1 + PKCE and keeps tokens in the Keychain (app and `minutes-cli vault`), with a legacy adapter
that mints `parachute auth mint-token --scope vault:default:write --ephemeral`. A Go daemon has no Keychain session
of its own, so p2p.video picks the option that stores no secret:

1. **Default: mint at send time.** `parachute auth mint-token --scope vault:<vault>:write --ephemeral --label studiolink`
   (argv only, 30 s timeout, golden-tested in `TestCLITokenSourceArgvAndErrors`). Ephemeral means a 1-hour JWT, used
   for that send and then dropped. The CLI is found at `--parachute-cli`, then `$PATH`, `~/.bun/bin/parachute`,
   `/opt/homebrew/bin`, `/usr/local/bin`. Its own folder leads the child's `PATH`, because the bun-installed CLI is
   `#!/usr/bin/env bun`. Its stderr is never surfaced, and `STUDIOLINK_SECRET_KEY` is not passed to it.
2. **Opt-in: `--parachute-token-file <path>`.** Refused unless it is a regular file (not a symlink), mode with no
   group/other bits (`chmod 600`), and owned by the daemon's user.

The token is never logged, never returned by the API and never shown in the UI. Server error messages are cut to 200
characters, and the token is redacted if a server echoes it. 401/403 report "the vault refused the credential".
Redirects are not followed, so the Authorization header never leaves the configured origin. The origin must be
`https://…`, or `http://` on loopback only (a local hub such as `http://127.0.0.1:1940`). Vault names are
`[A-Za-z0-9_-]{1,64}`.

## Configuration

```sh
p2pvideod --parachute-url https://hub.example.com --parachute-vault default [--parachute-folder Meetings] \
            [--parachute-cli /abs/parachute | --parachute-token-file /abs/token]
```

Without `--parachute-url` the action is shown as unavailable, with the reason. The daemon checks the origin, the vault
name and (when minting) that the CLI exists at start. The token file is checked on each send, and the first real check
of credentials is the first send.

## Security

- Routes are host-only (`requireHost`): `POST /api/v1/takes/{id}/parachute`, plus the status view. Funnel requests
  are refused even with a valid host token (`TestFunnelGuestJoinsWhileManagementStaysDenied`). Guests and other
  sessions' hosts get 401 (`TestTakeIntegrationRoutesAreHostOnly`).
- No hosted service is added. The vault is the user's own Parachute hub.

## Verification

`go test ./internal/parachute ./internal/integrations ./cmd/p2pvideod`: request shape (method, path, auth header,
content type, tags, metadata, Markdown body), update in place with `if_updated_at`, stale receipt → "edited in the
vault", recreate after deletion, path collision, token absent from errors and logs (including a hostile server that
echoes the Authorization header), CLI mint argv and environment, token-file permission and symlink rules, origin and
vault validation, and receipts written 0600.

**Unverified:** a real hub's exact response shape for PATCH. It is parsed like Minutes parses it, and that has worked
against the user's hub. Also unverified: a real `mint-token` call from the daemon under the Mac app.
