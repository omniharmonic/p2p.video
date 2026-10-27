#!/bin/zsh
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="$ROOT_DIR/build"
APP_NAME="p2p.video"
VERSION="1.0.0"
# Bump for every release build (CFBundleVersion must increase for updates).
BUILD_NUMBER="5"
APP_BUNDLE="$BUILD_DIR/$APP_NAME.app"
CONTENTS_DIR="$APP_BUNDLE/Contents"
MACOS_DIR="$CONTENTS_DIR/MacOS"
RESOURCES_DIR="$CONTENTS_DIR/Resources"
BIN_DIR="$RESOURCES_DIR/bin"

MEDIA_TOOLS="${STUDIOLINK_MEDIA_TOOLS:-$BUILD_DIR/media-tools}"
for tool in ffmpeg ffprobe; do
    [[ -x "$MEDIA_TOOLS/bin/$tool" ]] || { echo "Run scripts/build-ffmpeg-mac.sh first (missing $tool)." >&2; exit 1; }
done
[[ -d "$MEDIA_TOOLS/licenses" && -d "$MEDIA_TOOLS/source" ]] || { echo "Media licenses and corresponding source are required." >&2; exit 1; }
for tool in ffmpeg ffprobe; do
    license_text="$("$MEDIA_TOOLS/bin/$tool" -L 2>&1 | tr '\n' ' ')"
    if [[ "$license_text" != *"GNU Lesser General Public License"* || "$license_text" == *"nonfree and unredistributable"* ]]; then
        echo "Refusing $tool without verified LGPL licensing." >&2; exit 1
    fi
    if otool -L "$MEDIA_TOOLS/bin/$tool" | tail -n +2 | awk '{print $1}' | /usr/bin/grep -Ev '^(/usr/lib/|/System/Library/)'; then
        echo "Refusing $tool with dependencies outside macOS." >&2; exit 1
    fi
done

echo "==> Building web assets..."
(cd "$ROOT_DIR/web" && npm run build)

mkdir -p "$ROOT_DIR/bin"
BUILD_ID="$(date -u +%Y%m%dT%H%M%SZ)-$(uuidgen)"
echo "==> Building p2pvideod daemon ($BUILD_ID)..."
(cd "$ROOT_DIR/host" && go build -trimpath -ldflags "-s -w -X github.com/omniharmonic/studiolink/host/internal/buildinfo.ID=$BUILD_ID" -o "$ROOT_DIR/bin/p2pvideod" ./cmd/p2pvideod)

# In-app updates (Sparkle 2) are off unless STUDIOLINK_SPARKLE=1 [SL-P2-2].
# The feed and EdDSA public key come from STUDIOLINK_APPCAST_URL and
# STUDIOLINK_SPARKLE_PUBLIC_KEY; without both, the updater stays disabled at
# runtime. The private signing key is never read or stored by this script.
SPARKLE="${STUDIOLINK_SPARKLE:-0}"
SWIFT_ARGS=(-c release --arch arm64)
if [[ "$SPARKLE" == "1" ]]; then
    # SwiftPM caches the evaluated manifest; a separate scratch path keeps the
    # Sparkle and non-Sparkle builds from reusing each other's dependency graph.
    SWIFT_ARGS+=(--scratch-path "$ROOT_DIR/macapp/.build-sparkle")
    export STUDIOLINK_SPARKLE=1
fi

echo "==> Building native host app (macapp/)..."
(cd "$ROOT_DIR/macapp" && swift build "${SWIFT_ARGS[@]}")
SWIFT_BIN_DIR="$(cd "$ROOT_DIR/macapp" && swift build "${SWIFT_ARGS[@]}" --show-bin-path)"
[[ -x "$SWIFT_BIN_DIR/StudioLink" ]] || { echo "Swift build did not produce StudioLink." >&2; exit 1; }
[[ -x "$SWIFT_BIN_DIR/studiolink-icon" ]] || { echo "Swift build did not produce studiolink-icon." >&2; exit 1; }

# The app icon is drawn in code (macapp/Sources/StudioLinkArtwork) and
# converted here, so every build reproduces it from source.
echo "==> Rendering AppIcon.icns..."
ICONSET="$BUILD_DIR/AppIcon.iconset"
rm -rf "$ICONSET"
"$SWIFT_BIN_DIR/studiolink-icon" "$ICONSET"
iconutil -c icns "$ICONSET" -o "$BUILD_DIR/AppIcon.icns"

