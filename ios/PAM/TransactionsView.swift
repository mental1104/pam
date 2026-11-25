import SwiftUI

/// TransactionsView 按筛选条件浏览流水；只加载分页，不在客户端汇总或推断购买商品。
struct TransactionsView: View {
    @EnvironmentObject private var settings: AppSettings
    @State private var filter = TransactionFilter()
    @State private var records: [TransactionRecord] = []
    @State private var total = 0
    @State private var loading = false
    @State private var failure: String?
    @State private var showFilter = false
    @State private var generation = UUID()

    /// body 显示列表、搜索、日期筛选及分页按钮；配置变化由父视图重建状态。
    var body: some View {
        NavigationStack {
            Group {
                if !settings.isConfigured { ConnectionRequiredView() }
                else if records.isEmpty && loading { ProgressView("Loading transactions...") }
                else if records.isEmpty, let failure {
                    FailureView(message: failure) { Task { await load(reset: true) } }
                } else if records.isEmpty {
                    ContentUnavailableView.search(text: filter.search)
                } else {
                    List {
                        if let failure { Text(failure).foregroundStyle(.red) }
                        ForEach(records) { record in
                            NavigationLink { TransactionDetailView(initial: record) } label: {
                                TransactionRow(record: record)
                            }
                        }
                        if records.count < total {
                            Button { Task { await load(reset: false) } } label: {
                                HStack { Spacer(); if loading { ProgressView() } else { Text("Load more") }; Spacer() }
                            }.disabled(loading)
                        }
                    }
                    .refreshable { await load(reset: true) }
                }
            }
            .navigationTitle("Transactions")
            .searchable(text: $filter.search, prompt: "Merchant or description")
            .toolbar {
                ToolbarItem(placement: .topBarTrailing) {
                    Button { showFilter = true } label: { Label("Filter", systemImage: filter.dateEnabled || !filter.direction.isEmpty ? "line.3.horizontal.decrease.circle.fill" : "line.3.horizontal.decrease.circle") }
                }
            }
            .sheet(isPresented: $showFilter) { TransactionFilterView(filter: $filter) }
            .task(id: filter) {
                // 搜索变化会取消上一任务；短暂等待合并连续键入，避免每个按键都请求服务器。
                do { try await Task.sleep(for: .milliseconds(250)); await load(reset: true) }
                catch { /* 已取消的搜索不替换当前结果。 */ }
            }
        }
    }

    /// load 根据 reset 重置列表或读取下一页；过期请求不能覆盖较新的查询结果。
    /// 页面任务取消会传给 URLSession，失败保留已加载记录供继续查看。
    @MainActor private func load(reset: Bool) async {
        guard settings.isConfigured, reset || !loading else { return }
        if reset { generation = UUID(); records = []; total = 0 }
        let requestGeneration = generation
        loading = true
        failure = nil
        defer { if generation == requestGeneration { loading = false } }
        do {
            let page = try await settings.makeClient().transactions(filter: filter, offset: records.count)
            try Task.checkCancellation()
            guard generation == requestGeneration else { return }
            records.append(contentsOf: page.items)
            total = page.total
        } catch is CancellationError { return }
        catch {
            guard generation == requestGeneration, (error as NSError).code != NSURLErrorCancelled else { return }
            failure = error.localizedDescription
        }
    }
}

/// TransactionRow 展示商户、分类、时间及原始精度金额，不进行汇率换算。
struct TransactionRow: View {
    let record: TransactionRecord

    /// body 将方向作为正负视觉标记，金额正文沿用后端字符串。
    var body: some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 5) {
                Text(record.merchant.isEmpty ? "Unknown merchant" : record.merchant).font(.headline)
                Text(record.description).font(.subheadline).lineLimit(2)
                Text([DisplayFormat.date(record.occurredAt, precision: record.timestampPrecision, sourceDate: record.sourceCalendarDate), record.category ?? ""].filter { !$0.isEmpty }.joined(separator: " | "))
                    .font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Text("\(record.direction == "income" ? "+" : "-")\(record.amount) \(record.currency)")
                .font(.subheadline.monospacedDigit()).foregroundStyle(record.direction == "income" ? Color.green : Color.primary)
                .multilineTextAlignment(.trailing)
        }.padding(.vertical, 3)
    }
}

