// Package atlas 将用户已最终合并的账本转换为通用导入契约；不读取历史复核件或原平台重复账单。
package atlas

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mental1104/pam/internal/domain"
)

// Rejection 保留未能无歧义映射的来源行；Row 使用从 1 开始且不含表头的 CSV 记录序号。
type Rejection struct {
	Row      int               `json:"row"`
	Reason   string            `json:"reason"`
	Original map[string]string `json:"original"`
}

// Warning 说明接受行中的有损精度或确定性的金额符号转换，不猜测缺失日期或收支方向。
type Warning struct {
	Row  int    `json:"row"`
	Code string `json:"code"`
}

// Report 是准备阶段的清单；没有数据库写入，真实接受/去重结果由后续导入返回。
type Report struct {
	SourceFile      string         `json:"source_file"`
	SourceFileHash  string         `json:"source_file_hash"`
	Year            string         `json:"year"`
	InputRows       int            `json:"input_rows"`
	PreparedRows    int            `json:"prepared_rows"`
	RejectedRows    int            `json:"rejected_rows"`
	BatchFiles      []string       `json:"batch_files"`
	Warnings        []Warning      `json:"warnings"`
	Rejections      []Rejection    `json:"rejections"`
	DirectionCounts map[string]int `json:"direction_counts"`
	RejectionCounts map[string]int `json:"rejection_counts"`
}

var finalNamePattern = regexp.MustCompile(`^([0-9]{4})-收支流水-最终合并\.csv$`)

// Prepare 从只读 inputFile 读取最终合并 CSV，将最多 batchSize 行一批的 JSON 和 report.json 写入 outputDir。
// inputFile 必须是绝对路径的最终合并文件，outputDir 必须为绝对路径且目标文件不可已存在；返回不含财务正文的汇总可用于日志。
func Prepare(inputFile, outputDir string, batchSize int) (Report, error) {
	report := Report{SourceFile: inputFile, BatchFiles: []string{}, Warnings: []Warning{}, Rejections: []Rejection{}, DirectionCounts: map[string]int{}, RejectionCounts: map[string]int{}}
	match := finalNamePattern.FindStringSubmatch(filepath.Base(inputFile))
	if !filepath.IsAbs(inputFile) || !filepath.IsAbs(outputDir) || len(match) != 2 || strings.Contains(inputFile, "人工复核") {
		return report, errors.New("input must be an absolute final merged ledger path and output must be absolute")
	}
	if batchSize < 1 || batchSize > 500 {
		return report, errors.New("batch size must be between 1 and 500")
	}
	info, err := os.Stat(inputFile)
	if err != nil {
		return report, fmt.Errorf("inspect input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 100*1024*1024 {
		return report, errors.New("input must be a regular CSV file no larger than 100 MiB")
	}
	body, err := os.ReadFile(inputFile)
	if err != nil {
		return report, fmt.Errorf("read input: %w", err)
	}
	hash := sha256.Sum256(body)
	report.SourceFileHash = hex.EncodeToString(hash[:])
	report.Year = match[1]
	records, err := parse(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf}))
	if err != nil {
		return report, err
	}
	if len(records) == 0 {
		return report, errors.New("CSV header is missing")
	}
	header := records[0]
	if err = validateHeader(header); err != nil {
		return report, err
	}
	prepared := make([]domain.ImportRecord, 0, len(records)-1)
	seenIDs := map[string]bool{}
	for index, values := range records[1:] {
		rowIndex := index + 1
		report.InputRows++
		original := make(map[string]string, len(header)+4)
		for column, key := range header {
			if column < len(values) {
				original[key] = values[column]
			} else {
				original[key] = ""
			}
		}
		report.DirectionCounts[original["收支方向"]]++
		if len(values) != len(header) {
			report.reject(rowIndex, "column_count_mismatch", original)
			continue
		}
		row, warnings, reason := convert(original, rowIndex, report.Year, report.SourceFileHash)
		if reason != "" {
			report.reject(rowIndex, reason, original)
			continue
		}
		if seenIDs[row.ExternalID] {
			report.reject(rowIndex, "duplicate_merged_record_id_in_file", original)
			continue
		}
		seenIDs[row.ExternalID] = true
		prepared = append(prepared, row)
		report.Warnings = append(report.Warnings, warnings...)
	}
	report.PreparedRows = len(prepared)
	report.RejectedRows = len(report.Rejections)
	if err = os.MkdirAll(outputDir, 0700); err != nil {
		return report, fmt.Errorf("create output: %w", err)
	}
	// 提前检查所有目标，避免重复运行覆盖已经交接或人工修订的批次。
	for i := 0; i < len(prepared); i += batchSize {
		report.BatchFiles = append(report.BatchFiles, fmt.Sprintf("batch-%04d.json", i/batchSize+1))
	}
	for _, name := range append(append([]string{}, report.BatchFiles...), "report.json") {
		if _, err := os.Lstat(filepath.Join(outputDir, name)); err == nil {
			return report, errors.New("output files already exist")
		} else if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
	}
	for i, name := range report.BatchFiles {
		end := (i + 1) * batchSize
		if end > len(prepared) {
			end = len(prepared)
		}
		request := domain.ImportRequest{Source: "atlas_merged", IdempotencyKey: fmt.Sprintf("atlas-final:%s:%s:%04d", report.Year, report.SourceFileHash, i+1), SourceFileHash: report.SourceFileHash, Records: prepared[i*batchSize : end]}
		if err = writeJSON(filepath.Join(outputDir, name), request); err != nil {
			return report, err
		}
	}
	if err = writeJSON(filepath.Join(outputDir, "report.json"), report); err != nil {
		return report, err
	}
	return report, nil
}