echo "==> Constructing $APP_NAME.app bundle at $APP_BUNDLE..."
rm -rf "$APP_BUNDLE"
mkdir -p "$MACOS_DIR" "$BIN_DIR"

cp "$ROOT_DIR/bin/p2pvideod" "$BIN_DIR/p2pvideod"
chmod +x "$BIN_DIR/p2pvideod"
cp "$MEDIA_TOOLS/bin/ffmpeg" "$MEDIA_TOOLS/bin/ffprobe" "$BIN_DIR/"
cp -R "$MEDIA_TOOLS/licenses" "$RESOURCES_DIR/"
python3 "$ROOT_DIR/scripts/collect-go-notices.py" "$RESOURCES_DIR/licenses/Go-modules"
cp -R "$MEDIA_TOOLS/source" "$RESOURCES_DIR/"
cp "$ROOT_DIR/docs/INSTALL_MAC.md" "$RESOURCES_DIR/Getting Started.md"
# The app is AGPL-3.0; its license and the third-party notices ship with it.
cp "$ROOT_DIR/LICENSE" "$RESOURCES_DIR/LICENSE.txt"
cp "$ROOT_DIR/THIRD_PARTY_NOTICES.md" "$RESOURCES_DIR/THIRD_PARTY_NOTICES.md"

# The native host app (SL-P2-1) is the bundle executable. It supervises
# Resources/bin/p2pvideod with the launcher's exact-build safety checks.
# The SwiftPM product keeps its internal name; the bundle executable is the product name.
cp "$SWIFT_BIN_DIR/StudioLink" "$MACOS_DIR/$APP_NAME"
chmod +x "$MACOS_DIR/$APP_NAME"
cp "$BUILD_DIR/AppIcon.icns" "$RESOURCES_DIR/AppIcon.icns"

