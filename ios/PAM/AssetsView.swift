import SwiftUI

/// AssetsView 通过 REST 分页浏览资产，新增或编辑成功后重新读取服务器记录。
struct AssetsView: View {
    @EnvironmentObject private var settings: AppSettings
    @State private var records: [AssetRecord] = []
    @State private var total = 0
    @State private var loading = false
    @State private var failure: String?
    @State private var creating = false
    @State private var generation = UUID()

    /// body 以原生列表和表单完成资产管理，错误与空资产状态分别展示。
    var body: some View {
        NavigationStack {
            Group {
                if !settings.isConfigured { ConnectionRequiredView() }
                else if records.isEmpty && loading { ProgressView("Loading assets...") }
                else if records.isEmpty, let failure {
                    FailureView(message: failure) { Task { await load(reset: true) } }
                } else if records.isEmpty {
                    ContentUnavailableView("No assets yet", systemImage: "shippingbox", description: Text("Tap + to register your first asset."))
                } else {
                    List {
                        if let failure { Text(failure).foregroundStyle(.red) }
                        ForEach(records) { asset in
                            NavigationLink {
                                AssetDetailView(initial: asset) { Task { await load(reset: true) } }
                            } label: {
                                VStack(alignment: .leading, spacing: 4) {
                                    Text(asset.name).font(.headline)
                                    Text([asset.brand, asset.model, asset.category].filter { !$0.isEmpty }.joined(separator: " | "))
                                        .font(.subheadline).foregroundStyle(.secondary)
                                    Text(asset.status).font(.caption).foregroundStyle(.secondary)
                                }
                            }
                        }
                        if records.count < total {
                            Button { Task { await load(reset: false) } } label: {
                                HStack { Spacer(); if loading { ProgressView() } else { Text("Load more") }; Spacer() }
                            }.disabled(loading)
                        }
                    }.refreshable { await load(reset: true) }
                }
            }
            .navigationTitle("Assets")
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { creating = true } label: { Label("Add asset", systemImage: "plus") }.disabled(!settings.isConfigured)
                }
            }
            .sheet(isPresented: $creating) { AssetEditorView(existing: nil) { _ in Task { await load(reset: true) } } }
            .task { await load(reset: true) }
        }
    }

    /// load 在 reset 时创建新请求世代，否则追加一页；取消或过期响应不改变列表。
    @MainActor private func load(reset: Bool) async {
        guard settings.isConfigured, reset || !loading else { return }
        // 刷新期间保留已有行，避免正在查看详情时因导航源瞬间消失而被弹回列表。
        if reset { generation = UUID() }
        let requestGeneration = generation
        loading = true
        failure = nil
        defer { if generation == requestGeneration { loading = false } }
        do {
            let page = try await settings.makeClient().assets(offset: reset ? 0 : records.count)
            try Task.checkCancellation()
            guard generation == requestGeneration else { return }
            if reset { records = page.items } else { records.append(contentsOf: page.items) }
            total = page.total
        } catch is CancellationError { return }
        catch {
            guard generation == requestGeneration, (error as NSError).code != NSURLErrorCancelled else { return }
            failure = error.localizedDescription
        }
    }
}

/// AssetDetailView 展示实物资产和购买关联，写操作成功后通过 changed 通知列表刷新。
struct AssetDetailView: View {
    let initial: AssetRecord
    let changed: () -> Void
    @EnvironmentObject private var settings: AppSettings
    @Environment(\.dismiss) private var dismiss
    @State private var refreshed: AssetRecord?
    @State private var editing = false
    @State private var deleting = false
    @State private var busy = false
    @State private var failure: String?

    /// body 保持查看、编辑和删除操作独立；删除必须经过本机确认且服务端成功。
    var body: some View {
        let asset = refreshed ?? initial
        List {
            Section("Asset") {
                LabeledContent("Name", value: asset.name)
                LabeledContent("Category", value: asset.category)
                LabeledContent("Brand", value: asset.brand)
                LabeledContent("Model", value: asset.model)
                LabeledContent("Serial number", value: asset.serialNumber.isEmpty ? "Not provided" : asset.serialNumber)
                LabeledContent("Status", value: asset.status)
            }
            Section("Purchase") {
                LabeledContent("Date", value: asset.purchaseDate ?? "Not provided")
                LabeledContent("Price", value: asset.purchasePrice.map { "\($0) \(asset.currency)" } ?? "Not provided")
                LabeledContent("Merchant", value: asset.merchant.isEmpty ? "Not provided" : asset.merchant)
                if let transactionId = asset.relatedTransactionId {
                    Text("Related transaction: \(transactionId)").textSelection(.enabled)
                }
            }
            if !asset.notes.isEmpty { Section("Notes") { Text(asset.notes).textSelection(.enabled) } }
            if let failure { Section { Text(failure).foregroundStyle(.red); Button("Retry") { Task { await refresh() } } } }
            Section { Button("Delete asset", role: .destructive) { deleting = true }.disabled(busy) }
        }
        .navigationTitle(asset.name).navigationBarTitleDisplayMode(.inline)
        .toolbar { Button("Edit") { editing = true }.disabled(busy) }
        .sheet(isPresented: $editing) {
            AssetEditorView(existing: asset) { saved in refreshed = saved; changed() }
        }
        .confirmationDialog("Delete this asset?", isPresented: $deleting, titleVisibility: .visible) {
            Button("Delete asset", role: .destructive) { Task { await delete() } }
        } message: { Text("This removes the inventory record. Related transactions remain in your history.") }
        .task { await refresh() }
        .refreshable { await refresh() }
    }

