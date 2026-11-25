// Package store 以显式 SQL 实现 PAM 的持久化，不向业务层暴露数据库行或事务对象。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mental1104/pam/internal/domain"
)

// Store 保存并发安全连接池；连接池由调用者持有并负责关闭。
type Store struct{ pool *pgxpool.Pool }

// New 绑定应用写入连接池 pool，返回无额外 goroutine 或资源所有权的存储。
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Import 将 req 的 prepared 行作为一个批次保存；payloadHash 识别幂等键内容变化。
// ctx 控制锁等待及所有数据库操作；基础设施错误回滚整个批次，成功时返回互斥行统计。
func (s *Store) Import(ctx context.Context, req domain.ImportRequest, prepared []domain.PreparedRecord, payloadHash string) (domain.ImportResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.ImportResult{}, err
	}
	// 请求取消后仍给回滚独立的短清理窗口，网络故障时不无限占用写入连接。
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = tx.Rollback(cleanupCtx)
	}()
	// 单用户导入通过事务锁串行化，涵盖指纹候选检查和插入，避免并发请求绕过人工确认。
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(742190011)"); err != nil {
		return domain.ImportResult{}, err
	}
	var batchID, existingHash string
	var previous []byte
	err = tx.QueryRow(ctx, "SELECT id::text,payload_hash,result FROM pam.import_batches WHERE source=$1 AND idempotency_key=$2", req.Source, req.IdempotencyKey).Scan(&batchID, &existingHash, &previous)
	if err == nil {
		if existingHash != payloadHash {
			return domain.ImportResult{}, domain.ErrConflict
		}
		var replay domain.ImportResult
		if err = json.Unmarshal(previous, &replay); err != nil {
			return domain.ImportResult{}, fmt.Errorf("decode stored import result: %w", err)
		}
		// 重放只报告本次实际产生的新增数；原始逐行失败和待确认状态仍保留。
		replay.Replayed = true
		replay.Skipped += replay.Created
		replay.Created = 0
		for i := range replay.Rows {
			if replay.Rows[i].Status == "created" {
				replay.Rows[i].Status = "skipped"
				replay.Rows[i].Reason = "batch_replay"
			}
		}
		return replay, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ImportResult{}, err
	}
	err = tx.QueryRow(ctx, "INSERT INTO pam.import_batches(source,idempotency_key,source_file_hash,payload_hash) VALUES($1,$2,NULLIF($3,''),$4) RETURNING id::text", req.Source, req.IdempotencyKey, req.SourceFileHash, payloadHash).Scan(&batchID)
	if err != nil {
		return domain.ImportResult{}, err
	}
	result := domain.ImportResult{BatchID: batchID, Rows: make([]domain.ImportRowResult, 0, len(prepared))}
	for i, row := range prepared {
		outcome, err := importOne(ctx, tx, req, batchID, row)
		if err != nil {
			return domain.ImportResult{}, err
		}
		outcome.Index = i
		original := row.Input.Original
		if len(original) == 0 {
			original = json.RawMessage("null")
		}
		// 来源与规范化输入分列；去掉输入副本中的原始数据，避免混淆二者的责任。
		submission := row.Input
		submission.Original = nil
		normalized, err := json.Marshal(submission)
		if err != nil {
			return domain.ImportResult{}, err
		}
		var recordID string
		err = tx.QueryRow(ctx, "INSERT INTO pam.import_records(batch_id,row_index,row_key,original,normalized_submission,transaction_id,status,reason) VALUES($1,$2,NULLIF($3,''),$4::jsonb,$5::jsonb,$6,$7,$8) RETURNING id::text", batchID, i, row.Input.RowKey, string(original), string(normalized), outcome.TransactionID, outcome.Status, outcome.Reason).Scan(&recordID)
		if err != nil {
			return domain.ImportResult{}, err
		}
		// 待核与来源行同事务持久化，客户端断开不会丢失事项。
		if outcome.Status == "duplicated" || outcome.Status == "failed" {
			kind := "duplicate"
			if strings.Contains(outcome.Reason, "changed") {
				kind = "correction"
			}
			if outcome.Status == "failed" {
				kind = "parse_error"
			}
			if _, err = tx.Exec(ctx, "INSERT INTO pam.import_reviews(record_id,kind) VALUES($1,$2)", recordID, kind); err != nil {
				return domain.ImportResult{}, err
			}
		}
		result.Rows = append(result.Rows, outcome)
		switch outcome.Status {
		case "created":
			result.Created++
		case "skipped":
			result.Skipped++
		case "duplicated":
			result.Duplicated++
		case "failed":
			result.Failed++
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return domain.ImportResult{}, err
	}
	if _, err = tx.Exec(ctx, "UPDATE pam.import_batches SET result=$1::jsonb WHERE id=$2", string(encoded), batchID); err != nil {
		return domain.ImportResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return domain.ImportResult{}, err
	}
	return result, nil
}

