// audit 只向显式隔离测试数据库生成可视化夹具；不接受生产 DSN，不含真实财务数据。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"net/url"
	"os"
	"strings"
	"time"
)

// run 校验测试专用 DSN 后写入 52 条四状态夹具；ctx 控制取消，失败不输出连接字符串。
func run(ctx context.Context) error {
	dsn := os.Getenv("PAM_TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "pam-verify-postgres" && parsed.Hostname() != "127.0.0.1") || os.Getenv("PAM_AUDIT_FIXTURE_CONFIRM") != "isolated-only" {
		return fmt.Errorf("explicit isolated fixture target required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("fixture database unavailable")
	}
	defer pool.Close()
	base := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Hour)
	for index := 0; index < 52; index++ {
		status := "success"
		code := ""
		ms := int64(16 + index*7)
		if index%5 == 0 {
			status = "rejected"
			code = "function_access"
			ms = 2
		}
		if index%9 == 0 {
			status = "error"
			code = "57014"
			ms = 3000
		}
		if index > 49 {
			status = "running"
			code = ""
			ms = 0
		}
		pattern := []string{"SELECT count(*) FROM pam.transactions WHERE source = $1", "SELECT merchant, amount FROM pam.transactions WHERE occurred_at >= $1 LIMIT $2", "SELECT missing_field FROM pam.transactions", "<rejected:function_access>"}[index%4]
		digest := sha256.Sum256([]byte(pattern))
		fingerprint := hex.EncodeToString(digest[:])
		// 合成夹具覆盖既有 AST 16 位与固定拒绝模式 64 位两种标识；真实执行器另由 HTTP/MCP 验收覆盖。
		if index%4 != 3 {
			fingerprint = fingerprint[:16]
		}
		var raw, params *string
		if index%7 != 0 {
			value := pattern + " /* audit-visual-fixture */"
			if index == 1 {
				value = "SELECT '<img src=x onerror=window.PAM_AUDIT_XSS=1>' /* audit-visual-fixture */"
			}
			raw = &value
			body, _ := json.Marshal([]any{"Synthetic merchant <img src=x onerror=window.PAM_AUDIT_XSS=1>", json.Number("9007199254740993.0001")})
			encoded := string(body)
			params = &encoded
		}
		// 历史行只能使用模式，故将相同夹具标记放入已有模式注释供只读 UI 搜索定位。
		if raw == nil {
			pattern += " /* audit-visual-fixture */"
		}
		_, err = pool.Exec(ctx, `INSERT INTO pam.query_audit(fingerprint,normalized_sql,raw_sql,parameters,executed_at,status,error_code,duration_ms,row_count,source) VALUES($1,$2,$3,$4::jsonb,$5,$6,NULLIF($7,''),$8,$9,'fixture.visual.audit')`, fingerprint, pattern, raw, params, base.Add(time.Duration(index)*time.Hour), status, code, ms, index%20)
		if err != nil {
			return fmt.Errorf("cannot write isolated audit fixture")
		}
	}
	fmt.Println("synthetic_audit_records=52")
	return nil
}

// main 限时执行显式隔离夹具，错误只有固定说明，不打印财务原文或认证材料。
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
