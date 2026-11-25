// Package acceptance_test 仅通过公开 HTTP/MCP 协议验收运行中的 PAM，不链接内部业务实现。
package acceptance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// client 持有独立于服务容器的 HTTP 客户端；token 只进入认证头，绝不写入测试日志。
type client struct {
	baseURL string
	token   string
	http    *http.Client
	rpcID   int
	session string
}

// importRow 描述 Agent 提交的一行规范化记录及原始来源；金额使用字符串。
type importRow struct {
	OccurredAt  string         `json:"occurred_at"`
	Direction   string         `json:"direction"`
	Amount      string         `json:"amount"`
	Currency    string         `json:"currency"`
	Merchant    string         `json:"merchant"`
	Description string         `json:"description"`
	Category    string         `json:"category"`
	ExternalID  string         `json:"external_id,omitempty"`
	Original    map[string]any `json:"original"`
	ConfirmNew  bool           `json:"confirm_new,omitempty"`
}

// importRequest 是公开导入契约的独立客户端 DTO，不借用服务端域模型。
type importRequest struct {
	Source  string      `json:"source"`
	Key     string      `json:"idempotency_key"`
	Records []importRow `json:"records"`
}

// importResult 保存公开逐行统计与关联标识，用于验证真实持久化结果。
type importResult struct {
	BatchID    string `json:"batch_id"`
	Created    int    `json:"created"`
	Skipped    int    `json:"skipped"`
	Duplicated int    `json:"duplicated"`
	Failed     int    `json:"failed"`
	Replayed   bool   `json:"replayed"`
	Rows       []struct {
		Status        string  `json:"status"`
		TransactionID *string `json:"transaction_id"`
	} `json:"rows"`
}

// transaction 保存 REST 验收所需的流水事实；原始 JSON 必须能由详情再次读取。
type transaction struct {
	ID            string                     `json:"id"`
	Amount        string                     `json:"amount"`
	Merchant      string                     `json:"merchant"`
	Description   string                     `json:"description"`
	ImportBatchID string                     `json:"import_batch_id"`
	Original      map[string]json.RawMessage `json:"original"`
}

