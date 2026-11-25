package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mental1104/pam/internal/domain"
)

// Propose 在只读分类后保存 req 和 hash；prepared 是共享业务校验结果，ctx 控制取消。
// 重用相同幂等键但不同内容返回冲突；created 是预估数，不会创建交易。
func (s *Store) Propose(ctx context.Context, req domain.ImportRequest, prepared []domain.PreparedRecord, hash string) (domain.ImportProposal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ImportProposal{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(742190011)"); err != nil {
		return domain.ImportProposal{}, err
	}
	var id, prior string
	var priorRequest, priorPreview []byte
	var committed bool
	err = tx.QueryRow(ctx, "SELECT id::text,payload_hash,request,preview,committed FROM pam.import_proposals WHERE source=$1 AND idempotency_key=$2", req.Source, req.IdempotencyKey).Scan(&id, &prior, &priorRequest, &priorPreview, &committed)
	if err == nil {
		if prior != hash {
			return domain.ImportProposal{}, domain.ErrConflict
		}
		// 持锁时复用当前事务读取，避免并发请求占满连接池后再借连接造成饥饿。
		p := domain.ImportProposal{ID: id, Committed: committed}
		if err = json.Unmarshal(priorRequest, &p.Request); err != nil {
			return p, err
		}
		err = json.Unmarshal(priorPreview, &p.Preview)
		return p, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ImportProposal{}, err
	}
	preview := domain.ImportResult{Rows: []domain.ImportRowResult{}}
	for i, row := range prepared {
		outcome, err := classifyOne(ctx, tx, req, row)
		if err != nil {
			return domain.ImportProposal{}, err
		}
		// 同预检内相似行也必须待核，不能依赖后续提交才提示。
		if outcome.Status == "created" {
			for j, previous := range prepared[:i] {
				if previous.ValidationFailure != "" || preview.Rows[j].Status != "created" {
					continue
				}
				stable := (req.SourceFileHash != "" && row.Input.RowKey == previous.Input.RowKey) || (row.Input.ExternalID != "" && row.Input.ExternalID == previous.Input.ExternalID)
				if stable {
					outcome.Status = "skipped"
					outcome.Reason = "within_batch_identity_exists"
					if previous.Fingerprint != row.Fingerprint {
						outcome.Status = "duplicated"
						outcome.Reason = "within_batch_identity_changed"
					}
					break
				}
				if !row.Input.ConfirmNew && row.Input.ExternalID == "" && previous.Input.ExternalID == "" && previous.Fingerprint == row.Fingerprint {
					outcome.Status = "duplicated"
					outcome.Reason = "within_batch_requires_confirmation"
					break
				}
			}
		}
		outcome.Index = i
		preview.Rows = append(preview.Rows, outcome)
		switch outcome.Status {
		case "created":
			preview.Created++
		case "skipped":
			preview.Skipped++
		case "duplicated":
			preview.Duplicated++
		case "failed":
			preview.Failed++
		}
	}
	body, _ := json.Marshal(req)
	encoded, _ := json.Marshal(preview)
	err = tx.QueryRow(ctx, "INSERT INTO pam.import_proposals(source,idempotency_key,payload_hash,request,preview) VALUES($1,$2,$3,$4::jsonb,$5::jsonb) RETURNING id::text", req.Source, req.IdempotencyKey, hash, string(body), string(encoded)).Scan(&id)
	if err != nil {
		return domain.ImportProposal{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ImportProposal{}, err
	}
	return domain.ImportProposal{ID: id, Request: req, Preview: preview}, nil
}

// Proposal 读取持久预检 id；ctx 控制取消，不存在返回 ErrNotFound。
func (s *Store) Proposal(ctx context.Context, id string) (domain.ImportProposal, error) {
	var p domain.ImportProposal
	var request, preview []byte
	err := s.pool.QueryRow(ctx, "SELECT id::text,request,preview,committed FROM pam.import_proposals WHERE id=$1", id).Scan(&p.ID, &request, &preview, &p.Committed)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, domain.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(request, &p.Request); err != nil {
		return p, err
	}
	err = json.Unmarshal(preview, &p.Preview)
	return p, err
}

// MarkCommitted 标记已通过幂等导入完成的 id；ctx 取消时可安全重试确认。
func (s *Store) MarkCommitted(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, "UPDATE pam.import_proposals SET committed=true WHERE id=$1", id)
	return err
}

