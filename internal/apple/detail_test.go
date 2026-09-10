package apple

import (
	"errors"
	"fmt"
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
	mem, err := ParseOverviewMemory(loadDetailFixture(t), "2tb")
	if err != nil {
		t.Fatalf("应当解析出内存: %v", err)
	}
	if mem != "36gb" {
		t.Errorf("期望 36gb,实际 %q", mem)
	}
}

// 不知道存储容量时有两个候选,此时必须放弃而不是挑一个——
// 猜错会让规则匹配到配置完全不同的机器,比读不到更糟。
func TestParseOverviewMemoryAmbiguous(t *testing.T) {
	mem, err := ParseOverviewMemory(loadDetailFixture(t), "")
	if err == nil {
		t.Errorf("候选不唯一时应当放弃,却返回了 %q", mem)
	}
	if !errors.Is(err, ErrMemoryAmbiguous) {
		t.Errorf("应当报候选不唯一,实际 %v", err)
	}
}

// 存储容量对不上(例如上游改了维度值格式)同样会剩下两个候选,一样必须放弃。
func TestParseOverviewMemoryCapacityMismatch(t *testing.T) {
	if mem, err := ParseOverviewMemory(loadDetailFixture(t), "512gb"); err == nil {
		t.Errorf("存储锚点失配时应当放弃,却返回了 %q", mem)
	}
}

func TestParseOverviewMemoryNoOverview(t *testing.T) {
	_, err := ParseOverviewMemory([]byte("<html><body>没有概述</body></html>"), "2tb")
	if !errors.Is(err, ErrNoOverview) {
		t.Errorf("页面无概述时应当报 ErrNoOverview,实际 %v", err)
	}
}

// "460GB/s 内存带宽" 也含容量单位,若不排除就会和真正的内存条目一起变成两个候选,
// 导致本可解析的页面被判为无法解析。
func TestParseOverviewMemoryIgnoresBandwidth(t *testing.T) {
	html := []byte(`window.pageLevelData.Overview = {"tiles":{"groups":{"items":[` +
		`{"value":{"mutiValueAttributeSelector":{"attributeList":{"items":[` +
		`{"value":"460GB/s 内存带宽"},{"value":"48GB 统一内存"},{"value":"1TB 固态硬盘²"}]}}}}]}}};` + "\n")
	mem, err := ParseOverviewMemory(html, "1tb")
	if err != nil || mem != "48gb" {
		t.Errorf("期望 48gb,实际 %q err=%v", mem, err)
	}
}

// 法语站用 Go/To 表示 GB/TB,且数字与单位之间是**不间断空格**(实测 "24\u00a0Go")。
// Go 的 \s 不含 U+00A0,只写 \s* 会让整个法国站的补齐静默失效——这里用真实码位守着。
func TestParseOverviewMemoryFrenchUnits(t *testing.T) {
	// 码位必须拼接进来:反引号是原始字符串,里面的 \u00a0 只是六个字面字符,
	// 那样测的就不是不间断空格了,而且照样会通过——一个防不住任何东西的测试。
	nbsp := "\u00a0"
	html := []byte(`window.pageLevelData.Overview = {"tiles":{"groups":{"items":[` +
		`{"value":{"mutiValueAttributeSelector":{"attributeList":{"items":[` +
		`{"value":"M\u00e9moire unifi\u00e9e de 36` + nbsp + `Go"},` +
		`{"value":"SSD de 2` + nbsp + `To"}]}}}}]}}};` + "\n")
	mem, err := ParseOverviewMemory(html, "2tb")
	if err != nil || mem != "36gb" {
		t.Errorf("期望 36gb,实际 %q err=%v", mem, err)
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

// 各站点在数字与单位之间混用多种 Unicode 空白,逐个码位守住。
// 写成 \u 转义而非字面字符:字面的不间断空格在编辑中极易退化成普通空格,
// 那样这个测试会照常通过,却不再防任何东西。
func TestParseOverviewMemoryUnicodeSpaces(t *testing.T) {
	for name, sp := range map[string]string{
		"普通空格":       "\u0020",
		"不间断空格":      "\u00a0",
		"窄不间断空格":     "\u202f",
		"thin space": "\u2009",
		"全角空格":       "\u3000",
		"无空格":        "",
	} {
		html := []byte(`window.pageLevelData.Overview = {"tiles":{"groups":{"items":[` +
			`{"value":{"mutiValueAttributeSelector":{"attributeList":{"items":[` +
			`{"value":"48` + sp + `GB unified memory"},` +
			`{"value":"1` + sp + `TB SSD"}]}}}}]}}};` + "\n")
		mem, err := ParseOverviewMemory(html, "1tb")
		if err != nil || mem != "48gb" {
			t.Errorf("%s: 期望 48gb,实际 %q err=%v", name, mem, err)
		}
	}
}

// 没有存储锚点时必须放弃,而不是把唯一的容量条目当成内存。
// 实测 watch 详情页只有一条容量(表壳存储),不设这道闸就会把它写进 tsMemorySize。
func TestParseOverviewMemoryRequiresCapacityAnchor(t *testing.T) {
	html := []byte(`window.pageLevelData.Overview = {"tiles":{"groups":{"items":[` +
		`{"value":{"mutiValueAttributeSelector":{"attributeList":{"items":[` +
		`{"value":"32GB 存储容量"}]}}}}]}}};` + "\n")
	mem, err := ParseOverviewMemory(html, "")
	if !errors.Is(err, ErrMemoryAmbiguous) {
		t.Errorf("缺少锚点时应报 ErrMemoryAmbiguous,实际 mem=%q err=%v", mem, err)
	}
}

// 解析层面的失败是永久的(重试无用),传输层面的失败不是——调用方据此决定要不要
// 把「查过没查到」记进缓存。混为一谈会让一次超时永久废掉一个货号的补齐。
func TestIsPermanentMemoryFailure(t *testing.T) {
	permanent := []error{ErrNoOverview, ErrOverviewDecode, ErrMemoryAmbiguous, ErrProductGone}
	for _, e := range permanent {
		if !IsPermanentMemoryFailure(fmt.Errorf("包一层: %w", e)) {
			t.Errorf("%v 应判为永久性失败", e)
		}
	}
	transient := []error{
		errors.New("dial tcp: i/o timeout"),
		&httpError{Status: 503, URL: "https://example.com/p"},
		fmt.Errorf("重试 3 次后仍失败: %w", &httpError{Status: 500}),
	}
	for _, e := range transient {
		if IsPermanentMemoryFailure(e) {
			t.Errorf("%v 不该判为永久性失败,否则一次抖动会永久废掉这个货号的补齐", e)
		}
	}
}
