package domain

import "time"

// AuditFilter 保存审计中心的只读筛选；From 包含、To 排除，耗时单位毫秒，Limit/Offset 为明细分页。
type AuditFilter struct {
	From, To, Status, ErrorCode, Fingerprint, Search, Source, Sort string
	MinDuration                                                    int64
	Limit, Offset                                                  int
}

// AuditRecord 投影 SQL 模式、授权保存的原文/参数与运行元数据；旧行原文字段为 nil，不回填伪原文。
type AuditRecord struct {
	ID             string    `json:"id"`
	Fingerprint    string    `json:"fingerprint"`
	SQL            string    `json:"sql"`
	RawSQL         *string   `json:"raw_sql"`
	ParametersJSON *string   `json:"parameters_json"`
	RawTruncated   bool      `json:"raw_truncated"`
	ExecutedAt     time.Time `json:"executed_at"`
	DurationMS     int64     `json:"duration_ms"`
	RowCount       int64     `json:"row_count"`
	Status         string    `json:"status"`
	ErrorCode      string    `json:"error_code"`
	Source         string    `json:"source"`
}

// AuditSummary 的计数服从当前全部筛选；耗时仅统计 success/error，未完成不进入分位数。
type AuditSummary struct {
	Total     int64  `json:"total"`
	Success   int64  `json:"success"`
	Rejected  int64  `json:"rejected"`
	Errors    int64  `json:"errors"`
	Running   int64  `json:"running"`
	Slow      int64  `json:"slow"`
	AverageMS *int64 `json:"average_ms"`
	P95MS     *int64 `json:"p95_ms"`
	MaxMS     *int64 `json:"max_ms"`
}

// AuditBucket 表示固定时间桶；End 排除边界，零调用桶也必须返回供趋势图明确显示空白。
type AuditBucket struct {
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Success  int64     `json:"success"`
	Rejected int64     `json:"rejected"`
	Errors   int64     `json:"errors"`
	Running  int64     `json:"running"`
}

// AuditCount 是稳定错误码或耗时区间的分组计数，Key 不包含错误原文。
type AuditCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// AuditPattern 按现有指纹聚合当前筛选内调用；SQL 是已脱敏模式，不用模式文字重新计算身份。
type AuditPattern struct {
	Fingerprint string `json:"fingerprint"`
	SQL         string `json:"sql"`
	Calls       int64  `json:"calls"`
	Failures    int64  `json:"failures"`
	AverageMS   *int64 `json:"average_ms"`
	MaxMS       *int64 `json:"max_ms"`
}

// AuditDashboard 的所有统计和明细来自同一只读快照，刷新不会再生成一条 SQL 调用审计。
type AuditDashboard struct {
	GeneratedAt     time.Time         `json:"generated_at"`
	From            time.Time         `json:"from"`
	To              time.Time         `json:"to"`
	BucketSeconds   int64             `json:"bucket_seconds"`
	SlowThresholdMS int64             `json:"slow_threshold_ms"`
	Summary         AuditSummary      `json:"summary"`
	Timeline        []AuditBucket     `json:"timeline"`
	Latency         []AuditCount      `json:"latency"`
	Errors          []AuditCount      `json:"error_codes"`
	Patterns        []AuditPattern    `json:"patterns"`
	Records         Page[AuditRecord] `json:"records"`
}
