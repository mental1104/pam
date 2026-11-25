import Foundation
import XCTest
#if canImport(PAMCore)
@testable import PAMCore
#else
@testable import PAM
#endif

/// RealStatementRegressionTests 只接受显式私有语料，Linux 明确跳过真实 Vision 验收。
final class RealStatementRegressionTests: XCTestCase {
    /// Corpus 读取只读工具生成的真实图片及历史对照，不内嵌私人金融数据到 Git。
    private struct Corpus: Decodable {
        /// Sample 保存真实文件相对路径、SHA 和逐笔历史基线；不把旧文件 mtime 当拍摄日期。
        struct Sample: Decodable {
            /// Expected 是历史对照的五个可检查字段，不包含未经证实的分类。
            struct Expected: Decodable { var date: String; var time: String; var amount: String; var direction: String; var merchant: String }
            var platform: StatementPlatform
            var image: String
            var imageHash: String
            var captureDate: String?
            var expected: [Expected]
        }
        var missingImages: [String]
        var samples: [Sample]
    }

    /// testRealVisionCorpus 在可用 Apple Vision 环境执行全部选定真实截图；缺环境明确跳过，不冒充通过。
    func testRealVisionCorpus() throws {
        #if canImport(Vision) && canImport(CryptoKit)
        let environment = ProcessInfo.processInfo.environment
        guard let manifest = environment["PAM_REAL_CORPUS"], let root = environment["PAM_REAL_IMAGE_ROOT"], let output = environment["PAM_REAL_REPORT_DIR"] else {
            throw XCTSkip("Real corpus, source root and private report directory are required")
        }
        let corpus = try JSONDecoder().decode(Corpus.self, from: Data(contentsOf: URL(fileURLWithPath: manifest)))
        try FileManager.default.createDirectory(atPath: output, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        XCTAssertTrue(corpus.missingImages.isEmpty, "Real corpus has missing source images")
        for sample in corpus.samples {
            let input = try Data(contentsOf: URL(fileURLWithPath: root).appendingPathComponent(sample.image))
            let document = try LocalStatementOCR.recognize(input, captureDate: sample.captureDate)
            XCTAssertEqual(document.imageHash, sample.imageHash, "Real image hash differs from baseline")
            let parsed = StatementParser().parse(document, platform: sample.platform)
            let path = URL(fileURLWithPath: output).appendingPathComponent(sample.imageHash + ".json")
            try JSONEncoder().encode(parsed).write(to: path, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: path.path)
            XCTAssertEqual(parsed.rows.count, sample.expected.count, "Real transaction count mismatch; inspect private report")
            // 使用多重集合逐笔对照，不因同一时间同额记录而丢失合法双笔。
            var remaining = parsed.rows
            for expected in sample.expected {
                let index = remaining.firstIndex { row in
                    row.date == expected.date && row.time == expected.time && Decimal(string: row.amount) == Decimal(string: expected.amount) && row.direction == expected.direction && row.merchant == expected.merchant
                }
                XCTAssertNotNil(index, "Real field mismatch; inspect private report")
                if let index { remaining.remove(at: index) }
            }
        }
        #else
        throw XCTSkip("Apple Vision is unavailable; no real OCR acceptance was performed")
        #endif
    }
}
