package query

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationPools 从明确的隔离测试 DSN 创建 reader/writer；t 负责连接释放，缺少任一环境变量则跳过。
// 不运行迁移、不创建账号，也不会连接约定之外的现网数据库。
func integrationPools(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	readerURL, writerURL := os.Getenv("PAM_TEST_READER_URL"), os.Getenv("PAM_TEST_WRITER_URL")
	if readerURL == "" || writerURL == "" {
		t.Skip("PAM_TEST_READER_URL and PAM_TEST_WRITER_URL are required for database acceptance")
	}
	reader, err := pgxpool.New(context.Background(), readerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	writer, err := pgxpool.New(context.Background(), writerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	return reader, writer
}

// TestDatabaseQueryAndAudit 验证真实只读角色、精度、SQLSTATE 和审计隐私；t 接收失败证据。
func TestDatabaseQueryAndAudit(t *testing.T) {
	reader, writer := integrationPools(t)
	service := New(reader, writer, Limits{})
	ctx := context.Background()
	if err := service.Check(ctx); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	result, err := service.Execute(ctx, Request{SQL: `SELECT $1::numeric AS amount, 'private-literal-acceptance' AS "private-alias-acceptance"`, Parameters: []any{json.Number("9007199254740993.1234")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 1 || result.Rows[0][0] != "9007199254740993.1234" {
		t.Fatalf("unexpected exact numeric result: %#v", result)
	}
	// 参数中的引号和分号只能作为值返回，不能扩展已审核的单条 SELECT。
	injection := `'; DELETE FROM pam.assets; SELECT 'private-parameter`
	bound, err := service.Execute(ctx, Request{SQL: `SELECT $1::text`, Parameters: []any{injection}})
	if err != nil || bound.RowCount != 1 || bound.Rows[0][0] != injection {
		t.Fatalf("parameter binding did not preserve literal semantics: %#v %v", bound, err)
	}
	_, err = service.Execute(ctx, Request{SQL: `SELECT missing_private_column FROM pam.transactions`})
	if err == nil || errorCode(err) != "42703" {
		t.Fatalf("expected undefined-column SQLSTATE, got %v", err)
	}
	_, err = service.Execute(ctx, Request{SQL: `WITH d AS (DELETE FROM pam.assets RETURNING *) SELECT * FROM d`})
	if err == nil {
		t.Fatal("write CTE was accepted")
	}
	var patterns string
	var successes, failures, rejections int
	err = writer.QueryRow(ctx, `SELECT string_agg(normalized_sql,' '),count(*) FILTER(WHERE status='success'),count(*) FILTER(WHERE status='error'),count(*) FILTER(WHERE status='rejected') FROM pam.query_audit WHERE executed_at >= $1`, before).Scan(&patterns, &successes, &failures, &rejections)
	if err != nil {
		t.Fatal(err)
	}
	if successes < 1 || failures < 1 || rejections < 1 {
		t.Fatalf("incomplete audit statuses: %d %d %d", successes, failures, rejections)
	}
	for _, private := range []string{"9007199254740993", "private-literal-acceptance", "private-alias-acceptance", "missing_private_column", "private-parameter"} {
		if strings.Contains(patterns, private) {
			t.Errorf("audit leaked %q", private)
		}
	}
	var calls int
	if err = reader.QueryRow(ctx, `SELECT COALESCE(sum(calls),0)::int FROM pam.query_patterns`).Scan(&calls); err != nil || calls < 3 {
		t.Fatalf("audit summary unreadable: %d %v", calls, err)
	}
}

// TestDatabaseResourceLimits 验证公开结果的行/字节上限和 PostgreSQL 超时；t 接收失败证据。
func TestDatabaseResourceLimits(t *testing.T) {
	reader, writer := integrationPools(t)
	ctx := context.Background()
	service := New(reader, writer, Limits{MaxRows: 3, MaxResponseBytes: 160, Timeout: time.Second})
	result, err := service.Execute(ctx, Request{SQL: `SELECT * FROM generate_series(1,100)`})
	if err != nil || result.RowCount != 3 || !result.Truncated {
		t.Fatalf("row limit failed: %#v %v", result, err)
	}
	encoded, _ := json.Marshal(result)
	if len(encoded) > 160 {
		t.Fatalf("response byte cap failed: %d", len(encoded))
	}
	result, err = service.Execute(ctx, Request{SQL: `SELECT concat('long-',g::text,'-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa') AS value FROM generate_series(1,10) g`})
	if err != nil || !result.Truncated {
		t.Fatalf("byte limit failed: %#v %v", result, err)
	}
	encoded, _ = json.Marshal(result)
	if len(encoded) > 160 {
		t.Fatalf("response byte cap failed: %d", len(encoded))
	}
	fast := New(reader, writer, Limits{Timeout: 50 * time.Millisecond})
	_, err = fast.Execute(ctx, Request{SQL: `SELECT count(*) FROM generate_series(1,1000000000)`})
	if err == nil || errorCode(err) != "57014" {
		t.Fatalf("query timeout failed: %v", err)
	}
	var count int
	if err = writer.QueryRow(ctx, `SELECT count(*) FROM pam.query_audit WHERE error_code='57014'`).Scan(&count); err != nil || count < 1 {
		t.Fatalf("timeout was not audited: %v", err)
	}
}

// TestDatabaseAuditFailClosed 验证 writer 无法落盘时不交付查询结果；t 接收失败证据。
func TestDatabaseAuditFailClosed(t *testing.T) {
	reader, _ := integrationPools(t)
	service := New(reader, reader, Limits{})
	result, err := service.Execute(context.Background(), Request{SQL: `SELECT 1`})
	if err == nil || errorCode(err) != "audit_unavailable" || len(result.Rows) != 0 {
		t.Fatalf("audit failure did not fail closed: %#v %v", result, err)
	}
}

// TestDatabaseRoleDeniesDirectSideEffects 验证绕过应用 SQL 策略时，专用角色仍没有写表或危险函数权限。
// t 接收失败证据；每条语句使用独立自动事务，拒绝后不会污染后续连接状态。
func TestDatabaseRoleDeniesDirectSideEffects(t *testing.T) {
	reader, _ := integrationPools(t)
	for _, sql := range []string{
		`DELETE FROM pam.assets WHERE false`,
		`CREATE TABLE pam.forbidden_acceptance(id integer)`,
		`SELECT pg_catalog.pg_sleep(0)`,
		`SELECT pg_catalog.pg_read_file('/etc/passwd')`,
		`SELECT pg_catalog.set_config('application_name','forbidden',false)`,
		`SELECT pg_catalog.pg_advisory_lock(123456789)`,
	} {
		if _, err := reader.Exec(context.Background(), sql); err == nil {
			t.Errorf("read-only role unexpectedly executed: %s", sql)
		}
	}
}
