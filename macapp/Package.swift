// swift-tools-version: 6.0
// p2p.video native host app (SL-P2-1). `swift build -c release` produces the
// `StudioLink` executable (internal target name, kept stable); scripts/package-mac.sh
// assembles it into p2p.video.app as Contents/MacOS/p2p.video.
import Foundation
import PackageDescription

// The captions CLI embeds an Info.plist so macOS can attribute Speech
// Recognition permission to it (needed only by the pre-macOS 26 fallback).
let captionsCLIInfoPlist = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
    .appendingPathComponent("Support/captions-cli-Info.plist").path

// In-app updates (Sparkle 2) are a build setting [SL-P2-2]: only
// `STUDIOLINK_SPARKLE=1` adds the dependency (use a separate --scratch-path;
// SwiftPM caches manifests). Without it nothing is fetched or linked, and the
// app shows "built without the updater". See docs/DECISIONS.md.
let sparkle = ProcessInfo.processInfo.environment["STUDIOLINK_SPARKLE"] == "1"

var appDependencies: [Target.Dependency] = ["StudioLinkCore", "StudioLinkSpeech", "StudioLinkArtwork"]
var appLinker: [LinkerSetting] = []
var packageDependencies: [Package.Dependency] = []
if sparkle {
    packageDependencies.append(.package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.10.0"))
    appDependencies.append(.product(name: "Sparkle", package: "Sparkle"))
    // Sparkle.framework is embedded in Contents/Frameworks by scripts/package-mac.sh.
    appLinker.append(.unsafeFlags(["-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks"]))
}

let package = Package(
    name: "StudioLinkMac",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "StudioLink", targets: ["StudioLink"]),
        .executable(name: "studiolink-captions", targets: ["studiolink-captions"]),
        .executable(name: "studiolink-icon", targets: ["studiolink-icon"]),
        .library(name: "StudioLinkCore", targets: ["StudioLinkCore"]),
    ],
    dependencies: packageDependencies,
    targets: [
        // Pure logic: argv builders, parsers, policies. No UI, fully unit-tested.
        .target(name: "StudioLinkCore"),
        // Apple on-device speech behind the caption provider protocol [SL-P4-2].
        .target(name: "StudioLinkSpeech", dependencies: ["StudioLinkCore"]),
        // The app icon, drawn with CoreGraphics [SL-P2-2].
        .target(name: "StudioLinkArtwork"),
        // SwiftUI/AppKit shell: menu bar, control center, settings, supervisor.
        .executableTarget(name: "StudioLink", dependencies: appDependencies, linkerSettings: appLinker),
        // Caption provider harness: transcribe a file, or run headless as the provider.
        .executableTarget(
            name: "studiolink-captions",
            dependencies: ["StudioLinkCore", "StudioLinkSpeech"],
            linkerSettings: [.unsafeFlags(["-Xlinker", "-sectcreate", "-Xlinker", "__TEXT", "-Xlinker", "__info_plist", "-Xlinker", captionsCLIInfoPlist])]
        ),
        // Writes AppIcon.iconset for scripts/package-mac.sh.
        .executableTarget(name: "studiolink-icon", dependencies: ["StudioLinkArtwork"]),
        .testTarget(
            name: "StudioLinkCoreTests",
            dependencies: ["StudioLinkCore", "StudioLinkArtwork"],
            resources: [.copy("Fixtures")]
        ),
        // Real Apple on-device recognition of `say`-generated speech [SL-P4-3].
        // Opt-in (STUDIOLINK_REAL_SPEECH=1): it needs the on-device model.
        .testTarget(name: "StudioLinkSpeechTests", dependencies: ["StudioLinkCore", "StudioLinkSpeech"]),
    ]
)
