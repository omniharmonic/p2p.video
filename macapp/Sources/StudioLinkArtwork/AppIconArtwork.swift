import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers

/// The p2p.video app icon, drawn in code so every size is reproducible from
/// source [SL-P2-2]. Two linked rings (host and guest) on a graphite tile;
/// where they overlap, the red recording lens. Colours are the web design
/// tokens (`web/src/tokens.css`): graphite surfaces, ice accent, record red.
public enum AppIconArtwork {
    static func hex(_ v: UInt32, _ a: CGFloat = 1) -> CGColor {
        CGColor(srgbRed: CGFloat((v >> 16) & 0xff) / 255, green: CGFloat((v >> 8) & 0xff) / 255, blue: CGFloat(v & 0xff) / 255, alpha: a)
    }

    // Tokens: --sl-surface-3, --sl-canvas, --sl-accent, --sl-accent-strong, --sl-rec.
    static let tileTop = hex(0x2a3641)
    static let tileBottom = hex(0x0b0f13)
    static let accent = hex(0xbfdbe8)
    static let accentStrong = hex(0xdcedf5)
    static let rec = hex(0xe5524e)
    static let recDeep = hex(0xb8403c)

    /// Every PNG an `.iconset` needs: (file name, pixel size).
    public static let iconsetEntries: [(String, Int)] = [
        ("icon_16x16.png", 16), ("icon_16x16@2x.png", 32),
        ("icon_32x32.png", 32), ("icon_32x32@2x.png", 64),
        ("icon_128x128.png", 128), ("icon_128x128@2x.png", 256),
        ("icon_256x256.png", 256), ("icon_256x256@2x.png", 512),
        ("icon_512x512.png", 512), ("icon_512x512@2x.png", 1024),
    ]

