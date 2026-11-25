// Package mcpserver 将应用服务发布为官方 Go SDK 的 Streamable HTTP MCP 工具。
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/mental1104/pam/internal/app"
	"github.com/mental1104/pam/internal/domain"
	"github.com/mental1104/pam/internal/query"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// New 将 application 和 queries 连接到同一个 MCP Server；返回的 handler 必须由调用者包装认证。
// 使用无状态 Streamable HTTP，避免反向代理或重启丢失隐式会话；写入仅经导入和人工待核业务入口。
func New(application *app.Service, queries *query.Service) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "pam", Version: "0.1.0"}, nil)
	registerReviewTools(server, application)
	querySchema := map[string]any{"type": "object", "properties": map[string]any{"sql": map[string]any{"type": "string"}, "parameters": map[string]any{"type": "array", "items": map[string]any{"type": []string{"string", "number", "boolean", "null"}}}}, "required": []string{"sql"}, "additionalProperties": false}
	// 保留原始 JSON 数字到应用边界；SDK 的通用 any 解码不能替代金额精度约束。
	// 同一份 schema 也放进工具发现信息，让仅暴露 tools 的客户端仍能准确理解列和关系。
	server.AddTool(&mcp.Tool{Name: "query_readonly_sql", Description: "Execute one read-only PostgreSQL SELECT over pam tables. Monetary results are decimal strings. Payment descriptions are evidence of payment, not proof of a product. Original SQL and bound parameters are retained under the owner-authorized audit policy; application logs exclude their values. Schema is also available as pam://schema.\n\n" + schema, InputSchema: querySchema, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input query.Request
		if err := decode(r.Params.Arguments, &input); err != nil {
			input = query.Request{}
		}
		result, err := queries.Execute(ctx, input)
		if err != nil {
			return toolError(err), nil
		}
		// 查询内容只进入一次 structuredContent；文本仅提供摘要，避免翻倍突破响应字节上限。
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("rows=%d truncated=%t", result.RowCount, result.Truncated)}}, StructuredContent: result}, nil
	})
	// 导入使用固定契约和业务验证，不暴露任意 INSERT/UPDATE/DELETE。
	server.AddTool(&mcp.Tool{Name: "import_transactions", Description: "Import normalized transactions with original source objects. Requires source, stable idempotency_key, records. Use decimal strings; ambiguous fingerprints require explicit confirm_new. Replays return original batch statistics.", InputSchema: importSchema()}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input domain.ImportRequest
		if err := decode(r.Params.Arguments, &input); err != nil {
			return toolError(fmt.Errorf("invalid import JSON")), nil
		}
		result, err := application.Import(ctx, input)
		if err != nil {
			return toolError(err), nil
		}
		return toolResult(result)
	})
	// 模式资源说明列、关系和口径，不需要运行信息架构查询。
	server.AddResource(&mcp.Resource{URI: "pam://schema", Name: "PAM V0 schema", MIMEType: "text/plain", Description: "Tables, columns, relations, monetary semantics, import and query limits"}, func(ctx context.Context, r *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "pam://schema", MIMEType: "text/plain", Text: schema}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return boundRequestID(handler)
}

// boundRequestID 在 SDK 前限制 JSON-RPC id 的编码长度，避免大 id 随查询响应回显而突破传输预算。
// next 仍负责全部 MCP 语义校验；POST 正文最多 8MiB，兼容原有批量导入限额，不记录正文或凭据。
func boundRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		defer r.Body.Close()
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
		if err != nil {
			http.Error(w, "MCP request body is too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		var envelope struct {
			ID json.RawMessage `json:"id"`
		}
		// 无效 JSON 交回官方 SDK 处理；这里只检查能安全读取的请求标识，不解释业务参数。
		if json.Unmarshal(data, &envelope) == nil && len(envelope.ID) > 256 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Request id exceeds 256 encoded bytes"}}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		next.ServeHTTP(w, r)
	})
}

// decode 将 data 解码为 out，保留十进制参数并拒绝未知字段/多值；失败不回显输入正文。
func decode(data json.RawMessage, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

// toolResult 把 value 编码为文本及结构化结果；编码失败作为协议处理错误返回。
func toolResult(value any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}, StructuredContent: value}, nil
}

// toolError 只公开应用层已脱敏的 err 文本；返回 MCP 工具失败而非成功业务内容。
func toolError(err error) *mcp.CallToolResult {
	message := "operation failed"
	var validation domain.ValidationError
	var sqlError *query.Error
	switch {
	case errors.As(err, &validation):
		message = validation.Message
	case errors.As(err, &sqlError):
		message = sqlError.Error()
	case errors.Is(err, domain.ErrConflict):
		message = "record conflict"
	case errors.Is(err, domain.ErrNotFound):
		message = "record not found"
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: message}}}
}

