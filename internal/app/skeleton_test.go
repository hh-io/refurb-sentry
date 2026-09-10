package app

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/filter"
)

func testScope(t *testing.T, code, category string) scope {
	t.Helper()
	region, err := apple.LookupRegion(code)
	if err != nil {
		t.Fatal(err)
	}
	return scope{region: region, category: category}
}

// parseSkeleton 把骨架片段当成真配置解析,这正是用户复制粘贴后会发生的事。
func parseSkeleton(t *testing.T, snippet string) filter.Rule {
	t.Helper()
	var doc struct {
		Rules []filter.Rule `yaml:"rules"`
	}
	if err := yaml.Unmarshal([]byte("rules:\n"+snippet+"\n"), &doc); err != nil {
		t.Fatalf("骨架不是合法 YAML: %v\n%s", err, snippet)
	}
	if len(doc.Rules) != 1 {
		t.Fatalf("骨架应解析出 1 条规则,实际 %d 条", len(doc.Rules))
	}
	return doc.Rules[0]
}

// 骨架是给人整段复制进配置的。维度取值直接来自上游,含逗号或冒号的值随时可能冒出来,
// 裸写进 flow 序列会改变 YAML 语义——那种失败只会在用户粘贴的那天才暴露。
func TestRuleSkeletonQuotesAmbiguousValues(t *testing.T) {
	idx := dimensionIndex{
		keys: []string{"refurbClearModel", "tricky"},
		values: map[string][]string{
			"refurbClearModel": {"macbookpro", "macmini"},
			"tricky":           {"a,b", "c: d", "#e", "", " f ", `g"h`, `i\j`},
		},
		chips: []string{"M4 Pro", "M5 Max"},
	}

	rule := parseSkeleton(t, formatRuleSkeleton(testScope(t, "CN", "mac"), idx))

	if !slices.Equal(rule.Regions, []string{"CN"}) || !slices.Equal(rule.Categories, []string{"mac"}) {
		t.Errorf("范围不对: regions=%v categories=%v", rule.Regions, rule.Categories)
	}
	if !slices.Equal(rule.Chips, idx.chips) {
		t.Errorf("chips = %v, 期望 %v", rule.Chips, idx.chips)
	}
	for k, want := range idx.values {
		if got := rule.Dimensions[k]; !slices.Equal(got, want) {
			t.Errorf("dimensions[%s] = %q, 期望 %q", k, got, want)
		}
	}
}

// legends 声明了却当前无货的维度不能进骨架:写进规则等于加了一个永不匹配的 AND 条件,
// 用户会得到一条静默不推送的规则。表格里仍要列出它,那是「有这个维度,只是眼下没货」的信息。
func TestRuleSkeletonSkipsEmptyDimensions(t *testing.T) {
	idx := dimensionIndex{
		keys: []string{"refurbClearModel", "dimensionColor"},
		values: map[string][]string{
			"refurbClearModel": {"macmini"},
			"dimensionColor":   {},
		},
	}

	snippet := formatRuleSkeleton(testScope(t, "CN", "mac"), idx)
	if strings.Contains(snippet, "dimensionColor") {
		t.Errorf("无取值的维度不应出现在骨架里:\n%s", snippet)
	}
	if _, ok := parseSkeleton(t, snippet).Dimensions["refurbClearModel"]; !ok {
		t.Error("有取值的维度反而丢了")
	}
}

// 全无维度可列时不能吐出空的 dimensions: 映射——yaml 会把它解成 nil 无妨,
// 但骨架里留一个空壳会让人以为是自己删漏了。
func TestRuleSkeletonOmitsEmptySections(t *testing.T) {
	snippet := formatRuleSkeleton(testScope(t, "US", "ipad"), dimensionIndex{})
	if strings.Contains(snippet, "dimensions:") || strings.Contains(snippet, "chips:") {
		t.Errorf("没有取值时不该出现 dimensions/chips:\n%s", snippet)
	}
	parseSkeleton(t, snippet)
}

