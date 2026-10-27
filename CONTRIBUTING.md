# Contributing to p2p.video

Thanks for helping. p2p.video is a self-hosted video call and recording studio that runs
on your own Mac; please keep changes in that spirit: no hosted services, no telemetry,
no accounts.

## Before you start

- Read [AGENTS.md](AGENTS.md) (conventions and guardrails that apply to people too),
  then [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Product scope is in
  [docs/PRD.md](docs/PRD.md) and [docs/ROADMAP.md](docs/ROADMAP.md).
- For anything larger than a small fix, open an issue first so we can agree on the approach.
- Security problems: do not open an issue. Follow
  [docs/SECURITY.md › Reporting a vulnerability](docs/SECURITY.md#reporting-a-vulnerability).

## Build and test

You need Go (see `host/go.mod`), Node.js with pnpm, and on a Mac, Xcode command line tools
(Swift 6) for the desktop app. FFmpeg and ffprobe must be on `PATH` for the media tests.

```sh
cd web && pnpm install --frozen-lockfile && cd ..
make build          # web assets, bin/p2pvideod, bin/p2pvideo
make test           # Go, web unit tests, Mac app unit tests
make e2e            # Chromium end-to-end suites (needs Playwright's Chromium)
make macapp-probe   # the in-app studio window end to end (macOS)
```

Browser suites run against a temporary daemon on free ports and a temporary data folder;
they never touch a host you are running on port 7443.

## Pull requests

- Keep each PR to one change, with tests: every package has unit tests, and media or
  sync logic needs synthetic-signal tests.
- Go: `gofmt`, `go vet`. Web: TypeScript strict. Build ffmpeg command lines only through
  `host/internal/ffmpeg` with golden argv tests.
- The signaling protocol's source of truth is `host/internal/signaling/protocol.go`;
  update `web/src/lib/protocol.ts` and `testdata/protocol/` in the same change.
- Commit messages use conventional commits, e.g. `fix(upload): resume after 409`.
- Update the docs when behaviour changes.

## License

p2p.video is licensed under the [GNU AGPL v3](LICENSE). By contributing you agree that
your contribution is licensed under the same terms. Third-party code must be compatible
and listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md); never add GPL-only FFmpeg
builds or nonfree codecs to the package.
