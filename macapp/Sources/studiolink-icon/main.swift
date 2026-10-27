import Foundation
import StudioLinkArtwork

// Writes p2p.video's AppIcon.iconset (scripts/package-mac.sh turns it into
// AppIcon.icns with `iconutil`). Usage: studiolink-icon <out.iconset>
let args = CommandLine.arguments
guard args.count == 2 else {
    FileHandle.standardError.write(Data("usage: studiolink-icon <out.iconset>\n".utf8))
    exit(2)
}
do {
    try AppIconArtwork.writeIconset(to: URL(fileURLWithPath: args[1], isDirectory: true))
} catch {
    FileHandle.standardError.write(Data("studiolink-icon: \(error)\n".utf8))
    exit(1)
}
