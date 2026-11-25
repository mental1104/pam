import SwiftUI
import PhotosUI
import UIKit

/// LocalStatementDraft 保存人工编辑和服务端提案 ID；图片按 SHA 单独存放。
struct LocalStatementDraft: Codable, Identifiable {
    var result: StatementParseResult
    var reviewed = false
    var key = UUID().uuidString
    var proposal: StatementProposal?
    /// id 使用原图 SHA 与平台，允许针对同一图片纠正平台选择。
    var id: String { result.document.imageHash + "." + result.platform.rawValue }
}

/// StatementDraftStore 将草稿和原图保存在受系统文件保护的应用目录，重启后仍可继续复核。
@MainActor final class StatementDraftStore: ObservableObject {
    @Published var drafts: [LocalStatementDraft] = [] { didSet { persist() } }
    @Published var error = ""
    private let directory: URL
    private var canPersist = true

    /// init 恢复本机草稿；缺失表示首次运行，损坏则明确提示，不覆盖旧证据。
    init() {
        directory = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("StatementEvidence")
        do {
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            let path = directory.appendingPathComponent("drafts.json")
            if FileManager.default.fileExists(atPath: path.path) { drafts = try JSONDecoder().decode([LocalStatementDraft].self, from: Data(contentsOf: path)) }
        } catch { canPersist = false; self.error = "Could not restore saved statement drafts. Existing evidence was preserved." }
    }

    /// persist 原子保存全部草稿；失败保留旧文件并向界面报告，不写敏感日志。
    private func persist() {
        guard canPersist else { return }
        do { try JSONEncoder().encode(drafts).write(to: directory.appendingPathComponent("drafts.json"), options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication]) }
        catch { self.error = "Could not save statement drafts." }
    }

    /// add 保存 data 的原始字节一次，再追加 result；同平台同图重选保留已有人工编辑。
    func add(data: Data, result: StatementParseResult) throws {
        guard canPersist else { throw APIError.configuration("Restore the saved evidence before adding new drafts.") }
        let path = directory.appendingPathComponent(result.document.imageHash + ".image")
        if !FileManager.default.fileExists(atPath: path.path) { try data.write(to: path, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication]) }
        let draft = LocalStatementDraft(result: result)
        if !drafts.contains(where: { $0.id == draft.id }) { drafts.append(draft) }
    }

    /// evidence 返回 hash 原图中 row 的证据裁切；不改变原始文件，无法读取返回 nil。
    func evidence(hash: String, row: StatementRow, document: OCRDocument) -> UIImage? {
        guard let image = UIImage(contentsOfFile: directory.appendingPathComponent(hash + ".image").path)?.cgImage else { return nil }
        let boxes = document.texts.filter { row.evidence.contains($0.id) }.map(\.box)
        guard let top = boxes.map(\.y).min(), let bottom = boxes.map({ $0.y + $0.height }).max() else { return UIImage(cgImage: image) }
        let margin = (bottom - top) * 0.3
        let y = max(0, top - margin), end = min(1, bottom + margin)
        guard let crop = image.cropping(to: CGRect(x: 0, y: y * Double(image.height), width: Double(image.width), height: (end - y) * Double(image.height))) else { return nil }
        return UIImage(cgImage: crop)
    }
}

/// StatementImportView 在现有 App 中提供选图、本地识别、逐笔编辑、预检与确认。
@MainActor struct StatementImportView: View {
    @EnvironmentObject private var settings: AppSettings
    @StateObject private var store = StatementDraftStore()
    @State private var selection: [PhotosPickerItem] = []
    @State private var platform: StatementPlatform = .cmb
    @State private var captureDate = ""
    @State private var busy = false
    @State private var notice = ""
    @State private var expandedRow: String?

