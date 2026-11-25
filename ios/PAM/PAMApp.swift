import SwiftUI

/// PAMApp 仅负责应用生命周期与共享配置，不持有数据库连接或财务计算状态。
@main struct PAMApp: App {
    @StateObject private var settings = AppSettings()

    /// body 注入单用户配置并显示三个原生业务入口。
    var body: some Scene {
        WindowGroup {
            TabView {
                TransactionsView().id(settings.revision).tabItem { Label("Transactions", systemImage: "list.bullet.rectangle") }
                AssetsView().id(settings.revision).tabItem { Label("Assets", systemImage: "shippingbox") }
                StatementImportView().id(settings.revision).tabItem { Label("Import", systemImage: "doc.viewfinder") }
                SettingsView().tabItem { Label("Settings", systemImage: "gearshape") }
            }
            .environmentObject(settings)
        }
    }
}

/// ConnectionRequiredView 在尚未配置后端时说明下一步，不显示误导性的空业务结果。
struct ConnectionRequiredView: View {
    /// body 提示切换 Settings，配置完成后父视图会自动重新加载。
    var body: some View {
        ContentUnavailableView("Connect to PAM", systemImage: "network", description: Text("Add your HTTPS backend URL and access token in Settings."))
    }
}

/// FailureView 显示可恢复错误；retry 由父视图负责启动新的异步请求。
struct FailureView: View {
    let message: String
    let retry: () -> Void

    /// body 在空结果页面内展示错误与明确的重试操作。
    var body: some View {
        ContentUnavailableView {
            Label("Could not load records", systemImage: "exclamationmark.triangle")
        } description: {
            Text(message)
        } actions: {
            Button("Retry", action: retry)
        }
    }
}
