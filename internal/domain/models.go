// Package domain 定义 PAM 的稳定业务数据及约束；不依赖数据库或传输协议。
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound 表示请求的业务记录不存在。
var ErrNotFound = errors.New("record not found")

// ErrConflict 表示幂等键被不同内容复用，或业务关联阻止当前操作。
var ErrConflict = errors.New("record conflict")

// ValidationError 保存可向调用者公开的输入错误，不包含原始财务数据。
type ValidationError struct{ Message string }

// Error 返回稳定的输入错误说明；接收者保存已脱敏的说明文本。
func (e ValidationError) Error() string { return e.Message }

// Transaction 是规范化收支记录；金额始终使用十进制字符串，Original 是独立来源表的只读投影。
type Transaction struct {
	ID          string    `json:"id"`
	OccurredAt  time.Time `json:"occurred_at"`
	Direction   string    `json:"direction"`
	Amount      string    `json:"amount"`
	Currency    string    `json:"currency"`
	Merchant    string    `json:"merchant"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Source      string    `json:"source"`
	// Platforms 是已确认账单来源的去重数组；仅供展示/筛选，不由商户或摘要推断。
	Platforms     []string        `json:"platforms"`
	ExternalID    string          `json:"external_id,omitempty"`
	ImportBatchID string          `json:"import_batch_id"`
	Original      json.RawMessage `json:"original"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

// ImportRecord 是 Agent 提交的单行规范化数据；RowKey 是文件内稳定行标识，ConfirmNew 明确确认可疑的重复消费。
type ImportRecord struct {
	RowKey      string          `json:"row_key,omitempty"`
	OccurredAt  string          `json:"occurred_at"`
	Direction   string          `json:"direction"`
	Amount      string          `json:"amount"`
	Currency    string          `json:"currency"`
	Merchant    string          `json:"merchant"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	ExternalID  string          `json:"external_id,omitempty"`
	Original    json.RawMessage `json:"original"`
	ConfirmNew  bool            `json:"confirm_new,omitempty"`
}

// ImportRequest 标识一次来源批次；IdempotencyKey 必填，同键不同内容会冲突。
// SourceFileHash 是完整来源文件的 SHA-256；提供时每行必须提供 RowKey。
type ImportRequest struct {
	ReviewCrossSource bool `json:"review_cross_source,omitempty"`
	Source         string         `json:"source"`
	IdempotencyKey string         `json:"idempotency_key"`
	SourceFileHash string         `json:"source_file_hash,omitempty"`
	Records        []ImportRecord `json:"records"`
}

// ImportRowResult 说明单行创建、跳过、待确认重复或失败；TransactionID 仅在关联已知流水时存在。
type ImportRowResult struct {
	Index         int     `json:"index"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason"`
	TransactionID *string `json:"transaction_id,omitempty"`
}

// ImportResult 返回互斥的单行计数及逐行说明；Replayed 表示同一幂等批次的安全重放。
type ImportResult struct {
	BatchID    string            `json:"batch_id"`
	Created    int               `json:"created"`
	Skipped    int               `json:"skipped"`
	Duplicated int               `json:"duplicated"`
	Failed     int               `json:"failed"`
	Replayed   bool              `json:"replayed"`
	Rows       []ImportRowResult `json:"rows"`
}

// PreparedRecord 是应用层已经验证的导入行；ValidationFailure 非空时只保留来源，不创建流水。
type PreparedRecord struct {
	Input             ImportRecord
	OccurredAt        time.Time
	Amount            string
	Fingerprint       string
	ValidationFailure string
}

// TransactionFilter 定义可组合的列表过滤；From 包含边界，To 排除边界，金额上下界包含边界。
type TransactionFilter struct {
	From      string
	To        string
	Search    string
	Merchant  string
	MinAmount string
	MaxAmount string
	Direction string
	Category  string
	Source    string
	// Platform 是独立于批次 Source 的单值展示来源筛选，空值表示全部。
	Platform string
	Limit    int
	Offset   int
}

// Page 是具有总数和偏移量的稳定分页响应；Items 在无记录时仍序列化为空数组。
type Page[T any] struct {
	Items  []T   `json:"items"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
	Total  int64 `json:"total"`
}

// AssetInput 是库存创建和整体更新的业务输入；空指针表示未知的购买信息或未关联交易。
type AssetInput struct {
	Name                 string  `json:"name"`
	Category             string  `json:"category"`
	Brand                string  `json:"brand"`
	Model                string  `json:"model"`
	SerialNumber         string  `json:"serial_number"`
	PurchaseDate         *string `json:"purchase_date"`
	PurchasePrice        *string `json:"purchase_price"`
	Currency             string  `json:"currency"`
	Merchant             string  `json:"merchant"`
	RelatedTransactionID *string `json:"related_transaction_id"`
	Notes                string  `json:"notes"`
	Status               string  `json:"status"`
}

// Asset 在库存输入之上保存数据库标识和审计时间；不维护折旧或估值。
type Asset struct {
	AssetInput
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var moneyPattern = regexp.MustCompile(`^[0-9]{1,16}(\.[0-9]{1,4})?$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// NormalizeMoney 校验 value 是否为非负 numeric(20,4) 范围，并返回去除冗余零的精确字符串。
// 不接受符号、指数或空白；失败返回不含输入值的 ValidationError。
func NormalizeMoney(value string) (string, error) {
	if !moneyPattern.MatchString(value) {
		return "", ValidationError{"amount must be an unsigned decimal with at most 16 integer and 4 fractional digits"}
	}
	parts := strings.SplitN(value, ".", 2)
	whole := strings.TrimLeft(parts[0], "0")
	if whole == "" {
		whole = "0"
	}
	if len(parts) == 1 {
		return whole, nil
	}
	fraction := strings.TrimRight(parts[1], "0")
	if fraction == "" {
		return whole, nil
	}
	return whole + "." + fraction, nil
}

// ValidateCurrency 校验 currency 是三个大写 ASCII 字母；未知币种代码允许由来源自行表达。
func ValidateCurrency(currency string) error {
	if !currencyPattern.MatchString(currency) {
		return ValidationError{"currency must contain three uppercase ASCII letters"}
	}
	return nil
}

// ValidateID 校验 id 是 UUID 文本；失败返回输入错误，避免数据库类型转换错误泄漏。
func ValidateID(id string) error {
	if !uuidPattern.MatchString(id) {
		return ValidationError{"id must be a UUID"}
	}
	return nil
}

// ValidatePage 校验 limit 为 1..200 且 offset 非负；返回可公开的输入错误。
func ValidatePage(limit, offset int) error {
	if limit < 1 || limit > 200 || offset < 0 {
		return ValidationError{"limit must be between 1 and 200 and offset must be nonnegative"}
	}
	return nil
}

// ValidatePlatform 校验单值来源筛选；空字符串表示全部，未知枚举返回安全输入错误。
func ValidatePlatform(platform string) error {
	switch platform {
	case "", "wechat", "alipay", "cmb", "ccb", "other":
		return nil
	default:
		return ValidationError{Message: "platform must be wechat, alipay, cmb, ccb or other"}
	}
}

// ValidateText 校验 name 所指字段的 value 不含 NUL 且不超过 maxBytes 字节；错误只包含字段名。
func ValidateText(name, value string, maxBytes int) error {
	if len(value) > maxBytes || strings.ContainsRune(value, '\x00') {
		return ValidationError{fmt.Sprintf("%s is too long or contains NUL", name)}
	}
	return nil
}