SPARKLE_PLIST=""
if [[ "$SPARKLE" == "1" ]]; then
    SPARKLE_FRAMEWORK="$ROOT_DIR/macapp/.build-sparkle/artifacts/sparkle/Sparkle/Sparkle.xcframework/macos-arm64_x86_64/Sparkle.framework"
    [[ -d "$SPARKLE_FRAMEWORK" ]] || { echo "Sparkle.framework missing from the SwiftPM artifacts." >&2; exit 1; }
    mkdir -p "$CONTENTS_DIR/Frameworks"
    ditto "$SPARKLE_FRAMEWORK" "$CONTENTS_DIR/Frameworks/Sparkle.framework"
    if [[ -n "${STUDIOLINK_APPCAST_URL:-}" && -n "${STUDIOLINK_SPARKLE_PUBLIC_KEY:-}" ]]; then
        [[ "$STUDIOLINK_APPCAST_URL" == https://* ]] || { echo "STUDIOLINK_APPCAST_URL must be https." >&2; exit 1; }
        SPARKLE_PLIST="    <key>SUFeedURL</key>
    <string>$STUDIOLINK_APPCAST_URL</string>
    <key>SUPublicEDKey</key>
    <string>$STUDIOLINK_SPARKLE_PUBLIC_KEY</string>"
    else
        echo "Note: Sparkle is linked but STUDIOLINK_APPCAST_URL / STUDIOLINK_SPARKLE_PUBLIC_KEY are unset; updates stay off." >&2
    fi
fi
# Only system libraries (and Sparkle, when enabled) may be linked dynamically.
while IFS= read -r dependency; do
    case "$dependency" in
        /System/Library/*|/usr/lib/*) ;;
        @rpath/Sparkle.framework/*) [[ "$SPARKLE" == "1" ]] || { echo "Unexpected Sparkle link without STUDIOLINK_SPARKLE=1." >&2; exit 1; } ;;
        *) echo "Non-system dynamic dependency needs explicit embedding: $dependency" >&2; exit 1 ;;
    esac
done < <(otool -L "$MACOS_DIR/$APP_NAME" | awk 'NR > 1 { print $1 }')
print -r -- "$BUILD_ID" > "$RESOURCES_DIR/build-id.txt"
# Fallback: the previous zsh launcher still works from Resources
# (`zsh p2p.video.app/Contents/Resources/launch-mac.sh`), e.g. for headless hosts.
cp "$ROOT_DIR/scripts/launch-mac.sh" "$RESOURCES_DIR/launch-mac.sh"
chmod +x "$RESOURCES_DIR/launch-mac.sh"

cat <<EOF > "$CONTENTS_DIR/Info.plist"
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>$APP_NAME</string>
    <key>CFBundleIdentifier</key>
    <string>video.p2p.app</string>
    <key>CFBundleName</key>
    <string>$APP_NAME</string>
    <key>CFBundleDisplayName</key>
    <string>$APP_NAME</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleIconFile</key>
    <string>AppIcon</string>
    <key>CFBundleShortVersionString</key>
    <string>$VERSION</string>
    <key>CFBundleVersion</key>
    <string>$BUILD_NUMBER</string>
    <key>NSHumanReadableCopyright</key>
    <string>p2p.video is free software under the GNU AGPL v3.</string>
    <key>LSMinimumSystemVersion</key>
    <string>14.0</string>
    <key>LSApplicationCategoryType</key>
    <string>public.app-category.video</string>
    <key>NSHighResolutionCapable</key>
    <true/>
    <key>NSMicrophoneUsageDescription</key>
    <string>p2p.video uses your microphone when you host a meeting in a p2p.video window, to send your voice to guests and record pristine multi-track audio.</string>
    <key>NSSpeechRecognitionUsageDescription</key>
    <string>p2p.video transcribes your recordings and live captions with on-device speech recognition. Audio never leaves this Mac.</string>
    <key>NSCameraUsageDescription</key>
    <string>p2p.video uses your camera when you host a meeting in a p2p.video window, to send your video to guests and record high-definition isolated video.</string>
    <key>NSDownloadsFolderUsageDescription</key>
    <string>p2p.video saves the recordings, invites and transcripts you download from the studio window to your Downloads folder.</string>
    <key>NSCalendarsFullAccessUsageDescription</key>
    <string>p2p.video looks in your calendars for meetings with one of your room links, so it can remind you and start them, and (only if you turn it on) adds meetings you schedule to the calendar you choose. Nothing leaves this Mac.</string>
    <key>NSCalendarsUsageDescription</key>
    <string>p2p.video looks in your calendars for meetings with one of your room links, so it can remind you and start them. Nothing leaves this Mac.</string>
    <key>NSAppTransportSecurity</key>
    <dict>
        <key>NSAllowsLocalNetworking</key>
        <true/>
    </dict>
$SPARKLE_PLIST
</dict>
</plist>
EOF

ENTITLEMENTS="$ROOT_DIR/scripts/p2p.video.entitlements"
SIGNING_IDENTITY="${DEVELOPER_ID_APPLICATION:--}"

echo "==> Signing bundled executables..."
for tool in ffmpeg ffprobe; do
    if [[ "$SIGNING_IDENTITY" == "-" ]]; then
        codesign --force --sign - "$BIN_DIR/$tool"
    else
        codesign --force --options runtime --timestamp --sign "$SIGNING_IDENTITY" "$BIN_DIR/$tool"
    fi
done
if [[ "$SPARKLE" == "1" ]]; then
    # Sign Sparkle's helpers inside out, keeping the Downloader's entitlements (as Minutes does).
    if [[ "$SIGNING_IDENTITY" == "-" ]]; then SPARKLE_SIGN=(--force --sign -); else SPARKLE_SIGN=(--force --options runtime --timestamp --sign "$SIGNING_IDENTITY"); fi
    SPARKLE_EMBEDDED="$CONTENTS_DIR/Frameworks/Sparkle.framework"
    codesign "${SPARKLE_SIGN[@]}" "$SPARKLE_EMBEDDED/Versions/B/XPCServices/Installer.xpc"
    codesign "${SPARKLE_SIGN[@]}" --preserve-metadata=entitlements "$SPARKLE_EMBEDDED/Versions/B/XPCServices/Downloader.xpc"
    codesign "${SPARKLE_SIGN[@]}" "$SPARKLE_EMBEDDED/Versions/B/Autoupdate"
    codesign "${SPARKLE_SIGN[@]}" "$SPARKLE_EMBEDDED/Versions/B/Updater.app"
    codesign "${SPARKLE_SIGN[@]}" "$SPARKLE_EMBEDDED"
fi
# --deep would re-sign Sparkle's helpers with the app's entitlements; with
# Sparkle every nested item is already signed above, so it is left out.
DEEP=(--deep)
if [[ "$SPARKLE" == "1" ]]; then DEEP=(); fi
if [[ "$SIGNING_IDENTITY" == "-" ]]; then
    codesign --force --sign - --entitlements "$ENTITLEMENTS" "$BIN_DIR/p2pvideod"
    codesign --force "${DEEP[@]}" --sign - --entitlements "$ENTITLEMENTS" "$APP_BUNDLE"
else
    codesign --force --options runtime --timestamp --sign "$SIGNING_IDENTITY" --entitlements "$ENTITLEMENTS" "$BIN_DIR/p2pvideod"
    codesign --force "${DEEP[@]}" --options runtime --timestamp --sign "$SIGNING_IDENTITY" --entitlements "$ENTITLEMENTS" "$APP_BUNDLE"
fi

echo "==> Verifying code signature..."
codesign --verify --deep --strict --verbose=2 "$APP_BUNDLE"

if [[ -n "${NOTARYTOOL_PROFILE:-}" ]]; then
    if [[ "$SIGNING_IDENTITY" == "-" ]]; then
        echo "Notarization requires DEVELOPER_ID_APPLICATION, not an ad-hoc identity." >&2
        exit 1
    fi
    ditto -c -k --keepParent "$APP_BUNDLE" "$BUILD_DIR/$APP_NAME-notary.zip"
    xcrun notarytool submit "$BUILD_DIR/$APP_NAME-notary.zip" --keychain-profile "$NOTARYTOOL_PROFILE" --wait
    xcrun stapler staple "$APP_BUNDLE"
    xcrun stapler validate "$APP_BUNDLE"
fi
if [[ "$SIGNING_IDENTITY" == "-" ]]; then
    echo "Development build only: ad-hoc signature, not notarized for distribution."
elif [[ -z "${NOTARYTOOL_PROFILE:-}" ]]; then
    echo "Developer ID signed for local testing; notarization is still required for distribution."
fi
ZIP_NAME="$APP_NAME-$VERSION-$(uname -m).zip"
echo "==> Packaging application zip..."
ditto -c -k --keepParent "$APP_BUNDLE" "$BUILD_DIR/$ZIP_NAME"

echo "==> $APP_NAME application bundle successfully packaged at:"
echo "    App: $APP_BUNDLE"
echo "    Zip: $BUILD_DIR/$ZIP_NAME"

# A drag-to-Applications disk image is the primary end-user download.
DMG_STAGE="$(mktemp -d "$BUILD_DIR/dmg-stage.XXXXXX")"
trap 'rm -rf "$DMG_STAGE"' EXIT
ditto "$APP_BUNDLE" "$DMG_STAGE/$APP_NAME.app"
ln -s /Applications "$DMG_STAGE/Applications"
cp "$ROOT_DIR/docs/INSTALL_MAC.md" "$DMG_STAGE/Getting Started.txt"
DMG="$BUILD_DIR/$APP_NAME-$VERSION-$(uname -m).dmg"
rm -f "$DMG"
hdiutil create -volname "$APP_NAME" -srcfolder "$DMG_STAGE" -format UDZO -ov "$DMG"
if [[ "$SIGNING_IDENTITY" != "-" ]]; then
    codesign --force --timestamp --sign "$SIGNING_IDENTITY" "$DMG"
fi
if [[ -n "${NOTARYTOOL_PROFILE:-}" ]]; then
    xcrun notarytool submit "$DMG" --keychain-profile "$NOTARYTOOL_PROFILE" --wait
    xcrun stapler staple "$DMG"
    xcrun stapler validate "$DMG"
    spctl --assess --type open --context context:primary-signature --verbose=2 "$DMG"
fi
# The website's download button links to releases/latest/download/<this name>, so every
# release must also attach the disk image under a name without the version.
LATEST_DMG="$BUILD_DIR/$APP_NAME-$(uname -m).dmg"
cp -f "$DMG" "$LATEST_DMG"
(cd "$BUILD_DIR" && shasum -a 256 "$(basename "$DMG")" "$ZIP_NAME" "$(basename "$LATEST_DMG")" > SHA256SUMS.txt)
echo "Download: $DMG"
echo "Attach to the release as well: $LATEST_DMG"
