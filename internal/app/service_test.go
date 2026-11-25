package app

import (
	"encoding/json"
	"testing"

	"github.com/mental1104/pam/internal/domain"
)

// fixtureRow 返回携带原始来源的合法流水输入，供指纹和边界测试独立修改。
func fixtureRow() domain.ImportRecord {
	return domain.ImportRecord{OccurredAt: "2025-01-02T12:00:00+08:00", Direction: "expense", Amount: "999.00", Currency: "CNY", Merchant: "Yifang City", Description: "Electronic payment", Original: json.RawMessage(`{"amount":"999.00","merchant":"Yifang City"}`)}
}

// TestFingerprintCanonicalization 验证等价金额与时区共享候选指纹，而金额变化保留差异；t 是测试上下文。
func TestFingerprintCanonicalization(t *testing.T) {
	a := fixtureRow()
	b := a
	b.Amount = "999"
	b.OccurredAt = "2025-01-02T04:00:00Z"
	pa, pb := prepare("wechat", a, false), prepare("wechat", b, false)
	if pa.ValidationFailure != "" || pb.ValidationFailure != "" || pa.Fingerprint != pb.Fingerprint {
		t.Fatal("equivalent timestamps and decimals must share fingerprint")
	}
	b.Amount = "999.01"
	if prepare("wechat", b, false).Fingerprint == pa.Fingerprint {
		t.Fatal("different amounts must not share fingerprint")
	}
}

// TestPrepareRejectsUntraceableRows 验证完整文件来源必须有行身份且原始来源必须为对象；t 是测试上下文。
func TestPrepareRejectsUntraceableRows(t *testing.T) {
	a := fixtureRow()
	if p := prepare("wechat", a, true); p.ValidationFailure == "" {
		t.Fatal("accepted missing source row key")
	}
	a.RowKey = "line:4"
	a.Original = json.RawMessage(`[]`)
	if p := prepare("wechat", a, true); p.ValidationFailure == "" {
		t.Fatal("accepted array as source record")
	}
	a = fixtureRow()
	a.OccurredAt = "2025-01-02T12:00:00"
	if p := prepare("wechat", a, false); p.ValidationFailure == "" {
		t.Fatal("accepted ambiguous timezone")
	}
}

// TestAssetValidation 验证库存购买金额保持精确、关联 ID 和状态遵循业务规则；t 是测试上下文。
func TestAssetValidation(t *testing.T) {
	price := "1234.5600"
	date := "2025-03-02"
	a, err := validateAsset(domain.AssetInput{Name: "Camera", Currency: "CNY", PurchasePrice: &price, PurchaseDate: &date})
	if err != nil || *a.PurchasePrice != "1234.56" || a.Status != "active" {
		t.Fatalf("unexpected normalized asset: %+v, %v", a, err)
	}
	badDate := "2025-02-30"
	a.PurchaseDate = &badDate
	if _, err = validateAsset(a); err == nil {
		t.Fatal("accepted nonexistent purchase date")
	}
	a.PurchaseDate = nil
	a.Status = "unknown"
	if _, err = validateAsset(a); err == nil {
		t.Fatal("accepted invalid inventory status")
	}
}
