package apple

import (
	"errors"
	"os"
	"testing"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/grid.html")
	if err != nil {
		t.Fatalf("读取 fixture: %v", err)
	}
	return b
}

func TestParseGrid(t *testing.T) {
	region, err := LookupRegion("CN")
	if err != nil {
		t.Fatal(err)
	}
	g, err := ParseGrid(loadFixture(t), region, "mac")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	// 5 个 tile 中:1 个无货号、1 个价格不可解析,应剩 3 个
	if len(g.Products) != 3 {
		t.Fatalf("期望 3 件商品,实际 %d: %+v", len(g.Products), g.Products)
	}
	if g.Skipped != 1 {
		t.Errorf("期望跳过 1 件价格不可解析的商品,实际 %d", g.Skipped)
	}

	p := g.Products[0]
	if p.PartNumber != "FHFA4CH/A" || p.PriceCents != 1419900 || p.Currency != "CNY" {
		t.Errorf("首件商品解析有误: %+v", p)
	}
	if p.Dimensions["tsMemorySize"] != "24gb" {
		t.Errorf("维度未正确解析: %+v", p.Dimensions)
	}
	// fnode 追踪参数每次抓取都在变,必须剥离
	if want := "https://www.apple.com.cn/shop/product/fhfa4ch/a"; p.URL != want {
		t.Errorf("商品链接应剥离查询参数\n  got  %s\n  want %s", p.URL, want)
	}
	if p.Key() != "CN/mac/FHFA4CH/A" {
		t.Errorf("状态键有误: %s", p.Key())
	}

	// watch 分类的 amount 字段混有 HTML,价格必须取自 raw_amount
	if g.Products[1].PriceCents != 159900 {
		t.Errorf("含 HTML 的价格应从 raw_amount 解析,实际 %d", g.Products[1].PriceCents)
	}

	// 标题里的 </script> 曾是正则截取方案的致命伤
	if g.Products[2].PartNumber != "FBORK1CH/A" {
		t.Errorf("标题含 </script> 的商品应被完整解析,实际 %+v", g.Products[2])
	}
	// 解码必须止于第一个对象,不能吃进后面那个 script 里的数据
	for _, prod := range g.Products {
		if prod.PartNumber == "SHOULD_NOT_APPEAR/A" {
			t.Error("解析越界读到了后续 script 标签的内容")
		}
	}

	if len(g.Legends) != 2 || g.Legends[0].Key != "refurbClearModel" {
		t.Errorf("维度图例解析有误: %+v", g.Legends)
	}
}

func TestParseGridNoBootstrap(t *testing.T) {
	region, _ := LookupRegion("CN")
	_, err := ParseGrid([]byte("<html><body>没有商品数据</body></html>"), region, "mac")
	if !errors.Is(err, ErrNoBootstrap) {
		t.Fatalf("期望 ErrNoBootstrap,实际 %v", err)
	}
}

func TestParsePriceCents(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"14199.00", 1419900, false},
		{"999", 99900, false},
		{"102800.00", 10280000, false}, // 日元这类零小数货币
		{"1599.5", 159950, false},
		{"", 0, true},
		{"abc", 0, true},
	}
	for _, c := range cases {
		got, err := parsePriceCents(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePriceCents(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parsePriceCents(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
}

func TestFormatPrice(t *testing.T) {
	cases := []struct {
		cents    int64
		currency string
		want     string
	}{
		{1419900, "CNY", "RMB 14,199"},
		{59900, "USD", "$599"},
		{969900, "HKD", "HK$9,699"},
		{10280000, "JPY", "¥102,800"},
		{159950, "CNY", "RMB 1,599.50"},
		{99900, "XXX", "XXX 999"},
	}
	for _, c := range cases {
		if got := FormatPrice(c.cents, c.currency); got != c.want {
			t.Errorf("FormatPrice(%d, %s) = %q; want %q", c.cents, c.currency, got, c.want)
		}
	}
}

// 香港/日本站的 BaseURL 自带路径前缀,商品相对链接必须挂回站点根。
// 样本取自各站真实的 productDetailsUrl:HK/JP 的相对路径自带 /hk-zh、/jp 前缀,
// 而 CN 站因为域名本身就是 apple.com.cn,路径不带前缀。
func TestProductURL(t *testing.T) {
	cases := []struct {
		region string
		rel    string
		want   string
	}{
		{"HK", "/hk-zh/shop/product/fwuc3zp/a", "https://www.apple.com/hk-zh/shop/product/fwuc3zp/a"},
		{"JP", "/jp/shop/product/fhfa4j/a", "https://www.apple.com/jp/shop/product/fhfa4j/a"},
		{"CN", "/shop/product/fhfa4ch/a", "https://www.apple.com.cn/shop/product/fhfa4ch/a"},
		{"US", "/shop/product/fhfa4ll/a", "https://www.apple.com/shop/product/fhfa4ll/a"},
	}
	for _, c := range cases {
		r, err := LookupRegion(c.region)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.ProductURL(c.rel); got != c.want {
			t.Errorf("%s 商品链接\n  got  %s\n  want %s", c.region, got, c.want)
		}
	}

	// 缺链接时兜底到该地区翻新店首页,而不是某个具体分类
	hk, _ := LookupRegion("HK")
	if got, want := hk.ProductURL(""), "https://www.apple.com/hk-zh/shop/refurbished"; got != want {
		t.Errorf("空链接兜底\n  got  %s\n  want %s", got, want)
	}

	cn, _ := LookupRegion("CN")
	if got := cn.GridURL("mac"); got != "https://www.apple.com.cn/shop/refurbished/mac" {
		t.Errorf("列表页链接有误: %s", got)
	}
}