    /// Renders the icon at `pixels`×`pixels` with a transparent margin
    /// (Apple's macOS icon grid: an 824-unit tile in a 1024 canvas).
    public static func image(pixels: Int) -> CGImage? {
        let space = CGColorSpace(name: CGColorSpace.sRGB)!
        guard let ctx = CGContext(data: nil, width: pixels, height: pixels, bitsPerComponent: 8, bytesPerRow: 0,
                                  space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
        ctx.interpolationQuality = .high
        ctx.setShouldAntialias(true)
        let scale = CGFloat(pixels) / 1024
        ctx.scaleBy(x: scale, y: scale)
        draw(in: ctx, small: pixels <= 32)
        return ctx.makeImage()
    }

    /// Draws in a 1024×1024 coordinate space (origin bottom-left).
    public static func draw(in ctx: CGContext, small: Bool = false) {
        let tile = CGRect(x: 100, y: 100, width: 824, height: 824)
        let tilePath = CGPath(roundedRect: tile, cornerWidth: 185, cornerHeight: 185, transform: nil)

        // Soft drop shadow under the tile.
        ctx.saveGState()
        // Shadow offset and blur are in device pixels (the CTM doesn't scale
        // them), so convert from the 1024 design grid; this keeps the shadow
        // inside the transparent margin at every size.
        let device = ctx.ctm.a
        ctx.setShadow(offset: CGSize(width: 0, height: -10 * device), blur: 24 * device, color: hex(0x000000, 0.35))
        ctx.addPath(tilePath)
        ctx.setFillColor(tileBottom)
        ctx.fillPath()
        ctx.restoreGState()

        // Graphite gradient tile.
        ctx.saveGState()
        ctx.addPath(tilePath)
        ctx.clip()
        let grad = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB), colors: [tileTop, tileBottom] as CFArray, locations: [0, 1])!
        ctx.drawLinearGradient(grad, start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 100), options: [])
        // A faint top sheen.
        let sheen = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB),
                               colors: [hex(0xffffff, 0.07), hex(0xffffff, 0)] as CFArray, locations: [0, 1])!
        ctx.drawLinearGradient(sheen, start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 600), options: [])
        ctx.restoreGState()

        // Hairline edge.
        ctx.saveGState()
        ctx.addPath(CGPath(roundedRect: tile.insetBy(dx: 3, dy: 3), cornerWidth: 182, cornerHeight: 182, transform: nil))
        ctx.setStrokeColor(hex(0xffffff, 0.10))
        ctx.setLineWidth(6)
        ctx.strokePath()
        ctx.restoreGState()

        // Two linked rings.
        let radius: CGFloat = 190
        let offset: CGFloat = 112
        let stroke: CGFloat = small ? 76 : 60
        let left = CGPoint(x: 512 - offset, y: 512)
        let right = CGPoint(x: 512 + offset, y: 512)
        func circle(_ c: CGPoint, _ r: CGFloat) -> CGPath {
            CGPath(ellipseIn: CGRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r), transform: nil)
        }

        // The recording lens: where the two rings' discs overlap.
        ctx.saveGState()
        ctx.addPath(circle(left, radius - stroke / 2))
        ctx.clip()
        ctx.addPath(circle(right, radius - stroke / 2))
        ctx.clip()
        let lens = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB), colors: [rec, recDeep] as CFArray, locations: [0, 1])!
        ctx.drawLinearGradient(lens, start: CGPoint(x: 512, y: 512 + radius), end: CGPoint(x: 512, y: 512 - radius), options: [])
        ctx.restoreGState()

        ctx.saveGState()
        ctx.setLineWidth(stroke)
        ctx.setStrokeColor(accent)
        ctx.addPath(circle(left, radius))
        ctx.strokePath()
        ctx.setStrokeColor(accentStrong)
        ctx.addPath(circle(right, radius))
        ctx.strokePath()
        ctx.restoreGState()

        // Interlock: the left ring passes over the right ring at the top
        // crossing (the right ring is on top at the bottom one), with a thin
        // graphite gap so the weave reads at small sizes.
        let gap: CGFloat = small ? 20 : 24
        func weave(over: CGPoint, overColor: CGColor, under: CGPoint, half: CGRect, gapColor: CGColor) {
            ctx.saveGState()
            ctx.clip(to: half)
            // Only inside the under ring's band, so the gap never shows elsewhere.
            ctx.addPath(circle(under, radius))
            ctx.setLineWidth(stroke)
            ctx.replacePathWithStrokedPath()
            ctx.clip()
            ctx.setLineWidth(stroke + gap)
            ctx.setStrokeColor(gapColor)
            ctx.addPath(circle(over, radius))
            ctx.strokePath()
            ctx.setLineWidth(stroke)
            ctx.setStrokeColor(overColor)
            ctx.addPath(circle(over, radius))
            ctx.strokePath()
            ctx.restoreGState()
        }
        weave(over: left, overColor: accent, under: right, half: CGRect(x: 0, y: 512, width: 1024, height: 512), gapColor: hex(0x1d2630))
        weave(over: right, overColor: accentStrong, under: left, half: CGRect(x: 0, y: 0, width: 1024, height: 512), gapColor: hex(0x111820))
    }

    public enum WriteError: Error { case render(Int), write(URL) }

    /// Writes a complete `.iconset` directory (for `iconutil -c icns`).
    public static func writeIconset(to directory: URL) throws {
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        for (name, px) in iconsetEntries {
            guard let img = image(pixels: px) else { throw WriteError.render(px) }
            let url = directory.appendingPathComponent(name)
            guard let dest = CGImageDestinationCreateWithURL(url as CFURL, UTType.png.identifier as CFString, 1, nil) else { throw WriteError.write(url) }
            CGImageDestinationAddImage(dest, img, [kCGImagePropertyDPIWidth: 72 * px / max(1, sizeFromName(name)),
                                                   kCGImagePropertyDPIHeight: 72 * px / max(1, sizeFromName(name))] as CFDictionary)
            guard CGImageDestinationFinalize(dest) else { throw WriteError.write(url) }
        }
    }

    /// Point size encoded in an iconset file name (`icon_32x32@2x.png` → 32).
    static func sizeFromName(_ name: String) -> Int {
        let core = name.dropFirst("icon_".count).prefix { $0 != "x" }
        return Int(core) ?? 1
    }
}