// 端到端:骨架列的是当前全部在售取值,拿它编译成规则集后,本轮抓到的商品应当条条命中。
// 这一条防的是键名或取值在格式化途中被改写(大小写、trim、legend 名冒充 key),
// 那种错误不会让 YAML 解析失败,只会让用户配好规则却一条推送都收不到。
func TestRuleSkeletonMatchesEveryProductItCameFrom(t *testing.T) {
	sc := testScope(t, "CN", "mac")
	grid := &apple.Grid{
		Legends: []apple.DimensionLegend{
			{Key: "refurbClearModel", Legend: "机型"},
			{Key: "tsMemorySize", Legend: "内存"},
		},
		Products: []apple.Product{
			{
				Region: "CN", Category: "mac", PartNumber: "A", PriceCents: 100,
				Title:      "翻新 14 英寸 MacBook Pro Apple M4 Pro 芯片 (配备 12 核中央处理器和 16 核图形处理器)",
				Dimensions: map[string]string{"refurbClearModel": "macbookpro", "tsMemorySize": "24gb"},
			},
			{
				Region: "CN", Category: "mac", PartNumber: "B", PriceCents: 200,
				Title:      "翻新 Mac mini Apple M4 芯片 (配备 10 核中央处理器和 10 核图形处理器)",
				Dimensions: map[string]string{"refurbClearModel": "macmini", "tsMemorySize": "16gb"},
			},
		},
	}

	idx := indexDimensions(grid)
	set, err := filter.New([]filter.Rule{parseSkeleton(t, formatRuleSkeleton(sc, idx))})
	if err != nil {
		t.Fatalf("骨架编译成规则集失败: %v", err)
	}
	for _, p := range grid.Products {
		if ok, _ := set.Match(p, filter.ParseSpec(p.Title)); !ok {
			t.Errorf("骨架来自这批商品,却匹配不上 %s(%s)", p.PartNumber, p.Title)
		}
	}
}

// 键的顺序照搬上游 legends,legends 未声明的按字母序补在后面——
// 表格与骨架共用这一份顺序,两处对不上会让人怀疑自己看错了行。
func TestIndexDimensionsOrdersKeysByLegendThenAlpha(t *testing.T) {
	grid := &apple.Grid{
		Legends: []apple.DimensionLegend{{Key: "tsMemorySize"}, {Key: "refurbClearModel"}},
		Products: []apple.Product{{
			Dimensions: map[string]string{
				"tsMemorySize": "16gb", "refurbClearModel": "macmini",
				"zzz": "1", "aaa": "2",
			},
		}},
	}

	idx := indexDimensions(grid)
	want := []string{"tsMemorySize", "refurbClearModel", "aaa", "zzz"}
	if !slices.Equal(idx.keys, want) {
		t.Errorf("keys = %v, 期望 %v", idx.keys, want)
	}
}

// 骨架里的每一行都必须来自当前真实在售的数据。
//
// 曾经末尾附过 "# min_cpu_cores: 12" 与 "# max_price: 20000" 两行提示,
// 它们与这一刻的数据、与用户的需求都毫无关系,纯粹是从示例配置抄来的数字,
// 却混在一整段实测值里——读者会合理地认为整段都是同一性质。实测确实误导过用户。
// 更糟的是顺手去掉那个 # 之后,max_price: 20000 会把 CN 站 2.1 万起步的
// 高配 MacBook Pro 全部静默挡在门外,而这正是本项目最值得监控的一档。
//
// 字段说明属于 README 的规则字段速查表:文档里写「这个字段存在」是说明,
// 数据输出里写「= 20000」则像是个结论。
func TestRuleSkeletonCarriesNoInventedValues(t *testing.T) {
	sc := testScope(t, "CN", "mac")
	grid := &apple.Grid{
		Legends: []apple.DimensionLegend{{Key: "refurbClearModel", Legend: "机型"}},
		Products: []apple.Product{{
			Region: "CN", Category: "mac", PartNumber: "A", PriceCents: 100,
			Title:      "翻新 14 英寸 MacBook Pro Apple M4 Pro 芯片 (配备 12 核中央处理器和 16 核图形处理器)",
			Dimensions: map[string]string{"refurbClearModel": "macbookpro"},
		}},
	}

	got := formatRuleSkeleton(sc, indexDimensions(grid))
	for _, banned := range []string{"min_cpu_cores", "min_gpu_cores", "max_price", "min_price"} {
		if strings.Contains(got, banned) {
			t.Errorf("骨架不该出现 %q:它没有可枚举的取值,写进去的只能是编造的数字\n%s", banned, got)
		}
	}
}

// 骨架不该以换行结尾:调用方每段都会再补一个换行,留着会多出空行。
func TestRuleSkeletonHasNoTrailingNewline(t *testing.T) {
	sc := testScope(t, "CN", "watch")
	grid := &apple.Grid{
		Legends: []apple.DimensionLegend{{Key: "dimensionCaseSize", Legend: "表壳尺寸"}},
		Products: []apple.Product{{
			Region: "CN", Category: "watch", PartNumber: "W", PriceCents: 100,
			Title:      "翻新 Apple Watch Ultra 4",
			Dimensions: map[string]string{"dimensionCaseSize": "49mm"},
		}},
	}
	got := formatRuleSkeleton(sc, indexDimensions(grid))
	if strings.HasSuffix(got, "\n") {
		t.Errorf("骨架不该以换行结尾:\n%q", got)
	}
}
