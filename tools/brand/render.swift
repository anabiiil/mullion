// render.swift rasterizes Mullion's brand SVGs (assets/brand) into the PNGs
// the build embeds. Run it from the repo root via tools/brand/build.sh — it
// needs nothing beyond the macOS command line tools (NSImage reads SVG on
// macOS 13+).
//
//   swift tools/brand/render.swift <repo-root> <ico-png-out-dir>
//
// Writes:
//   internal/ui/favicon.png            256x256 mark (panel favicon, Windows app-window icon)
//   assets/brand/app-icon-1024.png     macOS app icon source (midnight tile + coral mark)
//   <ico-png-out-dir>/icon-<N>.png     16..256 frames for mullion.ico
import AppKit

let args = CommandLine.arguments
guard args.count == 3 else {
    FileHandle.standardError.write("usage: render.swift <repo-root> <ico-png-out-dir>\n".data(using: .utf8)!)
    exit(2)
}
let root = URL(fileURLWithPath: args[1])
let icoDir = URL(fileURLWithPath: args[2])
try? FileManager.default.createDirectory(at: icoDir, withIntermediateDirectories: true)

func loadSVG(_ rel: String) -> NSImage {
    guard let img = NSImage(contentsOf: root.appendingPathComponent(rel)) else {
        FileHandle.standardError.write("cannot load \(rel)\n".data(using: .utf8)!)
        exit(1)
    }
    return img
}

func canvas(_ px: Int, _ draw: (CGContext) -> Void) -> NSBitmapImageRep {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px,
                               bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    rep.size = NSSize(width: px, height: px) // 1 point == 1 pixel
    let ctx = NSGraphicsContext(bitmapImageRep: rep)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = ctx
    ctx.imageInterpolation = .high
    ctx.cgContext.clear(CGRect(x: 0, y: 0, width: px, height: px))
    draw(ctx.cgContext)
    NSGraphicsContext.restoreGraphicsState()
    return rep
}

func write(_ rep: NSBitmapImageRep, _ url: URL) {
    let data = rep.representation(using: .png, properties: [:])!
    try! data.write(to: url)
    print("wrote \(url.path) (\(rep.pixelsWide)x\(rep.pixelsHigh))")
}

func color(_ hex: UInt32, _ a: CGFloat = 1) -> CGColor {
    CGColor(srgbRed: CGFloat((hex >> 16) & 0xff) / 255, green: CGFloat((hex >> 8) & 0xff) / 255,
            blue: CGFloat(hex & 0xff) / 255, alpha: a)
}

let mark = loadSVG("assets/brand/mullion-mark.svg")
let markSmall = loadSVG("assets/brand/mullion-mark-small.svg")

// Favicon: the bare mark, full bleed.
write(canvas(256) { _ in mark.draw(in: NSRect(x: 0, y: 0, width: 256, height: 256)) },
      root.appendingPathComponent("internal/ui/favicon.png"))

// ICO frames: the pixel-aligned small variant up to 32px, the full mark above.
for px in [16, 24, 32, 48, 64, 128, 256] {
    let src = px <= 32 ? markSmall : mark
    write(canvas(px) { _ in src.draw(in: NSRect(x: 0, y: 0, width: px, height: px)) },
          icoDir.appendingPathComponent("icon-\(px).png"))
}

// macOS app icon, on Apple's 1024 grid: an 824pt body inset 100pt, a soft
// drop shadow below it, a midnight gradient tile, and the coral mark centred
// at ~62% of the body.
let appIcon = canvas(1024) { cg in
    let body = CGRect(x: 100, y: 100, width: 824, height: 824)
    let path = CGPath(roundedRect: body, cornerWidth: 185, cornerHeight: 185, transform: nil)

    cg.saveGState()
    cg.setShadow(offset: CGSize(width: 0, height: -12), blur: 28, color: color(0x000000, 0.30))
    cg.addPath(path)
    cg.setFillColor(color(0x1C2333))
    cg.fillPath()
    cg.restoreGState()

    let grad = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB),
                          colors: [color(0x2A3349), color(0x161C28)] as CFArray,
                          locations: [0, 1])!
    cg.saveGState()
    cg.addPath(path)
    cg.clip()
    cg.drawLinearGradient(grad, start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 100), options: [])
    // hairline highlight along the top edge
    cg.addPath(path)
    cg.setStrokeColor(color(0xFFFFFF, 0.07))
    cg.setLineWidth(4)
    cg.strokePath()
    cg.restoreGState()

    let side: CGFloat = 512
    let markRect = CGRect(x: 512 - side / 2, y: 512 - side / 2, width: side, height: side)
    cg.saveGState()
    cg.setShadow(offset: CGSize(width: 0, height: -10), blur: 30, color: color(0x000000, 0.35))
    mark.draw(in: markRect)
    cg.restoreGState()

    // Knock the mullion out to the tile itself, so the two coral panes read
    // as a window set into the icon rather than a bar pasted on top.
    let bar = CGRect(x: markRect.minX + markRect.width * 10.75 / 24, y: markRect.minY - 1,
                     width: markRect.width * 2.5 / 24, height: markRect.height + 2)
    cg.saveGState()
    cg.clip(to: bar)
    cg.drawLinearGradient(grad, start: CGPoint(x: 512, y: 924), end: CGPoint(x: 512, y: 100), options: [])
    cg.restoreGState()
}
write(appIcon, root.appendingPathComponent("assets/brand/app-icon-1024.png"))
