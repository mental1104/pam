// pam-statement-corpus 只读选定历史 CSV 和截图，生成私有回归对照；不连接数据库，不修改来源。
package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// expectedRow 对照历史 CSV 可识别字段；账本用途分类不作为截图 OCR 的真值。
type expectedRow struct {
	Date      string `json:"date"`
	Time      string `json:"time"`
	Amount    string `json:"amount"`
	Direction string `json:"direction"`
	Merchant  string `json:"merchant"`
}

// sample 保存一个真实截图和对应 CSV 行；CaptureDate 未知时保持空，禁止使用文件修改时间猜测。
type sample struct {
	Platform  string        `json:"platform"`
	Image     string        `json:"image"`
	ImageHash string        `json:"imageHash"`
	Width     int           `json:"width"`
	Height    int           `json:"height"`
	Expected  []expectedRow `json:"expected"`
}

// corpus 标记对照来源的可靠性边界；不把既有历史 CSV 自动升级为本轮人工逐笔复核。
type corpus struct {
	Reference     string   `json:"reference"`
	ReferenceHash string   `json:"referenceHash"`
	GroundTruth   string   `json:"groundTruth"`
	MissingImages []string `json:"missingImages"`
	Samples       []sample `json:"samples"`
}

// build 只读 root 下指定 input CSV；platform 限定三种截图，返回按图片分组的对照及缺图清单。
func build(root, input, platform string) (corpus, error) {
	result := corpus{Reference: input, GroundTruth: "Historical CSV baseline; not independently re-reviewed row by row in this run", MissingImages: []string{}, Samples: []sample{}}
	body, err := os.ReadFile(filepath.Join(root, input))
	if err != nil {
		return result, fmt.Errorf("cannot read selected CSV")
	}
	digest := sha256.Sum256(body)
	result.ReferenceHash = hex.EncodeToString(digest[:])
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\ufeff")))
	header, err := reader.Read()
	if err != nil {
		return result, fmt.Errorf("invalid header")
	}
	groups := map[string][]expectedRow{}
	for {
		values, e := reader.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return result, fmt.Errorf("invalid CSV")
		}
		fields := map[string]string{}
		for i, key := range header {
			fields[key] = values[i]
		}
		name := fields["来源图片"]
		if name == "" {
			return result, fmt.Errorf("missing image reference")
		}
		direction := ""
		if fields["收支方向"] == "支出" {
			direction = "expense"
		}
		if fields["收支方向"] == "收入" {
			direction = "income"
		}
		groups[name] = append(groups[name], expectedRow{Date: fields["日期"], Time: fields["时间"], Amount: strings.TrimLeft(fields["金额(元)"], "+-"), Direction: direction, Merchant: fields["交易对象或摘要"]})
	}
	names := []string{}
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		relative := filepath.Join(filepath.Dir(input), "原始截图", name)
		file, err := os.Open(filepath.Join(root, relative))
		if err != nil {
			result.MissingImages = append(result.MissingImages, relative)
			continue
		}
		config, _, err := image.DecodeConfig(file)
		if err != nil {
			file.Close()
			return result, fmt.Errorf("invalid source image")
		}
		file.Seek(0, io.SeekStart)
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		file.Close()
		if err != nil {
			return result, fmt.Errorf("cannot hash source image")
		}
		result.Samples = append(result.Samples, sample{Platform: platform, Image: relative, ImageHash: hex.EncodeToString(hash.Sum(nil)), Width: config.Width, Height: config.Height, Expected: groups[name]})
	}
	return result, nil
}

// main 校验显式路径和平台，以独占 0600 文件生成 JSON；错误不回显财务正文。
func main() {
	root := flag.String("root", "", "read-only finance root")
	input := flag.String("input", "", "relative selected CSV")
	platform := flag.String("platform", "", "cmb, alipay or wechat")
	output := flag.String("output", "", "new absolute private JSON file")
	flag.Parse()
	if !filepath.IsAbs(*root) || !filepath.IsAbs(*output) || filepath.IsAbs(*input) || strings.HasPrefix(filepath.Clean(*input), "..") || (*platform != "cmb" && *platform != "alipay" && *platform != "wechat") {
		fmt.Fprintln(os.Stderr, "invalid corpus arguments")
		os.Exit(1)
	}
	result, err := build(*root, *input, *platform)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	file, err := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create corpus output")
		os.Exit(1)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(result)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		fmt.Fprintln(os.Stderr, "cannot write corpus")
		os.Exit(1)
	}
	rows := 0
	for _, sample := range result.Samples {
		rows += len(sample.Expected)
	}
	fmt.Printf("samples=%d baseline_rows=%d missing_images=%d\n", len(result.Samples), rows, len(result.MissingImages))
}
