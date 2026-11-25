import Foundation
import XCTest
#if canImport(PAMCore)
@testable import PAMCore
#else
@testable import PAM
#endif
#if os(macOS)
import AppKit
#endif

/// LocalVisionTests 在 Apple 平台通过合成图片实际调用 Vision，不冒充真实账单回归。
final class LocalVisionTests: XCTestCase {
    /// testTallSyntheticImage 在长图多个分块识别重复文字，验证坐标、SHA 和重叠区不丢失独立证据。
    func testTallSyntheticImage() throws {
        #if os(macOS)
        let width = 900, height = 9000
        // AppKit 绘图上下文要求受支持的像素布局；显式使用 32-bit RGBA，避免 24-bit RGB 返回 nil。
        let representation = try XCTUnwrap(NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: width, pixelsHigh: height, bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: width * 4, bitsPerPixel: 32))
        let context = try XCTUnwrap(NSGraphicsContext(bitmapImageRep: representation))
        NSGraphicsContext.saveGraphicsState()
        defer { NSGraphicsContext.restoreGraphicsState() }
        NSGraphicsContext.current = context
        NSColor.white.setFill()
        NSRect(x: 0, y: 0, width: width, height: height).fill()
        let attributes: [NSAttributedString.Key: Any] = [.font: NSFont.systemFont(ofSize: 45), .foregroundColor: NSColor.black]
        // 同样文字位于图顶、重叠区附近和图底，不能按字符串去重。
        for y in [200, 2450, 7900] {
            ("PAM OCR TEST 123.45" as NSString).draw(at: NSPoint(x: 100, y: height - y), withAttributes: attributes)
        }
        let data = try XCTUnwrap(representation.representation(using: .png, properties: [:]))
        let document = try LocalStatementOCR.recognize(data, captureDate: nil)
        XCTAssertEqual(document.pixelWidth, width)
        XCTAssertEqual(document.pixelHeight, height)
        XCTAssertEqual(document.imageHash.count, 64)
        let amounts = document.texts.filter { $0.text.contains("123.45") }
        XCTAssertEqual(amounts.count, 3, "Synthetic repeated lines must survive tiling")
        for text in amounts {
            XCTAssertGreaterThan(text.confidence, 0)
            XCTAssertGreaterThanOrEqual(text.box.y, 0)
            XCTAssertLessThanOrEqual(text.box.y + text.box.height, 1.001)
        }
        #else
        throw XCTSkip("Synthetic Vision smoke test requires macOS")
        #endif
    }
}
