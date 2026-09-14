package history

import (
	"sort"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
)

// PricePoint 是一次售卖期间的一个价格,At 是该价格开始生效的时刻。
type PricePoint struct {
	At    time.Time
	Cents int64
}

// Sale 是一件商品从上架到下架的一次售卖,由档案里的多行折叠而来。
type Sale struct {
	Region     string
	Category   string
	PartNumber string
	Title      string
	URL        string
	Currency   string
	Dimensions map[string]string

	ListedAt time.Time
	// ListedApprox 表示真实上架时刻只知道不晚于 ListedAt:建档时它已在架,
	// 或者档案里缺了它的上架记录。
	ListedApprox bool
	// DelistedAt 为零值表示截至档案最后一行它仍在架。
	DelistedAt time.Time
	// Prices 按时间排列,第一项是上架价,至少有一项。
	Prices []PricePoint
}

// Price 返回最后一个价格。
func (s Sale) Price() int64 { return s.Prices[len(s.Prices)-1].Cents }

// Product 供规则引擎匹配。价格取最后一个:它就是这次售卖最终的标价。
func (s Sale) Product() apple.Product {
	return apple.Product{
		Region: s.Region, Category: s.Category, PartNumber: s.PartNumber,
		Title: s.Title, URL: s.URL, PriceCents: s.Price(), Currency: s.Currency,
		Dimensions: s.Dimensions,
	}
}

// Fold 把档案记录折叠成按上架时间排列的售卖列表。
//
// 档案允许出现重复:写完档案、落状态之前进程被杀,下一轮会把同一批变动再写一遍
// (重复优于丢失,见 app 包的 recordHistory);状态文件被删掉重建时,
// 在架商品会再记一遍 baseline。因此同一件商品在架期间再出现 listed/baseline,
// 一律并入当前这次售卖,而不是凭空多出一次。
func Fold(recs []Record) []Sale {
	sorted := make([]Record, len(recs))
	copy(sorted, recs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	var sales []*Sale
	open := map[string]*Sale{}
	for _, r := range sorted {
		k := r.Key()
		s := open[k]
		if s == nil {
			s = startSale(r)
			sales = append(sales, s)
			open[k] = s
		}
		s.merge(r)
		if r.Kind == KindDelisted {
			s.DelistedAt = r.Time
			delete(open, k)
		}
	}

	out := make([]Sale, len(sales))
	for i, s := range sales {
		out[i] = *s
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ListedAt.Before(out[j].ListedAt) })
	return out
}

func startSale(r Record) *Sale {
	s := &Sale{
		Region: r.Region, Category: r.Category, PartNumber: r.PartNumber,
		Dimensions: map[string]string{},
		ListedAt:   r.FirstSeen,
		// 只有看到 listed 才能确定上架时刻;以 price/delisted 开头说明前面的记录缺了。
		ListedApprox: r.Kind != KindListed,
	}
	if s.ListedAt.IsZero() || r.Kind == KindListed {
		s.ListedAt = r.Time
	}
	s.Prices = []PricePoint{{At: s.ListedAt, Cents: firstPrice(r)}}
	return s
}

// firstPrice 是一次售卖开头的价格。以调价记录开头时,调价之前的那个价格才是上架价。
func firstPrice(r Record) int64 {
	if r.Kind == KindPrice && r.OldPriceCents > 0 {
		return r.OldPriceCents
	}
	return r.PriceCents
}

func (s *Sale) merge(r Record) {
	s.Title, s.URL, s.Currency = r.Title, r.URL, r.Currency
	// 维度按「最后一次非空」合并:上架那一刻详情页补内存可能恰好失败,
	// 之后的记录(尤其是下架行,它取自状态库的最新值)会把它带回来。
	for k, v := range r.Dimensions {
		if v != "" {
			s.Dimensions[k] = v
		}
	}
	if r.PriceCents != s.Price() {
		s.Prices = append(s.Prices, PricePoint{At: r.Time, Cents: r.PriceCents})
	}
}
