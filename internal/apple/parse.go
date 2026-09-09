package apple

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNoBootstrap 表示页面返回了 200 但不含商品数据。可能是该分类当前真的空,
// 也可能是 Apple 改版。调用方必须把它与「抓取成功但列表为空」区别对待,
// 绝不能据此把已知商品判为下架。
var ErrNoBootstrap = errors.New("页面中未找到 REFURB_GRID_BOOTSTRAP")

const bootstrapVar = "window.REFURB_GRID_BOOTSTRAP"

// Grid 是一次抓取的解析结果。
type Grid struct {
	Products []Product
	Legends  []DimensionLegend
	// Skipped 记录因价格无法解析而丢弃的条目数,用于日志告警:
	// 持续非零说明 Apple 改了价格字段格式。
	Skipped int
}

// ParseGrid 从列表页 HTML 中提取商品。
//
// 不用正则截取 JSON:商品标题等字符串里可能出现 "</script>",正则会被提前截断。
// 改为定位赋值号后的第一个 '{',交给 json.Decoder 流式解码——它在对象闭合处
// 自然停止,天然正确处理任意深度的嵌套。
func ParseGrid(html []byte, region Region, category string) (*Grid, error) {
	i := bytes.Index(html, []byte(bootstrapVar))
	if i < 0 {
		return nil, ErrNoBootstrap
	}
	j := bytes.IndexByte(html[i:], '{')
	if j < 0 {
		return nil, ErrNoBootstrap
	}

	var bs bootstrap
	dec := json.NewDecoder(bytes.NewReader(html[i+j:]))
	if err := dec.Decode(&bs); err != nil {
		return nil, fmt.Errorf("解码 bootstrap JSON: %w", err)
	}

	g := &Grid{Legends: bs.Dimensions, Products: make([]Product, 0, len(bs.Tiles))}
	for _, t := range bs.Tiles {
		if t.PartNumber == "" {
			continue
		}
		cents, err := parsePriceCents(t.Price.CurrentPrice.RawAmount)
		if err != nil {
			// 价格不可解析的商品无法参与降价判定,只能丢弃;由 Skipped 暴露给日志。
			g.Skipped++
			continue
		}
		currency := t.Price.PriceCurrency
		if currency == "" {
			currency = region.Currency
		}
		g.Products = append(g.Products, Product{
			Region:     region.Code,
			Category:   category,
			PartNumber: t.PartNumber,
			Title:      strings.TrimSpace(t.Title),
			URL:        region.ProductURL(stripQuery(t.ProductDetailsURL)),
			PriceCents: cents,
			Currency:   currency,
			Dimensions: t.Filters.Dimensions,
		})
	}
	return g, nil
}

// stripQuery 去掉商品链接上的 fnode 追踪参数——它每次抓取都在变,
// 留着会让通知里的链接又长又不稳定。
func stripQuery(u string) string {
	if k := strings.IndexByte(u, '?'); k >= 0 {
		return u[:k]
	}
	return u
}
