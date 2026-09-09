package filter

import (
	"testing"

	"github.com/hh-io/refurb-sentry/internal/apple"
)

func mbp() apple.Product {
	return apple.Product{
		Region: "CN", Category: "mac", PartNumber: "FHFA4CH/A",
		Title:      "翻新 14 英寸 MacBook Pro Apple M5 Pro 芯片 (配备 12 核中央处理器和 16 核图形处理器) - 深空黑色",
		PriceCents: 1419900, Currency: "CNY",
		Dimensions: map[string]string{
			"refurbClearModel": "macbookpro", "tsMemorySize": "24gb",
			"dimensionCapacity": "1tb", "dimensionScreensize": "14inch",
		},
	}
}

func mustSet(t *testing.T, rules []Rule) *Set {
	t.Helper()
	s, err := New(rules)
	if err != nil {
		t.Fatalf("编译规则失败: %v", err)
	}
	return s
}

func match(t *testing.T, rules []Rule, p apple.Product) bool {
	t.Helper()
	ok, _ := mustSet(t, rules).Match(p, ParseSpec(p.Title))
	return ok
}

// 未配置规则时放行全部,即「监控整个翻新店」。
func TestEmptyRuleSetMatchesAll(t *testing.T) {
	if !match(t, nil, mbp()) {
		t.Error("空规则集应匹配全部商品")
	}
}

// 同一维度内多个候选值是 OR,不同维度之间是 AND。
func TestDimensionSemantics(t *testing.T) {
	p := mbp()

	if !match(t, []Rule{{Dimensions: map[string][]string{"tsMemorySize": {"16gb", "24gb"}}}}, p) {
		t.Error("同一维度内应为 OR 语义")
	}
	if match(t, []Rule{{Dimensions: map[string][]string{
		"refurbClearModel": {"macbookpro"},
		"tsMemorySize":     {"64gb"},
	}}}, p) {
		t.Error("不同维度之间应为 AND 语义")
	}
	// 规则要求了某个维度,但商品没有该维度(如拿 watch 的键去筛 mac)
	if match(t, []Rule{{Dimensions: map[string][]string{"dimensionCaseSize": {"40mm"}}}}, p) {
		t.Error("商品缺少规则要求的维度时不应匹配")
	}
	// 配置里的大小写不该影响匹配
	if !match(t, []Rule{{Dimensions: map[string][]string{"refurbClearModel": {"MacBookPro"}}}}, p) {
		t.Error("维度取值匹配应忽略大小写")
	}
}

func TestChipAndCores(t *testing.T) {
	p := mbp()

	if !match(t, []Rule{{Chips: []string{"M5 Pro", "M5 Max"}}}, p) {
		t.Error("芯片应命中 M5 Pro")
	}
	if match(t, []Rule{{Chips: []string{"M4"}}}, p) {
		t.Error("M4 不应命中 M5 Pro 的机器")
	}
	if !match(t, []Rule{{MinCPUCores: 12, MinGPUCores: 16}}, p) {
		t.Error("12 核 CPU / 16 核 GPU 应满足下限")
	}
	if match(t, []Rule{{MinCPUCores: 14}}, p) {
		t.Error("12 核不应满足 14 核下限")
	}

	// 核心数无法解析时(如显示器、手表)不得被当作满足下限
	noSpec := mbp()
	noSpec.Title = "翻新產品 Apple Studio Display,納米紋理玻璃"
	if match(t, []Rule{{MinCPUCores: 8}}, noSpec) {
		t.Error("核心数未识别时不应满足任何下限要求")
	}
}

func TestPriceRange(t *testing.T) {
	p := mbp() // 14199.00
	if !match(t, []Rule{{MaxPrice: 20000}}, p) {
		t.Error("低于上限应匹配")
	}
	if match(t, []Rule{{MaxPrice: 10000}}, p) {
		t.Error("高于上限不应匹配")
	}
	if match(t, []Rule{{MinPrice: 20000}}, p) {
		t.Error("低于下限不应匹配")
	}
	if !match(t, []Rule{{MinPrice: 10000, MaxPrice: 20000}}, p) {
		t.Error("区间内应匹配")
	}
}

func TestRegionAndCategoryScope(t *testing.T) {
	p := mbp()
	if match(t, []Rule{{Regions: []string{"US"}}}, p) {
		t.Error("地区不符不应匹配")
	}
	if !match(t, []Rule{{Regions: []string{"CN", "US"}}}, p) {
		t.Error("地区在候选列表中应匹配")
	}
	if match(t, []Rule{{Categories: []string{"ipad"}}}, p) {
		t.Error("分类不符不应匹配")
	}
}

// title_match 面对的应是归一化后的标题,否则德国站的 U+2011 会让正则失配。
func TestTitleMatchUsesNormalizedText(t *testing.T) {
	p := mbp()
	p.Title = `Refurbished 14" MacBook Pro mit Apple M5 Chip, 10‑Core CPU und 10‑Core GPU`
	if !match(t, []Rule{{TitleMatch: `10-Core CPU`}}, p) {
		t.Error("规则正则应作用于归一化后的标题")
	}
}

// 多条规则之间是 OR,并应回报命中的规则名。
func TestMultipleRulesReportNames(t *testing.T) {
	s := mustSet(t, []Rule{
		{Name: "太贵", MinPrice: 90000},
		{Name: "MBP", Dimensions: map[string][]string{"refurbClearModel": {"macbookpro"}}},
		{Name: "高内存", Dimensions: map[string][]string{"tsMemorySize": {"24gb"}}},
	})
	p := mbp()
	ok, names := s.Match(p, ParseSpec(p.Title))
	if !ok || len(names) != 2 || names[0] != "MBP" || names[1] != "高内存" {
		t.Fatalf("期望命中 MBP 与 高内存,实际 ok=%v names=%v", ok, names)
	}
}

func TestInvalidRulesRejected(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
	}{
		{"正则不合法", Rule{TitleMatch: "([unclosed"}},
		{"未知地区", Rule{Regions: []string{"ZZ"}}},
		{"未知分类", Rule{Categories: []string{"banana"}}},
		{"价格区间颠倒", Rule{MinPrice: 100, MaxPrice: 10}},
		{"维度无候选值", Rule{Dimensions: map[string][]string{"tsMemorySize": {}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New([]Rule{c.rule}); err == nil {
				t.Fatal("应当在编译规则时报错")
			}
		})
	}
}
