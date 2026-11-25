import Foundation

/// Page 表示 REST 的分页结果；金额始终由具体记录中的字符串保存。
struct Page<Item: Decodable>: Decodable {
    let items: [Item]
    let limit: Int
    let offset: Int
    let total: Int
}

/// TransactionRecord 是只读流水 DTO，保留来源及导入批次供详情追溯。
struct TransactionRecord: Decodable, Identifiable {
    let id: String
    let occurredAt: String
    let direction: String
    let amount: String
    let currency: String
    let merchant: String
    let description: String
    let category: String?
    let source: String
    let externalId: String?
    let importBatchId: String
    let original: JSONValue?
    let createdAt: String
    let updatedAt: String

    /// timestampPrecision 读取 Agent 保留的来源时间精度，未知时不擅自推断到秒。
    var timestampPrecision: String? {
        guard case .object(let fields)? = original, case .string(let precision)? = fields["pam_timestamp_precision"] else { return nil }
        return precision
    }

    /// sourceCalendarDate 优先读取规范来源日期，兼容已提供账单的“日期”字段，避免日期型来源跨时区变日。
    var sourceCalendarDate: String? {
        guard case .object(let fields)? = original else { return nil }
        for key in ["pam_source_date", "日期"] {
            if case .string(let date)? = fields[key] { return date }
        }
        return nil
    }
}

/// AssetRecord 是登记后的实物资产；日期与金额沿用 API 的文本精度。
struct AssetRecord: Codable, Identifiable {
    let id: String
    var name: String
    var category: String
    var brand: String
    var model: String
    var serialNumber: String
    var purchaseDate: String?
    var purchasePrice: String?
    var currency: String
    var merchant: String
    var relatedTransactionId: String?
    var notes: String
    var status: String
    let createdAt: String
    let updatedAt: String
}

/// AssetDraft 只包含可写业务字段，避免客户端修改服务端 ID 和时间戳。
struct AssetDraft: Codable {
    var name = ""
    var category = ""
    var brand = ""
    var model = ""
    var serialNumber = ""
    var purchaseDate: String?
    var purchasePrice: String?
    var currency = "CNY"
    var merchant = ""
    var relatedTransactionId: String?
    var notes = ""
    var status = "active"

    /// init 创建空草稿；服务器负责最终业务校验。
    init() {}

    /// init 从已有 asset 复制可写字段，编辑时不携带服务端元数据。
    init(asset: AssetRecord) {
        name = asset.name
        category = asset.category
        brand = asset.brand
        model = asset.model
        serialNumber = asset.serialNumber
        purchaseDate = asset.purchaseDate
        purchasePrice = asset.purchasePrice
        currency = asset.currency
        merchant = asset.merchant
        relatedTransactionId = asset.relatedTransactionId
        notes = asset.notes
        status = asset.status
    }
}

/// TransactionFilter 保存一次列表查询条件；日期区间使用包含起点、不包含终点。
struct TransactionFilter: Equatable {
    var search = ""
    var direction = ""
    var dateEnabled = false
    var from = Calendar.current.date(byAdding: .month, value: -1, to: Date()) ?? Date()
    var through = Date()

    /// queryItems 将 offset 分页和当前条件转成查询参数，返回新数组。
    /// offset 为已加载记录数；日期按设备日历取自然日，结束日推进一天。
    func queryItems(offset: Int) -> [URLQueryItem] {
        var items = [URLQueryItem(name: "limit", value: "50"), URLQueryItem(name: "offset", value: String(offset))]
        if !search.isEmpty { items.append(URLQueryItem(name: "search", value: search)) }
        if !direction.isEmpty { items.append(URLQueryItem(name: "direction", value: direction)) }
        if dateEnabled {
            let calendar = Calendar.current
            let start = calendar.startOfDay(for: from)
            let end = calendar.date(byAdding: .day, value: 1, to: calendar.startOfDay(for: through)) ?? through
            items.append(URLQueryItem(name: "from", value: ISO8601DateFormatter().string(from: start)))
            items.append(URLQueryItem(name: "to", value: ISO8601DateFormatter().string(from: end)))
        }
        return items
    }
}

/// JSONValue 保留原始导入 JSON 的结构；数字使用 Decimal，避免 Double 损失金额精度。
enum JSONValue: Codable {
    case object([String: JSONValue]), array([JSONValue]), string(String), number(Decimal), bool(Bool), null

    /// init 根据 decoder 的实际 JSON 类型解码；无法识别时向上传递解析错误。
    init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        if container.decodeNil() { self = .null }
        else if let value = try? container.decode(Bool.self) { self = .bool(value) }
        else if let value = try? container.decode(String.self) { self = .string(value) }
        else if let value = try? container.decode(Decimal.self) { self = .number(value) }
        else if let value = try? container.decode([String: JSONValue].self) { self = .object(value) }
        else { self = .array(try container.decode([JSONValue].self)) }
    }

    /// encode 将当前原始值写入 encoder，失败时保留标准编码错误。
    func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        switch self {
        case .object(let value): try container.encode(value)
        case .array(let value): try container.encode(value)
        case .string(let value): try container.encode(value)
        case .number(let value): try container.encode(value)
        case .bool(let value): try container.encode(value)
        case .null: try container.encodeNil()
        }
    }

    /// prettyText 返回适合详情展示的 JSON；编码失败仅显示固定提示，不输出敏感日志。
    var prettyText: String {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        return (try? String(data: encoder.encode(self), encoding: .utf8)) ?? "Unavailable"
    }
}

/// DisplayFormat 汇集不修改原始记录的日期展示规则。
enum DisplayFormat {
    /// date 将 RFC3339 value 按 precision 显示，date 不显示臆造时间，second 保留秒。
    /// sourceDate 是来源的 YYYY-MM-DD 自然日，日期型来源优先保留该日；locale/timeZone 用于其余时间展示。
    /// 无法解析时返回原文；参数不修改，测试可固定区域与时区。
    static func date(_ value: String, precision: String? = nil, sourceDate: String? = nil, locale: Locale = .current, timeZone: TimeZone = .current) -> String {
        if precision == "date", let sourceDate {
            let calendarParser = DateFormatter()
            calendarParser.calendar = Calendar(identifier: .gregorian)
            calendarParser.locale = Locale(identifier: "en_US_POSIX")
            calendarParser.timeZone = TimeZone(secondsFromGMT: 0)
            calendarParser.dateFormat = "yyyy-MM-dd"
            calendarParser.isLenient = false
            if let parsed = calendarParser.date(from: sourceDate), calendarParser.string(from: parsed) == sourceDate { return sourceDate }
        }
        let parser = ISO8601DateFormatter()
        parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let date = parser.date(from: value) ?? ISO8601DateFormatter().date(from: value)
        guard let date else { return value }
        let formatter = DateFormatter()
        formatter.locale = locale
        formatter.timeZone = timeZone
        formatter.dateStyle = .medium
        formatter.timeStyle = precision == "date" ? .none : precision == "second" ? .medium : .short
        return formatter.string(from: date)
    }
}
