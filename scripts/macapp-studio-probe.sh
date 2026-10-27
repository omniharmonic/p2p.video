#!/bin/zsh
# End-to-end in-app studio probe [SL-P2-3]: runs the native app's studio window
# (WKWebView, WebKit mock capture devices) against a test daemon on free ports
# with a temporary data directory, then prints the JSON report.
# Usage: scripts/macapp-studio-probe.sh [report.json]   (make macapp-probe)
# Never touches port 7443 or ~/Library/Application Support/p2p.video (or the
# legacy StudioLink folder): the temporary data dir also disables migration.
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DAEMON="${STUDIOLINK_DAEMON:-$ROOT_DIR/bin/p2pvideod}"
[[ -x "$DAEMON" ]] || { echo "Build the daemon first (make build)." >&2; exit 1; }
APP_BIN="${STUDIOLINK_APP_BIN:-$(cd "$ROOT_DIR/macapp" && swift build --show-bin-path)/StudioLink}"
[[ -x "$APP_BIN" ]] || { echo "Build the app first (cd macapp && swift build)." >&2; exit 1; }
# The daemon reports its -ldflags build ID ("development" for `make build`).
BUILD_ID="${STUDIOLINK_BUILD_ID:-development}"

free_port() {
    python3 -c 'import socket,sys; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM if sys.argv[1]=="udp" else socket.SOCK_STREAM); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()' "$1"
}
PORT="$(free_port tcp)"; TURN="$(free_port tcp)"; UDP="$(free_port udp)"
while [[ "$PORT" == 7443 ]]; do PORT="$(free_port tcp)"; done
DATA="$(mktemp -d "${TMPDIR:-/tmp}/p2pvideo-studio-probe.XXXXXX")"
REPORT="${1:-$DATA/studio-probe.json}"

echo "==> Studio probe on 127.0.0.1:$PORT (temporary data dir $DATA)"
rc=0
STUDIOLINK_PORT="$PORT" STUDIOLINK_DATA_DIR="$DATA" STUDIOLINK_UDP_PORT="$UDP" STUDIOLINK_TURN_LISTEN="127.0.0.1:$TURN" \
STUDIOLINK_DAEMON="$DAEMON" STUDIOLINK_BUILD_ID="$BUILD_ID" STUDIOLINK_NO_OPEN=1 STUDIOLINK_STUDIO_PROBE="$REPORT" \
    "$APP_BIN" || rc=$?
if [[ -f "$REPORT" ]]; then cat "$REPORT"; else echo "No report written." >&2; rc=1; fi
rm -rf "$DATA/sessions"
[[ -n "${1:-}" ]] || rm -rf "$DATA"
echo "==> Studio probe $([[ $rc == 0 ]] && echo passed || echo FAILED)"
exit $rc
