import Foundation
import XCTest
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
#if canImport(PAMCore)
@testable import PAMCore
#else
@testable import PAM
#endif

/// APIClientTests 通过独立请求与固定 JSON 校验认证、精度和分页合约。
final class APIClientTests: XCTestCase {
    /// testRejectsUnsafeURLs 确保凭据不能被配置到 HTTP、带身份或查询参数的地址。
    func testRejectsUnsafeURLs() throws {
        for value in ["http://pam.example.com", "https://user:secret@pam.example.com", "https://pam.example.com?token=x", "https://pam.example.com/#fragment"] {
            XCTAssertThrowsError(try APIClient(baseURL: XCTUnwrap(URL(string: value)), token: "test-token"))
        }
        XCTAssertThrowsError(try APIClient(baseURL: XCTUnwrap(URL(string: "https://pam.example.com")), token: " "))
    }

    /// testRequestUsesBearerAndEscapesSearch 验证 token 只进入认证头，搜索值由 URLComponents 编码。
    func testRequestUsesBearerAndEscapesSearch() throws {
        let client = try APIClient(baseURL: XCTUnwrap(URL(string: "https://pam.example.com/service")), token: "test-token")
        let request = try client.makeRequest(path: "api/v1/transactions", query: [URLQueryItem(name: "search", value: "JD & camera + lens")])
        XCTAssertEqual(request.url?.path, "/service/api/v1/transactions")
        XCTAssertEqual(URLComponents(url: try XCTUnwrap(request.url), resolvingAgainstBaseURL: false)?.queryItems?.first?.value, "JD & camera + lens")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer test-token")
        XCTAssertFalse(try XCTUnwrap(request.url?.absoluteString).contains("test-token"))
    }

    /// testTransactionDecodingKeepsExactMoney 验证最大范围金额与来源追溯字段不会经过浮点转换。
    func testTransactionDecodingKeepsExactMoney() throws {
        let json = #"{"items":[{"id":"8f26f916-5c5e-4d3b-b9ec-03b3872723e1","occurred_at":"2025-06-01T12:30:00Z","direction":"expense","amount":"9999999999999999.9999","currency":"CNY","merchant":"Yifang City","description":"Electronics purchase","category":"electronics","source":"fixture","external_id":"flow-1","import_batch_id":"8f26f916-5c5e-4d3b-b9ec-03b3872723e2","original":{"product_hint":"unconfirmed","source_amount":"9999999999999999.9999"},"created_at":"2026-11-24T00:00:00Z","updated_at":"2026-11-24T00:00:00Z"}],"limit":50,"offset":0,"total":1}"#
        let page = try APIClient.decode(Page<TransactionRecord>.self, from: Data(json.utf8))
        XCTAssertEqual(page.total, 1)
        XCTAssertEqual(page.items.first?.amount, "9999999999999999.9999")
        XCTAssertEqual(page.items.first?.importBatchId, "8f26f916-5c5e-4d3b-b9ec-03b3872723e2")
        XCTAssertTrue(try XCTUnwrap(page.items.first?.original?.prettyText).contains("source_amount"))
    }

    /// testAssetDraftSendsOnlyWritableFields 验证可选购买信息为空时不生成伪日期或零金额。
    func testAssetDraftSendsOnlyWritableFields() throws {
        var draft = AssetDraft()
        draft.name = "Camera"
        draft.purchasePrice = "128.1234"
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: encoder.encode(draft)) as? [String: Any])
        XCTAssertEqual(object["purchase_price"] as? String, "128.1234")
        XCTAssertEqual(object["serial_number"] as? String, "")
        XCTAssertNil(object["purchase_date"])
        XCTAssertNil(object["id"])
        XCTAssertNil(object["created_at"])
    }

    /// testDateFilterUsesExclusiveNextDay 确保 Through 的整天被包含，分页和方向条件完整保留。
    func testDateFilterUsesExclusiveNextDay() throws {
        var filter = TransactionFilter()
        filter.direction = "expense"
        filter.dateEnabled = true
        filter.from = Date(timeIntervalSince1970: 0)
        filter.through = Date(timeIntervalSince1970: 0)
        let items = filter.queryItems(offset: 50)
        let values = Dictionary(uniqueKeysWithValues: items.map { ($0.name, $0.value ?? "") })
        XCTAssertEqual(values["offset"], "50")
        XCTAssertEqual(values["direction"], "expense")
        let parser = ISO8601DateFormatter()
        let from = try XCTUnwrap(parser.date(from: XCTUnwrap(values["from"])))
        let to = try XCTUnwrap(parser.date(from: XCTUnwrap(values["to"])))
        XCTAssertEqual(Calendar.current.dateComponents([.day], from: from, to: to).day, 1)
    }

    /// testTimestampDisplayRespectsOriginalPrecision 验证日期来源不显示午夜、分钟不虚构秒、秒精度不丢失。
    func testTimestampDisplayRespectsOriginalPrecision() throws {
        let locale = Locale(identifier: "en_US_POSIX")
        let zone = try XCTUnwrap(TimeZone(secondsFromGMT: 0))
        let value = "2025-05-10T12:34:56Z"
        let day = DisplayFormat.date(value, precision: "date", locale: locale, timeZone: zone)
        let minute = DisplayFormat.date(value, precision: "minute", locale: locale, timeZone: zone)
        let second = DisplayFormat.date(value, precision: "second", locale: locale, timeZone: zone)
        XCTAssertFalse(day.contains(":"))
        XCTAssertTrue(minute.contains(":"))
        XCTAssertFalse(minute.contains(":56"))
        XCTAssertTrue(second.contains(":56"))
        XCTAssertEqual(DisplayFormat.date("invalid", precision: "date"), "invalid")
        let westernZone = try XCTUnwrap(TimeZone(secondsFromGMT: -8 * 3600))
        XCTAssertEqual(DisplayFormat.date("2025-05-09T16:00:00Z", precision: "date", sourceDate: "2025-05-10", locale: locale, timeZone: westernZone), "2025-05-10")
    }

    /// testHTTPErrorEnvelope 异步调用真实 URLSession 替身，确保 HTTP 401 不是解码错误或空成功。
    func testHTTPErrorEnvelope() async throws {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [UnauthorizedProtocol.self]
        let session = URLSession(configuration: configuration)
        defer { session.invalidateAndCancel() }
        let client = try APIClient(baseURL: XCTUnwrap(URL(string: "https://pam.example.com")), token: "test-token", session: session)
        do {
            _ = try await client.assets(offset: 0)
            XCTFail("Expected authentication failure")
        } catch APIError.server(let status, let code, let message) {
            XCTAssertEqual(status, 401)
            XCTAssertEqual(code, "unauthorized")
            XCTAssertEqual(message, "Authentication required")
        }
    }
}

/// UnauthorizedProtocol 作为公开 HTTP 响应替身，只返回固定脱敏错误，不访问真实后端。
private final class UnauthorizedProtocol: URLProtocol {
    /// canInit 接受此测试会话的任意 request，避免外部网络调用。
    override class func canInit(with request: URLRequest) -> Bool { true }

    /// canonicalRequest 保持 request 原样，认证头和 URL 仍由生产客户端构造。
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    /// startLoading 向 URLSession 客户端提供固定的 401 JSON 并结束请求。
    override func startLoading() {
        guard let url = request.url, let response = HTTPURLResponse(url: url, statusCode: 401, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"]) else { return }
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(#"{"error":{"code":"unauthorized","message":"Authentication required"}}"#.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    /// stopLoading 无资源需要释放；替身在 startLoading 内同步完成。
    override func stopLoading() {}
}