// importOne 在 tx 的批次中识别 row 是否已存在、需要确认或可创建；req 提供来源身份，batchID 是当前批次。
// ctx 传播取消；单行约束错误使用保存点恢复为 failed，其余错误交由调用者回滚批次。
func importOne(ctx context.Context, tx pgx.Tx, req domain.ImportRequest, batchID string, row domain.PreparedRecord) (domain.ImportRowResult, error) {
	decision, err := classifyOne(ctx, tx, req, row)
	if err != nil || decision.Status != "created" {
		return decision, err
	}
	return insertOne(ctx, tx, req, batchID, row)
}

// classifyOne 在 tx 中只读判断 req 的 row；created 表示候选新增，不写账本；ctx 控制取消。
func classifyOne(ctx context.Context, tx pgx.Tx, req domain.ImportRequest, row domain.PreparedRecord) (domain.ImportRowResult, error) {
	if row.ValidationFailure != "" {
		return domain.ImportRowResult{Status: "failed", Reason: row.ValidationFailure}, nil
	}
	var existingID, existingFingerprint string
	if req.SourceFileHash != "" {
		var reason string
		var normalized []byte
		err := tx.QueryRow(ctx, `SELECT t.id::text,t.fingerprint,r.reason,r.normalized_submission FROM pam.import_records r JOIN pam.import_batches b ON b.id=r.batch_id JOIN pam.transactions t ON t.id=r.transaction_id WHERE b.source=$1 AND b.source_file_hash=$2 AND r.row_key=$3 AND r.status IN ('created','skipped') ORDER BY r.created_at,r.id LIMIT 1`, req.Source, req.SourceFileHash, row.Input.RowKey).Scan(&existingID, &existingFingerprint, &reason, &normalized)
		if err == nil {
			if existingFingerprint == row.Fingerprint || (reason == "review_confirmed_existing" && sameSubmission(normalized, row)) {
				return domain.ImportRowResult{Status: "skipped", Reason: "source_row_exists", TransactionID: &existingID}, nil
			}
			return domain.ImportRowResult{Status: "duplicated", Reason: "source_row_changed", TransactionID: &existingID}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ImportRowResult{}, err
		}
	}
	if row.Input.ExternalID != "" {
		err := tx.QueryRow(ctx, "SELECT id::text,fingerprint FROM pam.transactions WHERE source=$1 AND external_id=$2", req.Source, row.Input.ExternalID).Scan(&existingID, &existingFingerprint)
		if err == nil {
			if existingFingerprint == row.Fingerprint {
				return domain.ImportRowResult{Status: "skipped", Reason: "external_id_exists", TransactionID: &existingID}, nil
			}
			return domain.ImportRowResult{Status: "duplicated", Reason: "external_id_changed", TransactionID: &existingID}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ImportRowResult{}, err
		}
	}
	if !row.Input.ConfirmNew && (row.Input.ExternalID == "" || req.ReviewCrossSource) {
		// 跨来源相似项仅产生候选；相近金额、时间、商户不构成同一交易的证明。
		err := tx.QueryRow(ctx, `SELECT id::text FROM pam.transactions WHERE ($9='' AND source=$6 AND fingerprint=$7) OR ($8 AND (source<>$6 OR $9='') AND currency=$1 AND direction=$2 AND amount=$3::numeric AND occurred_at BETWEEN $4::timestamptz - interval '1 minute' AND $4::timestamptz + interval '1 minute' AND merchant=$5) ORDER BY created_at,id LIMIT 1`, row.Input.Currency, row.Input.Direction, row.Amount, row.OccurredAt, row.Input.Merchant, req.Source, row.Fingerprint, req.ReviewCrossSource, row.Input.ExternalID).Scan(&existingID)
		if err == nil {
			return domain.ImportRowResult{Status: "duplicated", Reason: "fingerprint_requires_confirmation", TransactionID: &existingID}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ImportRowResult{}, err
		}
		if req.ReviewCrossSource && row.Input.ExternalID == "" {
			// 同来源同一时刻同商户但金额变化只是疑似更正，仍可能是合法独立交易。
			err = tx.QueryRow(ctx, `SELECT id::text FROM pam.transactions WHERE source=$1 AND occurred_at=$2 AND merchant=$3 AND currency=$4 AND direction=$5 AND amount<>$6::numeric ORDER BY created_at,id LIMIT 1`, req.Source, row.OccurredAt, row.Input.Merchant, row.Input.Currency, row.Input.Direction, row.Amount).Scan(&existingID)
			if err == nil {
				return domain.ImportRowResult{Status: "duplicated", Reason: "possible_history_changed", TransactionID: &existingID}, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return domain.ImportRowResult{}, err
			}
		}
	}
	return domain.ImportRowResult{Status: "created", Reason: "new_candidate"}, nil
}

