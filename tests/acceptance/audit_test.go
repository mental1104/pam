package acceptance_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestPublicAuditCenter 通过公开 MCP 产生审计，再由独立 REST 客户端核验可视化快照和原文参数。
func TestPublicAuditCenter(t *testing.T) {
	c := newClient(t)
	marker := uniqueSource(t)
	sql := "SELECT $1::text, '" + marker + "'::text"
	parameter := `<img src=x onerror=alert(1)> 9007199254740993.0001`
	call := c.call(t, "query_readonly_sql", map[string]any{"sql": sql, "parameters": []any{parameter}})
	if call.IsError {
		t.Fatal("fixture query failed")
	}
	path := "/api/v1/audit?search=" + url.QueryEscape(marker)
	c.request(t, "GET", path, nil, false, 401)
	// 两次读取不再增加审计，且全部图表/明细服从同一筛选。
	for i := 0; i < 2; i++ {
		var d struct {
			Summary struct {
				Total   int `json:"total"`
				Success int `json:"success"`
			} `json:"summary"`
			Records struct {
				Total int `json:"total"`
				Items []struct {
					Fingerprint string  `json:"fingerprint"`
					SQL         string  `json:"sql"`
					RawSQL      *string `json:"raw_sql"`
					Parameters  *string `json:"parameters_json"`
				} `json:"items"`
			} `json:"records"`
		}
		if err := json.Unmarshal(c.request(t, "GET", path, nil, true, 200), &d); err != nil {
			t.Fatal(err)
		}
		if d.Summary.Total != 1 || d.Summary.Success != 1 || d.Records.Total != 1 || len(d.Records.Items) != 1 {
			t.Fatal("audit read created its own records or inconsistent counts")
		}
		record := d.Records.Items[0]
		if record.RawSQL == nil || *record.RawSQL != sql || record.Parameters == nil {
			t.Fatal("original SQL capture missing")
		}
		var values []string
		if json.Unmarshal([]byte(*record.Parameters), &values) != nil || len(values) != 1 || values[0] != parameter {
			t.Fatal("bound parameters were changed")
		}
		if strings.Contains(record.SQL, marker) {
			t.Fatal("aggregation pattern unexpectedly contains literals")
		}
		// 指纹必须来自真实 MCP 执行器，避免手写夹具掩盖 AST 16 位和错误模式 64 位的差异。
		c.request(t, "GET", path+"&fingerprint="+url.QueryEscape(record.Fingerprint), nil, true, 200)
	}
	c.request(t, "GET", "/api/v1/audit?status=invalid", nil, true, 400)
}
