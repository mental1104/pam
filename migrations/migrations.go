// Package migrations 以数据库所有者连接执行嵌入式、按版本排序的数据库迁移。
package migrations

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// files 是版本化 SQL 的编译时快照，避免运行目录影响迁移结果。
//
//go:embed *.sql
var files embed.FS

// Apply 使用 pool 的所有者连接按文件名顺序迁移；ctx 控制锁等待和执行取消。
// 每个版本独立提交；同版本并发启动通过事务级 advisory lock 串行化，失败版本完整回滚。
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := fs.Glob(files, "*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(entries)
	for _, name := range entries {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		// 即使进程退出，事务结束也会释放锁；不留下永久迁移互斥状态。
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(742190010)"); err == nil {
			_, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS public.pam_schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())")
		}
		var applied bool
		if err == nil {
			err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM public.pam_schema_migrations WHERE version=$1)", name).Scan(&applied)
		}
		if err == nil && !applied {
			var body []byte
			body, err = files.ReadFile(name)
			if err == nil {
				_, err = tx.Exec(ctx, string(body))
			}
			if err == nil {
				_, err = tx.Exec(ctx, "INSERT INTO public.pam_schema_migrations(version) VALUES($1)", name)
			}
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
