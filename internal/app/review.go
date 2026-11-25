package app

import (
	"context"
	"encoding/json"
	"github.com/mental1104/pam/internal/domain"
	"strings"
)

// Propose 校验 req 并持久保存只读预检；ctx 控制取消，不创建账本交易。
func (s *Service) Propose(ctx context.Context, req domain.ImportRequest) (domain.ImportProposal, error) {
	req.ReviewCrossSource = true
	prepared, hash, err := prepareImport(req)
	if err != nil {
		return domain.ImportProposal{}, err
	}
	return s.store.Propose(ctx, req, prepared, hash)
}

// Proposal 读取 id 指定预检，ctx 控制取消；跨会话仍能恢复原请求。
func (s *Service) Proposal(ctx context.Context, id string) (domain.ImportProposal, error) {
	if err := domain.ValidateID(id); err != nil {
		return domain.ImportProposal{}, err
	}
	return s.store.Proposal(ctx, id)
}

// Confirm 对 id 的原请求重新执行去重并提交；ctx 控制取消，幂等导入避免确认重试重复写入。
// 预检后发生的新导入会被重新识别为重复；可疑行和异常行持久待核。
func (s *Service) Confirm(ctx context.Context, id string) (domain.ImportResult, error) {
	p, err := s.Proposal(ctx, id)
	if err != nil {
		return domain.ImportResult{}, err
	}
	result, err := s.Import(ctx, p.Request)
	if err != nil {
		return result, err
	}
	return result, s.store.MarkCommitted(ctx, id)
}

// Reviews 返回 state 的持久待核页；ctx 控制取消，limit/offset 遵循现有分页约束。
func (s *Service) Reviews(ctx context.Context, state string, limit, offset int) (domain.Page[domain.ImportReview], error) {
	if err := domain.ValidatePage(limit, offset); err != nil {
		return domain.Page[domain.ImportReview]{}, err
	}
	if state == "" {
		state = "pending"
	}
	if state != "pending" && state != "resolved" {
		return domain.Page[domain.ImportReview]{}, domain.ValidationError{Message: "state must be pending or resolved"}
	}
	return s.store.Reviews(ctx, state, limit, offset)
}

// Resolve 将 id 的显式 decision 转成持久决定；ctx 控制取消，新增或更正必须提交有效完整记录和原因。
func (s *Service) Resolve(ctx context.Context, id string, d domain.ReviewDecision) (json.RawMessage, error) {
	if err := domain.ValidateID(id); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.Reason) == "" {
		return nil, domain.ValidationError{Message: "reason is required"}
	}
	if err := domain.ValidateText("reason", d.Reason, 4000); err != nil {
		return nil, err
	}
	if d.TransactionID != "" {
		if err := domain.ValidateID(d.TransactionID); err != nil {
			return nil, err
		}
	}
	var prepared *domain.PreparedRecord
	if d.Action == "create" || d.Action == "correct" {
		if d.Record == nil {
			return nil, domain.ValidationError{Message: "record is required"}
		}
		// 指纹必须沿用待核来源；避免更正后同外部编号被误判为再次更正。
		source, err := s.store.ReviewSource(ctx, id)
		if err != nil {
			return nil, err
		}
		v := prepare(source, *d.Record, false)
		if v.ValidationFailure != "" {
			return nil, domain.ValidationError{Message: v.ValidationFailure}
		}
		prepared = &v
	}
	return s.store.Resolve(ctx, id, d, prepared)
}

// SourceRanges 返回按来源及已知账户的记录范围；ctx 控制取消，空完整覆盖不表示零交易。
func (s *Service) SourceRanges(ctx context.Context) ([]domain.SourceRange, error) {
	return s.store.SourceRanges(ctx)
}
