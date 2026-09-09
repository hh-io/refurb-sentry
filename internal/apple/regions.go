package apple

import (
	"fmt"
	"sort"
)

type Region struct {
	Code           string
	BaseURL        string
	AcceptLanguage string
	Currency       string // 期望货币,用于校验抓到的页面确实是目标地区
}

// regions 中每一项的 BaseURL 与货币均经过实测。新增地区照抄一行即可,
// 启动时的连通性校验会验证该地区/分类组合是否真实存在。
var regions = map[string]Region{
	"US": {Code: "US", BaseURL: "https://www.apple.com", AcceptLanguage: "en-US,en;q=0.9", Currency: "USD"},
	"CN": {Code: "CN", BaseURL: "https://www.apple.com.cn", AcceptLanguage: "zh-CN,zh;q=0.9", Currency: "CNY"},
	"HK": {Code: "HK", BaseURL: "https://www.apple.com/hk-zh", AcceptLanguage: "zh-HK,zh;q=0.9", Currency: "HKD"},
	"JP": {Code: "JP", BaseURL: "https://www.apple.com/jp", AcceptLanguage: "ja-JP,ja;q=0.9", Currency: "JPY"},
	"UK": {Code: "UK", BaseURL: "https://www.apple.com/uk", AcceptLanguage: "en-GB,en;q=0.9", Currency: "GBP"},
	"DE": {Code: "DE", BaseURL: "https://www.apple.com/de", AcceptLanguage: "de-DE,de;q=0.9", Currency: "EUR"},
	"SG": {Code: "SG", BaseURL: "https://www.apple.com/sg", AcceptLanguage: "en-SG,en;q=0.9", Currency: "SGD"},
	"CA": {Code: "CA", BaseURL: "https://www.apple.com/ca", AcceptLanguage: "en-CA,en;q=0.9", Currency: "CAD"},
	"AU": {Code: "AU", BaseURL: "https://www.apple.com/au", AcceptLanguage: "en-AU,en;q=0.9", Currency: "AUD"},
}

// Categories 是翻新店的全部分类。并非每个地区都提供全部分类
// (实测 CN/HK 无 iphone 与 appletv,请求会返回 404),由启动校验负责发现。
var Categories = []string{"mac", "ipad", "iphone", "watch", "airpods", "appletv", "homepod", "accessories"}

func LookupRegion(code string) (Region, error) {
	r, ok := regions[code]
	if !ok {
		return Region{}, fmt.Errorf("未知地区 %q,支持的地区:%v", code, RegionCodes())
	}
	return r, nil
}

func RegionCodes() []string {
	codes := make([]string, 0, len(regions))
	for c := range regions {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	return codes
}

func ValidCategory(c string) bool {
	for _, k := range Categories {
		if k == c {
			return true
		}
	}
	return false
}

// GridURL 返回某地区某分类的翻新列表页。该页一次返回全量商品,无分页。
func (r Region) GridURL(category string) string {
	return r.BaseURL + "/shop/refurbished/" + category
}

// ProductURL 把页面给的相对路径补成绝对地址。相对路径以 /shop/... 开头,
// 而 HK/JP 这类地区的 BaseURL 自带路径前缀,需拼在站点根之后。
func (r Region) ProductURL(rel string) string {
	if rel == "" {
		return r.GridURL("mac")
	}
	if len(rel) > 4 && (rel[:5] == "http:" || rel[:5] == "https") {
		return rel
	}
	root := r.BaseURL
	// 形如 https://www.apple.com/hk-zh -> 商品链接需挂在 https://www.apple.com 下
	if i := indexPathStart(root); i > 0 {
		root = root[:i]
	}
	return root + rel
}

// indexPathStart 返回 URL 中 host 之后路径部分的起始下标,无路径时返回 -1。
func indexPathStart(u string) int {
	const scheme = "https://"
	if len(u) <= len(scheme) {
		return -1
	}
	for i := len(scheme); i < len(u); i++ {
		if u[i] == '/' {
			return i
		}
	}
	return -1
}