// importSchema 返回导入工具的发现契约；source/records 详细约束由共享应用服务统一执行。
func importSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"source": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string"}, "source_file_hash": map[string]any{"type": "string"}, "records": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"row_key": map[string]any{"type": "string"}, "occurred_at": map[string]any{"type": "string"}, "direction": map[string]any{"type": "string", "enum": []string{"income", "expense"}}, "amount": map[string]any{"type": "string"}, "currency": map[string]any{"type": "string"}, "merchant": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}, "category": map[string]any{"type": "string"}, "external_id": map[string]any{"type": "string"}, "original": map[string]any{"type": "object"}, "confirm_new": map[string]any{"type": "boolean"}}, "required": []string{"occurred_at", "direction", "amount", "currency", "original"}, "additionalProperties": false}}}, "required": []string{"source", "idempotency_key", "records"}, "additionalProperties": false}
}

// schema 是随应用发布的发现文档；实际列与约束由版本化 migrations 维护。
const schema = `PAM V0 PostgreSQL schema pam. All timestamps are timestamptz; dates are calendar dates. Amounts are nonnegative numeric(20,4) and JSON decimal strings; direction is income/expense. Never sum currencies together without grouping. No balance or double-entry semantics.
import_proposals: id UUID PK, source, idempotency_key, payload_hash, request JSONB, preview JSONB, committed boolean, created_at. Preview counts are candidates, not ledger writes.
import_reviews: id UUID PK, record_id FK import_records.id, kind duplicate/correction/parse_error, state pending/resolved, resolution JSONB, created_at, resolved_at. Pending items survive restart.
transaction_revisions: id UUID PK, transaction_id FK transactions.id, review_id FK import_reviews.id, before_content JSONB, after_content JSONB, reason, evidence JSONB, created_at. Human decisions retain original evidence and before/after content.
transactions: id UUID PK, occurred_at, direction, amount, currency, merchant, description, category, source, external_id nullable, import_batch_id FK import_batches.id, fingerprint, created_at, updated_at. Merchant/description support candidate search; a payment alone does not prove a specific product. Original data is in import_records, not duplicated into normalized fields.
assets: id UUID PK, name, category, brand, model, serial_number, purchase_date nullable, purchase_price nullable, currency, merchant, related_transaction_id nullable FK transactions.id, notes, status (active/stored/loaned/sold/retired/lost), created_at, updated_at. No depreciation or valuation.
import_batches: id UUID PK, source, idempotency_key, source_file_hash nullable SHA256, payload_hash, result JSONB, created_at. UNIQUE(source,idempotency_key). Same key with changed content conflicts.
import_records: id UUID PK, batch_id FK import_batches.id, row_index zero-based, row_key nullable, original JSONB, normalized_submission JSONB, transaction_id nullable FK transactions.id, status (created/skipped/duplicated/failed), reason, created_at. Retains every input including skipped/rejected rows. Join transaction_id for provenance.
query_audit: id UUID PK, fingerprint, normalized_sql (literals redacted), executed_at, duration_ms, row_count, status, error_code nullable, source. Failed calls are retained. raw_sql nullable stores owner-authorized originals; parameters nullable JSONB stores bound values; raw_truncated flags capture limits. Legacy rows have no original SQL or parameters. Database error messages are never saved.
query_patterns: aggregate query behavior by fingerprint, normalized_sql; see column names in SELECT * FROM pam.query_patterns LIMIT 1. Aggregates frequency, timing and failures.
query_readonly_sql accepts one SELECT, CTE/subquery/joins/aggregates/window expressions, $1 parameter placeholders and string/number/bool/null parameters. Only pam relations and documented safe built-ins; no catalog scans, user-defined functions, writes, DDL, SELECT INTO or locks. Limits: 3 seconds, 500 rows, 256 KiB result, 32 KiB SQL. Results provide columns, rows, row_count and truncated. Truncation means results are incomplete; use filtered aggregate queries for totals.
import_transactions requires source, idempotency_key, records. Each record includes original JSON object and precise amount string. Stable external_id wins; source_file_hash + row_key protects exact same file rows. Cross-file fingerprint matches are duplicated/conflict candidates, never silently removed; confirm_new explicitly permits a legitimate repeated purchase. Split large imports into <=1000 records with distinct stable batch keys. Do not import merged ledgers plus their source ledgers as separate expenses. Preserve original classification flags for analysis; expense totals may include transfers/refunds unless filtered to the source's statistical policy. Adapt heterogeneous CSV outside the backend using Agent interpretation.
`