    /// body 使预检和确认成为两个明确操作；任一编辑使旧提案失效，原图仅保留本机。
    var body: some View {
        NavigationStack {
            Form {
                Section("Local screenshot recognition") {
                    Picker("Statement", selection: $platform) { ForEach(StatementPlatform.allCases, id: \.self) { Text($0.rawValue).tag($0) } }
                    TextField("Capture date (YYYY-MM-DD; for relative dates)", text: $captureDate)
                    PhotosPicker("Choose screenshots", selection: $selection, maxSelectionCount: 20, matching: .images)
                    Text("Images stay on this device. Review every result before submitting.")
                    if busy { ProgressView() }
                    if !store.error.isEmpty { Text(store.error).foregroundStyle(.red) }
                    if !notice.isEmpty { Text(notice).textSelection(.enabled) }
                    NavigationLink("Pending review") { StatementReviewsView() }
                }
                ForEach($store.drafts) { $draft in
                    Section(draft.result.platform.rawValue + " · " + draft.result.document.imageHash.prefix(8)) {
                        Text(draft.result.issues.joined(separator: ", ")).foregroundStyle(.orange)
                        if draft.result.rows.isEmpty, let image = store.evidence(hash: draft.result.document.imageHash, row: StatementRow(id: "original"), document: draft.result.document) {
                            Image(uiImage: image).resizable().scaledToFit()
                        }
                        ForEach($draft.result.rows) { $row in
                            DisclosureGroup(row.merchant.isEmpty ? "Incomplete transaction" : row.merchant, isExpanded: expansionBinding(draft.id + row.id)) {
                                if expandedRow == draft.id + row.id, let image = store.evidence(hash: draft.result.document.imageHash, row: row, document: draft.result.document) {
                                    Image(uiImage: image).resizable().scaledToFit()
                                }
                                Text(row.issues.joined(separator: ", ")).foregroundStyle(.orange)
                                TextField("Date", text: $row.date)
                                TextField("Time", text: $row.time)
                                TextField("Amount", text: $row.amount)
                                Picker("Direction", selection: $row.direction) { Text("Unknown").tag(""); Text("Expense").tag("expense"); Text("Income").tag("income") }
                                TextField("Merchant", text: $row.merchant)
                                TextField("Description", text: $row.description)
                                TextField("Account", text: $row.account)
                            }
                        }
                        Toggle("I checked the image and edited fields", isOn: $draft.reviewed)
                        Button("Check incremental import") { Task { await preview(draft.id) } }.disabled(busy || !draft.reviewed || draft.result.rows.isEmpty || !settings.isConfigured)
                        if let proposal = draft.proposal {
                            Text("New candidates: \(proposal.preview.created) · Existing: \(proposal.preview.skipped) · Review: \(proposal.preview.duplicated) · Invalid: \(proposal.preview.failed)")
                            ForEach(proposal.preview.rows, id: \.index) { row in Text("\(row.index + 1): \(row.status) · \(row.reason)") }
                            Button("Confirm import; keep disputed rows pending") { Task { await confirm(draft.id) } }.disabled(busy)
                        }
                    }
                    .onChange(of: try? JSONEncoder().encode(draft.result.rows)) { _, _ in invalidate(draft.id, clearReview: true) }
                    .onChange(of: draft.reviewed) { _, _ in invalidate(draft.id) }
                }
            }
            .navigationTitle("Import statements")
            .disabled(busy)
            .onChange(of: selection) { _, _ in Task { await recognize() } }
        }
    }

    /// expansionBinding 为 id 创建互斥展开状态，避免长截图的数百行同时解码原图耗尽内存。
    private func expansionBinding(_ id: String) -> Binding<Bool> {
        Binding(get: { expandedRow == id }, set: { expandedRow = $0 ? id : nil })
    }

    /// invalidate 撤销 id 的旧预检；clearReview 在字段编辑后撤销人工勾选，下一次提交使用新幂等键。
    private func invalidate(_ id: String, clearReview: Bool = false) {
        guard let index = store.drafts.firstIndex(where: { $0.id == id }) else { return }
        store.drafts[index].proposal = nil
        store.drafts[index].key = UUID().uuidString
        if clearReview { store.drafts[index].reviewed = false }
    }

    /// recognize 顺序处理多张已选截图，在后台执行 Vision，任一失败仍保存已经成功的证据。
    @MainActor private func recognize() async {
        busy = true; defer { busy = false }
        do {
            for item in selection {
                guard let data = try await item.loadTransferable(type: Data.self) else { throw APIError.invalidResponse }
                let date = captureDate.isEmpty ? nil : captureDate
                let selectedPlatform = platform
                let result = try await Task.detached(priority: .userInitiated) {
                    let document = try LocalStatementOCR.recognize(data, captureDate: date)
                    return StatementParser().parse(document, platform: selectedPlatform)
                }.value
                try store.add(data: data, result: result)
            }
            notice = "Recognition finished. Check the evidence for every row."
        } catch { notice = error.localizedDescription }
    }

    /// preview 提交 id 对应当前编辑版本，只上传结构化字段和文字坐标；返回后保存提案 ID。
    @MainActor private func preview(_ id: String) async {
        busy = true; defer { busy = false }
        do {
            guard let index = store.drafts.firstIndex(where: { $0.id == id }) else { return }
            let draft = store.drafts[index]
            let request = try StatementSubmission.request(draft.result, rows: draft.result.rows, reviewed: draft.reviewed, key: draft.key)
            store.drafts[index].proposal = try await settings.makeClient().propose(request)
        } catch { notice = error.localizedDescription }
    }

