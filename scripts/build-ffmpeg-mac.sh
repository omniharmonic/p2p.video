#!/bin/bash
# Build the source-distributed, LGPL-only media helpers used by the Mac package.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/build/media-tools"
VERSION=9.0.2
SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
FINGERPRINT=FCF986EA15E6E293A5644F10B4322F04D67658D8
[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || { echo 'Requires an Apple Silicon Mac.' >&2; exit 1; }
mkdir -p "$OUT"/{bin,licenses,source,evidence}
ARCHIVE="$OUT/source/ffmpeg-$VERSION.tar.xz"
fetch() { [[ -f "$2" ]] || curl --fail --location --retry 3 "$1" -o "$2"; }
fetch "https://ffmpeg.org/releases/ffmpeg-$VERSION.tar.xz" "$ARCHIVE"
fetch "https://ffmpeg.org/releases/ffmpeg-$VERSION.tar.xz.asc" "$ARCHIVE.asc"
fetch https://ffmpeg.org/ffmpeg-devel.asc "$OUT/source/ffmpeg-devel.asc"
echo "$SHA256  $ARCHIVE" | shasum -a 256 -c -
# Signature verification is isolated from the user's keyring; pin the official key.
command -v gpg >/dev/null || { echo 'GnuPG is required to authenticate source.' >&2; exit 1; }
KEYRING="$(mktemp -d "$OUT/.gpg.XXXXXX")"
trap 'rm -rf "$KEYRING"' EXIT
chmod 700 "$KEYRING"
gpg --homedir "$KEYRING" --batch --no-autostart --import "$OUT/source/ffmpeg-devel.asc"
gpg --homedir "$KEYRING" --batch --no-autostart --status-fd 1 --verify "$ARCHIVE.asc" "$ARCHIVE" > "$OUT/evidence/signature.txt" 2>&1
grep -q "VALIDSIG $FINGERPRINT " "$OUT/evidence/signature.txt"
[[ -d "$OUT/ffmpeg-$VERSION" ]] || tar -xf "$ARCHIVE" -C "$OUT"
SRC="$OUT/ffmpeg-$VERSION"
mkdir -p "$OUT/compile"
cd "$OUT/compile"
export MACOSX_DEPLOYMENT_TARGET=14.0
CONFIG=("$SRC/configure" --prefix="$OUT" --arch=arm64 --target-os=darwin
  --cc=clang --enable-static --disable-shared --disable-gpl --disable-nonfree
  --disable-version3 --disable-autodetect --enable-videotoolbox --enable-audiotoolbox
  # Livestream (ADR-014) needs rtmp/rtmps output: network + TLS via Apple's
  # Secure Transport framework and the system zlib. No GPL/nonfree/version3.
  --enable-securetransport --enable-zlib
  --disable-doc --disable-debug --disable-avdevice
  --extra-cflags=-mmacosx-version-min=14.0 --extra-ldflags=-mmacosx-version-min=14.0)
printf '%q ' "${CONFIG[@]}" > "$OUT/source/configure-command.txt"
printf '\n' >> "$OUT/source/configure-command.txt"
if [[ "${VERIFY_ONLY:-0}" != 1 ]]; then
  "${CONFIG[@]}" > "$OUT/evidence/configure.log" 2>&1
  make -j "${JOBS:-8}" > "$OUT/evidence/build.log" 2>&1
fi
install -m 755 ffmpeg ffprobe "$OUT/bin/"
cp "$SRC"/COPYING.* "$SRC/LICENSE.md" "$OUT/licenses/"
cp ffbuild/config.log "$OUT/evidence/config.log"
cp config.h "$OUT/evidence/config.h"
cp config_components.h "$OUT/evidence/config_components.h"
cp "$ROOT/scripts/build-ffmpeg-mac.sh" "$OUT/source/"
cat > "$OUT/licenses/NOTICE.txt" <<EOF
p2p.video includes FFmpeg $VERSION, built from unmodified official source under
the GNU Lesser General Public License version 2.1 or later (LGPL-2.1-or-later).
Copyright (c) the FFmpeg developers. https://ffmpeg.org/
The exact source archive, its signature, signing key, and build script accompany
these separate executables. See COPYING.LGPLv2.1 and LICENSE.md. You may replace
the executables with compatible modified builds. FFmpeg's own libraries are
linked statically into these tools; p2p.video invokes them as separate programs.
This software is based in part on the work of the Independent JPEG Group.
No changes were made to the FFmpeg sources, including its libjpeg-derived files.
Apple's system libraries/frameworks are provided by macOS, not redistributed.
Source: https://ffmpeg.org/releases/ffmpeg-$VERSION.tar.xz
Source SHA-256: $SHA256
EOF
for tool in ffmpeg ffprobe; do
  "$OUT/bin/$tool" -version > "$OUT/evidence/$tool-version.txt"
  "$OUT/bin/$tool" -L > "$OUT/evidence/$tool-license.txt" 2>&1
  grep -q 'GNU Lesser General Public' "$OUT/evidence/$tool-license.txt"
  otool -L "$OUT/bin/$tool" > "$OUT/evidence/$tool-dependencies.txt"
  # Only macOS system dylibs/frameworks may be dynamically linked.
  if tail -n +2 "$OUT/evidence/$tool-dependencies.txt" | awk '{print $1}' | grep -Ev '^(/usr/lib/|/System/Library/)'; then
    echo "Unexpected external dependency in $tool" >&2; exit 1
  fi
done
# Exercise the production filters/codecs with generated signals only. These
# commands never open an audio output or a camera/microphone capture device.
TEST="$OUT/evidence/generated-media"
mkdir -p "$TEST"
FF="$OUT/bin/ffmpeg"
"$FF" -v error -y -filter_complex \
  'testsrc2=size=320x240:rate=30:duration=2[v];sine=frequency=440:sample_rate=48000:duration=2[a]' \
  -map '[v]' -map '[a]' \
  -c:v h264_videotoolbox -b:v 1M -pix_fmt yuv420p -c:a aac -shortest "$TEST/input.mp4"
"$FF" -v error -y -i "$TEST/input.mp4" \
  -vf 'setpts=(PTS-STARTPTS)*1.001,fps=30' -fps_mode cfr \
  -c:v h264_videotoolbox -b:v 15M -pix_fmt yuv420p -timecode 00:00:00:00 -an "$TEST/video.mov"
"$FF" -v error -y -i "$TEST/input.mp4" -vn \
  -af 'aresample=48000,atempo=0.999000999001' -c:a pcm_s24le "$TEST/audio.wav"
"$FF" -v error -i "$TEST/video.mov" -f null -
"$FF" -v error -i "$TEST/audio.wav" -f null -
"$FF" -v error -y -i "$TEST/audio.wav" -vn -ac 1 -ar 48000 -t 1 -f f32le "$TEST/samples.f32"
[[ "$(wc -c < "$TEST/samples.f32" | tr -d ' ')" == 192000 ]]
# Livestream path (host/internal/ffmpeg BuildLivestreamArgs): RTMP(S) output
# protocols present; VideoToolbox H.264 + AAC through the tee muxer to FLV.
"$FF" -hide_banner -protocols > "$OUT/evidence/ffmpeg-protocols.txt"
for proto in rtmp rtmps tls tcp; do
  sed -n '/^Output:/,$p' "$OUT/evidence/ffmpeg-protocols.txt" | grep -qx "  $proto" || { echo "Missing $proto output protocol" >&2; exit 1; }
done
"$FF" -v error -y -i "$TEST/input.mp4" -map 0:v:0 -map 0:a:0 -fps_mode cfr -r 30 \
  -c:v h264_videotoolbox -realtime 1 -allow_sw 1 -b:v 3000k -maxrate 3000k -bufsize 6000k -g 60 \
  -c:a aac -b:a 160k -ar 48000 -ac 2 -flags +global_header -f tee "[f=flv:onfail=ignore]$TEST/live.flv"
"$FF" -v error -i "$TEST/live.flv" -f null -
for media in input.mp4 video.mov audio.wav live.flv; do
  "$OUT/bin/ffprobe" -v error -show_streams -show_format -of json "$TEST/$media" > "$TEST/$media.json"
done
shasum -a 256 "$OUT/bin/ffmpeg" "$OUT/bin/ffprobe" "$ARCHIVE" > "$OUT/evidence/SHA256SUMS"
echo "Built LGPL FFmpeg $VERSION in $OUT"
