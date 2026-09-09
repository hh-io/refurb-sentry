package apple

import (
	"fmt"
	"strconv"
	"strings"
)

// tile 对应页面内嵌 JSON 中 tiles[] 的单个元素。
// 只声明用得到的字段:Apple 随时会给页面加字段,encoding/json 默认忽略未知字段,
// 因此这里字段越少,解析越不容易被上游改版打断。
type tile struct {
	PartNumber        string `json:"partNumber"`
	Title             string `json:"title"`
	ProductDetailsURL string `json:"productDetailsUrl"`
	Price             struct {
		PriceCurrency string `json:"priceCurrency"`
		CurrentPrice  struct {
			// amount 字段在部分分类(如 watch)里混有 HTML 标签,
			// raw_amount 才是可解析的纯数字,价格一律以它为准。
			RawAmount string `json:"raw_amount"`
		} `json:"currentPrice"`
	} `json:"price"`
	Filters struct {
		// 维度 key 集合随分类而变(mac 有内存/容量,watch 有表壳尺寸/材质),
		// 不能定义成固定结构体。
		Dimensions map[string]string `json:"dimensions"`
	} `json:"filters"`
}

// DimensionLegend 是页面提供的维度中英文名,用于 -list-dims 输出可过滤的键。
type DimensionLegend struct {
	Key    string `json:"key"`
	Legend string `json:"legend"`
}

type bootstrap struct {
	Dimensions []DimensionLegend `json:"dimensions"`
	Tiles      []tile            `json:"tiles"`
}

// Product 是归一化后的领域对象,后续过滤、diff、通知都只认它。
type Product struct {
	Region     string            `json:"region"`
	Category   string            `json:"category"`
	PartNumber string            `json:"part_number"`
	Title      string            `json:"title"`
	URL        string            `json:"url"`
	PriceCents int64             `json:"price_cents"`
	Currency   string            `json:"currency"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
}

// Key 是商品在状态库中的唯一标识。partNumber 自带地区后缀(LL/CH/ZP),
// 理论上已全局唯一,但同一货号可能同时出现在多个分类页,故仍带上 region/category。
func (p Product) Key() string {
	return p.Region + "/" + p.Category + "/" + p.PartNumber
}

// DisplayPrice 自行格式化价格,而不是透传页面的 amount 字段——后者含 HTML。
func (p Product) DisplayPrice() string {
	return FormatPrice(p.PriceCents, p.Currency)
}

// FormatPrice 将分转为带千位分隔与货币符号的展示串。
func FormatPrice(cents int64, currency string) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	whole := cents / 100
	frac := cents % 100

	var b strings.Builder
	s := strconv.FormatInt(whole, 10)
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	amount := b.String()
	if frac != 0 {
		amount = fmt.Sprintf("%s.%02d", amount, frac)
	}

	// 符号沿用各地区 Apple 站点自身的展示习惯。
	sym := map[string]string{
		"CNY": "RMB ", "USD": "$", "HKD": "HK$", "JPY": "¥",
		"GBP": "£", "EUR": "€", "SGD": "S$", "CAD": "CA$", "AUD": "A$",
		"NZD": "NZ$", "TWD": "NT$", "KRW": "₩", "CHF": "CHF ",
	}[currency]
	if sym == "" {
		sym = currency + " "
	}
	if neg {
		return "-" + sym + amount
	}
	return sym + amount
}

// parsePriceCents 把 "4699.00" 解析为分。用整数避免浮点比较在降价判定上出现假阳性。
func parsePriceCents(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("价格为空")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("解析整数位 %q: %w", whole, err)
	}
	if !hasFrac {
		return w * 100, nil
	}
	// 只取两位小数,不足补零
	frac = (frac + "00")[:2]
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("解析小数位 %q: %w", frac, err)
	}
	return w*100 + f, nil
}