// insertOne 将 row 写入 req/batchID 的 tx；ctx 传播取消，保存点隔离单行约束失败，不提交事务。
func insertOne(ctx context.Context, tx pgx.Tx, req domain.ImportRequest, batchID string, row domain.PreparedRecord) (domain.ImportRowResult, error) {
	// 保存点只覆盖流水创建；单行约束失败不阻止同批次其他合法数据入库。
	if _, err := tx.Exec(ctx, "SAVEPOINT import_row"); err != nil {
		return domain.ImportRowResult{}, err
	}
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO pam.transactions(occurred_at,direction,amount,currency,merchant,description,category,source,external_id,import_batch_id,fingerprint) VALUES($1,$2,$3::numeric,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11) RETURNING id::text`, row.OccurredAt, row.Input.Direction, row.Amount, row.Input.Currency, row.Input.Merchant, row.Input.Description, row.Input.Category, req.Source, row.Input.ExternalID, batchID, row.Fingerprint).Scan(&id)
	if err != nil {
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) && (strings.HasPrefix(databaseError.Code, "22") || strings.HasPrefix(databaseError.Code, "23")) {
			if _, rollbackErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT import_row"); rollbackErr != nil {
				return domain.ImportRowResult{}, rollbackErr
			}
			if _, releaseErr := tx.Exec(ctx, "RELEASE SAVEPOINT import_row"); releaseErr != nil {
				return domain.ImportRowResult{}, releaseErr
			}
			return domain.ImportRowResult{Status: "failed", Reason: "database_constraint_" + databaseError.Code}, nil
		}
		return domain.ImportRowResult{}, err
	}
	if _, err = tx.Exec(ctx, "RELEASE SAVEPOINT import_row"); err != nil {
		return domain.ImportRowResult{}, err
	}
	return domain.ImportRowResult{Status: "created", Reason: "created", TransactionID: &id}, nil
}

// transactionFrom 保留最初原始证据，并投影最近人工更正；一对一连接避免来源重放放大流水计数。
const transactionFrom = `pam.transactions t LEFT JOIN LATERAL (
 SELECT r.original || COALESCE((SELECT jsonb_build_object('pam_last_correction',v.resolution,'pam_source_date',to_char(t.occurred_at AT TIME ZONE 'Asia/Shanghai','YYYY-MM-DD')) FROM pam.transaction_revisions rev JOIN pam.import_reviews v ON v.id=rev.review_id WHERE rev.transaction_id=t.id ORDER BY rev.created_at DESC,rev.id DESC LIMIT 1),'{}'::jsonb) AS original FROM pam.import_records r
 WHERE r.transaction_id=t.id AND r.status='created' ORDER BY r.created_at,r.id LIMIT 1
) provenance ON true`

// transactionPlatforms 同时用于投影与 WHERE，保证图标和全库筛选只有一个规范化口径。
// 只匹配来源字段的明确完整别名；无法识别的附加文字不覆盖已确认来源，完全未知才归 other。
const transactionPlatforms = `COALESCE(NULLIF(ARRAY(
 SELECT mapped.platform FROM (
  SELECT CASE lower(regexp_replace(btrim(label),'[[:space:]]+','','g'))
   WHEN '微信' THEN 'wechat' WHEN '微信支付' THEN 'wechat' WHEN 'wechat' THEN 'wechat' WHEN 'wechatpay' THEN 'wechat' WHEN 'weixin' THEN 'wechat'
   WHEN '支付宝' THEN 'alipay' WHEN 'alipay' THEN 'alipay' WHEN '用户补充（支付宝余额）' THEN 'alipay'
   WHEN '招商银行' THEN 'cmb' WHEN '中国招商银行' THEN 'cmb' WHEN '招行' THEN 'cmb' WHEN 'cmb' THEN 'cmb' WHEN '招商银行（用户补充）' THEN 'cmb'
   WHEN '建设银行' THEN 'ccb' WHEN '中国建设银行' THEN 'ccb' WHEN '建行' THEN 'ccb' WHEN 'ccb' THEN 'ccb'
   ELSE NULL END AS platform
  FROM regexp_split_to_table(concat_ws(';',
   provenance.original->>'来源平台',provenance.original->>'主记录平台',
   CASE WHEN t.source='cmb_statement' THEN 'cmb' ELSE NULL END),'[;；,，、/|]+') AS labels(label)
 ) mapped WHERE mapped.platform IS NOT NULL
 GROUP BY mapped.platform
 ORDER BY CASE mapped.platform WHEN 'wechat' THEN 1 WHEN 'alipay' THEN 2 WHEN 'cmb' THEN 3 WHEN 'ccb' THEN 4 END
),ARRAY[]::text[]),ARRAY['other']::text[])`

const transactionColumns = `t.id::text,t.occurred_at,t.direction,t.amount::text,t.currency,t.merchant,t.description,t.category,t.source,COALESCE(t.external_id,''),t.import_batch_id::text,COALESCE(provenance.original,'{}'::jsonb),t.created_at,t.updated_at,` + transactionPlatforms

// scanTransaction 读取 row 的固定投影及来源数组到新业务值；返回解码错误，不修改原始来源或共享状态。
func scanTransaction(row pgx.Row) (domain.Transaction, error) {
	var t domain.Transaction
	err := row.Scan(&t.ID, &t.OccurredAt, &t.Direction, &t.Amount, &t.Currency, &t.Merchant, &t.Description, &t.Category, &t.Source, &t.ExternalID, &t.ImportBatchID, &t.Original, &t.CreatedAt, &t.UpdatedAt, &t.Platforms)
	return t, err
}

// GetTransaction 读取 id 对应流水和初次来源；ctx 控制取消，缺失映射为 ErrNotFound。
func (s *Store) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	t, err := scanTransaction(s.pool.QueryRow(ctx, "SELECT "+transactionColumns+" FROM "+transactionFrom+" WHERE t.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return t, domain.ErrNotFound
	}
	return t, err
}

// ListTransactions 使用 filter 组合参数化条件并返回分页；ctx 控制取消，不内插用户 SQL。
// COUNT 和行查询共享包括 platform 在内的 WHERE，平台数组只由已确认来源字段派生，不回写数据。
func (s *Store) ListTransactions(ctx context.Context, filter domain.TransactionFilter) (domain.Page[domain.Transaction], error) {
	args := []any{}
	conditions := []string{"TRUE"}
	// add 的 condition 仅来自本文件常量，value 始终作为 PostgreSQL 参数传入。
	add := func(condition string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(condition, len(args)))
	}
	if filter.From != "" {
		add("t.occurred_at >= $%d::timestamptz", filter.From)
	}
	if filter.To != "" {
		add("t.occurred_at < $%d::timestamptz", filter.To)
	}
	if filter.Direction != "" {
		add("t.direction = $%d", filter.Direction)
	}
	if filter.Category != "" {
		add("t.category = $%d", filter.Category)
	}
	if filter.Source != "" {
		add("t.source = $%d", filter.Source)
	}
	if filter.Platform != "" {
		add("$%d = ANY("+transactionPlatforms+")", filter.Platform)
	}
	if filter.Merchant != "" {
		add("t.merchant ILIKE $%d", literalSearch(filter.Merchant))
	}
	if filter.MinAmount != "" {
		add("t.amount >= $%d::numeric", filter.MinAmount)
	}
	if filter.MaxAmount != "" {
		add("t.amount <= $%d::numeric", filter.MaxAmount)
	}
	if filter.Search != "" {
		args = append(args, literalSearch(filter.Search))
		conditions = append(conditions, fmt.Sprintf("(t.merchant ILIKE $%[1]d OR t.description ILIKE $%[1]d)", len(args)))
	}
	where := strings.Join(conditions, " AND ")
	page := domain.Page[domain.Transaction]{Items: []domain.Transaction{}, Limit: filter.Limit, Offset: filter.Offset}
	countFrom := "pam.transactions t"
	if filter.Platform != "" {
		countFrom = transactionFrom
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+countFrom+" WHERE "+where, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.pool.Query(ctx, "SELECT "+transactionColumns+" FROM "+transactionFrom+" WHERE "+where+fmt.Sprintf(" ORDER BY t.occurred_at DESC,t.id LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanTransaction(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

// literalSearch 将 value 转为字面包含搜索；转义通配符，避免普通搜索词改变匹配语义。
func literalSearch(value string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value) + "%"
}

const assetColumns = `id::text,name,category,brand,model,serial_number,purchase_date::text,purchase_price::text,currency,merchant,related_transaction_id::text,notes,status,created_at,updated_at`

// scanAsset 将 row 的固定列映射为库存值，保留未知购买日期、价格和关联 ID 的空值。
func scanAsset(row pgx.Row) (domain.Asset, error) {
	var a domain.Asset
	err := row.Scan(&a.ID, &a.Name, &a.Category, &a.Brand, &a.Model, &a.SerialNumber, &a.PurchaseDate, &a.PurchasePrice, &a.Currency, &a.Merchant, &a.RelatedTransactionID, &a.Notes, &a.Status, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// ListAssets 按创建时间返回 limit/offset 页；ctx 传播取消，空列表仍保留 total 和分页字段。
func (s *Store) ListAssets(ctx context.Context, limit, offset int) (domain.Page[domain.Asset], error) {
	page := domain.Page[domain.Asset]{Items: []domain.Asset{}, Limit: limit, Offset: offset}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM pam.assets").Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+assetColumns+" FROM pam.assets ORDER BY created_at DESC,id LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanAsset(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

// GetAsset 查询 id 的库存；ctx 控制取消，缺失返回 ErrNotFound。
func (s *Store) GetAsset(ctx context.Context, id string) (domain.Asset, error) {
	a, err := scanAsset(s.pool.QueryRow(ctx, "SELECT "+assetColumns+" FROM pam.assets WHERE id=$1", id))
	return a, assetError(err)
}

// assetArgs 将已校验的 input 按写入列顺序展开，保留空指针以写为 SQL NULL。
func assetArgs(input domain.AssetInput) []any {
	return []any{input.Name, input.Category, input.Brand, input.Model, input.SerialNumber, input.PurchaseDate, input.PurchasePrice, input.Currency, input.Merchant, input.RelatedTransactionID, input.Notes, input.Status}
}

// CreateAsset 在单条原子 INSERT 中保存 input；ctx 取消传播，关联流水不存在映射为输入错误。
func (s *Store) CreateAsset(ctx context.Context, input domain.AssetInput) (domain.Asset, error) {
	a, err := scanAsset(s.pool.QueryRow(ctx, `INSERT INTO pam.assets(name,category,brand,model,serial_number,purchase_date,purchase_price,currency,merchant,related_transaction_id,notes,status) VALUES($1,$2,$3,$4,$5,$6::date,$7::numeric,$8,$9,$10::uuid,$11,$12) RETURNING `+assetColumns, assetArgs(input)...))
	return a, assetError(err)
}

// UpdateAsset 原子替换 id 的可编辑字段为 input 并刷新更新时间；ctx 取消传播，缺失返回 ErrNotFound。
func (s *Store) UpdateAsset(ctx context.Context, id string, input domain.AssetInput) (domain.Asset, error) {
	args := append(assetArgs(input), id)
	a, err := scanAsset(s.pool.QueryRow(ctx, `UPDATE pam.assets SET name=$1,category=$2,brand=$3,model=$4,serial_number=$5,purchase_date=$6::date,purchase_price=$7::numeric,currency=$8,merchant=$9,related_transaction_id=$10::uuid,notes=$11,status=$12,updated_at=clock_timestamp() WHERE id=$13 RETURNING `+assetColumns, args...))
	return a, assetError(err)
}

// DeleteAsset 删除 id 的库存，不触及财务记录；ctx 控制取消，受影响行数为零返回 ErrNotFound。
func (s *Store) DeleteAsset(ctx context.Context, id string) error {
	result, err := s.pool.Exec(ctx, "DELETE FROM pam.assets WHERE id=$1", id)
	if err != nil {
		return assetError(err)
	}
	if result.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// assetError 把 err 的稳定数据库约束映射为业务错误；未知错误保留给应用日志，调用者不得直接公开。
func assetError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) && databaseError.Code == "23503" {
		return domain.ValidationError{Message: "related transaction does not exist"}
	}
	return err
}
