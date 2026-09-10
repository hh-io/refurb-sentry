package apple

import (
	"os"
	"testing"
)

func loadDetailFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/detail.html")
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	return b
}

// 详情页概述里同时存在内存与存储两个容量条目,靠列表页已知的存储容量把存储那条排除掉。
func TestParseOverviewMemory(t *testing.T) {
	mem, ok := ParseOverviewMemory(loadDetailFixture(t), "2tb")
	if !ok {
		t.Fatal("应当解析出内存")
	}
	if mem != "36gb" {
		t.Errorf("期望 36gb,实际 %q", mem)
	}
}

// 不知道存储容量时有两个候选,此时必须放弃而不是挑一个——
// 猜错会让规则匹配到配置完全不同的机器,比读不到更糟。
func TestParseOverviewMemoryAmbiguous(t *testing.T) {
	if mem, ok := ParseOverviewMemory(loadDetailFixture(t), ""); ok {
		t.Errorf("候选不唯一时应当放弃,却返回了 %q", mem)
	}
}

// 存储容量对不上(例如上游改了维度值格式)同样会剩下两个候选,一样必须放弃。
func TestParseOverviewMemoryCapacityMismatch(t *testing.T) {
	if mem, ok := ParseOverviewMemory(loadDetailFixture(t), "512gb"); ok {
		t.Errorf("存储锚点失配时应当放弃,却返回了 %q", mem)
	}
}

func TestParseOverviewMemoryNoOverview(t *testing.T) {
	if _, ok := ParseOverviewMemory([]byte("<html><body>没有概述</body></html>"), "2tb"); ok {
		t.Error("页面无概述时应当返回 false")
	}
}

// "460GB/s 内存带宽" 也含容量单位,若不排除就会和真正的内存条目一起变成两个候选,
// 导致本可解析的页面被判为无法解析。
func TestParseOverviewMemoryIgnoresBandwidth(t *testing.T) {
	html := []byte(`window.pageLevelData.Overview = {"tiles":{"groups":{"items":[` +
		`{"value":{"mutiValueAttributeSelector":{"attributeList":{"items":[` +
		`{"value":"460GB/s 内存带宽"},{"value":"48GB 统一内存"},{"value":"1TB 固态硬盘²"}]}}}}]}}};` + "\n")
	mem, ok := ParseOverviewMemory(html, "1tb")
	if !ok || mem != "48gb" {
		t.Errorf("期望 48gb,实际 %q ok=%v", mem, ok)
	}
}

// 法语站用 Go/To 表示 GB/TB,归一化后才能与列表页的 dimensionCapacity 比较。
func TestParseOverviewMemoryFrenchUnits(t *testing.T) {
	html := []byte(`window.pageLevelData.Overview = {"tiles":{"groups":{"items":[` +
		`{"value":{"mutiValueAttributeSelector":{"attributeList":{"items":[` +
		`{"value":"Mémoire unifiée de 36 Go"},{"value":"SSD de 2 To"}]}}}}]}}};` + "\n")
	mem, ok := ParseOverviewMemory(html, "2tb")
	if !ok || mem != "36gb" {
		t.Errorf("期望 36gb,实际 %q ok=%v", mem, ok)
	}
}

func TestNormalizeCapacity(t *testing.T) {
	cases := map[string]string{
		"2TB": "2tb", "2 TB": "2tb", "2tb": "2tb",
		"36GB": "36gb", "36 Go": "36gb", "2 To": "2tb",
		"512GB": "512gb",
	}
	for in, want := range cases {
		if got := normalizeCapacity(in); got != want {
			t.Errorf("normalizeCapacity(%q) = %q,期望 %q", in, got, want)
		}
	}
}

// 缓存必须区分「查过且没查到」与「没查过」,否则上游永远不给内存的机器
// 会在每一轮都被重新抓一次详情页。
func TestMemoryCacheDistinguishesMissFromUnknown(t *testing.T) {
	c := NewMemoryCache()
	if _, ok := c.Get("FGE74CH/A"); ok {
		t.Error("未查过的货号不该命中缓存")
	}
	c.Put("FGE74CH/A", "")
	v, ok := c.Get("FGE74CH/A")
	if !ok {
		t.Error("查过但没查到的货号也必须命中缓存,否则会被反复重试")
	}
	if v != "" {
		t.Errorf("期望空串,实际 %q", v)
	}
}
