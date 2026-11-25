package domain

import "testing"

// TestNormalizeMoney 验证金额以字符串精确规范化且拒绝舍入、指数、负数和超范围输入；t 是当前测试上下文。
func TestNormalizeMoney(t *testing.T) {
	for input, want := range map[string]string{"0001.2300": "1.23", "0.0001": "0.0001", "9999999999999999.9999": "9999999999999999.9999", "0000.0000": "0", "42": "42"} {
		got, err := NormalizeMoney(input)
		if err != nil || got != want {
			t.Errorf("NormalizeMoney(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1", "+1", " 1", "1 ", "1e3", "NaN", "1.00001", "10000000000000000", ".1", "1."} {
		if _, err := NormalizeMoney(input); err == nil {
			t.Errorf("accepted invalid decimal %q", input)
		}
	}
}

// TestValidationBoundaries 验证货币、UUID 和分页边界；t 收集错误，不依赖外部服务。
func TestValidationBoundaries(t *testing.T) {
	if err := ValidateCurrency("CNY"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrency("cny"); err == nil {
		t.Fatal("accepted lowercase currency")
	}
	if err := ValidateID("d63cd6a7-5d27-4b73-9841-333193f5e858"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateID("not-an-id"); err == nil {
		t.Fatal("accepted invalid UUID")
	}
	for _, page := range [][2]int{{0, 0}, {201, 0}, {1, -1}} {
		if err := ValidatePage(page[0], page[1]); err == nil {
			t.Errorf("accepted invalid page %v", page)
		}
	}
}
