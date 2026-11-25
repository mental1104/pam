package app

import (
	"context"
	"github.com/mental1104/pam/internal/domain"
	"regexp"
	"time"
)

// auditFingerprint 接受既有 AST 的 64-bit 指纹和固定错误模式的 SHA-256，不改变任何历史身份。
var auditFingerprint = regexp.MustCompile(`^([0-9a-f]{16}|[0-9a-f]{64})$`)

// AuditDashboard 校验 filter 后读取同一快照；ctx 取消或五秒预算用尽即终止，不生成自引用审计。
// 时间未给定表示全部历史；空数据返回真实零计数和空分位数，绝不生成演示数值。
func (s *Service) AuditDashboard(ctx context.Context, filter domain.AuditFilter) (domain.AuditDashboard, error) {
	if err := domain.ValidatePage(filter.Limit, filter.Offset); err != nil {
		return domain.AuditDashboard{}, err
	}
	if filter.Limit > 100 {
		return domain.AuditDashboard{}, domain.ValidationError{Message: "audit limit must not exceed 100"}
	}
	if filter.Status != "" && filter.Status != "success" && filter.Status != "rejected" && filter.Status != "error" && filter.Status != "running" {
		return domain.AuditDashboard{}, domain.ValidationError{Message: "invalid audit status"}
	}
	if filter.Sort != "" && filter.Sort != "recent" && filter.Sort != "slow" {
		return domain.AuditDashboard{}, domain.ValidationError{Message: "invalid audit sort"}
	}
	if filter.MinDuration < 0 || filter.MinDuration > 86400000 {
		return domain.AuditDashboard{}, domain.ValidationError{Message: "invalid minimum duration"}
	}
	if filter.Fingerprint != "" && !auditFingerprint.MatchString(filter.Fingerprint) {
		return domain.AuditDashboard{}, domain.ValidationError{Message: "invalid audit fingerprint"}
	}
	for name, value := range map[string]string{"error_code": filter.ErrorCode, "source": filter.Source, "search": filter.Search} {
		if err := domain.ValidateText(name, value, 200); err != nil {
			return domain.AuditDashboard{}, err
		}
	}
	var from, to time.Time
	for index, value := range []string{filter.From, filter.To} {
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || parsed.Year() < 1 || parsed.Year() > 9999 {
			return domain.AuditDashboard{}, domain.ValidationError{Message: "audit dates must use RFC3339 with timezone"}
		}
		if index == 0 {
			from = parsed
		} else {
			to = parsed
		}
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return domain.AuditDashboard{}, domain.ValidationError{Message: "from must be before to"}
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.store.AuditDashboard(bounded, filter, from, to)
}
