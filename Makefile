SHELL := /bin/sh
.PHONY: web build test e2e rehearsal dev macapp macapp-test macapp-probe
web:
	cd web && npm run build
build: web
	mkdir -p bin
	cd host && go build -o ../bin/p2pvideod ./cmd/p2pvideod
	cd host && go build -o ../bin/p2pvideo ./cmd/p2pvideo
	# Pre-rename names (StudioLink) keep working for local scripts.
	ln -sf p2pvideod bin/studiolinkd
	ln -sf p2pvideo bin/slctl
test:
	cd host && go test ./...
	cd web && npm test
	cd macapp && swift test
# Native host app (docs/DESKTOP_APP.md). The .app bundle comes from scripts/package-mac.sh.
macapp:
	cd macapp && swift build -c release --arch arm64
macapp-test:
	cd macapp && swift test
# In-app studio end to end [SL-P2-3]: the real studio window (WKWebView, WebKit
# mock devices) joins, records, uploads, closes mid-take and resumes against a
# test daemon on free ports with a temporary data dir. Opens a window briefly.
macapp-probe: build
	cd macapp && swift build
	scripts/macapp-studio-probe.sh $(PROBE_REPORT)
# Requires an installed Playwright Chromium (or PLAYWRIGHT_MODULE/CHROMIUM_PATH);
# the Safari/Firefox guest suites [SL-P1-12] also need Playwright's WebKit and
# Firefox (`cd web && npx playwright install webkit firefox`).
e2e: build
	cd web && STUDIO_BINARY="$(CURDIR)/bin/p2pvideod" node tests/studio.browser.mjs
	cd web && STUDIO_BINARY="$(CURDIR)/bin/p2pvideod" node tests/crossbrowser.browser.mjs
	cd web && STUDIO_BINARY="$(CURDIR)/bin/p2pvideod" node tests/iphone.browser.mjs
# Full-episode rehearsal [SL-QA-1]: every podcast-day feature together
# (REHEARSAL_MINUTES, default 3). Summary JSON under REHEARSAL_OUT; an
# RTMP-capable ffmpeg via STUDIO_FFMPEG_DIR (default build/media-tools/bin).
rehearsal: build
	cd web && STUDIO_BINARY="$(CURDIR)/bin/p2pvideod" node tests/rehearsal.browser.mjs
# Built UI, isolated development storage; restart to pick up source changes.
# The dashboard opens signed in via the per-boot launch token (docs/SECURITY.md);
# `./bin/p2pvideo dashboard --data-dir .studio-dev` prints the URL again.
dev: build
	(sleep 1 && ./bin/p2pvideo dashboard --data-dir .studio-dev --open) &
	./bin/p2pvideod --dev --no-funnel --data-dir .studio-dev
