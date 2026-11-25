package cmb

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mental1104/pam/internal/domain"
)

// fixtureRow 返回合成的根表行；每次分配独立映射，不含真实金融信息。
func fixtureRow() map[string]string {
	return map[string]string{"日期": "2026-05-01", "时间": "12:34", "交易对象或摘要": "Synthetic merchant", "收支方向": "支出", "金额(元)": "-12.50", "币种": "CNY", "账户类型": "储蓄卡", "卡号尾号": "1234", "交易后余额(元)": "100.00", "入账标记": "已入账", "收支统计标记": "未标注", "退款标记": "否", "原始日期标签": "合成原始日期", "来源图片": "month.jpeg", "原图纵坐标": "100", "备注": "Current root note"}
}

// csvFixture 以带 BOM/CRLF 的真实 CSV 写入 path；header/rows 指定完整样本，t 报告写入失败。
func csvFixture(t *testing.T, path string, header []string, rows []map[string]string) {
	t.Helper()
	var body bytes.Buffer
	body.Write([]byte{0xef, 0xbb, 0xbf})
	writer := csv.NewWriter(&body)
	writer.UseCRLF = true
	if err := writer.Write(header); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		values := []string{}
		for _, name := range header {
			values = append(values, row[name])
		}
		if err := writer.Write(values); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	if writer.Error() != nil {
		t.Fatal(writer.Error())
	}
	if err := os.WriteFile(path, body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

// readBatch 读取 directory 内 name 批次；t 解码失败时停止场景，不操作数据库。
func readBatch(t *testing.T, directory, name string) domain.ImportRequest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	var batch domain.ImportRequest
	if json.Unmarshal(body, &batch) != nil {
		t.Fatal("invalid prepared batch")
	}
	return batch
}

// TestPreparationPreservesRootAndReference 验证新备注/统计标记保持原值，旧分类只保留为独立参考。
// t 同时核对真实 SHA、精确金额、分钟精度及不伪造银行流水号。
func TestPreparationPreservesRootAndReference(t *testing.T) {
	dir := t.TempDir()
	input, ref := filepath.Join(dir, "current.csv"), filepath.Join(dir, "reference.csv")
	row := fixtureRow()
	row["金额(元)"] = "-9007199254740993.0001"
	row["备注"] = "Current note, with quote \" and\nnew line"
	csvFixture(t, input, bankHeader, []map[string]string{row})
	old := fixtureRow()
	for key, value := range row {
		old[key] = value
	}
	old["备注"] = "Old reference note"
	old["来源图片"] = "old-image.JPEG"
	old["收支统计标记"] = "旧统计"
	old["一级分类"] = "Old category"
	csvFixture(t, ref, append(append([]string{}, bankHeader...), classificationHeader...), []map[string]string{old})
	output := filepath.Join(dir, "prepared")
	report, err := Prepare(input, ref, output, 100)
	if err != nil {
		t.Fatal(err)
	}
	if report.PreparedRows != 1 || report.RejectedRows != 0 || report.ReferenceCounts["matched"] != 1 {
		t.Fatal("unexpected source coverage")
	}
	batch := readBatch(t, output, report.BatchFiles[0])
	record := batch.Records[0]
	if batch.Source != Source || record.ExternalID != "" || record.Amount != "9007199254740993.0001" || record.Category != "" || record.ConfirmNew {
		t.Fatal("canonical fields or bank ID boundary changed")
	}
	var original map[string]json.RawMessage
	if json.Unmarshal(record.Original, &original) != nil {
		t.Fatal("missing original")
	}
	for key, value := range row {
		var actual string
		if json.Unmarshal(original[key], &actual) != nil || actual != value {
			t.Fatal("root source field was overwritten")
		}
	}
	var precision string
	_ = json.Unmarshal(original["pam_timestamp_precision"], &precision)
	if precision != "minute" || record.OccurredAt != "2026-05-01T12:34:00+08:00" {
		t.Fatal("source time precision changed")
	}
	if len(original["pam_classification_reference"]) == 0 {
		t.Fatal("unique classification reference missing")
	}
	body, _ := os.ReadFile(input)
	sum := sha256.Sum256(body)
	if batch.SourceFileHash != hex.EncodeToString(sum[:]) {
		t.Fatal("source SHA is not the actual file hash")
	}
	for _, hash := range []string{report.SourceFileHash, report.ReferenceFileHash} {
		if !strings.Contains(batch.IdempotencyKey, hash) {
			t.Fatal("idempotency key omitted source/reference hash")
		}
	}
	if len(batch.IdempotencyKey) > 200 {
		t.Fatal("idempotency key exceeds import limit")
	}
	info, _ := os.Stat(output)
	if info.Mode().Perm() != 0700 {
		t.Fatal("output is not private")
	}
	info, _ = os.Stat(filepath.Join(output, "report.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("report is not private")
	}
}

// TestStableLocationAndContentKeys 验证插入无关行不会漂移既有 key，缺坐标重复也保留不同 occurrence。
func TestStableLocationAndContentKeys(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "current.csv")
	a, b := fixtureRow(), fixtureRow()
	b["原图纵坐标"] = ""
	b["时间"] = "13:00"
	csvFixture(t, input, bankHeader, []map[string]string{a, b})
	one, err := Prepare(input, "", filepath.Join(dir, "one"), 50)
	if err != nil {
		t.Fatal(err)
	}
	before := readBatch(t, filepath.Join(dir, "one"), one.BatchFiles[0])
	extra := fixtureRow()
	extra["原图纵坐标"] = "999"
	extra["时间"] = "10:00"
	csvFixture(t, input, bankHeader, []map[string]string{extra, a, b})
	two, err := Prepare(input, "", filepath.Join(dir, "two"), 50)
	if err != nil {
		t.Fatal(err)
	}
	after := readBatch(t, filepath.Join(dir, "two"), two.BatchFiles[0])
	if before.Records[0].RowKey != after.Records[1].RowKey || before.Records[1].RowKey != after.Records[2].RowKey {
		t.Fatal("unrelated row insertion changed identity")
	}
	c := convert(sourceRow{original: b}, "fixture")
	base, kind := identity(c)
	if kind != "business_content" || base == "" {
		t.Fatal("missing-position identity was not recorded")
	}
}

// TestIndependentDuplicatesRequireBalanceEvidence 验证同分同金额碰撞仅在余额变化精确对应时明确确认。
func TestIndependentDuplicatesRequireBalanceEvidence(t *testing.T) {
	for _, direction := range []string{"支出", "收入"} {
		t.Run(direction, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "current.csv")
			a, b := fixtureRow(), fixtureRow()
			a["收支方向"], b["收支方向"] = direction, direction
			if direction == "收入" {
				a["金额(元)"], b["金额(元)"] = "12.50", "12.50"
			}
			b["原图纵坐标"] = "200"
			b["交易后余额(元)"] = "87.50"
			csvFixture(t, input, bankHeader, []map[string]string{a, b})
			r, err := Prepare(input, "", filepath.Join(dir, "confirmed"), 100)
			if err != nil {
				t.Fatal(err)
			}
			batch := readBatch(t, filepath.Join(dir, "confirmed"), r.BatchFiles[0])
			if r.PreparedRows != 2 || r.ConfirmedDuplicateRows != 1 || batch.Records[0].ConfirmNew || !batch.Records[1].ConfirmNew || batch.Records[0].RowKey == batch.Records[1].RowKey {
				t.Fatal("independent entries were not preserved explicitly")
			}
			b["交易后余额(元)"] = "100.00"
			csvFixture(t, input, bankHeader, []map[string]string{a, b})
			held, err := Prepare(input, "", filepath.Join(dir, "held"), 100)
			if err != nil {
				t.Fatal(err)
			}
			if held.PreparedRows != 1 || held.RejectedRows != 1 || held.ConfirmedDuplicateRows != 0 {
				t.Fatal("different positions alone confirmed a duplicate")
			}
		})
	}
}