// parse 将 UTF-8 body 解析为完整 CSV 记录，保留多行引号字段；非法 CSV 整体拒绝，避免行位置漂移。
func parse(body []byte) ([][]string, error) {
	reader := csv.NewReader(bytes.NewReader(body))
	reader.FieldsPerRecord = -1
	rows := [][]string{}
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("CSV syntax is invalid; no batches were written")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// validateHeader 检查 header 的必要列和重复列，返回不带来源正文的契约错误。
func validateHeader(header []string) error {
	seen := map[string]bool{}
	for _, key := range header {
		if key == "" || seen[key] {
			return errors.New("CSV has empty or duplicate column names")
		}
		seen[key] = true
	}
	for _, required := range []string{"日期", "时间", "交易对象或摘要", "收支方向", "金额(元)", "币种", "备注", "一级分类", "二级分类", "合并记录ID", "主记录平台", "来源平台", "交易性质", "收支统计标记", "来源记录明细"} {
		if !seen[required] {
			return fmt.Errorf("required CSV column is missing: %s", strconv.QuoteToASCII(required))
		}
	}
	return nil
}

// reject 把 rowIndex 的拒绝 reason 和 original 原始列加入 report；只修改准备报告，不写数据库。
func (report *Report) reject(rowIndex int, reason string, original map[string]string) {
	report.Rejections = append(report.Rejections, Rejection{Row: rowIndex, Reason: reason, Original: original})
	report.RejectionCounts[reason]++
}

// convert 将 original 的一行按显式字段转换；rowIndex 是原文件记录位置，year 用于稳定 ID 命名，fileHash 保留来源身份。
// 返回规范化数据、必要精度警告和拒绝原因；拒绝时不猜测产品、交易日期或资金流向。
func convert(original map[string]string, rowIndex int, year, fileHash string) (domain.ImportRecord, []Warning, string) {
	row := domain.ImportRecord{RowKey: strconv.Itoa(rowIndex), Currency: strings.TrimSpace(original["币种"]), Merchant: original["交易对象或摘要"], Description: original["备注"]}
	warnings := []Warning{}
	switch strings.TrimSpace(original["收支方向"]) {
	case "支出":
		row.Direction = "expense"
	case "收入":
		row.Direction = "income"
	default:
		return row, warnings, "unsupported_or_unknown_direction"
	}
	if err := domain.ValidateCurrency(row.Currency); err != nil {
		return row, warnings, "invalid_currency"
	}
	amount := strings.TrimSpace(original["金额(元)"])
	if strings.HasPrefix(amount, "-") {
		if row.Direction != "expense" {
			return row, warnings, "amount_sign_conflicts_with_direction"
		}
		amount = strings.TrimPrefix(amount, "-")
		original["pam_amount_normalization"] = "negative expense converted to unsigned amount"
		warnings = append(warnings, Warning{Row: rowIndex, Code: "expense_sign_normalized"})
	} else if strings.HasPrefix(amount, "+") {
		amount = strings.TrimPrefix(amount, "+")
		warnings = append(warnings, Warning{Row: rowIndex, Code: "explicit_plus_removed"})
	}
	var err error
	row.Amount, err = domain.NormalizeMoney(amount)
	if err != nil {
		return row, warnings, "invalid_amount"
	}
	dateText := strings.TrimSpace(original["日期"])
	date, err := time.Parse("2006-01-02", dateText)
	if err != nil || date.Year() < 1 || date.Format("2006-01-02") != dateText {
		return row, warnings, "missing_or_invalid_date"
	}
	clock := strings.TrimSpace(original["时间"])
	precision := "second"
	layout := "2006-01-02 15:04:05"
	switch clock {
	case "", "未知", "未标注", "不详", "--", "—":
		clock = "00:00:00"
		precision = "date"
		warnings = append(warnings, Warning{Row: rowIndex, Code: "time_unknown_midnight_placeholder"})
	default:
		if len(clock) == 5 {
			layout = "2006-01-02 15:04"
			precision = "minute"
		}
	}
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	timestamp, err := time.ParseInLocation(layout, dateText+" "+clock, location)
	if err != nil {
		return row, warnings, "invalid_time"
	}
	row.OccurredAt = timestamp.Format(time.RFC3339)
	id := strings.TrimSpace(original["合并记录ID"])
	if id == "" {
		return row, warnings, "missing_merged_record_id"
	}
	row.ExternalID = year + ":" + id
	primary, secondary := strings.TrimSpace(original["一级分类"]), strings.TrimSpace(original["二级分类"])
	row.Category = primary
	if secondary != "" {
		if primary != "" {
			row.Category += " / "
		}
		row.Category += secondary
	}
	original["pam_timestamp_precision"] = precision
	original["pam_source_date"] = dateText
	original["pam_source_timezone"] = "Asia/Shanghai"
	original["pam_source_file_sha256"] = fileHash
	original["pam_source_row"] = strconv.Itoa(rowIndex)
	row.Original, err = json.Marshal(original)
	if err != nil {
		return row, warnings, "invalid_original_json"
	}
	return row, warnings, ""
}

// writeJSON 以 0600 权限独占创建 path 并编码 value；不覆盖文件，失败时保留部分文件提示该次准备需检查。
func writeJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create prepared JSON: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(value)
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("write prepared JSON: %w", encodeErr)
	}
	return closeErr
}
