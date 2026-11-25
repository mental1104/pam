#if canImport(Vision) && canImport(CryptoKit) && canImport(ImageIO)
import Foundation
import Vision
import CryptoKit
import ImageIO

/// LocalStatementOCR 只在本机执行 Vision；按图宽分块，原图 SHA 作为证据唯一身份。
enum LocalStatementOCR {
    /// recognize 对 data 原始截图 OCR；captureDate 是用户确认的拍摄日期，不用当前日期代替。
    /// 返回原图坐标文字；格式/方向不支持或任一分块失败时抛错，不返回伪完整结果。
    static func recognize(_ data: Data, captureDate: String?) throws -> OCRDocument {
        guard let source = CGImageSourceCreateWithData(data as CFData, nil),
              let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else { throw APIError.configuration("Unreadable image.") }
        let properties = CGImageSourceCopyPropertiesAtIndex(source, 0, nil) as? [CFString: Any]
        guard (properties?[kCGImagePropertyOrientation] as? Int ?? 1) == 1 else {
            throw APIError.configuration("Rotate the image upright before recognition.")
        }
        let width = image.width, height = image.height
        let tileHeight = min(height, width * 3)
        let overlap = max(1, width / 2)
        let step = max(1, tileHeight - overlap)
        var starts = [0]
        while starts.last! + tileHeight < height { starts.append(starts.last! + step) }
        var texts: [OCRText] = []
        for (index, start) in starts.enumerated() {
            try Task.checkCancellation()
            let size = min(tileHeight, height - start)
            guard let tile = image.cropping(to: CGRect(x: 0, y: start, width: width, height: size)) else { throw APIError.invalidResponse }
            let request = VNRecognizeTextRequest()
            request.recognitionLevel = .accurate
            request.recognitionLanguages = ["zh-Hans", "en-US"]
            request.usesLanguageCorrection = false
            request.minimumTextHeight = 0
            try VNImageRequestHandler(cgImage: tile, options: [:]).perform([request])
            // 重叠区按文本中心分配唯一所属分块，避免同一交易重复和相同商户误合并。
            let lower = index == 0 ? 0 : start + overlap / 2
            let upper = index == starts.count - 1 ? height : starts[index + 1] + overlap / 2
            for observation in request.results ?? [] {
                guard let candidate = observation.topCandidates(1).first else { continue }
                let box = observation.boundingBox
                // Vision 的 CGFloat 在几何边界显式转换，避免 Apple 编译器选择混合浮点重载。
                let y = Double(start) + (1.0 - Double(box.maxY)) * Double(size)
                let textHeight = Double(box.height) * Double(size)
                let center = y + textHeight / 2.0
                guard center >= Double(lower), center < Double(upper) else { continue }
                texts.append(OCRText(id: "t\(texts.count)", text: candidate.string, confidence: candidate.confidence,
                    box: OCRBox(x: Double(box.minX), y: y / Double(height), width: Double(box.width), height: textHeight / Double(height))))
            }
        }
        return OCRDocument(imageHash: SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined(), pixelWidth: width, pixelHeight: height, captureDate: captureDate, texts: texts)
    }
}
#endif
