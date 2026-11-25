package query

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReadOnlyPolicy 验证真实 PostgreSQL 解析树覆盖常见分析语法和隐藏写入；t 接收失败证据。
func TestReadOnlyPolicy(t *testing.T) {
	accepted := []string{
		`SELECT id, amount, merchant FROM pam.transactions WHERE merchant ILIKE $1 ORDER BY occurred_at DESC LIMIT 50`,
		`WITH spending AS (SELECT category, sum(amount) total FROM transactions WHERE direction='expense' GROUP BY category) SELECT * FROM spending ORDER BY total DESC`,
		`SELECT a.name,t.amount FROM assets a LEFT JOIN transactions t ON a.related_transaction_id=t.id WHERE t.occurred_at >= '2025-01-01'::timestamptz`,
		`SELECT date_trunc('month',occurred_at),sum(amount) FILTER (WHERE direction='income') FROM transactions GROUP BY 1`,
		`SELECT merchant, row_number() OVER (PARTITION BY category ORDER BY amount DESC) FROM transactions`,
		`SELECT 'a; b' AS note, 10.25::numeric, jsonb_build_object('kind','fixture')`,
		`SELECT * FROM generate_series(1,10) AS n`,
		`SELECT count(*) FROM query_audit UNION ALL SELECT count(*) FROM transactions`,
		`WITH RECURSIVE n AS (SELECT 1 x UNION ALL SELECT x+1 FROM n WHERE x<10) SELECT * FROM n`,
		`WITH first AS (SELECT 1 x), second AS (SELECT * FROM first) SELECT * FROM second`,
	}
	for _, sql := range accepted {
		if err := validate(sql, 16384); err != nil {
			t.Errorf("expected accepted SQL %q: %v", sql, err)
		}
	}
	rejected := []string{
		`INSERT INTO pam.assets(name) VALUES ('x')`,
		`SELECT 1; SELECT 2`,
		`WITH deleted AS (DELETE FROM pam.assets RETURNING *) SELECT * FROM deleted`,
		`WITH changed AS (UPDATE pam.assets SET name='x' RETURNING *) SELECT * FROM changed`,
		`SELECT * INTO pam.copy FROM pam.transactions`,
		`SELECT * FROM pam.assets FOR UPDATE`,
		`SELECT pg_read_file('/etc/passwd')`,
		`SELECT pg_sleep(1)`,
		`SELECT set_config('search_path','public',false)`,
		`SELECT nextval('x')`,
		`SELECT lo_import('/tmp/source')`,
		`SELECT pg_advisory_lock(1)`,
		`SELECT * FROM pg_catalog.pg_authid`,
		`SELECT * FROM information_schema.tables`,
		`SELECT * FROM public.transactions`,
		`SELECT * FROM pam.pam_schema_migrations`,
		`SELECT 'dangerous'::regprocedure`,
		`SELECT public.lower('x')`,
		`SELECT 1 OPERATOR(public.+) 2`,
		`COPY (SELECT * FROM assets) TO PROGRAM 'cat'`,
		`EXPLAIN ANALYZE SELECT * FROM assets`,
		`SELECT * FROM assets TABLESAMPLE system(10)`,
		`SELECT * FROM pg_roles WHERE EXISTS (WITH pg_roles AS (SELECT 1) SELECT 1)`,
		`WITH first AS (SELECT * FROM pg_roles), pg_roles AS (SELECT 1) SELECT * FROM first`,
		`WITH pg_roles AS (SELECT * FROM pg_roles) SELECT * FROM pg_roles`,
	}
	for _, sql := range rejected {
		if err := validate(sql, 16384); err == nil {
			t.Errorf("expected rejection: %q", sql)
		}
	}
}

// TestAuditPrivacy 验证敏感字面值、注释、别名和语法错误均不进入模式；t 接收失败证据。
func TestAuditPrivacy(t *testing.T) {
	a, first := normalized(`SELECT * FROM transactions WHERE merchant = 'JD' /* sensitive-comment */`, 16384)
	b, second := normalized(`SELECT * FROM transactions WHERE merchant = 'Taobao'`, 16384)
	if a != b || first != second {
		t.Errorf("equivalent patterns differ: %q / %q", first, second)
	}
	_, aliases := normalized(`SELECT 'secret-literal' AS "secret-alias", $1::numeric FROM transactions "private-alias"`, 16384)
	for _, private := range []string{"JD", "Taobao", "sensitive-comment", "secret-literal", "secret-alias", "private-alias"} {
		if strings.Contains(first+second+aliases, private) {
			t.Errorf("audit leaked private input %q", private)
		}
	}
	_, malformed := normalized(`SELECT 'unterminated-private`, 16384)
	if strings.Contains(malformed, "unterminated-private") {
		t.Fatal("malformed query leaked input")
	}
	_, ddl := normalized(`CREATE TABLE "private-ddl-name" (secret text)`, 16384)
	if strings.Contains(ddl, "private-ddl-name") {
		t.Fatal("rejected statement leaked identifier")
	}
}

// TestParameterPrecision 验证十进制参数不经 float64，非法复合类型被拒绝；t 接收失败证据。
func TestParameterPrecision(t *testing.T) {
	values, err := safeParameters([]any{json.Number("9007199254740993.1234"), "1299.00", true, nil})
	if err != nil || values[0] != "9007199254740993.1234" {
		t.Fatalf("decimal was not preserved: %#v %v", values, err)
	}
	for _, value := range []any{1.25, map[string]any{"x": 1}, []any{1}} {
		if _, err := safeParameters([]any{value}); err == nil {
			t.Errorf("unsafe parameter accepted: %#v", value)
		}
	}
}

// TestDecodedNumericPrecision 验证金额与嵌套 JSON 大整数保持原始精度；t 接收失败证据。
func TestDecodedNumericPrecision(t *testing.T) {
	if got := decodeValue(1700, []byte("9007199254740993.1234")); got != "9007199254740993.1234" {
		t.Fatal("numeric precision lost")
	}
	value := decodeValue(3802, []byte(`{"amount":9007199254740993.1234}`)).(map[string]any)
	if value["amount"].(json.Number).String() != "9007199254740993.1234" {
		t.Fatal("JSON precision lost")
	}
}
