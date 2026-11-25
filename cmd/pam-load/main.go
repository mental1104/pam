// pam-load 只提交已经由 Agent/准备工具生成的规范化批次，不解析或推断异构账单。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mental1104/pam/internal/domain"
)

// receipt 记录每个请求内容的摘要和后端统计；不重复保存原始财务正文或访问凭据。
type receipt struct {
	File   string              `json:"file"`
	SHA256 string              `json:"sha256"`
	Result domain.ImportResult `json:"result"`
}

// summary 汇总本次实际写入情况；仅完整成功后附带 COMPLETE 文件，便于区分中途取消。
type summary struct {
	Batches    int `json:"batches"`
	Created    int `json:"created"`
	Skipped    int `json:"skipped"`
	Duplicated int `json:"duplicated"`
	Failed     int `json:"failed"`
}

var batchName = regexp.MustCompile(`^batch-[0-9]{4}\.json$`)

// main 读取明确 --apply 操作和文件路径；SIGINT 取消后停止下一批，已提交批次可安全重放。
func main() {
	directory := flag.String("directory", "", "absolute prepared batch directory")
	output := flag.String("output", "", "new absolute receipt directory")
	base := flag.String("url", os.Getenv("PAM_BASE_URL"), "backend origin")
	apply := flag.Bool("apply", false, "submit prepared batches")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, *directory, *output, *base, *apply); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run 将 directory 中已准备批次按文件顺序提交到 base，并将独立回执写入新的 output。
// ctx 取消中止请求；apply 必须显式为真，token 只从环境读取；HTTP/业务失败立即停止，保留已完成回执。
func run(ctx context.Context, directory, output, base string, apply bool) error {
	if !apply || !filepath.IsAbs(directory) || !filepath.IsAbs(output) {
		return fmt.Errorf("--apply and absolute directory/output paths are required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid backend origin")
	}
	token := os.Getenv("PAM_ACCESS_TOKEN")
	if len(token) < 32 {
		return fmt.Errorf("PAM_ACCESS_TOKEN is required")
	}
	files := []string{}
	// 文件名只接受准备工具的固定输出；不会把报告或任意 JSON 误提交为金融导入。
	err = filepath.WalkDir(directory, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinks are not accepted")
		}
		if !d.IsDir() && batchName.MatchString(d.Name()) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cannot enumerate prepared batches")
	}
	sort.Strings(files)
	if len(files) == 0 {
		return fmt.Errorf("no prepared batches found")
	}
	if err = os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("receipt directory must not already exist")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	endpoint := strings.TrimRight(base, "/") + "/api/v1/imports/transactions"
	totals := summary{}
	for i, file := range files {
		body, err := os.ReadFile(file)
		if err != nil || len(body) > 8<<20 {
			return fmt.Errorf("batch %d is unreadable or too large", i+1)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("cannot create import request")
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("batch %d request failed", i+1)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != 200 {
			return fmt.Errorf("batch %d returned HTTP %d", i+1, response.StatusCode)
		}
		var result domain.ImportResult
		if err = json.Unmarshal(data, &result); err != nil || result.BatchID == "" {
			return fmt.Errorf("batch %d returned invalid statistics", i+1)
		}
		digest := sha256.Sum256(body)
		record := receipt{File: file, SHA256: hex.EncodeToString(digest[:]), Result: result}
		if err = save(filepath.Join(output, fmt.Sprintf("receipt-%04d.json", i+1)), record); err != nil {
			return err
		}
		totals.Batches++
		totals.Created += result.Created
		totals.Skipped += result.Skipped
		totals.Duplicated += result.Duplicated
		totals.Failed += result.Failed
		if result.Duplicated != 0 || result.Failed != 0 {
			return fmt.Errorf("batch %d requires review; saved receipt before stopping", i+1)
		}
	}
	if err = save(filepath.Join(output, "summary.json"), totals); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(output, "COMPLETE"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0600); err != nil {
		return fmt.Errorf("cannot mark import complete")
	}
	data, _ := json.Marshal(totals)
	fmt.Println(string(data))
	return nil
}

// save 将 value 编码为可审阅 JSON 并排他写入 path；失败返回通用错误，避免输出财务内容。
func save(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode receipt")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("cannot create receipt")
	}
	_, err = file.Write(append(data, '\n'))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("cannot write receipt")
	}
	return nil
}
