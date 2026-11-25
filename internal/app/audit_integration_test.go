package app

import (
	"context"
	"github.com/mental1104/pam/internal/domain"
	"strings"
	"testing"
	"time"
)

// TestAuditDashboardSnapshot 在隔离库验证统计口径、零填充、下钻、分页、旧原文缺失与新增原文参数。
func TestAuditDashboardSnapshot(t *testing.T) {
	s, pool := testService(t)
	ctx := context.Background()
	source := uniqueSource(t)
	start := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	statuses := []string{"success", "success", "rejected", "error", "error", "running"}
	durations := []int{50, 1500, 2, 3000, 5000, 0}
	for index, status := range statuses {
		code := ""
		if status == "rejected" {
			code = "function_access"
		}
		if status == "error" {
			code = "57014"
		}
		var raw *string
		var params *string
		if index > 0 {
			sql := "SELECT 'original-visible' /* " + source + " */"
			raw = &sql
			value := `["<img src=x onerror=alert(1)>", 9007199254740993.0001]`
			params = &value
		}
		fingerprint := strings.Repeat("a", 16)
		if status == "rejected" {
			fingerprint = strings.Repeat("b", 64)
		}
		_, err := pool.Exec(ctx, `INSERT INTO pam.query_audit(fingerprint,normalized_sql,raw_sql,parameters,executed_at,status,error_code,duration_ms,row_count,source) VALUES($1,'SELECT $1', $2,$3::jsonb,$4,$5,NULLIF($6,''),$7,$8,$9)`, fingerprint, raw, params, start.Add(time.Duration(index*3)*time.Hour), status, code, durations[index], index, source)
		if err != nil {
			t.Fatal(err)
		}
	}
	filter := domain.AuditFilter{Source: source, From: start.Format(time.RFC3339), To: start.Add(24 * time.Hour).Format(time.RFC3339), Limit: 2, Sort: "slow"}
	d, err := s.AuditDashboard(ctx, filter)
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary.Total != 6 || d.Summary.Success != 2 || d.Summary.Rejected != 1 || d.Summary.Errors != 2 || d.Summary.Running != 1 || d.Summary.Slow != 3 {
		t.Fatalf("incorrect summary: %+v", d.Summary)
	}
	if d.Summary.P95MS == nil || *d.Summary.P95MS != 5000 || d.Summary.AverageMS == nil || *d.Summary.AverageMS != 2388 {
		t.Fatalf("incorrect execution latency: %+v", d.Summary)
	}
	sum := int64(0)
	zero := false
	for _, b := range d.Timeline {
		n := b.Success + b.Rejected + b.Errors + b.Running
		sum += n
		if n == 0 {
			zero = true
		}
	}
	if sum != 6 || !zero {
		t.Fatal("timeline dropped calls or omitted zero buckets")
	}
	if len(d.Records.Items) != 2 || d.Records.Total != 6 || d.Records.Items[0].DurationMS != 5000 || d.Records.Items[0].RawSQL == nil || !strings.Contains(*d.Records.Items[0].ParametersJSON, "9007199254740993.0001") {
		t.Fatal("records lost ordering, exact parameters or original SQL")
	}
	latency := int64(0)
	for _, item := range d.Latency {
		latency += item.Count
	}
	if latency != 4 {
		t.Fatal("latency included rejected or unfinished calls")
	}
	for fingerprint, count := range map[string]int64{strings.Repeat("a", 16): 5, strings.Repeat("b", 64): 1} {
		filter.Fingerprint = fingerprint
		d, err = s.AuditDashboard(ctx, filter)
		if err != nil || d.Summary.Total != count {
			t.Fatalf("existing fingerprint format rejected: %v", err)
		}
	}
	filter.Fingerprint = ""
	filter.Status = "rejected"
	d, err = s.AuditDashboard(ctx, filter)
	if err != nil || d.Summary.Total != 1 || d.Summary.P95MS != nil {
		t.Fatalf("rejection drill-down: %+v %v", d.Summary, err)
	}
	filter.Status = ""
	filter.Search = "original-visible"
	d, err = s.AuditDashboard(ctx, filter)
	if err != nil || d.Summary.Total != 5 {
		t.Fatal("original SQL search is missing")
	}
	filter.Search = ""
	filter.MinDuration = 1000
	filter.Offset = 2
	d, err = s.AuditDashboard(ctx, filter)
	if err != nil || d.Summary.Total != 3 || len(d.Records.Items) != 1 {
		t.Fatal("slow pagination mismatch")
	}
	filter.Search = "no-match-" + source
	d, err = s.AuditDashboard(ctx, filter)
	if err != nil || d.Summary.Total != 0 || len(d.Records.Items) != 0 || d.Summary.AverageMS != nil {
		t.Fatal("empty results were not explicit")
	}
}