// Reviews 返回 state 的持久待核页；limit/offset 已由应用校验，ctx 控制取消。
func (s *Store) Reviews(ctx context.Context, state string, limit, offset int) (domain.Page[domain.ImportReview], error) {
	p := domain.Page[domain.ImportReview]{Items: []domain.ImportReview{}, Limit: limit, Offset: offset}
	err := s.pool.QueryRow(ctx, "SELECT count(*) FROM pam.import_reviews WHERE state=$1", state).Scan(&p.Total)
	if err != nil {
		return p, err
	}
	rows, err := s.pool.Query(ctx, `SELECT v.id::text,v.kind,v.state,b.source,r.normalized_submission,r.original,r.transaction_id::text,COALESCE(v.resolution,'null'::jsonb) FROM pam.import_reviews v JOIN pam.import_records r ON r.id=v.record_id JOIN pam.import_batches b ON b.id=r.batch_id WHERE v.state=$1 ORDER BY v.created_at,v.id LIMIT $2 OFFSET $3`, state, limit, offset)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var v domain.ImportReview
		var normalized, original []byte
		if err = rows.Scan(&v.ID, &v.Kind, &v.State, &v.Source, &normalized, &original, &v.TransactionID, &v.Resolution); err != nil {
			return p, err
		}
		if err = json.Unmarshal(normalized, &v.Record); err != nil {
			return p, err
		}
		v.Record.Original = original
		p.Items = append(p.Items, v)
	}
	return p, rows.Err()
}

