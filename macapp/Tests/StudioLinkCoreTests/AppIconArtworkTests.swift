import CoreGraphics
import Foundation
import StudioLinkArtwork
import XCTest

final class AppIconArtworkTests: XCTestCase {
    func testRendersEveryIconsetSizeWithTransparentMarginAndRecordingLens() throws {
        XCTAssertEqual(AppIconArtwork.iconsetEntries.count, 10)
        for (_, px) in AppIconArtwork.iconsetEntries {
            let img = try XCTUnwrap(AppIconArtwork.image(pixels: px))
            XCTAssertEqual(img.width, px)
            XCTAssertEqual(img.height, px)
        }
        let img = try XCTUnwrap(AppIconArtwork.image(pixels: 256))
        let data = try XCTUnwrap(img.dataProvider?.data as Data?)
        let bpr = img.bytesPerRow
        func rgba(_ x: Int, _ y: Int) -> [UInt8] { let o = y * bpr + x * 4; return Array(data[o..<o + 4]) }
        XCTAssertEqual(rgba(2, 2)[3], 0, "corner outside the tile is transparent")
        let center = rgba(128, 128)
        XCTAssertGreaterThan(center[0], 180, "the lens is record red")
        XCTAssertLessThan(center[1], 110)
        XCTAssertEqual(center[3], 255)
    }

    func testIconsetDirectoryHasAllFiles() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".iconset")
        defer { try? FileManager.default.removeItem(at: dir) }
        try AppIconArtwork.writeIconset(to: dir)
        let names = try FileManager.default.contentsOfDirectory(atPath: dir.path).sorted()
        XCTAssertEqual(names, AppIconArtwork.iconsetEntries.map(\.0).sorted())
    }
}
