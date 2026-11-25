// Package app 实现 REST 与 MCP 共用的业务规则；数据库边界由 store 包承接。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mental1104/pam/internal/domain"
	"github.com/mental1104/pam/internal/store"
)

// Service 保存可并发使用的业务存储；不处理 HTTP、MCP 或身份校验。
type Service struct{ store *store.Store }

// New 使用 pool 创建服务；pool 的生命周期由应用入口负责，服务不会关闭连接池。
func New(pool *pgxpool.Pool) *Service { return &Service{store: store.New(pool)} }

var fileHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Import 校验 req 并原子保存批次、每行来源和成功流水；ctx 取消时整个未提交批次回滚。
// 单行业务错误计入 Failed，数据库不可用等基础设施错误返回 error 而不伪装为成功。
func (s *Service) Import(ctx context.Context, req domain.ImportRequest) (domain.ImportResult, error) {
	prepared, hash, err := prepareImport(req)
	if err != nil {
		return domain.ImportResult{}, err
	}
	return s.store.Import(ctx, req, prepared, hash)
}

// prepareImport 对 req 复用正式导入校验；返回逐行准备结果和幂等内容摘要，不访问存储。
func prepareImport(req domain.ImportRequest) ([]domain.PreparedRecord, string, error) {
	if strings.TrimSpace(req.Source) == "" || strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, "", domain.ValidationError{Message: "source and idempotency_key are required"}
	}
	if len(req.Records) < 1 || len(req.Records) > 1000 {
		return nil, "", domain.ValidationError{Message: "records must contain between 1 and 1000 rows"}
	}
	if err := domain.ValidateText("source", req.Source, 200); err != nil {
		return nil, "", err
	}
	if err := domain.ValidateText("idempotency_key", req.IdempotencyKey, 200); err != nil {
		return nil, "", err
	}
	if req.SourceFileHash != "" && !fileHashPattern.MatchString(req.SourceFileHash) {
		return nil, "", domain.ValidationError{Message: "source_file_hash must be a lowercase SHA-256 hex digest"}
	}
	prepared := make([]domain.PreparedRecord, len(req.Records))
	for i, row := range req.Records {
		prepared[i] = prepare(req.Source, row, req.SourceFileHash != "")
	}
	// 哈希只用于识别幂等键内容变化，不把原始请求写入日志或审计。
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", domain.ValidationError{Message: "original records must contain valid JSON"}
	}
	digest := sha256.Sum256(body)
	return prepared, hex.EncodeToString(digest[:]), nil
}

// prepare 为 source 来源的 row 建立精确金额、绝对时间及指纹；requiresRowKey 表示有完整文件身份。
// 返回值携带单行失败原因，调用者仍须保留原始来源以便修正和追溯。
func prepare(source string, row domain.ImportRecord, requiresRowKey bool) domain.PreparedRecord {
	p := domain.PreparedRecord{Input: row}
	fail := func(message string) domain.PreparedRecord { p.ValidationFailure = message; return p }
	if requiresRowKey && strings.TrimSpace(row.RowKey) == "" {
		return fail("row_key_required_for_source_file")
	}
	for name, value := range map[string]string{"row_key": row.RowKey, "merchant": row.Merchant, "description": row.Description, "category": row.Category, "external_id": row.ExternalID} {
		if err := domain.ValidateText(name, value, 4000); err != nil {
			return fail(err.Error())
		}
	}
	if row.Direction != "income" && row.Direction != "expense" {
		return fail("direction_must_be_income_or_expense")
	}
	if err := domain.ValidateCurrency(row.Currency); err != nil {
		return fail(err.Error())
	}
	var err error
	p.Amount, err = domain.NormalizeMoney(row.Amount)
	if err != nil {
		return fail(err.Error())
	}
	p.OccurredAt, err = time.Parse(time.RFC3339Nano, row.OccurredAt)
	if err != nil || p.OccurredAt.Year() < 1 || p.OccurredAt.Year() > 9999 {
		return fail("occurred_at_must_be_RFC3339_with_timezone")
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(row.Original, &original); err != nil || original == nil {
		return fail("original_must_be_a_JSON_object")
	}
	if issues, ok := original["pam_parse_issues"]; ok {
		var diagnostics []string
		var reviewed bool
		_ = json.Unmarshal(original["pam_human_reviewed"], &reviewed)
		if json.Unmarshal(issues, &diagnostics) != nil || (len(diagnostics) > 0 && !reviewed) {
			return fail("parser_review_required")
		}
	}
	// 时间和金额先规范化，避免 10.00/10 或等价时区写法绕过去重候选识别。
	canonical, _ := json.Marshal([]string{source, p.OccurredAt.UTC().Format(time.RFC3339Nano), row.Direction, p.Amount, row.Currency, row.Merchant, row.Description})
	hash := sha256.Sum256(canonical)
	p.Fingerprint = hex.EncodeToString(hash[:])
	return p
}

// ListTransactions 校验 filter 的时间、方向、金额和 platform 后查询分页；ctx 取消传播到数据库。
// 来源筛选与其它条件共同在数据库计数和分页前执行，不只过滤已经取回的一页。
func (s *Service) ListTransactions(ctx context.Context, filter domain.TransactionFilter) (domain.Page[domain.Transaction], error) {
	if err := domain.ValidatePage(filter.Limit, filter.Offset); err != nil {
		return domain.Page[domain.Transaction]{}, err
	}
	if err := domain.ValidatePlatform(filter.Platform); err != nil {
		return domain.Page[domain.Transaction]{}, err
	}
	if filter.Direction != "" && filter.Direction != "income" && filter.Direction != "expense" {
		return domain.Page[domain.Transaction]{}, domain.ValidationError{Message: "direction must be income or expense"}
	}
	for _, value := range []string{filter.From, filter.To} {
		if value != "" {
			if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
				return domain.Page[domain.Transaction]{}, domain.ValidationError{Message: "date filters must use RFC3339 with timezone"}
			}
		}
	}
	if filter.From != "" && filter.To != "" {
		from, _ := time.Parse(time.RFC3339Nano, filter.From)
		to, _ := time.Parse(time.RFC3339Nano, filter.To)
		if !from.Before(to) {
			return domain.Page[domain.Transaction]{}, domain.ValidationError{Message: "from must be before to"}
		}
	}
	for _, value := range []string{filter.MinAmount, filter.MaxAmount} {
		if value != "" {
			if _, err := domain.NormalizeMoney(value); err != nil {
				return domain.Page[domain.Transaction]{}, err
			}
		}
	}
	for name, value := range map[string]string{"search": filter.Search, "merchant": filter.Merchant, "category": filter.Category, "source": filter.Source} {
		if err := domain.ValidateText(name, value, 4000); err != nil {
			return domain.Page[domain.Transaction]{}, err
		}
	}
	return s.store.ListTransactions(ctx, filter)
}