// transactionPage 表示独立 REST 分页响应；总数只针对本轮唯一 source 断言。
type transactionPage struct {
	Items  []transaction `json:"items"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// assetInput 是库存公开写入契约；不允许客户端生成 ID 或修改服务端时间。
type assetInput struct {
	Name                 string  `json:"name"`
	Category             string  `json:"category"`
	Brand                string  `json:"brand"`
	Model                string  `json:"model"`
	SerialNumber         string  `json:"serial_number"`
	PurchaseDate         *string `json:"purchase_date"`
	PurchasePrice        *string `json:"purchase_price"`
	Currency             string  `json:"currency"`
	Merchant             string  `json:"merchant"`
	RelatedTransactionID *string `json:"related_transaction_id"`
	Notes                string  `json:"notes"`
	Status               string  `json:"status"`
}

// assetResult 在业务输入外保存服务端分配的库存标识。
type assetResult struct {
	assetInput
	ID string `json:"id"`
}

// rpcResponse 保存 JSON-RPC 结果或协议错误；业务工具拒绝由 toolResult.IsError 表达。
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// toolResult 接收 Streamable HTTP 工具响应，同时保留文本及结构化业务内容。
type toolResult struct {
	IsError    bool            `json:"isError"`
	Structured json.RawMessage `json:"structuredContent"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// queryResult 保存 SQL 的列和行；值按 RawMessage 读取，防止测试客户端自己损失精度。
type queryResult struct {
	Columns   []string            `json:"columns"`
	Rows      [][]json.RawMessage `json:"rows"`
	RowCount  int                 `json:"row_count"`
	Truncated bool                `json:"truncated"`
}

// newClient 从明确环境配置创建验收客户端；t 缺少任一变量时跳过，不猜测现网地址。
func newClient(t *testing.T) *client {
	t.Helper()
	base, token := os.Getenv("PAM_ACCEPTANCE_URL"), os.Getenv("PAM_ACCESS_TOKEN")
	if base == "" || token == "" {
		t.Skip("PAM_ACCEPTANCE_URL and PAM_ACCESS_TOKEN are required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("invalid acceptance URL")
	}
	return &client{baseURL: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
}

// request 通过公开 method/path 发送合成 payload，期望 expected 状态；authenticated 控制是否加认证头。
// t 日志保留请求及响应正文供复查，只用于合成资料，不打印 token 或请求头。
func (c *client) request(t *testing.T, method, path string, payload any, authenticated bool, expected int) []byte {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if path == "/mcp" {
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		if c.session != "" {
			req.Header.Set("Mcp-Session-Id", c.session)
		}
	}
	t.Logf("REQUEST %s %s\n%s", method, path, body)
	response, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	if session := response.Header.Get("Mcp-Session-Id"); session != "" {
		c.session = session
	}
	t.Logf("RESPONSE HTTP %d\n%s", response.StatusCode, data)
	if response.StatusCode != expected {
		t.Fatalf("%s %s: expected HTTP %d, received %d", method, path, expected, response.StatusCode)
	}
	return data
}

// decoded 将 data 解码为独立客户端类型 T；t 失败时终止当前场景，避免继续使用零值。
func decoded[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("invalid protocol JSON: %v", err)
	}
	return value
}

// rpc 调用 method 对应 MCP 方法，params 为协议参数；返回结果对象，协议错误直接使 t 失败。
func (c *client) rpc(t *testing.T, method string, params any) json.RawMessage {
	t.Helper()
	c.rpcID++
	response := c.request(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": c.rpcID, "method": method, "params": params}, true, 200)
	// 服务器配置 JSONResponse；仍兼容标准 SSE data 帧，便于验证反向代理部署。
	if bytes.HasPrefix(bytes.TrimSpace(response), []byte("event:")) || bytes.HasPrefix(bytes.TrimSpace(response), []byte("data:")) {
		for _, line := range bytes.Split(response, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				response = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				break
			}
		}
	}
	envelope := decoded[rpcResponse](t, response)
	if envelope.Error != nil {
		t.Fatalf("MCP protocol error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Result
}

// call 调用 name 业务工具，arguments 为公开 JSON 合约；返回含明确 IsError 的工具结果。
func (c *client) call(t *testing.T, name string, arguments any) toolResult {
	t.Helper()
	return decoded[toolResult](t, c.rpc(t, "tools/call", map[string]any{"name": name, "arguments": arguments}))
}

// content 提取 tool 的结构化正文；未提供结构化正文时使用标准文本 JSON，缺失则使 t 失败。
func content(t *testing.T, tool toolResult) json.RawMessage {
	t.Helper()
	if len(tool.Structured) > 0 {
		return tool.Structured
	}
	for _, item := range tool.Content {
		if item.Type == "text" {
			return json.RawMessage(item.Text)
		}
	}
	t.Fatal("MCP tool omitted business content")
	return nil
}

// query 调用只读 SQL 工具；fail 为 true 时必须明确拒绝，否则解码精度安全的查询结果。
func (c *client) query(t *testing.T, sql string, parameters []any, fail bool) queryResult {
	t.Helper()
	if parameters == nil {
		parameters = []any{}
	}
	result := c.call(t, "query_readonly_sql", map[string]any{"sql": sql, "parameters": parameters})
	if result.IsError != fail {
		t.Fatalf("SQL expected error=%t, received error=%t", fail, result.IsError)
	}
	if fail {
		return queryResult{}
	}
	return decoded[queryResult](t, content(t, result))
}

// text 将一格 cell 的字符串或数值转换为比较文本；不使用浮点数解析金额。
func text(t *testing.T, cell json.RawMessage) string {
	t.Helper()
	var value string
	if json.Unmarshal(cell, &value) == nil {
		return value
	}
	return string(cell)
}

// uniqueSource 创建带 UTC 时间和随机字节的合成来源；t 用于报告系统随机源故障。
func uniqueSource(t *testing.T) string {
	t.Helper()
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	return "pam-acceptance-" + time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(suffix[:])
}

// TestPublicRESTAndMCP 从独立客户端证明导入→持久化→SQL分析→审计和库存 CRUD 的完整公开协议闭环。
// t 仅写带唯一 source 的合成流水，并在清理时只删除自己创建的资产；合成流水由部署者另行归档清理。
func TestPublicRESTAndMCP(t *testing.T) {
	c := newClient(t)
	source := uniqueSource(t)
	started := time.Now().UTC().Format(time.RFC3339Nano)
	t.Logf("SYNTHETIC_SOURCE %s", source)
	c.request(t, "GET", "/health/ready", nil, false, 200)
	c.request(t, "GET", "/api/v1/transactions", nil, false, 401)
	c.request(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}, false, 401)
	rows := []importRow{
		{OccurredAt: "2025-05-10T12:30:00+08:00", Direction: "expense", Amount: "999.00", Currency: "CNY", Merchant: "Synthetic Mall camera shop", Description: "Camera accessories; Synthetic Gadget product unconfirmed (synthetic fixture)", Category: "electronics", ExternalID: source + "-gadget", Original: map[string]any{"fixture": source, "merchant_raw": "Synthetic Mall camera shop", "amount_text": "999.00", "product_confirmed": false}},
		{OccurredAt: "2025-05-11T13:30:00+08:00", Direction: "income", Amount: "9007199254740993.0001", Currency: "CNY", Merchant: "Precision fixture", Description: "Synthetic exact decimal boundary", Category: "test", Original: map[string]any{"fixture": source, "amount_text": "9007199254740993.0001"}},
	}
	input := importRequest{Source: source, Key: "initial", Records: rows}
	first := decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/transactions", input, true, 200))
	if first.Created != 2 || first.Failed != 0 || len(first.Rows) != 2 || first.Rows[0].TransactionID == nil || first.Rows[1].TransactionID == nil {
		t.Fatalf("initial import did not create both exact records: %+v", first)
	}
	transactionID := *first.Rows[0].TransactionID
	precisionID := *first.Rows[1].TransactionID

	t.Run("ImportIdempotencyAndProvenance", func(t *testing.T) {
		replay := decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/transactions", input, true, 200))
		if !replay.Replayed || replay.Created != 0 || replay.Skipped != 2 || replay.BatchID != first.BatchID {
			t.Fatalf("unsafe replay statistics: %+v", replay)
		}
		changed := input
		changed.Records = append([]importRow(nil), rows...)
		changed.Records[0].Amount = "1000"
		c.request(t, "POST", "/api/v1/imports/transactions", changed, true, 409)
		external := importRequest{Source: source, Key: "external-duplicate", Records: []importRow{rows[0]}}
		duplicate := decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/transactions", external, true, 200))
		if duplicate.Skipped != 1 || duplicate.Created != 0 {
			t.Fatalf("external ID duplicate was not skipped: %+v", duplicate)
		}
		ambiguous := rows[0]
		ambiguous.ExternalID = ""
		candidate := importRequest{Source: source, Key: "fingerprint-candidate", Records: []importRow{ambiguous}}
		conflict := decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/transactions", candidate, true, 200))
		if conflict.Duplicated != 1 || conflict.Created != 0 {
			t.Fatalf("fingerprint candidate was silently discarded or duplicated: %+v", conflict)
		}
		ambiguous.ConfirmNew = true
		candidate.Key = "confirmed-repeat"
		candidate.Records = []importRow{ambiguous}
		confirmed := decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/transactions", candidate, true, 200))
		if confirmed.Created != 1 {
			t.Fatalf("explicit repeated purchase was lost: %+v", confirmed)
		}
		detail := decoded[transaction](t, c.request(t, "GET", "/api/v1/transactions/"+transactionID, nil, true, 200))
		if detail.ImportBatchID != first.BatchID || text(t, detail.Original["fixture"]) != source || string(detail.Original["product_confirmed"]) != "false" {
			t.Fatalf("original source traceability was lost: %+v", detail)
		}
		precise := decoded[transaction](t, c.request(t, "GET", "/api/v1/transactions/"+precisionID, nil, true, 200))
		if precise.Amount != "9007199254740993.0001" {
			t.Fatalf("money lost decimal precision: %s", precise.Amount)
		}
	})

	t.Run("RESTFiltersAndPagination", func(t *testing.T) {
		params := url.Values{"source": {source}, "search": {"Synthetic Mall"}, "direction": {"expense"}, "category": {"electronics"}, "min_amount": {"999"}, "max_amount": {"999"}, "from": {"2025-01-01T00:00:00Z"}, "to": {"2026-01-01T00:00:00Z"}, "limit": {"1"}, "offset": {"0"}}
		page := decoded[transactionPage](t, c.request(t, "GET", "/api/v1/transactions?"+params.Encode(), nil, true, 200))
		if page.Total != 2 || len(page.Items) != 1 || page.Limit != 1 || page.Offset != 0 {
			t.Fatalf("combined filters/pagination failed: %+v", page)
		}
		params.Set("offset", "1")
		next := decoded[transactionPage](t, c.request(t, "GET", "/api/v1/transactions?"+params.Encode(), nil, true, 200))
		if len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
			t.Fatal("pagination returned duplicate/missing row")
		}
		params.Set("offset", "2")
		empty := decoded[transactionPage](t, c.request(t, "GET", "/api/v1/transactions?"+params.Encode(), nil, true, 200))
		if len(empty.Items) != 0 || empty.Total != 2 {
			t.Fatal("empty page lost filtered total")
		}
		c.request(t, "GET", "/api/v1/transactions?limit=0", nil, true, 400)
	})

	// 使用真正的 Streamable HTTP 初始化与发现流程，不直接调用 MCP handler。
	c.rpc(t, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "pam-blackbox-acceptance", "version": "0.1.0"}})
	c.request(t, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, true, 202)
	t.Run("MCPDiscoveryAndImport", func(t *testing.T) {
		tools := string(c.rpc(t, "tools/list", map[string]any{}))
		for _, name := range []string{"query_readonly_sql", "import_transactions"} {
			if !strings.Contains(tools, name) {
				t.Fatalf("missing MCP tool %s", name)
			}
		}
		resources := string(c.rpc(t, "resources/list", map[string]any{}))
		if !strings.Contains(resources, "pam://schema") {
			t.Fatal("schema discovery missing")
		}
		schema := string(c.rpc(t, "resources/read", map[string]any{"uri": "pam://schema"}))
		for _, name := range []string{"transactions", "assets", "query_audit", "import_records"} {
			if !strings.Contains(schema, name) {
				t.Fatalf("schema omitted %s", name)
			}
		}
		invalid := rows[0]
		invalid.Amount = "-1"
		invalid.ExternalID = ""
		result := c.call(t, "import_transactions", importRequest{Source: source, Key: "invalid-source-retained", Records: []importRow{invalid}})
		if result.IsError {
			t.Fatal("batch validation failure should return explicit row statistics")
		}
		stats := decoded[importResult](t, content(t, result))
		if stats.Failed != 1 || stats.Created != 0 {
			t.Fatalf("invalid import statistics: %+v", stats)
		}
		trace := c.query(t, `SELECT r.status, r.original FROM pam.import_records r JOIN pam.import_batches b ON r.batch_id=b.id WHERE b.source=$1 ORDER BY r.created_at`, []any{source}, false)
		if trace.RowCount < 6 {
			t.Fatalf("expected retained created/skipped/conflicting/failed source records, got %d", trace.RowCount)
		}
	})

	t.Run("MCPTransactionCandidateAndExactMoney", func(t *testing.T) {
		result := c.query(t, `SELECT occurred_at,merchant,amount,description FROM pam.transactions WHERE source=$1 AND merchant ILIKE $2 AND occurred_at >= $3::timestamptz AND occurred_at < $4::timestamptz ORDER BY occurred_at`, []any{source, "%Synthetic Mall%", "2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}, false)
		if result.RowCount != 2 || text(t, result.Rows[0][1]) != "Synthetic Mall camera shop" || !strings.Contains(text(t, result.Rows[0][3]), "unconfirmed") {
			t.Fatal("candidate search lost payment evidence or uncertainty")
		}
		precise := c.query(t, `SELECT amount FROM pam.transactions WHERE id=$1::uuid AND source=$2`, []any{precisionID, source}, false)
		if precise.RowCount != 1 || text(t, precise.Rows[0][0]) != "9007199254740993.0001" {
			t.Fatal("SQL result lost exact monetary representation")
		}
	})

	t.Run("AssetCRUDAndSQLCorrelation", func(t *testing.T) {
		price, date := "999.00", "2025-05-10"
		draft := assetInput{Name: "Synthetic Gadget candidate " + source, Category: "photography", Brand: "Synthetic Gadget", Model: "Model A", PurchaseDate: &date, PurchasePrice: &price, Currency: "CNY", Merchant: rows[0].Merchant, RelatedTransactionID: &transactionID, Notes: "Synthetic fixture; product association remains unconfirmed", Status: "active"}
		missing := "00000000-0000-0000-0000-000000000000"
		invalid := draft
		invalid.RelatedTransactionID = &missing
		c.request(t, "POST", "/api/v1/assets", invalid, true, 400)
		created := decoded[assetResult](t, c.request(t, "POST", "/api/v1/assets", draft, true, 201))
		if created.ID == "" || created.PurchasePrice == nil || *created.PurchasePrice != "999.0000" {
			t.Fatal("asset creation lost purchase data")
		}
		deleted := false
		t.Cleanup(func() {
			if !deleted {
				c.request(t, "DELETE", "/api/v1/assets/"+created.ID, nil, true, 204)
			}
		})
		c.request(t, "GET", "/api/v1/assets/"+created.ID, nil, true, 200)
		draft.Status = "stored"
		draft.Notes = "Synthetic fixture updated; product unconfirmed"
		updated := decoded[assetResult](t, c.request(t, "PUT", "/api/v1/assets/"+created.ID, draft, true, 200))
		if updated.Status != "stored" || updated.Notes != draft.Notes {
			t.Fatal("asset update was not persisted")
		}
		correlation := c.query(t, `SELECT a.name,t.merchant,t.amount FROM pam.assets a JOIN pam.transactions t ON a.related_transaction_id=t.id WHERE t.source=$1 AND a.id=$2::uuid`, []any{source, created.ID}, false)
		if correlation.RowCount != 1 || text(t, correlation.Rows[0][1]) != rows[0].Merchant {
			t.Fatal("SQL could not correlate inventory and transaction")
		}
		c.request(t, "DELETE", "/api/v1/assets/"+created.ID, nil, true, 204)
		deleted = true
		c.request(t, "GET", "/api/v1/assets/"+created.ID, nil, true, 404)
	})

	t.Run("SQLRestrictionsAndAuditing", func(t *testing.T) {
		for _, sql := range []string{
			`INSERT INTO pam.assets(name,currency) VALUES ('synthetic blocked','CNY')`,
			`CREATE TABLE pam.acceptance_forbidden(id integer)`,
			`SELECT 1; SELECT 2`,
			`WITH d AS (DELETE FROM pam.assets WHERE name='synthetic-blocked-` + source + `' RETURNING *) SELECT * FROM d`,
			`SELECT pg_read_file('/etc/passwd')`,
			`SELECT set_config('search_path','public',false)`,
			`SELECT 'unsafe'::regprocedure`,
			`SELECT * FROM pg_catalog.pg_roles`,
			`SELECT * FROM pam.assets FOR UPDATE`,
		} {
			c.query(t, sql, nil, true)
		}
		c.query(t, `SELECT missing_acceptance_column FROM pam.transactions WHERE source=$1`, []any{source}, true)
		status := c.query(t, `SELECT status,count(*) FROM pam.query_audit WHERE executed_at >= $1::timestamptz GROUP BY status`, []any{started}, false)
		counts := map[string]int{}
		for _, row := range status.Rows {
			var count int
			if _, err := fmt.Sscan(text(t, row[1]), &count); err != nil {
				t.Fatal(err)
			}
			counts[text(t, row[0])] = count
		}
		if counts["rejected"] < 9 || counts["error"] < 1 || counts["success"] < 1 {
			t.Fatalf("incomplete audit terminal states: %+v", counts)
		}
		// 不同商户字面值须聚合为同一查询模式，且模式内不保留商户或来源值。
		for _, merchant := range []string{"audit-merchant-one-" + source, "audit-merchant-two-" + source} {
			c.query(t, `SELECT count(*) FROM pam.transactions WHERE source=$1 AND merchant='`+merchant+`'`, []any{source}, false)
		}
		patterns := c.query(t, `SELECT fingerprint,normalized_sql FROM pam.query_audit WHERE status='success' ORDER BY executed_at DESC LIMIT 2`, nil, false)
		if patterns.RowCount != 2 || text(t, patterns.Rows[0][0]) != text(t, patterns.Rows[1][0]) || text(t, patterns.Rows[0][1]) != text(t, patterns.Rows[1][1]) {
			t.Fatal("equivalent merchant queries did not share a fingerprint")
		}
		if strings.Contains(text(t, patterns.Rows[0][1]), source) || strings.Contains(text(t, patterns.Rows[0][1]), "audit-merchant-") {
			t.Fatal("audit normalization leaked literal values")
		}
		summary := c.query(t, `SELECT calls FROM pam.query_patterns WHERE fingerprint=$1`, []any{text(t, patterns.Rows[0][0])}, false)
		var calls int
		if summary.RowCount != 1 {
			t.Fatal("pattern aggregation missing")
		}
		if _, err := fmt.Sscan(text(t, summary.Rows[0][0]), &calls); err != nil || calls < 2 {
			t.Fatalf("pattern frequency not aggregated: %d %v", calls, err)
		}
		privacy := c.query(t, `SELECT count(*) FROM pam.query_audit WHERE executed_at >= $1::timestamptz AND (normalized_sql LIKE $2 OR normalized_sql LIKE $3)`, []any{started, "%" + source + "%", "%9007199254740993%"}, false)
		if privacy.RowCount != 1 || text(t, privacy.Rows[0][0]) != "0" {
			t.Fatal("query audit retained sensitive literal or parameter content")
		}
	})

	t.Run("SQLResponseRowLimit", func(t *testing.T) {
		result := c.query(t, `SELECT * FROM generate_series(1,10000)`, nil, false)
		if !result.Truncated || result.RowCount < 1 || result.RowCount > 500 || result.RowCount != len(result.Rows) {
			t.Fatalf("SQL response limit failed: rows=%d truncated=%t", result.RowCount, result.Truncated)
		}
		c.query(t, `SELECT count(*) FROM pam.transactions WHERE source=$1`, []any{source}, false)
	})
	t.Logf("SYNTHETIC_SOURCE_RETAINED %s; own assets deleted; imported source/audit retained for root archival cleanup", source)
}
