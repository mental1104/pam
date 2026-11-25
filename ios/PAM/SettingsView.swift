import SwiftUI

/// SettingsView 编辑 HTTPS 入口与 Keychain 凭据；不会将现有 token 回填到文本框。
struct SettingsView: View {
    @EnvironmentObject private var settings: AppSettings
    @State private var url = ""
    @State private var token = ""
    @State private var message: String?
    @State private var saving = false

    /// body 展示连接表单与凭据删除动作，保存成功后清空 token 输入。
    var body: some View {
        NavigationStack {
            Form {
                Section("Backend") {
                    TextField("https://pam.example.com", text: $url)
                        .keyboardType(.URL).textInputAutocapitalization(.never).autocorrectionDisabled()
                    SecureField(settings.isConfigured ? "Leave blank to keep saved token" : "Access token", text: $token)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                    Button("Save and connect") { save() }.disabled(saving)
                }
                Section {
                    Text("The access token is stored in this device's Keychain. Financial records remain on your server.")
                        .font(.footnote).foregroundStyle(.secondary)
                    if settings.isConfigured {
                        Button("Remove saved token", role: .destructive) { removeToken() }
                    }
                }
                if let message { Section { Text(message).accessibilityIdentifier("connectionMessage") } }
            }
            .navigationTitle("Settings")
            .onAppear { url = settings.backendURL }
        }
    }

    /// save 校验并保存当前输入后通过真实 REST 请求验证连接，失败时保留可修正配置。
    private func save() {
        saving = true
        message = nil
        Task {
            defer { saving = false }
            do {
                try settings.save(url: url, credential: token)
                token = ""
                _ = try await settings.makeClient().assets(offset: 0)
                message = "Connected successfully."
            } catch { message = error.localizedDescription }
        }
    }

    /// removeToken 删除已保存凭据；系统拒绝时显示原因并保持现有状态。
    private func removeToken() {
        do {
            try settings.clearCredential()
            token = ""
            message = "Saved token removed."
        } catch { message = error.localizedDescription }
    }
}
