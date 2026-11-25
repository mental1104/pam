package acceptance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// boundedRPC 发送带 id 的只读 sql，并以期望的 HTTP status 接收完整协议字节。
// c 提供认证客户端；t 只记录大小，不输出大型合成结果、id 或任何凭据。
func boundedRPC(t *testing.T, c *client, id any, sql string, status int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": "query_readonly_sql", "arguments": map[string]any{"sql": sql}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	response, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (256<<10)+1))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MCP_WIRE HTTP=%d request_bytes=%d response_bytes=%d", response.StatusCode, len(body), len(data))
	if response.StatusCode != status {
		t.Fatalf("unexpected HTTP status: %d", response.StatusCode)
	}
	if len(data) > 256<<10 {
		t.Fatalf("MCP query response exceeds 256 KiB: %d", len(data))
	}
	return data
}

// TestMCPQueryWireLimit 验证实际 JSON-RPC 响应的完整字节数，覆盖大单元格、边界截断、转义和请求 id。
// t 通过独立 HTTP 客户端只执行合成 SELECT，不导入财务记录，也不依赖业务表中现有内容。
func TestMCPQueryWireLimit(t *testing.T) {
	c := newClient(t)
	cases := []struct {
		name, sql string
		size      int
		truncated bool
	}{
		{"LargeSingleCell", `SELECT string_agg('a','') AS value FROM generate_series(1,200000)`, 200000, false},
		{"NearByteLimit", `SELECT string_agg('a','') AS value FROM generate_series(1,257000)`, 257000, false},
		{"OverByteLimit", `SELECT string_agg('a','') AS value FROM generate_series(1,260000)`, 0, true},
		{"EscapedContent", `SELECT string_agg('"','') AS value FROM generate_series(1,100000)`, 100000, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			data := boundedRPC(t, c, strings.Repeat("i", 254), test.sql, http.StatusOK)
			var envelope rpcResponse
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal("invalid JSON-RPC response")
			}
			if envelope.Error != nil {
				t.Fatalf("MCP protocol error %d", envelope.Error.Code)
			}
			var tool toolResult
			if json.Unmarshal(envelope.Result, &tool) != nil || tool.IsError {
				t.Fatal("query tool failed")
			}
			var result queryResult
			if json.Unmarshal(tool.Structured, &result) != nil {
				t.Fatal("structured query result missing")
			}
			if result.Truncated != test.truncated {
				t.Fatalf("unexpected truncation: %t", result.Truncated)
			}
			if test.truncated {
				if result.RowCount != 0 {
					t.Fatal("oversized cell was not omitted")
				}
				return
			}
			if result.RowCount != 1 || len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
				t.Fatal("expected one result cell")
			}
			var cell string
			if json.Unmarshal(result.Rows[0][0], &cell) != nil || len(cell) != test.size {
				t.Fatalf("unexpected exact cell size: %d", len(cell))
			}
			for _, item := range tool.Content {
				if item.Type == "text" && len(item.Text) > 128 {
					t.Fatal("text content duplicates structured rows")
				}
			}
		})
	}
	t.Run("OversizedRequestID", func(t *testing.T) {
		data := boundedRPC(t, c, strings.Repeat("i", 1024), `SELECT 1`, http.StatusBadRequest)
		var envelope struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &envelope) != nil || envelope.Error == nil || envelope.Error.Code != -32600 || string(envelope.ID) != "null" {
			t.Fatal("oversized request id must be rejected without echoing it")
		}
	})
}
