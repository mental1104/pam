import Foundation

/// StatementPlatform 隔离三个实际截图版式；不从商户推断支付渠道。
enum StatementPlatform: String, Codable, CaseIterable { case cmb, alipay, wechat }

/// OCRBox 使用原图左上角为原点的归一化坐标；金额计算不使用这些浮点几何值。
struct OCRBox: Codable, Equatable {
    var x: Double
    var y: Double
    var width: Double
    var height: Double
    /// centerY 返回文本中心的原图相对纵坐标。
    var centerY: Double { y + height / 2 }
}

/// OCRText 保留 Vision 原文、置信度及原图坐标，id 是持久证据内的文本标识。
struct OCRText: Codable, Identifiable {
    var id: String
    var text: String
    var confidence: Float
    var box: OCRBox
}

/// OCRDocument 关联完整图片 SHA、尺寸和所有文本；captureDate 必须来自明确拍摄信息或人工确认。
struct OCRDocument: Codable {
    var imageHash: String
    var pixelWidth: Int
    var pixelHeight: Int
    var captureDate: String?
    var texts: [OCRText]
}

/// StatementRow 保存可编辑字段和不可变证据引用；issues 非空时必须人工复核。
struct StatementRow: Codable, Identifiable {
    var id: String
    var date = ""
    var time = ""
    var amount = ""
    var direction = ""
    var merchant = ""
    var description = ""
    var balance: String?
    var account = ""
    var issues: [String] = []
    var evidence: [String] = []
}

/// StatementParseResult 保留版本和文档诊断；未知版式不生成可直接提交的流水。
struct StatementParseResult: Codable {
    var version: String
    var platform: StatementPlatform
    var document: OCRDocument
    var rows: [StatementRow]
    var issues: [String]
}

/// StatementParser 以实际文本尺寸、金额语义锚点和日期上下文切分，不依赖设备像素或固定行高。
struct StatementParser {
    static let version = "statement-local-v1"

    /// matches 从 text 按 pattern 提取首个匹配的捕获组；不匹配返回空数组，不改变输入。
    private func matches(_ pattern: String, _ text: String) -> [String] {
        guard let regex = try? NSRegularExpression(pattern: pattern),
              let match = regex.firstMatch(in: text, range: NSRange(text.startIndex..., in: text)) else { return [] }
        return (0..<match.numberOfRanges).map { index in
            Range(match.range(at: index), in: text).map { String(text[$0]) } ?? ""
        }
    }

