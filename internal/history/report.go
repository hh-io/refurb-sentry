package history

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/filter"
)

// Filter 是 -history 的查询条件。Sets 之间是 AND:命令行临时拼出的条件
// 与 -rule 引用的配置规则须同时满足。空的 Set 放行一切,正好充当「没给这项条件」。
//
// 匹配直接复用推送规则的引擎,而不是为查询另写一套:两套各自演进,
// 迟早会出现「规则能命中、查询却查不到」这种静默的不一致。
type Filter struct {
	Sets []*filter.Set
	// NanoTexture 为 nil 表示不限。规则引擎没有这一项,由查询单独判断。
	NanoTexture *bool
}

func (f Filter) match(s Sale, spec filter.Spec) bool {
	if f.NanoTexture != nil && spec.NanoTexture != *f.NanoTexture {
		return false
	}
	p := s.Product()
	for _, set := range f.Sets {
		if ok, _ := set.Match(p, spec); !ok {
			return false
		}
	}
	return true
}

// unknownMemory 报告这次售卖是否只因缺内存维度才没能匹配。
func (f Filter) unknownMemory(s Sale, spec filter.Spec) bool {
	if s.Dimensions[apple.MemoryDimension] != "" {
		return false
	}
	if f.NanoTexture != nil && spec.NanoTexture != *f.NanoTexture {
		return false
	}
	p := s.Product()
	usesMemory := false
	for _, set := range f.Sets {
		if set.UsesDimension(apple.MemoryDimension) {
			usesMemory = true
			if !set.MayMatchWithout(p, spec, apple.MemoryDimension) {
				return false
			}
		} else if ok, _ := set.Match(p, spec); !ok {
			return false
		}
	}
	return usesMemory
}

// group 是同一配置的全部售卖。
type group struct {
	label string
	specs string
	sales []Sale
}

// groupKey 用规格而不是货号分组:同一配置再次上架是否沿用同一个货号没有实测过,
// 按规格分组在两种情况下都成立。标题里已含颜色、芯片与核心数,再补上
// 标题里没有的维度(内存、容量等)就足以区分配置。
func groupKey(s Sale, spec filter.Spec) string {
	keys := make([]string, 0, len(s.Dimensions))
	for k := range s.Dimensions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s|%s|%d|%d|%t", s.Region, s.Category, s.Currency, spec.Chip, spec.CPUCores, spec.GPUCores, spec.NanoTexture)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, s.Dimensions[k])
	}
	return b.String()
}

// Report 按配置分组打印匹配的售卖记录。now 用于计算仍在架商品的在架时长。
func Report(sales []Sale, f Filter, now time.Time, out func(string)) {
	groups := map[string]*group{}
	var order []*group
	matched, unknown := 0, 0
	for _, s := range sales {
		spec := filter.ParseSpec(s.Title)
		if !f.match(s, spec) {
			if f.unknownMemory(s, spec) {
				unknown++
			}
			continue
		}
		matched++
		k := groupKey(s, spec)
		g := groups[k]
		if g == nil {
			g = &group{specs: specLine(s, spec)}
			groups[k] = g
			order = append(order, g)
		}
		g.sales = append(g.sales, s)
		// 标题取最近一次的:上游偶尔改文案,旧标题没有参考价值。
		g.label = s.Region + " · " + s.Title
	}

	if matched == 0 {
		out(fmt.Sprintf("没有匹配的历史记录(档案共 %d 次售卖)", len(sales)))
	}

	// 最近有动静的配置排在前面;组内按时间正序,读起来是一条时间线。
	sort.SliceStable(order, func(i, j int) bool {
		return lastListed(order[i]).After(lastListed(order[j]))
	})
	for _, g := range order {
		out("")
		out(g.label)
		lo, hi := priceRange(g.sales)
		cur := g.sales[0].Currency
		rng := apple.FormatPrice(lo, cur)
		if hi != lo {
			rng += " ~ " + apple.FormatPrice(hi, cur)
		}
		out(fmt.Sprintf("  %s共 %d 次 · %s", g.specs, len(g.sales), rng))
		for _, s := range g.sales {
			out("    " + saleLine(s, now))
		}
	}

	if matched > 0 {
		out("")
		out(fmt.Sprintf("共 %d 个配置、%d 次售卖(档案共 %d 次售卖)", len(order), matched, len(sales)))
	}
	if unknown > 0 {
		out(fmt.Sprintf("另有 %d 次售卖缺少内存维度,无法判断是否匹配(开启 http.fill_missing_memory 可为之后的记录补齐)", unknown))
	}
}

// specLine 列出标题里没有的关键规格。芯片、核心数与颜色已在标题里,不再重复。
func specLine(s Sale, spec filter.Spec) string {
	var parts []string
	if v := s.Dimensions[apple.MemoryDimension]; v != "" {
		parts = append(parts, "内存 "+v)
	}
	if v := s.Dimensions[apple.CapacityDimension]; v != "" {
		parts = append(parts, "存储 "+v)
	}
	if spec.NanoTexture {
		parts = append(parts, "纳米纹理")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ") + " · "
}

func saleLine(s Sale, now time.Time) string {
	const layout = "2006-01-02 15:04"
	listed := " " + s.ListedAt.In(now.Location()).Format(layout)
	if s.ListedApprox {
		listed = "≤" + s.ListedAt.In(now.Location()).Format(layout)
	}
	end, delisted := now, " 在售            "
	if !s.DelistedAt.IsZero() {
		end = s.DelistedAt
		delisted = " " + s.DelistedAt.In(now.Location()).Format(layout)
		if s.DelistedApprox {
			delisted = "≤" + s.DelistedAt.In(now.Location()).Format(layout)
		}
	}
	dur := humanDuration(end.Sub(s.ListedAt))
	// 上架时刻是近似值时它只会比真实值晚,在架时长因此只会被低估;下架时刻是近似值时
	// 则只会被高估。两头都近似时连方向都说不准,不给数字。
	switch {
	case s.ListedApprox && s.DelistedApprox:
		dur = "未知"
	case s.ListedApprox:
		dur = "≥" + dur
	case s.DelistedApprox:
		dur = "≤" + dur
	}

	prices := make([]string, len(s.Prices))
	for i, p := range s.Prices {
		prices[i] = apple.FormatPrice(p.Cents, s.Currency)
	}
	return fmt.Sprintf("%s →%s  在架 %-10s %s  %s",
		listed, delisted, dur, strings.Join(prices, " → "), s.PartNumber)
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%d天%d小时", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d小时%d分", hours, mins)
	default:
		return fmt.Sprintf("%d分", mins)
	}
}

func priceRange(sales []Sale) (lo, hi int64) {
	lo, hi = sales[0].Prices[0].Cents, sales[0].Prices[0].Cents
	for _, s := range sales {
		for _, p := range s.Prices {
			lo, hi = min(lo, p.Cents), max(hi, p.Cents)
		}
	}
	return lo, hi
}

func lastListed(g *group) time.Time {
	return g.sales[len(g.sales)-1].ListedAt
}
