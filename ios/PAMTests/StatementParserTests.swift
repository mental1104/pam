import XCTest
#if canImport(PAMCore)
@testable import PAMCore
#else
@testable import PAM
#endif

/// StatementParserTests 的 OCR 坐标均为人工构造，不代表 Apple Vision 真实识别准确率。
final class StatementParserTests: XCTestCase {
    /// document 为指定 platform 构造包含两笔同额交易的可变高度文档；height 只影响几何。
    private func document(_ platform: StatementPlatform, height: Int = 2000) -> OCRDocument {
        let headers: [StatementPlatform: String] = [.cmb: "收支 银行卡 2026.10", .alipay: "搜索交易记录 收支分析 2026-10", .wechat: "账单 查找交易 2026年10月"]
        var texts: [OCRText] = []
        /// add 按原图像素生成归一化证据，text 为可见原文，x/y 是原图左上角位置。
        func add(_ text: String, _ x: Double, _ y: Double) {
            texts.append(OCRText(id: "t\(texts.count)", text: text, confidence: 0.99, box: OCRBox(x: x, y: y / Double(height), width: 0.15, height: 20 / Double(height))))
        }
        add(headers[platform]!, 0.05, 50)
        add("10.22", 0.08, 180)
        for y in [300.0, 520.0] {
            add("Example Shop", 0.20, y)
            add("-¥7.00", 0.80, y)
            add(platform == .cmb ? "储蓄卡0123 10:01" : platform == .wechat ? "10月22日 10:01" : "10-22 10:01", 0.20, y + 35)
        }
        return OCRDocument(imageHash: String(repeating: "a", count: 64), pixelWidth: 900, pixelHeight: height, texts: texts)
    }

    /// testPlatformsAndVariableHeight 验证三版式不依赖总高度，保留同商户同额独立行和金额精度。
    func testPlatformsAndVariableHeight() {
        for platform in StatementPlatform.allCases {
            for height in [1000, 40000] {
                let result = StatementParser().parse(document(platform, height: height), platform: platform)
                XCTAssertEqual(result.rows.count, 2)
                XCTAssertEqual(result.rows.map(\.amount), ["7.00", "7.00"])
                XCTAssertEqual(result.rows.map(\.direction), ["expense", "expense"])
                XCTAssertEqual(result.rows.map(\.date), ["2026-10-22", "2026-10-22"])
                XCTAssertEqual(result.rows.map(\.time), ["10:01", "10:01"])
                XCTAssertEqual(result.rows.map(\.merchant), ["Example Shop", "Example Shop"])
            }
        }
    }

    /// testUnknownAndMissingFields 验证未知版式、漏符号、相对日期和底部残行阻断自动导入。
    func testUnknownAndMissingFields() {
        var input = document(.cmb)
        input.texts[0].text = "Unknown bank"
        XCTAssertEqual(CMBStatementParser().parse(input).issues, ["unknown_layout"])
        input = document(.cmb)
        input.texts[1].text = "今天"
        input.texts[3].text = "7.00"
        input.texts.removeLast()
        let result = CMBStatementParser().parse(input)
        XCTAssertTrue(result.rows[0].issues.contains("ambiguous_direction"))
        XCTAssertTrue(result.rows[0].issues.contains("missing_or_invalid_date"))
        XCTAssertTrue(result.rows[1].issues.contains("missing_time"))
        XCTAssertTrue(result.rows[1].issues.contains("possibly_incomplete_edge"))
    }

    /// testBalanceMismatch 对同账户相邻交易的余额断裂发出诊断，不修改识别金额。
    func testBalanceMismatch() {
        var input = document(.cmb)
        input.texts.append(OCRText(id: "b1", text: "余额:¥301.49", confidence: 0.99, box: OCRBox(x: 0.70, y: 335.0/2000, width: 0.2, height: 0.01)))
        input.texts.append(OCRText(id: "b2", text: "余额:¥319.49", confidence: 0.99, box: OCRBox(x: 0.70, y: 555.0/2000, width: 0.2, height: 0.01)))
        XCTAssertTrue(CMBStatementParser().parse(input).rows[0].issues.contains("balance_discontinuity"))
    }

    /// testSubmissionPreservesEvidenceAndReviewFlag 验证本地结果转换保持精确金额、真实文件身份和待核信息，不携带截图字节。
    func testSubmissionPreservesEvidenceAndReviewFlag() throws {
        let result = CMBStatementParser().parse(document(.cmb))
        let request = try StatementSubmission.request(result, rows: result.rows, reviewed: false, key: "stable-retry-key")
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        let wire = try XCTUnwrap(JSONSerialization.jsonObject(with: encoder.encode(request)) as? [String: Any])
        XCTAssertEqual(wire["source_file_hash"] as? String, result.document.imageHash)
        XCTAssertEqual(wire["idempotency_key"] as? String, "stable-retry-key")
        let rows = try XCTUnwrap(wire["records"] as? [[String: Any]])
        XCTAssertEqual(rows.count, 2)
        XCTAssertEqual(rows[0]["amount"] as? String, "7.00")
        XCTAssertNil(rows[0]["external_id"])
        let original = try XCTUnwrap(rows[0]["original"] as? [String: Any])
        XCTAssertEqual(original["pam_human_reviewed"] as? Bool, false)
        XCTAssertEqual(original["pam_image_width"] as? Int, 900)
        XCTAssertFalse(try XCTUnwrap(original["pam_parse_issues"] as? [String]).isEmpty)
        XCTAssertFalse(try XCTUnwrap(original["pam_ocr_evidence"] as? [[String: Any]]).isEmpty)
        XCTAssertNil(original["image_data"])
    }
}