    /// money 从完整金额标签提取精确十进制文本；仅移除合法千分组，拒绝 OCR 混淆字符。
    private func money(_ text: String) -> (String, String)? {
        let value = text.replacingOccurrences(of: " ", with: "").replacingOccurrences(of: "−", with: "-")
        let groups = matches(#"^([+-]?)[¥￥]?((?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)\.[0-9]{2})$"#, value)
        guard groups.count == 3 else { return nil }
        return (groups[2].replacingOccurrences(of: ",", with: ""), groups[1] == "-" ? "expense" : groups[1] == "+" ? "income" : "")
    }

    /// parse 解析 document 中指定 platform 版式；返回全部候选与阻断诊断，绝不访问网络或写数据库。
    func parse(_ document: OCRDocument, platform: StatementPlatform) -> StatementParseResult {
        let texts = document.texts.sorted { $0.box.centerY < $1.box.centerY }
        let all = texts.map(\.text).joined(separator: "\n")
        let known: Bool
        switch platform {
        case .cmb: known = all.contains("收支") && all.contains("银行卡")
        case .alipay: known = all.contains("搜索交易记录") && all.contains("收支分析")
        case .wechat: known = all.contains("账单") && all.contains("查找交易")
        }
        var result = StatementParseResult(version: Self.version, platform: platform, document: document, rows: [], issues: [])
        guard known, document.pixelWidth > 0, document.pixelHeight > 0 else {
            result.issues = ["unknown_layout"]
            return result
        }
        var year = ""
        var context = ""
        // 标题年份只从来源读取；相对日期绝不能使用运行当天替代截图拍摄日。
        for text in texts {
            let groups = matches(#"(20[0-9]{2})[年.\-]([0-9]{1,2})"#, text.text)
            if groups.count > 2 { year = groups[1]; break }
        }
        let firstDateGroup = texts.first { text in
            text.box.x < 0.35 && (text.text == "今天" || text.text == "昨天" || !matches(#"^[0-9]{1,2}[.]\d{1,2}$"#, text.text).isEmpty)
        }?.box.y
        let anchors = texts.filter { $0.box.x > 0.65 && money($0.text) != nil && (platform != .cmb || firstDateGroup == nil || $0.box.y > firstDateGroup!) }
        for (index, anchor) in anchors.enumerated() {
            let h = anchor.box.height
            let previousY = index > 0 ? anchors[index - 1].box.centerY : 0
            let nextY = index + 1 < anchors.count ? anchors[index + 1].box.centerY : 1
            // 日期组只继承到下一个明确日期组，保留顶部缺失上下文的交易。
            for label in texts where label.box.centerY > previousY && label.box.centerY < anchor.box.centerY && label.box.x < 0.35 {
                let day = matches(#"^([0-9]{1,2})[.\-月]([0-9]{1,2})日?$"#, label.text)
                if day.count == 3, !year.isEmpty { context = String(format: "%@-%02d-%02d", year, Int(day[1]) ?? 0, Int(day[2]) ?? 0) }
                if label.text == "今天" || label.text == "昨天" {
                    context = ""
                    if let date = document.captureDate, let parsed = Self.dateFormatter.date(from: date) {
                        context = Self.dateFormatter.string(from: parsed.addingTimeInterval(label.text == "昨天" ? -86400 : 0))
                    }
                }
            }
            let band = texts.filter { $0.box.centerY >= anchor.box.centerY - h * 0.8 && $0.box.centerY < min(nextY - h * 0.8, anchor.box.centerY + h * 5) }
            let title = band.filter { $0.box.x > 0.10 && $0.box.x < 0.65 && abs($0.box.centerY - anchor.box.centerY) < h * 0.8 }
            let detail = band.filter { $0.box.centerY > anchor.box.centerY + h * 0.6 }
            let detailText = detail.map(\.text).joined(separator: " ")
            let value = money(anchor.text)!
            var row = StatementRow(id: anchor.id, date: context, amount: value.0, direction: value.1, merchant: title.map(\.text).joined(separator: " "), evidence: band.map(\.id))
            let clock = matches(#"([0-9]{2}:[0-9]{2}(?::[0-9]{2})?)"#, detailText)
            if clock.count > 1 { row.time = clock[1] }
            if platform != .cmb {
                let day = matches(platform == .wechat ? #"([0-9]{1,2})月([0-9]{1,2})日"# : #"([0-9]{2})-([0-9]{2})"#, detailText)
                if day.count > 2, !year.isEmpty { row.date = String(format: "%@-%02d-%02d", year, Int(day[1]) ?? 0, Int(day[2]) ?? 0) }
                // 无符号金额可能是不计收支或 OCR 漏号，不能仅凭正数判定收入。
                row.description = detail.filter { !$0.text.contains(":") }.map(\.text).joined(separator: " ")
            } else {
                let account = matches(#"(储蓄卡|信用卡)\s*([0-9]{4})"#, detailText)
                if account.count > 2 { row.account = account[1] + account[2] }
                let balance = matches(#"余额[:：]?[¥￥]?([+-]?(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)\.[0-9]{2})"#, detailText)
                if balance.count > 1 { row.balance = balance[1].replacingOccurrences(of: ",", with: "") }
                if band.contains(where: { $0.text.contains("未入账") }) { row.issues.append("unposted_transaction") }
                if row.account.isEmpty { row.issues.append("missing_account") }
            }
            if row.merchant.isEmpty { row.issues.append("missing_merchant") }
            if row.merchant.contains("…") || row.merchant.contains("...") { row.issues.append("truncated_merchant") }
            if Self.dateFormatter.date(from: row.date).map({ Self.dateFormatter.string(from: $0) }) != row.date { row.issues.append("missing_or_invalid_date") }
            if row.time.isEmpty { row.issues.append("missing_time") }
            if row.direction.isEmpty { row.issues.append("ambiguous_direction") }
            if band.contains(where: { $0.confidence < 0.85 }) { row.issues.append("low_confidence") }
            if anchor.box.y < h || detail.isEmpty || band.contains(where: { $0.box.y + $0.box.height > 1 - h }) { row.issues.append("possibly_incomplete_edge") }
            result.rows.append(row)
        }
        if result.rows.isEmpty { result.issues.append("no_transaction_anchors") }
        let assigned = Set(result.rows.flatMap(\.evidence))
        let unassignedDetails = texts.filter { text in
            guard !assigned.contains(text.id) else { return false }
            switch platform {
            case .cmb: return text.text.contains("储蓄卡") || text.text.contains("信用卡")
            case .alipay: return !matches(#"[0-9]{2}-[0-9]{2}\s+[0-9]{2}:[0-9]{2}"#, text.text).isEmpty
            case .wechat: return !matches(#"[0-9]{1,2}月[0-9]{1,2}日\s+[0-9]{2}:[0-9]{2}"#, text.text).isEmpty
            }
        }
        if !unassignedDetails.isEmpty { result.issues.append("unassigned_transaction_details_possible_missing_or_clipped_amount") }
        // 仅在相邻且同账户、均有余额的降序交易之间核对；失败提示漏行/符号/排序异常，不自动修数。
        for index in 0..<max(0, result.rows.count - 1) {
            let newer = result.rows[index], older = result.rows[index + 1]
            if !newer.account.isEmpty, newer.account == older.account,
               let a = newer.balance.flatMap({ Decimal(string: $0) }), let b = older.balance.flatMap({ Decimal(string: $0) }),
               let amount = Decimal(string: newer.amount), !newer.direction.isEmpty,
               a - b != (newer.direction == "expense" ? -amount : amount) {
                result.rows[index].issues.append("balance_discontinuity")
            }
        }
        // 所有本轮版式尚待 Apple Vision 真实样本验收；人必须复核，避免把规则实验当自动导入许可。
        result.issues.append("layout_requires_human_review")
        return result
    }

    /// dateFormatter 返回固定中国日历日期解析器；严格解析，避免设备时区改变来源日期。
    private static var dateFormatter: DateFormatter {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 8 * 3600)
        formatter.dateFormat = "yyyy-MM-dd"
        formatter.isLenient = false
        return formatter
    }
}

/// CMBStatementParser 是招商截图的独立入口，复用几何及证据模型。
struct CMBStatementParser {
    /// parse 返回 document 的招商候选及诊断，不产生数据库副作用。
    func parse(_ document: OCRDocument) -> StatementParseResult { StatementParser().parse(document, platform: .cmb) }
}

/// AlipayStatementParser 是支付宝列表截图的独立入口，不把无符号金额猜作收入。
struct AlipayStatementParser {
    /// parse 返回 document 的支付宝候选及诊断，不产生数据库副作用。
    func parse(_ document: OCRDocument) -> StatementParseResult { StatementParser().parse(document, platform: .alipay) }
}

/// WeChatStatementParser 是微信账单列表的独立入口，支持月日和标题年份上下文。
struct WeChatStatementParser {
    /// parse 返回 document 的微信候选及诊断，不产生数据库副作用。
    func parse(_ document: OCRDocument) -> StatementParseResult { StatementParser().parse(document, platform: .wechat) }
}
