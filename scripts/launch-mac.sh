#!/bin/zsh
set -euo pipefail
BUNDLE_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
RESOURCES="$BUNDLE_DIR/Contents/Resources"
DAEMON="$RESOURCES/bin/p2pvideod"
EXPECTED_BUILD="$(cat "$RESOURCES/build-id.txt")"
PORT="${STUDIOLINK_PORT:-7443}"
export PATH="$RESOURCES/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
DATA_DIR="${P2PVIDEO_DATA_DIR:-${STUDIOLINK_DATA_DIR:-$HOME/Library/Application Support/p2p.video}}"
# Fallback launcher only: it never moves data. Until the p2p.video app has
# migrated the old StudioLink folder, keep using that folder in place.
LEGACY_DATA_DIR="$HOME/Library/Application Support/StudioLink"
if [[ -z "${P2PVIDEO_DATA_DIR:-}${STUDIOLINK_DATA_DIR:-}" && ! -e "$DATA_DIR" && -d "$LEGACY_DATA_DIR" ]]; then
    DATA_DIR="$LEGACY_DATA_DIR"
fi
mkdir -p "$DATA_DIR"
fail() {
    print -u2 "$1"
    osascript -e 'on run argv' -e 'display alert "p2p.video could not start" message (item 1 of argv)' -e 'end run' "$1" || true
    exit 1
}
check_health() {
    local health
    health="$(curl -fsS --max-time 2 "http://127.0.0.1:$PORT/health" 2>/dev/null)" || return 1
    local product build status_value
    product="$(print -r -- "$health" | /usr/bin/plutil -extract product raw -o - - 2>/dev/null)" || return 2
    build="$(print -r -- "$health" | /usr/bin/plutil -extract buildId raw -o - - 2>/dev/null)" || return 2
    status_value="$(print -r -- "$health" | /usr/bin/plutil -extract status raw -o - - 2>/dev/null)" || return 2
    [[ "$product" == com.omniharmonic.studiolink && "$build" == "$EXPECTED_BUILD" && "$status_value" == ok ]] || return 2
}
health_result=0
check_health || health_result=$?
if [[ "$health_result" == 2 ]]; then
    fail "Another service or a different p2p.video build is using port $PORT. Finish any active session, stop the old host yourself, then reopen this app. No running service was stopped."
fi
if [[ "$health_result" != 0 ]]; then
    args=(--no-funnel --port "$PORT" --data-dir "$DATA_DIR")
    if [[ -n "${STUDIOLINK_PUBLIC_URL:-}" ]]; then
        args+=(--public-url "$STUDIOLINK_PUBLIC_URL")
    elif [[ -f "$DATA_DIR/public-url.txt" ]]; then
        args+=(--public-url "$(cat "$DATA_DIR/public-url.txt")")
    fi
    nohup "$DAEMON" "${args[@]}" >> "$DATA_DIR/daemon.log" 2>&1 </dev/null &
    ready=false
    for attempt in {1..50}; do
        health_result=0
        check_health || health_result=$?
        if [[ "$health_result" == 0 ]]; then ready=true; break; fi
        if [[ "$health_result" == 2 ]]; then
            fail "Port $PORT is serving a different product or build. Finish any active session, stop the old host yourself, then reopen this app."
        fi
        sleep 0.1
    done
    [[ "$ready" == true ]] || fail "The expected p2p.video build did not start. See $DATA_DIR/daemon.log for details."
fi
# The dashboard opens signed in: the daemon writes a per-boot launch token to
# a 0600 file only this user can read, and sets an HttpOnly cookie when the
# page is opened with it (docs/SECURITY.md). Never print the URL to the log.
dashboard_url() {
    local token
    token="$(tr -d '[:space:]' < "$DATA_DIR/.launch_token" 2>/dev/null)" || token=""
    if [[ ${#token} -ge 32 ]]; then
        print -r -- "http://127.0.0.1:$PORT/?launch=$token"
    else
        print -r -- "http://127.0.0.1:$PORT/"
    fi
}
if [[ "${STUDIOLINK_NO_OPEN:-0}" != 1 ]]; then open "$(dashboard_url)"; fi
if [[ -n "${STUDIOLINK_PRINT_DASHBOARD_URL_TO:-}" ]]; then dashboard_url > "$STUDIOLINK_PRINT_DASHBOARD_URL_TO"; fi