// TestAmbiguityAndInvalidRowsRemainCovered 验证无法唯一匹配分类、缺日期/方向不猜测，报告覆盖所有来源行。
func TestAmbiguityAndInvalidRowsRemainCovered(t *testing.T) {
	dir := t.TempDir()
	input, ref := filepath.Join(dir, "current.csv"), filepath.Join(dir, "reference.csv")
	a, b, c := fixtureRow(), fixtureRow(), fixtureRow()
	b["日期"] = ""
	c["收支方向"] = "未标注"
	csvFixture(t, input, bankHeader, []map[string]string{a, b, c})
	csvFixture(t, ref, append(append([]string{}, bankHeader...), classificationHeader...), []map[string]string{a, a})
	r, err := Prepare(input, ref, filepath.Join(dir, "prepared"), 100)
	if err != nil {
		t.Fatal(err)
	}
	if r.InputRows != 3 || r.PreparedRows != 1 || r.RejectedRows != 2 || len(r.Rows) != 3 || r.ReferenceCounts["ambiguous"] != 1 {
		t.Fatal("source rows or ambiguous reference were lost")
	}
	batch := readBatch(t, filepath.Join(dir, "prepared"), r.BatchFiles[0])
	var original map[string]any
	_ = json.Unmarshal(batch.Records[0].Original, &original)
	if original["pam_classification_reference"] != nil {
		t.Fatal("ambiguous reference was attached")
	}
	if _, err = Prepare(input, ref, filepath.Join(dir, "prepared"), 100); err == nil {
		t.Fatal("existing prepared output was overwritten")
	}
}

// TestDatePrecisionAndBatching 验证日期未知时刻保留 date 精度，按 50 行分批且不生成外部流水号。
func TestDatePrecisionAndBatching(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "current.csv")
	rows := []map[string]string{}
	for i := 0; i < 51; i++ {
		row := fixtureRow()
		row["时间"] = "未知"
		row["原图纵坐标"] = ""
		row["交易对象或摘要"] = strings.Repeat("m", i+1)
		rows = append(rows, row)
	}
	csvFixture(t, input, bankHeader, rows)
	r, err := Prepare(input, "", filepath.Join(dir, "prepared"), 50)
	if err != nil {
		t.Fatal(err)
	}
	if r.PreparedRows != 51 || len(r.BatchFiles) != 2 || r.PrecisionCounts["date"] != 51 || r.IdentityCounts["business_content"] != 51 {
		t.Fatal("date precision or batch coverage changed")
	}
}
