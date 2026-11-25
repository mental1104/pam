// pam-prepare 只读解析一份最终合并 CSV 并输出待导入 JSON，不连接网络或数据库。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/mental1104/pam/internal/atlas"
)

// main 从 --input、--output 和 --batch-size 读取路径及批大小；错误以非零退出，成功只打印汇总计数。
func main() {
	input := flag.String("input", "", "absolute final merged CSV path")
	output := flag.String("output", "", "absolute private output directory")
	batchSize := flag.Int("batch-size", 500, "records per prepared batch (1-500)")
	flag.Parse()
	report, err := atlas.Prepare(*input, *output, *batchSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prepare failed:", err)
		os.Exit(1)
	}
	// 日志不输出商户、金额、来源明细或实际记录标识；完整报告只存在指定私有目录。
	_ = json.NewEncoder(os.Stdout).Encode(struct {
		InputRows    int `json:"input_rows"`
		PreparedRows int `json:"prepared_rows"`
		RejectedRows int `json:"rejected_rows"`
		Batches      int `json:"batches"`
		Warnings     int `json:"warnings"`
	}{report.InputRows, report.PreparedRows, report.RejectedRows, len(report.BatchFiles), len(report.Warnings)})
}
