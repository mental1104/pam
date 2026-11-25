package store

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/mental1104/pam/internal/domain"
	"strings"
	"time"
)

// auditWhere 为 f 和绝对 from/to 生成参数化条件；返回 SQL 与绑定值，不插入用户 SQL 或排序表达式。
func auditWhere(f domain.AuditFilter, from, to time.Time) (string, []any) {
	clauses := []string{"executed_at >= $1", "executed_at < $2"}
	args := []any{from, to}
	// 字段名是编译时固定白名单；用户文本只进入绑定参数。
	for _, pair := range [][2]string{{"status", f.Status}, {"error_code", f.ErrorCode}, {"fingerprint", f.Fingerprint}, {"source", f.Source}} {
		if pair[1] != "" {
			args = append(args, pair[1])
			clauses = append(clauses, fmt.Sprintf("%s=$%d", pair[0], len(args)))
		}
	}
	if f.MinDuration > 0 {
		args = append(args, f.MinDuration)
		clauses = append(clauses, fmt.Sprintf("duration_ms >= $%d", len(args)))
	}
	if f.Search != "" {
		args = append(args, literalSearch(f.Search))
		clauses = append(clauses, fmt.Sprintf("(normalized_sql ILIKE $%[1]d OR raw_sql ILIKE $%[1]d)", len(args)))
	}
	return strings.Join(clauses, " AND "), args
}

