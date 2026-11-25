import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// APIError 是可展示的客户端错误；不包含 token、SQL 或响应原文。
enum APIError: LocalizedError {
    case configuration(String)
    case invalidResponse
    case server(Int, String, String)

    /// errorDescription 返回安全的用户提示，不泄露认证材料。
    var errorDescription: String? {
        switch self {
        case .configuration(let message): return message
        case .invalidResponse: return "The server returned an invalid response."
        case .server(let status, let code, let message):
            return "\(message) (\(code), HTTP \(status))"
        }
    }
}

/// ErrorEnvelope 对应 REST 统一错误格式；消息由服务端脱敏后生成。
private struct ErrorEnvelope: Decodable {
    /// Detail 保存稳定错误码及可展示说明。
    struct Detail: Decodable { let code: String; let message: String }
    let error: Detail
}

/// APIClient 以不可变配置调用 REST；每次请求只向经过校验的 HTTPS 后端发送凭据。
struct APIClient {
    let baseURL: URL
    private let token: String
    private let session: URLSession

    /// init 校验 baseURL 和非空 token，session 默认系统会话，可注入测试替身。
    /// 禁止 URL 内嵌凭据、查询串或片段；失败抛 configuration。
    init(baseURL: URL, token: String, session: URLSession = .shared) throws {
        guard baseURL.scheme?.lowercased() == "https", baseURL.host != nil,
              baseURL.user == nil, baseURL.password == nil,
              baseURL.query == nil, baseURL.fragment == nil else {
            throw APIError.configuration("Use an HTTPS backend URL without credentials, query, or fragment.")
        }
        guard !token.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            throw APIError.configuration("Add an access token in Settings.")
        }
        self.baseURL = baseURL
        self.token = token
        self.session = session
    }

    /// makeRequest 为相对 path、HTTP method、query 和可选 body 构建认证请求。
    /// 返回新 URLRequest；无效 URL 抛 configuration，不发起网络操作。
    func makeRequest(path: String, method: String = "GET", query: [URLQueryItem] = [], body: Data? = nil) throws -> URLRequest {
        guard var components = URLComponents(url: baseURL.appendingPathComponent(path), resolvingAgainstBaseURL: false) else {
            throw APIError.configuration("The backend URL is invalid.")
        }
        components.queryItems = query.isEmpty ? nil : query
        guard let url = components.url else { throw APIError.configuration("The request URL is invalid.") }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.timeoutInterval = 30
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        if let body {
            request.httpBody = body
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        return request
    }

    /// data 发送 request 并检查状态；返回成功正文，HTTP 错误只暴露统一错误模型。
    /// 任务取消沿 URLSession 传播；拒绝跨源重定向，避免 token 跟随跳转。
    private func data(for request: URLRequest) async throws -> Data {
        let (data, response) = try await session.data(for: request, delegate: SameOriginRedirectDelegate(origin: baseURL))
        guard let response = response as? HTTPURLResponse else { throw APIError.invalidResponse }
        guard (200..<300).contains(response.statusCode) else {
            let error = try? JSONDecoder().decode(ErrorEnvelope.self, from: data)
            throw APIError.server(response.statusCode, error?.error.code ?? "request_failed", error?.error.message ?? "The request could not be completed.")
        }
        return data
    }

    /// decode 将 data 按 snake_case REST 合约解码成 type，错误由调用视图展示。
    static func decode<T: Decodable>(_ type: T.Type, from data: Data) throws -> T {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return try decoder.decode(type, from: data)
    }

    /// transactions 按 filter 与 offset 查询分页流水；返回服务端原始金额文本。
    func transactions(filter: TransactionFilter, offset: Int) async throws -> Page<TransactionRecord> {
        let request = try makeRequest(path: "api/v1/transactions", query: filter.queryItems(offset: offset))
        return try Self.decode(Page<TransactionRecord>.self, from: await data(for: request))
    }

    /// transaction 根据 id 重新读取详情；不存在或访问失败时抛 APIError。
    func transaction(id: String) async throws -> TransactionRecord {
        let request = try makeRequest(path: "api/v1/transactions/\(id)")
        return try Self.decode(TransactionRecord.self, from: await data(for: request))
    }

    /// assets 按 offset 查询资产分页；返回新列表且不修改已有视图状态。
    func assets(offset: Int) async throws -> Page<AssetRecord> {
        let request = try makeRequest(path: "api/v1/assets", query: [URLQueryItem(name: "limit", value: "50"), URLQueryItem(name: "offset", value: String(offset))])
        return try Self.decode(Page<AssetRecord>.self, from: await data(for: request))
    }

    /// asset 按 id 重新读取资产详情，确保编辑后的购买资料及时更新。
    func asset(id: String) async throws -> AssetRecord {
        let request = try makeRequest(path: "api/v1/assets/\(id)")
        return try Self.decode(AssetRecord.self, from: await data(for: request))
    }

    /// save 将 draft 新增或写回 id 对应资产，返回服务端确认的完整记录。
    /// id 为 nil 表示新增；JSON 金额保持字符串，业务校验失败不更新本地列表。
    func save(draft: AssetDraft, id: String?) async throws -> AssetRecord {
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        let request = try makeRequest(path: id.map { "api/v1/assets/\($0)" } ?? "api/v1/assets", method: id == nil ? "POST" : "PUT", body: encoder.encode(draft))
        return try Self.decode(AssetRecord.self, from: await data(for: request))
    }

    /// delete 删除 id 对应资产；仅在服务端返回成功后允许视图移除该项。
    func delete(id: String) async throws {
        _ = try await data(for: makeRequest(path: "api/v1/assets/\(id)", method: "DELETE"))
    }

    /// importCall 对 path 执行 method、query 和可选 body，返回 type 指定的共享导入 DTO；网络错误向上传递。
    func importCall<T: Decodable>(_ type: T.Type, path: String, method: String = "GET", query: [URLQueryItem] = [], body: Data? = nil) async throws -> T {
        try Self.decode(type, from: await data(for: makeRequest(path: "api/v1/imports/" + path, method: method, query: query, body: body)))
    }

    /// propose 提交 request 的结构化文本证据，返回持久预检；不上传原图，不写入账本。
    func propose(_ request: StatementImportRequest) async throws -> StatementProposal {
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        return try await importCall(StatementProposal.self, path: "proposals", method: "POST", body: encoder.encode(request))
    }
}

/// SameOriginRedirectDelegate 只允许同源 HTTPS 跳转，防止认证头被转发给其他主机。
private final class SameOriginRedirectDelegate: NSObject, URLSessionTaskDelegate {
    private let origin: URL

    /// init 保存本次请求的 origin，用于校验重定向目标。
    init(origin: URL) { self.origin = origin }

    /// urlSession 比较 request 目标与 origin；completionHandler 接收允许的请求或 nil。
    /// session、task 和 response 为 URLSession 提供的本次重定向上下文。
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
        let target = request.url
        let allowed = target?.scheme == origin.scheme && target?.host == origin.host && (target?.port ?? 443) == (origin.port ?? 443)
        completionHandler(allowed ? request : nil)
    }
}
