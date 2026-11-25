// Package cmb 只把用户指定的 2026 招商银行根表转成导入批次；旧核算表仅提供独立分类参考。
package cmb

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mental1104/pam/internal/domain"
)

// Source 是持续使用的来源名称，不包含文件版本或截图名字。
const Source = "cmb_statement"

// MappingVersion 在规范化或引用规则变化时更新，避免复用内容不同的批次幂等键。
const MappingVersion = "cmb-2026-v1"

var bankHeader = []string{"日期", "时间", "交易对象或摘要", "收支方向", "金额(元)", "币种", "账户类型", "卡号尾号", "交易后余额(元)", "入账标记", "收支统计标记", "退款标记", "原始日期标签", "来源图片", "原图纵坐标", "备注"}
var classificationHeader = []string{"一级分类", "二级分类", "分类可信度", "交易性质", "净支出贡献(元)", "分类依据/待核实说明"}

// RowCoverage 记录每个 CSV 数据行的归宿；Row 从 1 开始，CSVLine 是含表头的实际起始行号。
type RowCoverage struct {
	Row             int    `json:"row"`
	CSVLine         int    `json:"csv_line"`
	RowKey          string `json:"row_key"`
	IdentityKind    string `json:"identity_kind"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	ReferenceStatus string `json:"reference_status"`
	ConfirmNew      bool   `json:"confirm_new"`
}

// Rejection 在私有报告保留未准备行的全部原字段及原始列值，不丢弃异常来源。
type Rejection struct {
	Row      int               `json:"row"`
	Reason   string            `json:"reason"`
	Original map[string]string `json:"original"`
	Values   []string          `json:"values"`
}

// Collision 记录与后端相同的规范化指纹碰撞；Confirmed 表示全部后续行都有独立余额变化证据。
type Collision struct {
	Fingerprint string `json:"fingerprint"`
	Rows        []int  `json:"rows"`
	Confirmed   bool   `json:"confirmed"`
	Evidence    string `json:"evidence"`
}

// Report 是仅保存在指定私有目录的覆盖报告，统计不等于后续数据库导入结果。
type Report struct {
	Source                 string         `json:"source"`
	MappingVersion         string         `json:"mapping_version"`
	SourceFile             string         `json:"source_file"`
	SourceFileHash         string         `json:"source_file_hash"`
	ReferenceFile          string         `json:"reference_file,omitempty"`
	ReferenceFileHash      string         `json:"reference_file_hash,omitempty"`
	InputRows              int            `json:"input_rows"`
	PreparedRows           int            `json:"prepared_rows"`
	RejectedRows           int            `json:"rejected_rows"`
	ConfirmedDuplicateRows int            `json:"confirmed_duplicate_rows"`
	BatchFiles             []string       `json:"batch_files"`
	DirectionCounts        map[string]int `json:"direction_counts"`
	PrecisionCounts        map[string]int `json:"precision_counts"`
	IdentityCounts         map[string]int `json:"identity_counts"`
	ReferenceCounts        map[string]int `json:"reference_counts"`
	RejectionCounts        map[string]int `json:"rejection_counts"`
	Rows                   []RowCoverage  `json:"rows"`
	Rejections             []Rejection    `json:"rejections"`
	Collisions             []Collision    `json:"fingerprint_collisions"`
	Notes                  []string       `json:"notes"`
}

// sourceRow 是只读 CSV 记录；Values 保留结构错误时无法映射到列名的原始值。
type sourceRow struct {
	row, line int
	original  map[string]string
	values    []string
}

// statement 保存完整文件 SHA 和全部数据行，不读取目录中的其它 CSV 或图片。
type statement struct {
	hash string
	rows []sourceRow
}

// candidate 保存一个待准备行及其私有元数据；record 不携带伪造的外部流水号。
type candidate struct {
	source                                              sourceRow
	record                                              domain.ImportRecord
	original                                            map[string]any
	identity, precision, reason, reference, fingerprint string
}

// Prepare 只读 inputFile 根表和可选 referenceFile 核算表，将批次及报告写入新的 outputDir。
// 三个非空路径必须绝对，batchSize 为 50–100；失败不覆盖旧输出，也不连接数据库或网络。
func Prepare(inputFile, referenceFile, outputDir string, batchSize int) (Report, error) {
	r := Report{Source: Source, MappingVersion: MappingVersion, SourceFile: inputFile, ReferenceFile: referenceFile, BatchFiles: []string{}, DirectionCounts: map[string]int{}, PrecisionCounts: map[string]int{}, IdentityCounts: map[string]int{}, ReferenceCounts: map[string]int{}, RejectionCounts: map[string]int{}, Rows: []RowCoverage{}, Rejections: []Rejection{}, Collisions: []Collision{}, Notes: []string{
		"Only the explicitly selected root bank CSV creates transactions; reference classifications never overwrite its fields or notes.",
		"Location identity uses image basename, original vertical coordinate, account and occurrence; changing those values changes identity.",
		"Missing-position identity uses canonical business fields, balance, original image and occurrence; corrections to those fields change identity.",
		"No bank transaction ID is invented. Source-file SHA is the real file hash; cross-version imports still require backend duplicate review.",
		"Date/minute precision is retained in original metadata; zero seconds or midnight placeholders are not evidence of exact transaction time.",
	}}
	if !filepath.IsAbs(inputFile) || !filepath.IsAbs(outputDir) || (referenceFile != "" && !filepath.IsAbs(referenceFile)) || batchSize < 50 || batchSize > 100 {
		return r, errors.New("absolute paths and batch size between 50 and 100 are required")
	}
	for _, part := range strings.Split(filepath.ToSlash(inputFile), "/") {
		if part == "旧版备份" || part == "核算整理" || part == "原始截图" {
			return r, errors.New("only the root bank statement may create transactions")
		}
	}
	current, err := load(inputFile, bankHeader)
	if err != nil {
		return r, err
	}
	r.SourceFileHash = current.hash
	r.InputRows = len(current.rows)
	references := map[string][]sourceRow{}
	if referenceFile != "" {
		header := append(append([]string{}, bankHeader...), classificationHeader...)
		ref, err := load(referenceFile, header)
		if err != nil {
			return r, err
		}
		r.ReferenceFileHash = ref.hash
		for _, row := range ref.rows {
			key := referenceKey(row.original)
			references[key] = append(references[key], row)
		}
	}
	items := make([]candidate, 0, len(current.rows))
	occurrences := map[string]int{}
	for _, row := range current.rows {
		c := convert(row, current.hash)
		r.DirectionCounts[row.original["收支方向"]]++
		base, kind := identity(c)
		occurrences[base]++
		c.identity = kind
		c.record.RowKey = "cmb:v1:" + kind + ":" + base + ":" + strconv.Itoa(occurrences[base])
		c.original["pam_source_record_identity"] = c.record.RowKey
		c.original["pam_identity_kind"] = kind
		found := references[referenceKey(row.original)]
		c.reference = "unmatched"
		if len(found) > 1 {
			c.reference = "ambiguous"
		} else if len(found) == 1 {
			c.reference = "matched"
			c.original["pam_classification_reference"] = map[string]any{"source_file": referenceFile, "source_file_sha256": r.ReferenceFileHash, "row": found[0].row, "csv_line": found[0].line, "match_basis": "unique_business_fields_and_post_transaction_balance", "original": found[0].original, "scope": "reference_only_root_statement_fields_are_authoritative"}
		}
		if c.reason == "" {
			c.fingerprint = fingerprint(c.record)
		}
		items = append(items, c)
	}
	groups := map[string][]int{}
	order := []string{}
	for i, c := range items {
		if c.reason != "" {
			continue
		}
		if _, ok := groups[c.fingerprint]; !ok {
			order = append(order, c.fingerprint)
		}
		groups[c.fingerprint] = append(groups[c.fingerprint], i)
	}
	for _, key := range order {
		indexes := groups[key]
		if len(indexes) < 2 {
			continue
		}
		group := Collision{Fingerprint: key, Rows: []int{}, Confirmed: true, Evidence: "same_image_distinct_positions_same_account_and_exact_balance_step"}
		for _, index := range indexes {
			group.Rows = append(group.Rows, items[index].source.row)
		}
		for position, index := range indexes {
			if position == 0 {
				continue
			}
			witness := -1
			for _, prior := range indexes[:position] {
				if independent(items[prior], items[index]) {
					witness = prior
					break
				}
			}
			if witness < 0 {
				items[index].reason = "fingerprint_collision_requires_review"
				group.Confirmed = false
				group.Evidence = "insufficient_independent_transaction_evidence"
				continue
			}
			items[index].record.ConfirmNew = true
			r.ConfirmedDuplicateRows++
			items[index].original["pam_duplicate_confirmation"] = map[string]any{"basis": "same_image_distinct_positions_same_account_and_exact_balance_step", "witness_row_key": items[witness].record.RowKey, "witness_source_row": items[witness].source.row, "source_row": items[index].source.row, "note": "Identical timestamps are not deduplicated when separate account balance changes prove separate statement entries."}
		}
		r.Collisions = append(r.Collisions, group)
	}
	prepared := []domain.ImportRecord{}
	for _, c := range items {
		coverage := RowCoverage{Row: c.source.row, CSVLine: c.source.line, RowKey: c.record.RowKey, IdentityKind: c.identity, Status: "prepared", ReferenceStatus: c.reference, ConfirmNew: c.record.ConfirmNew}
		r.IdentityCounts[c.identity]++
		r.ReferenceCounts[c.reference]++
		if c.reason != "" {
			coverage.Status = "rejected"
			coverage.Reason = c.reason
			r.RejectionCounts[c.reason]++
			r.Rejections = append(r.Rejections, Rejection{c.source.row, c.reason, c.source.original, c.source.values})
		} else {
			c.record.Original, err = json.Marshal(c.original)
			if err != nil {
				return r, errors.New("cannot encode original source metadata")
			}
			prepared = append(prepared, c.record)
			r.PrecisionCounts[c.precision]++
		}
		r.Rows = append(r.Rows, coverage)
	}
	r.PreparedRows = len(prepared)
	r.RejectedRows = len(r.Rejections)
	if err = os.MkdirAll(filepath.Dir(outputDir), 0700); err != nil {
		return r, errors.New("cannot create output parent directory")
	}
	if err = os.Mkdir(outputDir, 0700); err != nil {
		return r, errors.New("output directory must not already exist")
	}
	refHash := r.ReferenceFileHash
	if refHash == "" {
		refHash = "none"
	}
	for start := 0; start < len(prepared); start += batchSize {
		end := start + batchSize
		if end > len(prepared) {
			end = len(prepared)
		}
		number := start/batchSize + 1
		name := fmt.Sprintf("batch-%04d.json", number)
		batch := domain.ImportRequest{Source: Source, SourceFileHash: current.hash, IdempotencyKey: fmt.Sprintf("%s:%s:%s:n%d:b%04d", MappingVersion, current.hash, refHash, batchSize, number), Records: prepared[start:end]}
		if len(batch.IdempotencyKey) > 200 {
			return r, errors.New("prepared idempotency key exceeds contract limit")
		}
		if err = writeJSON(filepath.Join(outputDir, name), batch); err != nil {
			return r, err
		}
		r.BatchFiles = append(r.BatchFiles, name)
	}
	if err = writeJSON(filepath.Join(outputDir, "report.json"), r); err != nil {
		return r, err
	}
	return r, nil
}

// load 读取 path 的唯一 CSV，严格核对 header；返回完整字节的 SHA 与原字段，不扫描兄弟文件。
func load(path string, header []string) (statement, error) {
	var result statement
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 100<<20 {
		return result, errors.New("CSV input must be a readable regular file no larger than 100 MiB")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return result, errors.New("cannot read CSV input")
	}
	sum := sha256.Sum256(body)
	result.hash = hex.EncodeToString(sum[:])
	reader := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf})))
	reader.FieldsPerRecord = -1
	actual, err := reader.Read()
	if err != nil || len(actual) != len(header) {
		return result, errors.New("CSV header does not match the selected bank/reference contract")
	}
	for i, key := range header {
		if actual[i] != key {
			return result, errors.New("CSV header does not match the selected bank/reference contract")
		}
	}
	for {
		values, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, errors.New("invalid CSV syntax; no batches were written")
		}
		line, _ := reader.FieldPos(0)
		row := sourceRow{row: len(result.rows) + 1, line: line, original: map[string]string{}, values: values}
		for i, key := range header {
			if i < len(values) {
				row.original[key] = values[i]
			} else {
				row.original[key] = ""
			}
		}
		result.rows = append(result.rows, row)
	}
	return result, nil
}

// convert 只从 row 的明确方向、日期、时间和金额生成规范化记录；hash 为真实来源文件摘要。
// 返回候选保存全部原字段；异常以 reason 标记，不能通过参考表补猜。
func convert(row sourceRow, hash string) candidate {
	c := candidate{source: row, record: domain.ImportRecord{Merchant: row.original["交易对象或摘要"], Description: row.original["备注"], Currency: strings.TrimSpace(row.original["币种"])}, original: map[string]any{}}
	for key, value := range row.original {
		c.original[key] = value
	}
	c.original["pam_source_file_sha256"] = hash
	c.original["pam_source_row"] = row.row
	c.original["pam_source_csv_line"] = row.line
	c.original["pam_mapping_version"] = MappingVersion
	c.original["pam_statement_identity"] = "cmb_statement:2026"
	c.original["pam_source_timezone"] = "Asia/Shanghai"
	if len(row.values) != len(bankHeader) {
		c.reason = "column_count_mismatch"
		return c
	}
	switch strings.TrimSpace(row.original["收支方向"]) {
	case "支出":
		c.record.Direction = "expense"
	case "收入":
		c.record.Direction = "income"
	default:
		c.reason = "unsupported_or_unknown_direction"
		return c
	}
	amount := strings.TrimSpace(row.original["金额(元)"])
	if strings.HasPrefix(amount, "-") {
		if c.record.Direction != "expense" {
			c.reason = "amount_sign_conflicts_with_direction"
			return c
		}
		amount = strings.TrimPrefix(amount, "-")
		c.original["pam_amount_normalization"] = "negative_expense_to_unsigned"
	} else {
		amount = strings.TrimPrefix(amount, "+")
	}
	var err error
	c.record.Amount, err = domain.NormalizeMoney(amount)
	if err != nil {
		c.reason = "invalid_amount"
		return c
	}
	if domain.ValidateCurrency(c.record.Currency) != nil {
		c.reason = "invalid_currency"
		return c
	}
	dateText := strings.TrimSpace(row.original["日期"])
	date, err := time.Parse("2006-01-02", dateText)
	if err != nil || date.Year() != 2026 || date.Format("2006-01-02") != dateText {
		c.reason = "missing_invalid_or_non_2026_date"
		return c
	}
	clock := strings.TrimSpace(row.original["时间"])
	layout := "2006-01-02 15:04:05"
	c.precision = "second"
	switch clock {
	case "", "未知", "未标注", "不详", "--", "—":
		clock = "00:00:00"
		c.precision = "date"
	default:
		if len(clock) == 5 {
			layout = "2006-01-02 15:04"
			c.precision = "minute"
		}
	}
	stamp, err := time.ParseInLocation(layout, dateText+" "+clock, time.FixedZone("Asia/Shanghai", 8*60*60))
	if err != nil {
		c.reason = "invalid_time"
		return c
	}
	c.record.OccurredAt = stamp.Format(time.RFC3339)
	c.original["pam_timestamp_precision"] = c.precision
	c.original["pam_source_date"] = dateText
	return c
}

// imageName 返回来源图片的文件名，兼容 Windows 和 POSIX 分隔符；不打开图片或推断内容。
func imageName(row map[string]string) string {
	return filepath.Base(strings.ReplaceAll(strings.TrimSpace(row["来源图片"]), "\\", "/"))
}

// position 将非空十进制纵坐标规范化；无定位信息时返回空，不为此拒绝有效流水。
func position(row map[string]string) string {
	raw := strings.TrimSpace(row["原图纵坐标"])
	if raw == "" {
		return ""
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatUint(value, 10)
}

// identity 以截图位置优先生成 c 的身份摘要；缺位置时使用完整规范业务字段和补充来源。
// 不含 CSV 整表行号或文件 SHA，新增其它行不会改变已有独立交易的 key。
func identity(c candidate) (string, string) {
	o := c.source.original
	image, y := imageName(o), position(o)
	if image != "" && image != "." && y != "" {
		return digest([]string{image, y, strings.TrimSpace(o["账户类型"]), strings.TrimSpace(o["卡号尾号"])}), "image_position"
	}
	return digest([]string{c.record.OccurredAt, c.record.Direction, c.record.Amount, c.record.Currency, c.record.Merchant, c.record.Description, strings.TrimSpace(o["账户类型"]), strings.TrimSpace(o["卡号尾号"]), strings.TrimSpace(o["交易后余额(元)"]), image}), "business_content"
}

// referenceKey 以业务字段和交易后余额匹配旧分类；图片改名和更新备注不会覆盖新来源事实。
// 返回值只用于唯一性匹配，不充当真实银行交易号。
func referenceKey(row map[string]string) string {
	amount := strings.TrimLeft(strings.TrimSpace(row["金额(元)"]), "+-")
	if value, err := domain.NormalizeMoney(amount); err == nil {
		amount = value
	}
	clock := strings.TrimSpace(row["时间"])
	if len(clock) == 5 {
		clock += ":00"
	}
	return digest([]string{strings.TrimSpace(row["日期"]), clock, strings.TrimSpace(row["交易对象或摘要"]), strings.TrimSpace(row["收支方向"]), amount, strings.TrimSpace(row["币种"]), strings.TrimSpace(row["账户类型"]), strings.TrimSpace(row["卡号尾号"]), strings.TrimSpace(row["交易后余额(元)"])})
}

// fingerprint 完全沿用后端的业务指纹字段与时间/金额规范化，避免准备报告漏掉实际冲突。
func fingerprint(row domain.ImportRecord) string {
	stamp, _ := time.Parse(time.RFC3339Nano, row.OccurredAt)
	return digest([]string{Source, stamp.UTC().Format(time.RFC3339Nano), row.Direction, row.Amount, row.Currency, row.Merchant, row.Description})
}

// scaledMoney 将 value 无损转换为万分之一整数，支持正负余额；无法表示时返回 nil。
func scaledMoney(value string) *big.Int {
	value = strings.TrimSpace(value)
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimLeft(value, "+-")
	normal, err := domain.NormalizeMoney(value)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(normal, ".", 2)
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	fraction += strings.Repeat("0", 4-len(fraction))
	integer, ok := new(big.Int).SetString(parts[0]+fraction, 10)
	if !ok {
		return nil
	}
	if negative {
		integer.Neg(integer)
	}
	return integer
}

// independent 判断 first/next 是否有独立交易证据；只看不同截图位置不足以确认，余额必须精确变化一笔金额。
func independent(first, next candidate) bool {
	a, b := first.source.original, next.source.original
	if imageName(a) == "." || imageName(a) != imageName(b) || position(a) == "" || position(b) == "" || position(a) == position(b) || strings.TrimSpace(a["账户类型"]) == "" || strings.TrimSpace(a["卡号尾号"]) == "" || a["账户类型"] != b["账户类型"] || a["卡号尾号"] != b["卡号尾号"] {
		return false
	}
	x, y, amount := scaledMoney(a["交易后余额(元)"]), scaledMoney(b["交易后余额(元)"]), scaledMoney(first.record.Amount)
	if x == nil || y == nil || amount == nil || amount.Sign() == 0 {
		return false
	}
	difference := new(big.Int).Sub(x, y)
	difference.Abs(difference)
	return difference.Cmp(amount) == 0
}

// digest 为公开约定的字符串字段生成无碰撞分隔的 SHA；values 不进入运行日志。
func digest(values []string) string {
	body, _ := json.Marshal(values)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// writeJSON 以 0600 独占创建 path 并写入 value；失败保留未完成文件，不覆盖人工或先前准备结果。
func writeJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create prepared JSON")
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(value)
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		return errors.New("cannot write prepared JSON")
	}
	return nil
}
