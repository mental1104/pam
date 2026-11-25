// pam-prepare-cmb 只读用户指定的 2026 招商银行根表，输出私有导入批次，不连接网络或数据库。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/mental1104/pam/internal/cmb"
)

// main 读取明确文件路径及 50–100 行批大小；完整报告只写入 output，控制台仅打印覆盖计数。
func main() {
	input := flag.String("input", "", "absolute authoritative root CSV path")
	reference := flag.String("reference", "", "optional absolute old classification CSV path")
	output := flag.String("output", "", "new absolute private output directory")
	batch := flag.Int("batch-size", 100, "records per batch, between 50 and 100")
	flag.Parse()
	report, err := cmb.Prepare(*input, *reference, *output, *batch)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CMB preparation failed:", err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		Source              string `json:"source"`
		InputRows           int    `json:"input_rows"`
		PreparedRows        int    `json:"prepared_rows"`
		RejectedRows        int    `json:"rejected_rows"`
		Batches             int    `json:"batches"`
		ConfirmedDuplicates int    `json:"confirmed_duplicate_rows"`
		ReferenceMatches    int    `json:"reference_matches"`
	}{report.Source, report.InputRows, report.PreparedRows, report.RejectedRows, len(report.BatchFiles), report.ConfirmedDuplicateRows, report.ReferenceCounts["matched"]})
}