// GetTransaction 根据 UUID id 读取一条流水；ctx 取消终止查询，不存在返回 ErrNotFound。
func (s *Service) GetTransaction(ctx context.Context, id string) (domain.Transaction, error) {
	if err := domain.ValidateID(id); err != nil {
		return domain.Transaction{}, err
	}
	return s.store.GetTransaction(ctx, id)
}

// ListAssets 返回 limit/offset 指定的库存页；ctx 控制取消，非法分页返回 ValidationError。
func (s *Service) ListAssets(ctx context.Context, limit, offset int) (domain.Page[domain.Asset], error) {
	if err := domain.ValidatePage(limit, offset); err != nil {
		return domain.Page[domain.Asset]{}, err
	}
	return s.store.ListAssets(ctx, limit, offset)
}

// GetAsset 根据 UUID id 查询库存；ctx 控制取消，不存在返回 ErrNotFound。
func (s *Service) GetAsset(ctx context.Context, id string) (domain.Asset, error) {
	if err := domain.ValidateID(id); err != nil {
		return domain.Asset{}, err
	}
	return s.store.GetAsset(ctx, id)
}

// CreateAsset 校验 input 并创建库存；ctx 取消时数据库写入取消，未知关联流水返回 ValidationError。
func (s *Service) CreateAsset(ctx context.Context, input domain.AssetInput) (domain.Asset, error) {
	normalized, err := validateAsset(input)
	if err != nil {
		return domain.Asset{}, err
	}
	return s.store.CreateAsset(ctx, normalized)
}

// UpdateAsset 以 input 整体替换 id 对应库存的可编辑字段；ctx 取消传播，不存在返回 ErrNotFound。
func (s *Service) UpdateAsset(ctx context.Context, id string, input domain.AssetInput) (domain.Asset, error) {
	if err := domain.ValidateID(id); err != nil {
		return domain.Asset{}, err
	}
	normalized, err := validateAsset(input)
	if err != nil {
		return domain.Asset{}, err
	}
	return s.store.UpdateAsset(ctx, id, normalized)
}

// DeleteAsset 删除 UUID id 对应库存；ctx 控制取消，不存在返回 ErrNotFound，不删除关联财务流水。
func (s *Service) DeleteAsset(ctx context.Context, id string) error {
	if err := domain.ValidateID(id); err != nil {
		return err
	}
	return s.store.DeleteAsset(ctx, id)
}

// validateAsset 校验 input 的库存字段并返回金额规范化的新值；错误不包含私有原始输入。
func validateAsset(input domain.AssetInput) (domain.AssetInput, error) {
	if strings.TrimSpace(input.Name) == "" {
		return input, domain.ValidationError{Message: "asset name is required"}
	}
	for name, value := range map[string]string{"name": input.Name, "category": input.Category, "brand": input.Brand, "model": input.Model, "serial_number": input.SerialNumber, "merchant": input.Merchant, "notes": input.Notes} {
		if err := domain.ValidateText(name, value, 8000); err != nil {
			return input, err
		}
	}
	if err := domain.ValidateCurrency(input.Currency); err != nil {
		return input, err
	}
	if input.PurchasePrice != nil {
		amount, err := domain.NormalizeMoney(*input.PurchasePrice)
		if err != nil {
			return input, err
		}
		input.PurchasePrice = &amount
	}
	if input.PurchaseDate != nil {
		date, err := time.Parse("2006-01-02", *input.PurchaseDate)
		if err != nil || date.Format("2006-01-02") != *input.PurchaseDate || date.Year() < 1 {
			return input, domain.ValidationError{Message: "purchase_date must use YYYY-MM-DD"}
		}
	}
	if input.RelatedTransactionID != nil {
		if err := domain.ValidateID(*input.RelatedTransactionID); err != nil {
			return input, err
		}
	}
	if input.Status == "" {
		input.Status = "active"
	}
	switch input.Status {
	case "active", "stored", "loaned", "sold", "retired", "lost":
	default:
		return input, domain.ValidationError{Message: "invalid asset status"}
	}
	return input, nil
}