    /// refresh 读取 initial.id 的当前资产；网络失败时保留已有详情。
    @MainActor private func refresh() async {
        do { refreshed = try await settings.makeClient().asset(id: initial.id); failure = nil }
        catch is CancellationError { return }
        catch { if (error as NSError).code != NSURLErrorCancelled { failure = error.localizedDescription } }
    }

    /// delete 等待服务端确认后刷新列表并退出详情，失败时保持当前界面以便重试。
    @MainActor private func delete() async {
        busy = true
        defer { busy = false }
        do { try await settings.makeClient().delete(id: initial.id); changed(); dismiss() }
        catch { failure = error.localizedDescription }
    }
}

/// AssetEditorView 保存一次完整资产草稿；最终有效性仍由服务器共享业务规则判断。
struct AssetEditorView: View {
    let existing: AssetRecord?
    let saved: (AssetRecord) -> Void
    @EnvironmentObject private var settings: AppSettings
    @Environment(\.dismiss) private var dismiss
    @State private var draft: AssetDraft
    @State private var usePurchaseDate: Bool
    @State private var purchaseDate: Date
    @State private var busy = false
    @State private var failure: String?

    /// init 从 existing 创建编辑副本；nil 表示新增，saved 仅在服务端确认后调用一次。
    init(existing: AssetRecord?, saved: @escaping (AssetRecord) -> Void) {
        self.existing = existing
        self.saved = saved
        _draft = State(initialValue: existing.map(AssetDraft.init(asset:)) ?? AssetDraft())
        _usePurchaseDate = State(initialValue: existing?.purchaseDate != nil)
        _purchaseDate = State(initialValue: existing?.purchaseDate.flatMap(Self.dateFormatter.date(from:)) ?? Date())
    }

    /// dateFormatter 只处理 API 的日历日期，不对购买日期附加时间或时区。
    private static var dateFormatter: DateFormatter {
        let formatter = DateFormatter()
        formatter.calendar = Calendar(identifier: .gregorian)
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "yyyy-MM-dd"
        return formatter
    }

    /// body 将可选购买字段与必要字段分组，保存中禁止重复提交或中途关闭。
    var body: some View {
        NavigationStack {
            Form {
                Section("Asset") {
                    TextField("Name (required)", text: $draft.name)
                    TextField("Category", text: $draft.category)
                    TextField("Brand", text: $draft.brand)
                    TextField("Model", text: $draft.model)
                    TextField("Serial number", text: $draft.serialNumber).textInputAutocapitalization(.never)
                    Picker("Status", selection: $draft.status) {
                        ForEach(["active", "stored", "loaned", "sold", "retired", "lost"], id: \.self) { Text($0.capitalized).tag($0) }
                    }
                }
                Section("Purchase") {
                    Toggle("Add purchase date", isOn: $usePurchaseDate)
                    if usePurchaseDate { DatePicker("Date", selection: $purchaseDate, displayedComponents: .date) }
                    TextField("Price (optional)", text: optional(\.purchasePrice)).keyboardType(.decimalPad)
                    TextField("Currency", text: $draft.currency).textInputAutocapitalization(.characters).autocorrectionDisabled()
                    TextField("Merchant", text: $draft.merchant)
                    TextField("Related transaction ID (optional)", text: optional(\.relatedTransactionId))
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                }
                Section("Notes") { TextEditor(text: $draft.notes).frame(minHeight: 100) }
                if let failure { Section { Text(failure).foregroundStyle(.red) } }
            }
            .disabled(busy)
            .navigationTitle(existing == nil ? "New asset" : "Edit asset")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.disabled(busy) }
                ToolbarItem(placement: .confirmationAction) {
                    Button { Task { await save() } } label: { if busy { ProgressView() } else { Text("Save") } }
                        .disabled(busy || draft.name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                }
            }
            .interactiveDismissDisabled(busy)
        }
    }

    /// optional 为 draft 的可选文本 path 建立绑定；清空文本时写回 nil，避免伪造值。
    private func optional(_ path: WritableKeyPath<AssetDraft, String?>) -> Binding<String> {
        Binding(get: { draft[keyPath: path] ?? "" }, set: { draft[keyPath: path] = $0.isEmpty ? nil : $0 })
    }

    /// save 校验金额文本与关联 ID 后提交快照；失败保留草稿，成功调用 saved 并关闭。
    @MainActor private func save() async {
        busy = true
        failure = nil
        defer { busy = false }
        do {
            draft.name = draft.name.trimmingCharacters(in: .whitespacesAndNewlines)
            draft.currency = draft.currency.uppercased().trimmingCharacters(in: .whitespacesAndNewlines)
            draft.purchaseDate = usePurchaseDate ? Self.dateFormatter.string(from: purchaseDate) : nil
            // 只接受十进制文本；不经 Double，避免界面校验改变精确金额。
            if let price = draft.purchasePrice {
                guard price.range(of: "^[0-9]{1,16}(\\.[0-9]{1,4})?$", options: .regularExpression) != nil,
                      Decimal(string: price, locale: Locale(identifier: "en_US_POSIX")) != nil else {
                    throw APIError.configuration("Enter a non-negative price using a decimal point.")
                }
            }
            if let id = draft.relatedTransactionId, UUID(uuidString: id) == nil {
                throw APIError.configuration("The related transaction ID must be a UUID.")
            }
            let asset = try await settings.makeClient().save(draft: draft, id: existing?.id)
            saved(asset)
            dismiss()
        } catch { failure = error.localizedDescription }
    }
}
