import Foundation

/// StatementImportRecord 保持金额文本和原始证据，禁止截图行伪装成银行外部交易号。
struct StatementImportRecord: Codable {
    var rowKey: String
    var occurredAt: String
    var direction: String
    var amount: String
    var currency = "CNY"
    var merchant: String
    var description: String
    var original: JSONValue
}

/// StatementImportRequest 每张原图单独使用实际 SHA，人工编辑后应创建新提案键。
struct StatementImportRequest: Codable {
    var source: String
    var idempotencyKey: String
    var sourceFileHash: String
    var records: [StatementImportRecord]
}

/// StatementImportResult 表示后端互斥回执；预检中的 created 只是候选数。
struct StatementImportResult: Codable {
    /// Row 保存逐行结果，不把 duplicated 解释为已删除的重复。
    struct Row: Codable { var index: Int; var status: String; var reason: String; var transactionId: String? }
    var batchId: String
    var created: Int
    var skipped: Int
    var duplicated: Int
    var failed: Int
    var rows: [Row]
}

/// StatementProposal 用服务端 ID 恢复确认，不依赖内存隐式会话。
struct StatementProposal: Codable {
    var id: String
    var preview: StatementImportResult
    var committed: Bool
}

/// StatementReview 是后端待核投影，record 允许完整展示和修正。
struct StatementReview: Decodable, Identifiable {
    var id: String
    var kind: String
    var state: String
    var source: String
    var record: JSONValue
    var transactionId: String?
}

/// StatementSubmission 统一把人工复核状态和修改后的字段关联回本地 OCR 证据。
enum StatementSubmission {
    /// request 将 result 的 rows 转成单图请求；reviewed 只表达人工已核对，仍由服务端严格校验字段。
    /// key 是本次编辑版本的稳定 UUID，重试必须复用；返回不包含图片字节的请求。
    static func request(_ result: StatementParseResult, rows: [StatementRow], reviewed: Bool, key: String) throws -> StatementImportRequest {
        let records = try rows.map { row -> StatementImportRecord in
            let evidence = result.document.texts.filter { row.evidence.contains($0.id) }
            let encoded = try JSONEncoder().encode(evidence)
            let original: JSONValue = .object([
                "pam_parser_version": .string(result.version),
                "pam_image_sha256": .string(result.document.imageHash),
                "pam_image_width": .number(Decimal(result.document.pixelWidth)),
                "pam_image_height": .number(Decimal(result.document.pixelHeight)),
                "pam_parse_issues": .array((result.issues + row.issues).map { .string($0) }),
                "pam_human_reviewed": .bool(reviewed),
                "pam_source_date": .string(row.date),
                "pam_timestamp_precision": .string(row.time.count == 8 ? "second" : "minute"),
                "pam_account": .string(row.account),
                "pam_balance": row.balance.map { .string($0) } ?? .null,
                "来源平台": .string(result.platform.rawValue),
                "pam_ocr_evidence": try JSONDecoder().decode(JSONValue.self, from: encoded)
            ])
            return StatementImportRecord(rowKey: row.id, occurredAt: row.date + "T" + row.time + (row.time.count == 5 ? ":00" : "") + "+08:00", direction: row.direction, amount: row.amount, merchant: row.merchant, description: row.description, original: original)
        }
        return StatementImportRequest(source: result.platform == .cmb ? "cmb_statement" : result.platform.rawValue + "_statement", idempotencyKey: key, sourceFileHash: result.document.imageHash, records: records)
    }
}

/// StatementSourceRange 只表示记录范围；confirmedCoverage 为空时不能声称完整覆盖。
struct StatementSourceRange: Decodable {
    var source: String
    var account: String
    var count: Int
    var first: String
    var last: String
    var confirmedCoverage: [String]
}
