package acceptance_test

import (
	"encoding/json"
	"net/url"
	"testing"
)

// proposalResponse 是公开预检的独立客户端 DTO，不链接服务端内部包。
type proposalResponse struct {
	ID        string       `json:"id"`
	Preview   importResult `json:"preview"`
	Committed bool         `json:"committed"`
}

// reviewResponse 只保留协议所需待核身份及来源，不假定候选已经是同一交易。
type reviewResponse struct {
	ID            string         `json:"id"`
	Source        string         `json:"source"`
	Kind          string         `json:"kind"`
	Record        map[string]any `json:"record"`
	TransactionID string         `json:"transaction_id"`
}

// TestPublicIncrementalReview 用独立 HTTP/MCP 客户端验证预检、确认、持久待核、更正和来源范围一致性。
func TestPublicIncrementalReview(t *testing.T) {
	c := newClient(t)
	source := uniqueSource(t)
	row := map[string]any{"row_key": "one", "occurred_at": "2026-10-22T10:01:00+08:00", "direction": "expense", "amount": "7.00", "currency": "CNY", "merchant": source, "description": "synthetic local OCR", "original": map[string]any{"pam_parse_issues": []string{"low_confidence"}, "pam_human_reviewed": true, "pam_account": "synthetic-account"}}
	req := map[string]any{"source": source, "idempotency_key": "first", "source_file_hash": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "records": []any{row}}
	proposal := decoded[proposalResponse](t, c.request(t, "POST", "/api/v1/imports/proposals", req, true, 200))
	if proposal.Preview.Created != 1 {
		t.Fatal("preview did not identify new candidate")
	}
	page := decoded[transactionPage](t, c.request(t, "GET", "/api/v1/transactions?source="+url.QueryEscape(source), nil, true, 200))
	if page.Total != 0 {
		t.Fatal("preview wrote ledger")
	}
	// 新客户端恢复持久提案，再经 MCP 确认；两个入口共用同一业务结果。
	other := newClient(t)
	restored := decoded[proposalResponse](t, other.request(t, "GET", "/api/v1/imports/proposals/"+proposal.ID, nil, true, 200))
	if restored.ID != proposal.ID {
		t.Fatal("proposal restore failed")
	}
	result := other.call(t, "manage_imports", map[string]any{"action": "confirm", "id": proposal.ID})
	if result.IsError {
		t.Fatal("MCP confirm failed")
	}
	imported := decoded[importResult](t, result.Structured)
	if imported.Created != 1 {
		t.Fatal("MCP did not create candidate")
	}
	replay := decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/proposals/"+proposal.ID+"/confirm", nil, true, 200))
	if replay.Created != 0 || replay.Skipped != 1 {
		t.Fatal("confirm replay created duplicates")
	}
	transactionID := *imported.Rows[0].TransactionID
	// 新旧混合截图：同图旧行跳过，新增行创建。
	newer := map[string]any{}
	for k, v := range row {
		newer[k] = v
	}
	newer["row_key"] = "two"
	newer["occurred_at"] = "2026-10-22T10:05:00+08:00"
	newer["amount"] = "8.00"
	req["idempotency_key"] = "overlap"
	req["records"] = []any{row, newer}
	proposal = decoded[proposalResponse](t, c.request(t, "POST", "/api/v1/imports/proposals", req, true, 200))
	imported = decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/proposals/"+proposal.ID+"/confirm", nil, true, 200))
	if imported.Created != 1 || imported.Skipped != 1 {
		t.Fatal("incremental overlap failed")
	}
	// 不同平台相似项不直接消失，明确保存在待核。
	req["source"] = source + "-alternate"
	req["idempotency_key"] = "alternate"
	req["source_file_hash"] = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	req["records"] = []any{row}
	proposal = decoded[proposalResponse](t, c.request(t, "POST", "/api/v1/imports/proposals", req, true, 200))
	imported = decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/proposals/"+proposal.ID+"/confirm", nil, true, 200))
	if imported.Duplicated != 1 {
		t.Fatal("cross-source candidate lost")
	}
	reviews := decoded[struct {
		Items []reviewResponse `json:"items"`
	}](t, other.request(t, "GET", "/api/v1/imports/reviews?limit=200", nil, true, 200))
	found := ""
	for _, v := range reviews.Items {
		if v.Source == source+"-alternate" {
			found = v.ID
		}
	}
	if found == "" {
		t.Fatal("pending review not persistent")
	}
	linked := other.call(t, "manage_imports", map[string]any{"action": "resolve", "id": found, "decision": map[string]any{"action": "link", "reason": "Human checked original images", "transaction_id": transactionID}})
	if linked.IsError {
		t.Fatal("MCP resolve failed")
	}
	req["idempotency_key"] = "alternate-again"
	proposal = decoded[proposalResponse](t, c.request(t, "POST", "/api/v1/imports/proposals", req, true, 200))
	if proposal.Preview.Skipped != 1 {
		t.Fatal("confirmed same source row did not retain identity")
	}
	// 更正保留修改前后内容和证据；通过只读 MCP 独立核对数据库产生的历史。
	row["amount"] = "7.25"
	req["source"] = source
	req["source_file_hash"] = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	req["idempotency_key"] = "correction"
	proposal = decoded[proposalResponse](t, c.request(t, "POST", "/api/v1/imports/proposals", req, true, 200))
	imported = decoded[importResult](t, c.request(t, "POST", "/api/v1/imports/proposals/"+proposal.ID+"/confirm", nil, true, 200))
	if imported.Duplicated != 1 {
		t.Fatal("correction was silently applied")
	}
	reviews = decoded[struct {
		Items []reviewResponse `json:"items"`
	}](t, c.request(t, "GET", "/api/v1/imports/reviews?limit=200", nil, true, 200))
	found = ""
	for _, v := range reviews.Items {
		if v.Source == source && v.Kind == "correction" {
			found = v.ID
		}
	}
	if found == "" {
		t.Fatal("correction review missing")
	}
	decision := map[string]any{"action": "correct", "reason": "Manually verified screenshot amount", "record": row}
	c.request(t, "POST", "/api/v1/imports/reviews/"+found+"/resolve", decision, true, 200)
	transaction := decoded[transaction](t, c.request(t, "GET", "/api/v1/transactions/"+transactionID, nil, true, 200))
	if transaction.Amount != "7.2500" {
		t.Fatal("correction not visible")
	}
	revisions := c.call(t, "query_readonly_sql", map[string]any{"sql": "SELECT before_content->>'amount',after_content->>'amount',reason,evidence FROM pam.transaction_revisions WHERE review_id=$1", "parameters": []any{found}})
	if revisions.IsError {
		t.Fatal("revision evidence query rejected")
	}
	var body struct {
		Rows [][]json.RawMessage `json:"rows"`
	}
	if json.Unmarshal(revisions.Structured, &body) != nil || len(body.Rows) != 1 {
		t.Fatal("revision history missing")
	}
	ranges := c.call(t, "manage_imports", map[string]any{"action": "ranges"})
	if ranges.IsError {
		t.Fatal("ranges unavailable")
	}
}
