# AGENTS.md: working on p2p.video

## Before you start
1. Read `docs/PRD.md`, then `docs/ARCHITECTURE.md`, then `docs/IMPLEMENTATION_PLAN.md`.
2. Pick the first unchecked task in `STATUS.md` whose dependencies are checked. Don't skip phase 0 spikes.
3. When you finish, tick the box, append a dated line to the STATUS log (what you did, what's left, any surprises), and update docs if reality differs from the plan.

## Conventions
- **Go** ≥ 1.23, module root `host/`. `gofmt`, `go vet`, `staticcheck`. No CGO (use `modernc.org/sqlite`). Use `log/slog` for logs. Wrap errors with `%w`.
- **Web** in `web/`: TypeScript strict, Svelte 5 runes, Vite, vitest, Playwright. No UI framework beyond Svelte; plain CSS variables.
- **Protocol:** `host/internal/signaling/protocol.go` is the source of truth. Update `web/src/lib/protocol.ts` and the JSON fixtures in `testdata/protocol/` in the same change.
- **ffmpeg:** build commands only through `host/internal/ffmpeg` builders, with golden argv tests. Never shell-interpolate strings.
- **Tests:** every package has unit tests. Media/sync logic needs synthetic-signal tests. E2E uses Chromium fake media devices (`testdata/`).
- **Make targets:** `make dev` (daemon + vite dev), `make test`, `make e2e`, `make build`.
- **Commits:** conventional commits, referencing the task ID, e.g. `feat(upload): resumable chunk API [SL-2.4]`.

## Guardrails
- Never expose the host dashboard or admin endpoints through Funnel. There's a test for this; keep it green.
- Never log tokens, TURN secrets or invite URLs with their tokens.
- Don't add a dependency on any hosted service (SaaS, cloud TURN, analytics). Public STUN is policy-controlled and off-able.
- Don't bundle GPL ffmpeg builds.
- Keep recording independent of the call path. Nothing in the recording pipeline may read from WebRTC-received media except `refrec` (alignment reference).
- If a spike disproves an architectural assumption, stop and update `docs/DECISIONS.md` + `ARCHITECTURE.md` before building on it.