/// TransactionFilterView 使用独立草稿，只有点 Apply 才会变更父列表条件。
struct TransactionFilterView: View {
    @Binding var filter: TransactionFilter
    @Environment(\.dismiss) private var dismiss
    @State private var draft: TransactionFilter

    /// init 用 filter 当前值建立草稿，取消关闭时不会提交修改。
    init(filter: Binding<TransactionFilter>) {
        _filter = filter
        _draft = State(initialValue: filter.wrappedValue)
    }

    /// body 提供方向和自然日区间；禁止提交结束日早于开始日的条件。
    var body: some View {
        NavigationStack {
            Form {
                Picker("Direction", selection: $draft.direction) {
                    Text("All").tag(""); Text("Income").tag("income"); Text("Expense").tag("expense")
                }
                Section("Date range") {
                    Toggle("Use date range", isOn: $draft.dateEnabled)
                    if draft.dateEnabled {
                        DatePicker("From", selection: $draft.from, displayedComponents: .date)
                        DatePicker("Through", selection: $draft.through, in: draft.from..., displayedComponents: .date)
                    }
                }
            }
            .navigationTitle("Filter")
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Apply") { filter = draft; dismiss() }.disabled(draft.dateEnabled && draft.through < draft.from)
                }
            }
        }
    }
}

/// TransactionDetailView 展示不可编辑流水、导入来源与原始记录，保留证据边界。
struct TransactionDetailView: View {
    let initial: TransactionRecord
    @EnvironmentObject private var settings: AppSettings
    @State private var refreshed: TransactionRecord?
    @State private var failure: String?

    /// body 先显示列表快照，再从详情接口补齐原始数据；刷新失败保留已有事实。
    var body: some View {
        let record = refreshed ?? initial
        List {
            Section("Transaction") {
                LabeledContent("Merchant", value: record.merchant)
                LabeledContent("Amount", value: "\(record.amount) \(record.currency)")
                LabeledContent("Direction", value: record.direction)
                LabeledContent("Date", value: DisplayFormat.date(record.occurredAt, precision: record.timestampPrecision, sourceDate: record.sourceCalendarDate))
                LabeledContent("Category", value: record.category ?? "Uncategorized")
                Text(record.description).textSelection(.enabled)
            }
            Section("Traceability") {
                LabeledContent("Source", value: record.source)
                LabeledContent("External ID", value: record.externalId ?? "Not provided")
                Text("Transaction: \(record.id)").textSelection(.enabled)
                Text("Import batch: \(record.importBatchId)").textSelection(.enabled)
            }
            if let original = record.original {
                Section("Original source record") { Text(original.prettyText).font(.system(.caption, design: .monospaced)).textSelection(.enabled) }
            }
            Section { Text("A payment record alone does not confirm the purchased product.").font(.footnote).foregroundStyle(.secondary) }
            if let failure { Section { Text(failure).foregroundStyle(.red); Button("Retry") { Task { await refresh() } } } }
        }
        .navigationTitle("Transaction").navigationBarTitleDisplayMode(.inline)
        .task { await refresh() }
        .refreshable { await refresh() }
    }

    /// refresh 读取 initial.id 的详情；取消忽略，其他错误只更新错误状态。
    @MainActor private func refresh() async {
        do { refreshed = try await settings.makeClient().transaction(id: initial.id); failure = nil }
        catch is CancellationError { return }
        catch { if (error as NSError).code != NSURLErrorCancelled { failure = error.localizedDescription } }
    }
}