// Resolve 在单事务内处理 id 的 decision，prepared 为可选修正行；ctx 控制锁及取消。
// 再次相同决定安全重放，不同决定冲突；更正保留前后账本和证据，不更改历史来源正文。
func (s *Store) Resolve(ctx context.Context, id string, d domain.ReviewDecision, prepared *domain.PreparedRecord) (json.RawMessage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(742190011)"); err != nil {
		return nil, err
	}
	var state, source, batch, recordID string
	var normalized, original, prior []byte
	var target *string
	err = tx.QueryRow(ctx, `SELECT v.state,b.source,b.id::text,r.id::text,r.normalized_submission,r.original,r.transaction_id::text,v.resolution FROM pam.import_reviews v JOIN pam.import_records r ON r.id=v.record_id JOIN pam.import_batches b ON b.id=r.batch_id WHERE v.id=$1 FOR UPDATE OF v`, id).Scan(&state, &source, &batch, &recordID, &normalized, &original, &target, &prior)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(d)
	if state == "resolved" {
		var old domain.ReviewDecision
		if json.Unmarshal(prior, &old) != nil {
			return nil, domain.ErrConflict
		}
		check, _ := json.Marshal(old)
		if string(check) != string(encoded) {
			return nil, domain.ErrConflict
		}
		return prior, nil
	}
	if d.TransactionID != "" {
		target = &d.TransactionID
	}
	switch d.Action {
	case "dismiss": // 明确忽略仍保留来源及理由。
	case "link", "correct":
		if target == nil {
			return nil, domain.ValidationError{Message: "transaction_id is required"}
		}
		var before []byte
		err = tx.QueryRow(ctx, "SELECT to_jsonb(t) FROM pam.transactions t WHERE id=$1 FOR UPDATE", *target).Scan(&before)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if d.Action == "correct" {
			if prepared == nil {
				return nil, domain.ValidationError{Message: "correct requires a valid record"}
			}
			r := prepared.Input
			var priorFields struct {
				Source string `json:"source"`
			}
			if err = json.Unmarshal(before, &priorFields); err != nil {
				return nil, err
			}
			canonical, _ := json.Marshal([]string{priorFields.Source, prepared.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), r.Direction, prepared.Amount, r.Currency, r.Merchant, r.Description})
			digest := sha256.Sum256(canonical)
			prepared.Fingerprint = hex.EncodeToString(digest[:])
			// 禁止通过更正改变稳定来源身份；只修改明确可编辑的业务字段。
			_, err = tx.Exec(ctx, `UPDATE pam.transactions SET occurred_at=$2,direction=$3,amount=$4::numeric,currency=$5,merchant=$6,description=$7,category=$8,fingerprint=$9,updated_at=clock_timestamp() WHERE id=$1`, *target, prepared.OccurredAt, r.Direction, prepared.Amount, r.Currency, r.Merchant, r.Description, r.Category, prepared.Fingerprint)
			if err != nil {
				return nil, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO pam.transaction_revisions(transaction_id,review_id,before_content,after_content,reason,evidence) SELECT id,$2,$3::jsonb,to_jsonb(t),$4,$5::jsonb FROM pam.transactions t WHERE id=$1`, *target, id, string(before), d.Reason, string(original))
			if err != nil {
				return nil, err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE pam.import_records SET transaction_id=$2,status='skipped',reason='review_confirmed_existing' WHERE id=$1", recordID, *target); err != nil {
			return nil, err
		}
	case "create":
		var row domain.PreparedRecord
		if prepared == nil {
			return nil, domain.ValidationError{Message: "create requires a valid record"}
		}
		row = *prepared
		var originalSubmission domain.ImportRecord
		if err = json.Unmarshal(normalized, &originalSubmission); err != nil {
			return nil, err
		}
		if (originalSubmission.RowKey != "" && row.Input.RowKey != originalSubmission.RowKey) || (originalSubmission.ExternalID != "" && row.Input.ExternalID != originalSubmission.ExternalID) {
			return nil, domain.ValidationError{Message: "source identity cannot be changed during review"}
		}
		// 明确确认新增可跳过相似候选；真实外部身份和同文件行身份仍不可绕过。
		row.Input.ConfirmNew = true
		req := domain.ImportRequest{Source: source}
		var fileHash string
		if err = tx.QueryRow(ctx, "SELECT COALESCE(source_file_hash,'') FROM pam.import_batches WHERE id=$1", batch).Scan(&fileHash); err != nil {
			return nil, err
		}
		req.SourceFileHash = fileHash
		if fileHash != "" && row.Input.RowKey == "" {
			return nil, domain.ValidationError{Message: "row_key is required"}
		}
		outcome, e := importOne(ctx, tx, req, batch, row)
		if e != nil {
			return nil, e
		}
		if outcome.Status != "created" {
			return nil, domain.ErrConflict
		}
		_, err = tx.Exec(ctx, "UPDATE pam.import_records SET transaction_id=$2,row_key=NULLIF($3,''),status='created',reason='review_confirmed_new' WHERE id=$1", recordID, outcome.TransactionID, row.Input.RowKey)
		if err != nil {
			return nil, err
		}
	default:
		return nil, domain.ValidationError{Message: "unsupported review action"}
	}
	if _, err = tx.Exec(ctx, "UPDATE pam.import_reviews SET state='resolved',resolution=$2::jsonb,resolved_at=clock_timestamp() WHERE id=$1", id, string(encoded)); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return encoded, nil
}

// SourceRanges 以已确认来源的账户标记汇总已入库范围；ctx 控制取消，不把观察范围宣称为完整账单覆盖。
func (s *Store) SourceRanges(ctx context.Context) ([]domain.SourceRange, error) {
	rows, err := s.pool.Query(ctx, `WITH observed AS (SELECT DISTINCT b.source,t.id,COALESCE(r.original->>'pam_account',concat(r.original->>'账户类型',r.original->>'卡号尾号')) AS account,t.occurred_at FROM pam.import_records r JOIN pam.import_batches b ON b.id=r.batch_id JOIN pam.transactions t ON t.id=r.transaction_id WHERE r.status IN ('created','skipped')) SELECT source,account,count(*),min(occurred_at)::text,max(occurred_at)::text FROM observed GROUP BY source,account ORDER BY source,account`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.SourceRange{}
	for rows.Next() {
		var v domain.SourceRange
		v.ConfirmedCoverage = []string{}
		if err = rows.Scan(&v.Source, &v.Account, &v.Count, &v.First, &v.Last); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}

// ReviewSource 根据 id 读取稳定来源；ctx 控制取消，不受待核分页或已解决状态影响。
func (s *Store) ReviewSource(ctx context.Context, id string) (string, error) {
	var source string
	err := s.pool.QueryRow(ctx, `SELECT b.source FROM pam.import_reviews v JOIN pam.import_records r ON r.id=v.record_id JOIN pam.import_batches b ON b.id=r.batch_id WHERE v.id=$1`, id).Scan(&source)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrNotFound
	}
	return source, err
}

// sameSubmission 对照 normalized 来源行与 row 的规范字段；跨来源人工链接不能比较含来源的账本指纹。
func sameSubmission(normalized []byte, row domain.PreparedRecord) bool {
	var prior domain.ImportRecord
	if json.Unmarshal(normalized, &prior) != nil {
		return false
	}
	amount, err := domain.NormalizeMoney(prior.Amount)
	if err != nil {
		return false
	}
	stamp, err := time.Parse(time.RFC3339Nano, prior.OccurredAt)
	if err != nil {
		return false
	}
	return amount == row.Amount && stamp.Equal(row.OccurredAt) && prior.Direction == row.Input.Direction && prior.Currency == row.Input.Currency && prior.Merchant == row.Input.Merchant && prior.Description == row.Input.Description
}
