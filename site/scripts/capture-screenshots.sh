#!/bin/sh
# Regenerate the product screenshots on the p2p.video site [SL-REL-2].
#
# 1. Builds the daemon (unless SKIP_BUILD=1).
# 2. Renders illustrated stand-in camera feeds (feeds/portraits.html) and turns
#    them into Y4M clips, so the real app UI is captured around calm pictures
#    instead of Chromium's green test pattern.
# 3. Runs scenes.mjs: a throwaway daemon on a free port with a temporary data
#    dir, three Chromium browsers with fake devices, three scenes in light and
#    dark at 2x (call while recording, people with someone waiting, phone).
# 4. Converts them to WebP (plus small PNG fallbacks) in site/img/app/.
#
# With FULL_SUITES=1 it also runs the app's own UX screenshot suites
# (web/tests/ux-screens.browser.mjs, web/tests/ux-call-screens.browser.mjs)
# into the raw dir with the same stand-in feeds, for picking other shots.
#
# Never touches port 7443 or the owner's data.
#
# Usage:  site/scripts/capture-screenshots.sh [raw-output-dir]
# Env:    SKIP_CAPTURE=1 (convert only), PLAYWRIGHT_MODULE (default web/node_modules/playwright/index.mjs),
#         CHROMIUM_PATH, SKIP_BUILD=1, FULL_SUITES=1
# Needs:  go, node, ffmpeg, cwebp (brew install webp), sips (macOS).
set -eu

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
RAW=${1:-"$ROOT/build/site-shots"}
DEST="$ROOT/site/img/app"
mkdir -p "$RAW" "$DEST"
RAW=$(cd "$RAW" && pwd)

if [ "${SKIP_BUILD:-0}" != 1 ]; then
  # The web dist is committed (host/internal/webassets/dist); `make build`
  # also rebuilds it when web/node_modules is installed.
  if [ -d "$ROOT/web/node_modules" ]; then
    make -C "$ROOT" build
  else
    mkdir -p "$ROOT/bin"
    for cmd in "$ROOT"/host/cmd/*d; do
      (cd "$ROOT/host" && go build -o "$ROOT/bin/$(basename "$cmd")" "./cmd/$(basename "$cmd")")
    done
  fi
fi
DAEMON=$(ls "$ROOT"/bin/*d | head -n 1)

PLAYWRIGHT_MODULE=${PLAYWRIGHT_MODULE:-"$ROOT/web/node_modules/playwright/index.mjs"}
STUDIO_BINARY=$DAEMON
export PLAYWRIGHT_MODULE STUDIO_BINARY

FEEDS="$RAW/.feeds"
if [ "${SKIP_CAPTURE:-0}" != 1 ]; then
rm -rf "$FEEDS"; mkdir -p "$FEEDS"
node "$ROOT/site/scripts/feeds/render.mjs" "$FEEDS"
for i in 0 1 2; do
  ffmpeg -loglevel error -y -loop 1 -i "$FEEDS/p$i.png" -t 2 -r 15 -pix_fmt yuv420p "$FEEDS/p$i.y4m"
done

cd "$ROOT/web"
node "$ROOT/site/scripts/scenes.mjs" "$FEEDS" "$RAW"

if [ "${FULL_SUITES:-0}" = 1 ]; then
  REAL_CHROMIUM=${CHROMIUM_PATH:-$(node -e "import(process.env.PLAYWRIGHT_MODULE).then(p=>console.log(p.chromium.executablePath()))")}
  # Each new browser gets the next feed.
  cat > "$FEEDS/chromium" <<WRAP
#!/bin/sh
n=\$(cat "$FEEDS/counter" 2>/dev/null || echo 0)
echo \$((n + 1)) > "$FEEDS/counter"
exec "$REAL_CHROMIUM" --use-file-for-fake-video-capture="$FEEDS/p\$((n % 3)).y4m" "\$@"
WRAP
  chmod +x "$FEEDS/chromium"
  STUDIO_SCREENSHOT_DIR="$RAW/suites"
  export STUDIO_SCREENSHOT_DIR
  CHROMIUM_PATH="$FEEDS/chromium" node tests/ux-screens.browser.mjs || echo "warning: ux-screens failed; keeping the shots it wrote" >&2
  echo 0 > "$FEEDS/counter"
  # Fixed non-7443 port; its last steps (uploads/export) can time out.
  CHROMIUM_PATH="$FEEDS/chromium" STUDIO_TEST_PORT=${STUDIO_TEST_PORT:-18977} node tests/ux-call-screens.browser.mjs || echo "warning: ux-call-screens failed; keeping the shots it wrote" >&2
fi
fi # SKIP_CAPTURE

convert_one() { # $1 raw png, $2 output basename, $3 width
  tmp="$RAW/.site-$2.png"
  cp "$RAW/$1" "$tmp"
  sips --resampleWidth "$3" "$tmp" >/dev/null
  cwebp -quiet -q 78 -m 6 -sharp_yuv "$tmp" -o "$DEST/$2.webp"
  # JPEG fallback for browsers without WebP: smaller, rarely fetched.
  sips --resampleWidth $(( $3 * 3 / 4 )) -s format jpeg -s formatOptions 70 "$tmp" --out "$DEST/$2.jpg" >/dev/null
  rm -f "$tmp"
}
for s in light dark; do
  convert_one "call-$s.png" "call-$s" 1600
  convert_one "people-$s.png" "people-$s" 1600
  convert_one "phone-$s.png" "phone-$s" 600
done
# The studio band is always dark.
convert_one "studio-dark.png" "studio-dark" 1600

echo "Screenshots written to $DEST"
du -ch "$DEST"/* | tail -n 1
