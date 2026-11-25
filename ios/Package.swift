// swift-tools-version: 5.9
import PackageDescription

// 仅将与系统界面无关的 REST 模型和网络层作为可移植测试目标；应用由 Xcode 工程构建。
let package = Package(
    name: "PAMCore",
    platforms: [.iOS(.v17), .macOS(.v14)],
    products: [.library(name: "PAMCore", targets: ["PAMCore"])],
    targets: [
        .target(name: "PAMCore", path: "PAM", exclude: ["AppSettings.swift", "PAMApp.swift", "SettingsView.swift", "TransactionsView.swift", "AssetsView.swift", "StatementImportView.swift"], sources: ["Models.swift", "APIClient.swift", "StatementParser.swift", "StatementImport.swift", "LocalStatementOCR.swift"]),
        .testTarget(name: "PAMCoreTests", dependencies: ["PAMCore"], path: "PAMTests")
    ]
)
