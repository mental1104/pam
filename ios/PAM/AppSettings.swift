import Foundation
import Combine
import Security

/// CredentialStore 将单用户 token 存入本机 Keychain，不写入偏好设置或日志。
enum CredentialStore {
    private static let service = "com.mental1104.pam"
    private static let account = "backend-access-token"

    /// read 返回已存 token；尚未配置时返回空串，系统错误由配置界面重新保存恢复。
    static func read() -> String {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: account, kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne]
        var result: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess, let data = result as? Data else { return "" }
        return String(data: data, encoding: .utf8) ?? ""
    }

    /// save 保存 token；空值删除凭据，失败抛出错误，不覆盖为不安全存储。
    static func save(_ token: String) throws {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: account]
        if token.isEmpty {
            let status = SecItemDelete(query as CFDictionary)
            guard status == errSecSuccess || status == errSecItemNotFound else { throw APIError.configuration("Could not remove the saved credential.") }
            return
        }
        let attributes: [String: Any] = [kSecValueData as String: Data(token.utf8)]
        var status = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if status == errSecItemNotFound {
            var item = query.merging(attributes) { _, replacement in replacement }
            item[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
            status = SecItemAdd(item as CFDictionary, nil)
        }
        guard status == errSecSuccess else { throw APIError.configuration("Could not save the credential in Keychain.") }
    }
}

/// AppSettings 在主线程发布已确认配置；token 只保存在内存与 Keychain。
@MainActor final class AppSettings: ObservableObject {
    @Published private(set) var backendURL = UserDefaults.standard.string(forKey: "backendURL") ?? ""
    @Published private(set) var revision = 0
    private var token = CredentialStore.read()

    /// isConfigured 表示已有 URL 与 token；请求时仍会执行完整 URL 校验。
    var isConfigured: Bool { !backendURL.isEmpty && !token.isEmpty }

    /// makeClient 返回当前配置的不可变客户端；配置缺失或 URL 不安全时抛错误。
    func makeClient() throws -> APIClient {
        guard let url = URL(string: backendURL) else { throw APIError.configuration("Configure the backend URL in Settings.") }
        return try APIClient(baseURL: url, token: token)
    }

    /// save 校验 url 与 credential 后先写 Keychain，再发布新配置使列表重新加载。
    /// credential 为空时保留现有 token，首次配置必须提供有效 token。
    func save(url: String, credential: String) throws {
        let normalized = url.trimmingCharacters(in: .whitespacesAndNewlines)
        let nextToken = credential.isEmpty ? token : credential.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let parsed = URL(string: normalized) else { throw APIError.configuration("Enter a valid HTTPS backend URL.") }
        _ = try APIClient(baseURL: parsed, token: nextToken)
        try CredentialStore.save(nextToken)
        token = nextToken
        backendURL = normalized
        UserDefaults.standard.set(normalized, forKey: "backendURL")
        revision += 1
    }

    /// clearCredential 删除认证 token 并触发界面返回未配置状态；失败保留当前内存配置。
    func clearCredential() throws {
        try CredentialStore.save("")
        token = ""
        revision += 1
    }
}
