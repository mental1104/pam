package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/mental1104/pam/internal/domain"
)

// TestTransactionPlatformsAndGlobalFiltering 使用隔离 PostgreSQL 实测来源规范化、组合去重、全库 total 和分页。
// t 只创建随机 source 的合成流水；原始来源有未知部分时保留已知标签，不从商户猜测。
func TestTransactionPlatformsAndGlobalFiltering(t *testing.T) {
	s, _ := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source := uniqueSource(t)
	cases := []struct {
		original map[string]string
		expected []string
	}{
		{map[string]string{"来源平台": "微信"}, []string{"wechat"}},
		{map[string]string{"来源平台": "招商银行；微信", "主记录平台": "微信"}, []string{"wechat", "cmb"}},
		{map[string]string{"来源平台": "支付宝；银行待识别"}, []string{"alipay"}},
		{map[string]string{"来源平台": "用户补充（支付宝余额）"}, []string{"alipay"}},
		{map[string]string{"来源平台": "招商银行（用户补充）"}, []string{"cmb"}},
		{map[string]string{"来源平台": "建设银行"}, []string{"ccb"}},
		{map[string]string{"来源平台": "银行待识别"}, []string{"other"}},
		{map[string]string{}, []string{"other"}},
		{map[string]string{"来源平台": "WeChat Pay", "主记录平台": "支付宝; 招商银行"}, []string{"wechat", "alipay", "cmb"}},
		{map[string]string{"来源平台": "疑似微信"}, []string{"other"}},
		{map[string]string{"来源平台": "微信，微信/ 微信支付|wechat"}, []string{"wechat"}},
	}
	rows := []domain.ImportRecord{}
	stamp, _ := time.Parse(time.RFC3339, "2025-01-02T12:00:00+08:00")
	for i, item := range cases {
		row := fixtureRow()
		row.ExternalID = fmt.Sprintf("platform-fixture-%02d", i)
		row.Merchant = fmt.Sprintf("Synthetic WeChat Alipay merchant %02d", i)
		row.Category = "platform-fixture"
		row.OccurredAt = stamp.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		row.Original, _ = json.Marshal(item.original)
		rows = append(rows, row)
	}
	req := domain.ImportRequest{Source: source, IdempotencyKey: "initial", Records: rows}
	result, err := s.Import(ctx, req)
	if err != nil || result.Created != len(rows) {
		t.Fatal("cannot import synthetic platform fixtures")
	}
	for i, item := range cases {
		got, err := s.GetTransaction(ctx, *result.Rows[i].TransactionID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Platforms, item.expected) {
			t.Fatalf("platform case %d returned %v instead of %v", i, got.Platforms, item.expected)
		}
	}
	// 用小页证明筛选发生在数据库分页前，并证明组合标签不会复制 total 或列表项。
	for platform, total := range map[string]int64{"wechat": 4, "alipay": 3, "cmb": 3, "ccb": 1, "other": 3} {
		seen := map[string]bool{}
		for offset := int64(0); offset <= total; offset++ {
			page, err := s.ListTransactions(ctx, domain.TransactionFilter{Source: source, Platform: platform, Limit: 1, Offset: int(offset)})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != total {
				t.Fatalf("platform %s total=%d expected=%d", platform, page.Total, total)
			}
			if offset == total {
				if len(page.Items) != 0 {
					t.Fatal("filtered empty page contained rows")
				}
				continue
			}
			if len(page.Items) != 1 || seen[page.Items[0].ID] {
				t.Fatal("global platform pagination omitted or duplicated a row")
			}
			seen[page.Items[0].ID] = true
			contains := false
			for _, label := range page.Items[0].Platforms {
				if label == platform {
					contains = true
				}
			}
			if !contains {
				t.Fatal("returned source labels disagree with filter")
			}
		}
	}
	combined, err := s.ListTransactions(ctx, domain.TransactionFilter{Source: source, Platform: "cmb", Search: "merchant 01", Direction: "expense", Category: "platform-fixture", MinAmount: "999", MaxAmount: "999.00", From: "2025-01-01T00:00:00+08:00", To: "2026-01-01T00:00:00+08:00", Limit: 1})
	if err != nil || combined.Total != 1 || len(combined.Items) != 1 || combined.Items[0].ID != *result.Rows[1].TransactionID {
		t.Fatal("platform did not combine with existing filters")
	}
	// 同一流水再次导入产生来源追溯行，但不能让读取来源的连接放大返回行数。
	req.IdempotencyKey = "repeated-provenance"
	replay, err := s.Import(ctx, req)
	if err != nil || replay.Skipped != len(rows) {
		t.Fatal("cannot create repeated provenance fixture")
	}
	page, err := s.ListTransactions(ctx, domain.TransactionFilter{Source: source, Platform: "wechat", Limit: 200})
	if err != nil || page.Total != 4 || len(page.Items) != 4 {
		t.Fatal("additional provenance multiplied transactions")
	}
}

// TestCMBStatementPlatformPreservesExplicitOrigins 验证银行来源默认 cmb，同时保留未来明确记录的支付平台。
// t 使用随机外部编号和搜索标识隔离同名 source 的测试数据，不读取真实财务记录。
func TestCMBStatementPlatformPreservesExplicitOrigins(t *testing.T) {
	s, _ := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	marker := uniqueSource(t)
	a, b := fixtureRow(), fixtureRow()
	a.Merchant = marker + " WeChat Alipay"
	b.Merchant = a.Merchant
	a.ExternalID = marker + "-bank"
	b.ExternalID = marker + "-explicit"
	a.Original = json.RawMessage(`{"备注":"Synthetic WeChat Alipay merchant text is not a source"}`)
	b.Original = json.RawMessage(`{"来源平台":"微信；支付宝","主记录平台":"招商银行"}`)
	result, err := s.Import(ctx, domain.ImportRequest{Source: "cmb_statement", IdempotencyKey: marker, Records: []domain.ImportRecord{a, b}})
	if err != nil || result.Created != 2 {
		t.Fatal("cannot import synthetic CMB fixtures")
	}
	for i, expected := range [][]string{{"cmb"}, {"wechat", "alipay", "cmb"}} {
		got, err := s.GetTransaction(ctx, *result.Rows[i].TransactionID)
		if err != nil || !reflect.DeepEqual(got.Platforms, expected) {
			t.Fatal("bank statement source discarded confirmed labels or inferred a merchant platform")
		}
	}
	for platform, total := range map[string]int64{"cmb": 2, "wechat": 1, "alipay": 1, "other": 0} {
		page, err := s.ListTransactions(ctx, domain.TransactionFilter{Source: "cmb_statement", Search: marker, Platform: platform, Limit: 50})
		if err != nil || page.Total != total || int64(len(page.Items)) != total {
			t.Fatal("CMB filtering disagrees with returned platform labels")
		}
	}
}
