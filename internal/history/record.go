// Package history 维护翻新商品的历史档案:每一次上架、调价、下架各追加一行 JSON。
//
// 它服务的问题是「某个配置以前卖过几次、每次卖多少钱」,而不是推送。
// 状态库只保留当前在架的商品,价格被原地覆盖、下架即删除,这些信息在别处都留不下来。
package history

import (
	"sort"
	"time"

	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// Kind 是档案行的类型。
type Kind string

const (
	// KindBaseline 表示建档时该商品已经在架,真实上架时刻只知道不晚于 FirstSeen。
	KindBaseline Kind = "baseline"
	KindListed   Kind = "listed"
	// KindPrice 涨价与降价都记。状态库对涨价静默更新,只看事件的话走势会缺点。
	KindPrice    Kind = "price"
	KindDelisted Kind = "delisted"
)

// Record 是档案中的一行。每行自带完整的商品信息而不是只记差量:
// 用 jq / duckdb 直接查文件时不必先做关联,折叠逻辑也因此简单。
type Record struct {
	Time          time.Time         `json:"ts"`
	Kind          Kind              `json:"kind"`
	Region        string            `json:"region"`
	Category      string            `json:"category"`
	PartNumber    string            `json:"part_number"`
	Title         string            `json:"title"`
	URL           string            `json:"url"`
	PriceCents    int64             `json:"price_cents"`
	OldPriceCents int64             `json:"old_price_cents,omitempty"`
	Currency      string            `json:"currency"`
	Dimensions    map[string]string `json:"dims,omitempty"`
	// Spec 只是写给外部工具看的便利字段。本程序读档时一律从 Title 重新解析:
	// 标题解析是静默失败的,解析器日后修好了,旧档案应当跟着得到修正。
	Spec      filter.Spec `json:"spec"`
	FirstSeen time.Time   `json:"first_seen"`
}

// Key 与状态库的键一致。
func (r Record) Key() string {
	return r.Region + "/" + r.Category + "/" + r.PartNumber
}

func newRecord(kind Kind, e state.Entry, now time.Time) Record {
	return Record{
		Time: now, Kind: kind,
		Region: e.Region, Category: e.Category, PartNumber: e.PartNumber,
		Title: e.Title, URL: e.URL, PriceCents: e.PriceCents, Currency: e.Currency,
		Dimensions: e.Dimensions,
		Spec:       filter.ParseSpec(e.Title),
		FirstSeen:  e.FirstSeen.Truncate(time.Second),
	}
}

// Diff 对比一轮 Apply 前后的状态,得出应当归档的记录。
//
// 刻意从前后两份状态推导,而不是复用 state.Event:事件在冷启动时被丢弃、涨价时不产生,
// 而这两类恰恰是档案需要的。连续空结果未攒够阈值时 Apply 不删条目,
// 这里自然也不会记出下架——那条防误报的约束对档案原样成立。
//
// 调用方必须只在本轮基线确定提交时调用:回滚的那轮若也写了,
// 下一轮重新产生的同一批变动会被记第二遍。
func Diff(before, after *state.State, now time.Time) []Record {
	now = now.Truncate(time.Second)
	var out []Record
	for k, a := range after.Items {
		b, existed := before.Items[k]
		switch {
		case !existed:
			kind := KindListed
			// 该范围本轮才建立基线:这些商品早已在架,只是第一次被看到。
			if !before.IsBootstrapped(a.Region, a.Category) {
				kind = KindBaseline
			}
			out = append(out, newRecord(kind, a, now))
		case a.PriceCents != b.PriceCents:
			r := newRecord(KindPrice, a, now)
			r.OldPriceCents = b.PriceCents
			out = append(out, r)
		}
	}
	for k, b := range before.Items {
		if _, ok := after.Items[k]; !ok {
			out = append(out, newRecord(KindDelisted, b, now))
		}
	}
	sortRecords(out)
	return out
}

// Seed 把状态库里现有的全部商品记为 baseline,用于档案文件还不存在时。
//
// 没有这一步,已经跑了一段时间的部署开启档案后,当时在架的商品不会有任何上架记录,
// 只会在某天冒出一条孤零零的下架。FirstSeen 取自状态库,是本程序真实的首次发现时刻。
func Seed(s *state.State, now time.Time) []Record {
	now = now.Truncate(time.Second)
	out := make([]Record, 0, len(s.Items))
	for _, e := range s.Items {
		out = append(out, newRecord(KindBaseline, e, now))
	}
	sortRecords(out)
	return out
}

// sortRecords 让同一批记录的落盘顺序确定:状态库是 map,不排序的话同样的变动每次写出的顺序都不同。
func sortRecords(rs []Record) {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Key() < rs[j].Key() })
}
