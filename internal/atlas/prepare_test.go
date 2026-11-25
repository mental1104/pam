package atlas

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mental1104/pam/internal/domain"
)

// TestPrepareFinalLedger 验证真实列契约的合成样例能精确保留金额、源列、日期精度和拒绝记录；t 管理临时私有目录。
func TestPrepareFinalLedger(t *testing.T) {
	input, err := filepath.Abs("../../testdata/synthetic/2025-收支流水-最终合并.csv")
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	report, err := Prepare(input, output, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.InputRows != 4 || report.PreparedRows != 2 || report.RejectedRows != 2 || len(report.BatchFiles) != 2 {
		t.Fatalf("unexpected preparation counts: %+v", report)
	}
	for index, name := range report.BatchFiles {
		body, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		var req domain.ImportRequest
		if err = json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req.Source != "atlas_merged" || req.SourceFileHash != report.SourceFileHash || len(req.Records) != 1 {
			t.Fatalf("unexpected batch contract: %+v", req)
		}
		row := req.Records[0]
		if !strings.HasPrefix(row.ExternalID, "2025:") {
			t.Fatal("merged ID was not scoped by source year")
		}
		var original map[string]string
		if err = json.Unmarshal(row.Original, &original); err != nil {
			t.Fatal(err)
		}
		if original["来源记录明细"] == "" || original["收支统计标记"] == "" {
			t.Fatal("source provenance columns were lost")
		}
		if original["pam_source_date"] != original["日期"] || original["pam_source_timezone"] != "Asia/Shanghai" {
			t.Fatal("canonical source date and timezone provenance were lost")
		}
		if index == 0 && (row.Amount != "999" || row.Direction != "expense" || original["金额(元)"] != "-999.00" || original["pam_timestamp_precision"] != "minute") {
			t.Fatal("expense sign normalization or source precision was lost")
		}
		if index == 1 && (row.Amount != "100.01" || !strings.HasSuffix(row.OccurredAt, "T00:00:00+08:00") || original["pam_timestamp_precision"] != "date") {
			t.Fatal("date-only timestamp was not explicitly marked")
		}
	}
	if _, err = Prepare(input, output, 1); err == nil {
		t.Fatal("repeated preparation overwrote existing output")
	}
	if report.RejectionCounts["unsupported_or_unknown_direction"] != 1 || report.RejectionCounts["missing_or_invalid_date"] != 1 {
		t.Fatal("ambiguous records must be explicitly rejected")
	}
}

// TestConvertRejectsSignAndTimeAmbiguity 验证方向与符号冲突及无效时间不被猜测修复；t 是独立测试上下文。
func TestConvertRejectsSignAndTimeAmbiguity(t *testing.T) {
	original := map[string]string{"日期": "2025-01-01", "时间": "25:00", "收支方向": "支出", "金额(元)": "-5", "币种": "CNY", "合并记录ID": "fixture"}
	if _, _, reason := convert(original, 1, "2025", strings.Repeat("a", 64)); reason != "invalid_time" {
		t.Fatalf("unexpected reason: %s", reason)
	}
	original["时间"] = "12:00"
	original["收支方向"] = "收入"
	if _, _, reason := convert(original, 1, "2025", strings.Repeat("a", 64)); reason != "amount_sign_conflicts_with_direction" {
		t.Fatalf("unexpected reason: %s", reason)
	}
}
