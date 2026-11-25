package query

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestOriginalAuditCapture 通过真实执行器确认授权的 SQL/参数持久化，同时保持归一化模式不含字面值。
func TestOriginalAuditCapture(t *testing.T) {
	reader, writer := integrationPools(t)
	service := New(reader, writer, Limits{})
	ctx := context.Background()
	marker := "original-fixture-" + time.Now().UTC().Format("150405.000000000")
	sql := "SELECT $1::text, '" + marker + "'::text"
	parameter := `<img src=x onerror=alert(1)>`
	before := time.Now()
	if _, err := service.Execute(ctx, Request{SQL: sql, Parameters: []any{parameter}}); err != nil {
		t.Fatal(err)
	}
	var raw, pattern, params string
	var truncated bool
	err := writer.QueryRow(ctx, `SELECT raw_sql,normalized_sql,parameters::text,raw_truncated FROM pam.query_audit WHERE executed_at >= $1 AND raw_sql=$2 ORDER BY executed_at DESC LIMIT 1`, before, sql).Scan(&raw, &pattern, &params, &truncated)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	if json.Unmarshal([]byte(params), &values) != nil || len(values) != 1 || values[0] != parameter || raw != sql || truncated {
		t.Fatal("authorized originals were not preserved")
	}
	if strings.Contains(pattern, marker) || strings.Contains(pattern, parameter) {
		t.Fatal("normalized aggregation key leaked original values")
	}
}

// TestOriginalRejectedAndFailedAudit 验证策略拒绝与数据库错误同样留下授权原文，不只保留成功调用。
func TestOriginalRejectedAndFailedAudit(t *testing.T) {
	reader, writer := integrationPools(t)
	service := New(reader, writer, Limits{})
	ctx := context.Background()
	for _, test := range []struct{ sql, status string }{
		{"UPDATE pam.transactions SET merchant=$1", "rejected"},
		{"SELECT missing_original_audit_column FROM pam.transactions WHERE merchant=$1", "error"},
	} {
		before := time.Now()
		if _, err := service.Execute(ctx, Request{SQL: test.sql, Parameters: []any{"synthetic-original-parameter"}}); err == nil {
			t.Fatal("expected rejected or failed query")
		}
		var status, parameters string
		if err := writer.QueryRow(ctx, `SELECT status,parameters::text FROM pam.query_audit WHERE executed_at >= $1 AND raw_sql=$2 ORDER BY executed_at DESC LIMIT 1`, before, test.sql).Scan(&status, &parameters); err != nil || status != test.status || !strings.Contains(parameters, "synthetic-original-parameter") {
			t.Fatalf("failed attempt lost original evidence: status=%s error=%v", status, err)
		}
	}
}

// TestCaptureBounds 验证超限 UTF-8 SQL 保持有效且显式标记，不生成看似完整的截断参数 JSON。
func TestCaptureBounds(t *testing.T) {
	sql, params, truncated := captureOriginal(Request{SQL: strings.Repeat("中", 50000), Parameters: []any{strings.Repeat("x", 150000)}})
	if !truncated || len(sql) > 128*1024 || params != nil || !json.Valid([]byte(`"`+sql+`"`)) {
		t.Fatal("capture limits were not explicit or broke UTF-8")
	}
	_, params, truncated = captureOriginal(Request{SQL: "SELECT 1"})
	if truncated || params == nil || *params != "[]" {
		t.Fatal("empty bound parameters must be a known empty array")
	}
}