// AuditDashboard 在单个 repeatable-read 只读事务读取 f 的统计与明细；ctx 传播取消。
// from/to 为用户区间或零值；零起点使用真实最早记录，零终点使用快照时刻，不触发 query.Execute。
func (s *Store) AuditDashboard(ctx context.Context, f domain.AuditFilter, from, to time.Time) (domain.AuditDashboard, error) {
	d := domain.AuditDashboard{SlowThresholdMS: 1000, Timeline: []domain.AuditBucket{}, Latency: []domain.AuditCount{}, Errors: []domain.AuditCount{}, Patterns: []domain.AuditPattern{}, Records: domain.Page[domain.AuditRecord]{Items: []domain.AuditRecord{}, Limit: f.Limit, Offset: f.Offset}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return d, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var earliest *time.Time
	if err = tx.QueryRow(ctx, "SELECT now(),min(executed_at) FROM pam.query_audit").Scan(&d.GeneratedAt, &earliest); err != nil {
		return d, err
	}
	if to.IsZero() {
		to = d.GeneratedAt
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
		if earliest != nil && earliest.Before(to) {
			from = *earliest
		}
	}
	if !from.Before(to) {
		return d, domain.ValidationError{Message: "from must be before to"}
	}
	d.From = from
	d.To = to
	where, args := auditWhere(f, from, to)
	err = tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE status='success'),count(*) FILTER(WHERE status='rejected'),count(*) FILTER(WHERE status='error'),count(*) FILTER(WHERE status='running'),count(*) FILTER(WHERE status IN ('success','error') AND duration_ms>=1000),round(avg(duration_ms) FILTER(WHERE status IN ('success','error')))::bigint,percentile_disc(0.95) WITHIN GROUP(ORDER BY duration_ms) FILTER(WHERE status IN ('success','error')),max(duration_ms) FILTER(WHERE status IN ('success','error')) FROM pam.query_audit WHERE `+where, args...).Scan(&d.Summary.Total, &d.Summary.Success, &d.Summary.Rejected, &d.Summary.Errors, &d.Summary.Running, &d.Summary.Slow, &d.Summary.AverageMS, &d.Summary.P95MS, &d.Summary.MaxMS)
	if err != nil {
		return d, err
	}
	d.Records.Total = d.Summary.Total
	// 选择整小时桶且最多约 36 桶；超长历史也不会生成随记录数增长的 SVG 节点。
	span := to.Unix() - from.Unix()
	hours := (span + 36*3600 - 1) / (36 * 3600)
	if hours < 1 {
		hours = 1
	}
	d.BucketSeconds = hours * 3600
	buckets := int(span/d.BucketSeconds) + 1
	if buckets > 37 {
		buckets = 37
	}
	for index := 0; index < buckets; index++ {
		start := time.Unix(from.Unix()+int64(index)*d.BucketSeconds, int64(from.Nanosecond()))
		if !start.Before(to) {
			break
		}
		end := time.Unix(start.Unix()+d.BucketSeconds, int64(start.Nanosecond()))
		if end.After(to) {
			end = to
		}
		d.Timeline = append(d.Timeline, domain.AuditBucket{Start: start, End: end})
	}
	timelineArgs := append(append([]any{}, args...), d.BucketSeconds)
	rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT floor(extract(epoch FROM (executed_at-$1::timestamptz))/$%d)::integer,count(*) FILTER(WHERE status='success'),count(*) FILTER(WHERE status='rejected'),count(*) FILTER(WHERE status='error'),count(*) FILTER(WHERE status='running') FROM pam.query_audit WHERE %s GROUP BY 1 ORDER BY 1`, len(timelineArgs), where), timelineArgs...)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var index int
		var b domain.AuditBucket
		if err = rows.Scan(&index, &b.Success, &b.Rejected, &b.Errors, &b.Running); err != nil {
			rows.Close()
			return d, err
		}
		if index >= 0 && index < len(d.Timeline) {
			b.Start = d.Timeline[index].Start
			b.End = d.Timeline[index].End
			d.Timeline[index] = b
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	// 耗时分布不混入策略拒绝和未完成调用，以免将拦截耗时解读成数据库执行性能。
	rows, err = tx.Query(ctx, `SELECT CASE WHEN duration_ms<100 THEN '0-100' WHEN duration_ms<500 THEN '100-500' WHEN duration_ms<1000 THEN '500-1000' WHEN duration_ms<3000 THEN '1000-3000' ELSE '3000+' END,count(*) FROM pam.query_audit WHERE `+where+` AND status IN ('success','error') GROUP BY 1`, args...)
	if err != nil {
		return d, err
	}
	latency := map[string]int64{}
	for rows.Next() {
		var key string
		var count int64
		if err = rows.Scan(&key, &count); err != nil {
			rows.Close()
			return d, err
		}
		latency[key] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	for _, key := range []string{"0-100", "100-500", "500-1000", "1000-3000", "3000+"} {
		d.Latency = append(d.Latency, domain.AuditCount{Key: key, Count: latency[key]})
	}
	rows, err = tx.Query(ctx, `SELECT error_code,count(*) FROM pam.query_audit WHERE `+where+` AND error_code IS NOT NULL GROUP BY error_code ORDER BY count(*) DESC,error_code LIMIT 12`, args...)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var c domain.AuditCount
		if err = rows.Scan(&c.Key, &c.Count); err != nil {
			rows.Close()
			return d, err
		}
		d.Errors = append(d.Errors, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	rows, err = tx.Query(ctx, `SELECT fingerprint,min(normalized_sql),count(*),count(*) FILTER(WHERE status IN ('error','rejected')),round(avg(duration_ms) FILTER(WHERE status IN ('success','error')))::bigint,max(duration_ms) FILTER(WHERE status IN ('success','error')) FROM pam.query_audit WHERE `+where+` GROUP BY fingerprint ORDER BY count(*) DESC,fingerprint LIMIT 8`, args...)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var p domain.AuditPattern
		if err = rows.Scan(&p.Fingerprint, &p.SQL, &p.Calls, &p.Failures, &p.AverageMS, &p.MaxMS); err != nil {
			rows.Close()
			return d, err
		}
		d.Patterns = append(d.Patterns, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return d, err
	}
	order := "executed_at DESC,id DESC"
	if f.Sort == "slow" {
		order = "duration_ms DESC,executed_at DESC,id DESC"
	}
	pageArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err = tx.Query(ctx, fmt.Sprintf(`SELECT id::text,fingerprint,normalized_sql,raw_sql,parameters::text,raw_truncated,executed_at,duration_ms,row_count,status,COALESCE(error_code,''),source FROM pam.query_audit WHERE %s ORDER BY %s LIMIT $%d OFFSET $%d`, where, order, len(pageArgs)-1, len(pageArgs)), pageArgs...)
	if err != nil {
		return d, err
	}
	for rows.Next() {
		var r domain.AuditRecord
		if err = rows.Scan(&r.ID, &r.Fingerprint, &r.SQL, &r.RawSQL, &r.ParametersJSON, &r.RawTruncated, &r.ExecutedAt, &r.DurationMS, &r.RowCount, &r.Status, &r.ErrorCode, &r.Source); err != nil {
			rows.Close()
			return d, err
		}
		d.Records.Items = append(d.Records.Items, r)
	}
	err = rows.Err()
	rows.Close()
	return d, err
}