    /// confirm 对 id 的持久提案显式确认；网络中断可复用 ID 重试，争议行留在服务端待核。
    @MainActor private func confirm(_ id: String) async {
        busy = true; defer { busy = false }
        do {
            guard let draft = store.drafts.first(where: { $0.id == id }), let proposal = draft.proposal else { return }
            let result = try await settings.makeClient().importCall(StatementImportResult.self, path: "proposals/\(proposal.id)/confirm", method: "POST")
            notice = "Created: \(result.created), existing: \(result.skipped), pending: \(result.duplicated + result.failed)"
        } catch { notice = error.localizedDescription }
    }
}

/// StatementReviewsView 从同一后端读取跨会话待核，并以明确理由提交人工决定。
@MainActor struct StatementReviewsView: View {
    @EnvironmentObject private var settings: AppSettings
    @State private var rows: [StatementReview] = []
    @State private var notice = ""
    @State private var offset = 0
    @State private var total = 0
    @State private var ranges: [StatementSourceRange] = []

    /// body 展示待核正文及证据，不将疑似重复默认删除。
    var body: some View {
        List {
            Text(notice)
            Section("Observed records; complete coverage unconfirmed") {
                ForEach(Array(ranges.enumerated()), id: \.offset) { _, range in
                    Text("\(range.source) · \(range.account) · \(range.count)\n\(range.first) – \(range.last)")
                }
            }
            ForEach(rows) { row in NavigationLink(row.kind + " · " + row.source) { StatementReviewEditor(review: row) } }
            if offset < total { Button("Load more") { Task { await load(reset: false) } } }
        }.navigationTitle("Pending review").task { await load(reset: true) }.refreshable { await load(reset: true) }
    }

    /// load 根据 reset 替换首页或追加下一页；失败只提示，不丢弃已加载记录。
    @MainActor private func load(reset: Bool) async {
        do {
            let start = reset ? 0 : offset
            let page = try await settings.makeClient().importCall(Page<StatementReview>.self, path: "reviews", query: [URLQueryItem(name: "offset", value: String(start)), URLQueryItem(name: "limit", value: "50")])
            rows = reset ? page.items : rows + page.items
            offset = start + page.items.count
            total = page.total
            ranges = try await settings.makeClient().importCall([StatementSourceRange].self, path: "ranges")
        } catch { notice = error.localizedDescription }
    }
}

/// StatementReviewEditor 提供受限的四种人工决定；更正和独立新增要求完整结构化记录。
@MainActor struct StatementReviewEditor: View {
    @EnvironmentObject private var settings: AppSettings
    let review: StatementReview
    @State private var action = "link"
    @State private var reason = ""
    @State private var target = ""
    @State private var record = ""
    @State private var notice = ""
    @State private var busy = false

    /// body 保留完整记录编辑器与明确提交，不在选择动作时自动写入。
    var body: some View {
        Form {
            Text(review.kind)
            Text(review.record.prettyText).font(.caption).textSelection(.enabled)
            Picker("Decision", selection: $action) { Text("Same transaction").tag("link"); Text("Separate transaction").tag("create"); Text("Correct history").tag("correct"); Text("Dismiss").tag("dismiss") }
            TextField("Reason", text: $reason)
            TextField("Transaction ID", text: $target)
            if action == "create" || action == "correct" { TextEditor(text: $record).frame(minHeight: 250) }
            Button("Confirm decision") { Task { await resolve() } }.disabled(busy || reason.isEmpty)
            Text(notice)
        }.navigationTitle("Review transaction").onAppear { target = review.transactionId ?? ""; record = review.record.prettyText }
    }

    /// resolve 校验 JSON 并提交当前人工选择；失败保留编辑内容，成功后仍保留服务端修改历史。
    @MainActor private func resolve() async {
        busy = true; defer { busy = false }
        do {
            var fields: [String: JSONValue] = ["action": .string(action), "reason": .string(reason)]
            if !target.isEmpty { fields["transaction_id"] = .string(target) }
            if action == "create" || action == "correct" { fields["record"] = try JSONDecoder().decode(JSONValue.self, from: Data(record.utf8)) }
            let _: JSONValue = try await settings.makeClient().importCall(JSONValue.self, path: "reviews/\(review.id)/resolve", method: "POST", body: JSONEncoder().encode(JSONValue.object(fields)))
            notice = "Decision saved."
        } catch { notice = error.localizedDescription }
    }
}
