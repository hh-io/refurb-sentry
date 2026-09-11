package state

import (
	"strings"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
)

// stateVersion 用于将来变更磁盘格式时做迁移判断。
// v2 起 bootstrapped 由全局布尔改为按 region/category 记录。
const stateVersion = 2

// emptyStreakThreshold 是「抓到空列表」需连续出现多少轮才认定该分类真的清空了。
// 单次空结果更可能是上游抖动或改版,据此判下架会一次性误报整个分类。
const emptyStreakThreshold = 3

// Entry 是单件商品在状态库中的留存记录。保留 Title/Dimensions 是为了让
// 「下架」事件也能走一遍完整的规则过滤——否则会推送一堆用户并不关心的机型。
type Entry struct {
	Region     string            `json:"region"`
	Category   string            `json:"category"`
	PartNumber string            `json:"part_number"`
	Title      string            `json:"title"`
	URL        string            `json:"url"`
	PriceCents int64             `json:"price_cents"`
	Currency   string            `json:"currency"`
	Dimensions map[string]string `json:"dimensions,omitempty"`
	FirstSeen  time.Time         `json:"first_seen"`
	LastSeen   time.Time         `json:"last_seen"`
}

func (e Entry) Product() apple.Product {
	return apple.Product{
		Region:     e.Region,
		Category:   e.Category,
		PartNumber: e.PartNumber,
		Title:      e.Title,
		URL:        e.URL,
		PriceCents: e.PriceCents,
		Currency:   e.Currency,
		Dimensions: e.Dimensions,
	}
}

// Counter 是单个 region/category 自上次日报以来的事件计数。
// Pushed 是其中真正推送出去的条数(已过规则过滤):它与前三项的差额
// 正是「规则挡掉了多少」,规则写得太紧时一眼可见。
type Counter struct {
	Listed    int `json:"listed,omitempty"`
	PriceDrop int `json:"price_drop,omitempty"`
	Delisted  int `json:"delisted,omitempty"`
	Pushed    int `json:"pushed,omitempty"`
}

type State struct {
	Version   int              `json:"version"`
	UpdatedAt time.Time        `json:"updated_at"`
	Items     map[string]Entry `json:"items"`

	// EmptyStreak 按 "region/category" 记录连续抓到空列表的轮数。
	EmptyStreak map[string]int `json:"empty_streak,omitempty"`

	// Counters 按 "region/category" 累计自 CountersSince 以来的事件数,供日报汇总。
	//
	// 放进 State 而不是 Runner 的内存里有两个理由:重启不清零;
	// 以及推送全败回滚基线时它必须跟着一起退回——那批事件下一轮会重新产生,
	// 不回滚就会被计两次,日报数字凭空翻倍。Clone 因此必须一并深拷贝。
	Counters map[string]Counter `json:"counters,omitempty"`

	// CountersSince 是当前这批计数的起点,只在日报成功发出后推进。
	// 与 LastSummaryAt 分开:连续送达失败放弃某次汇总时 LastSummaryAt 会推进
	// (否则当天会一直重试),而计数要留到下一份日报里,那时「自 X 以来」仍须准确。
	CountersSince time.Time `json:"counters_since,omitempty"`

	// LastSummaryAt 是上次发出日报的时刻,用于判断今天是否已经发过。
	LastSummaryAt time.Time `json:"last_summary_at,omitempty"`

	// Bootstrapped 按 "region/category" 记录该范围是否已建立基线。
	//
	// 刻意做成按 scope 而不是一个全局标志:全局标志有两个漏洞——
	// 其一,首轮若某个地区抓取失败,其余地区成功即把全局标志置真,
	// 该地区下一轮成功时数百件在架商品会被当成新上架推出去;
	// 其二,给已在运行的部署新增一个地区或分类时,同样会刷屏。
	Bootstrapped map[string]bool `json:"bootstrapped"`
}

func New() *State {
	return &State{
		Version:      stateVersion,
		Items:        make(map[string]Entry),
		EmptyStreak:  make(map[string]int),
		Bootstrapped: make(map[string]bool),
		Counters:     make(map[string]Counter),
	}
}

type EventKind string

const (
	EventListed    EventKind = "listed"
	EventPriceDrop EventKind = "price_drop"
	EventDelisted  EventKind = "delisted"
)

type Event struct {
	Kind    EventKind
	Product apple.Product
	// OldPriceCents 仅在降价事件中有意义。
	OldPriceCents int64
	// Rules 是命中的规则名,用于在通知里说明推送原因。
	Rules []string
}

// IsBootstrapped 表示该地区/分类是否已建立过基线。
func (s *State) IsBootstrapped(region, category string) bool {
	return s.Bootstrapped[region+"/"+category]
}

// Apply 用一次**成功**抓取的结果更新状态并计算事件。
// 调用方必须保证只在抓取成功时调用——抓取失败时调用会把整个分类误判为下架。
//
// 该 scope 尚未建立基线时只落盘、不产生事件。
func (s *State) Apply(region, category string, products []apple.Product, now time.Time) []Event {
	scope := region + "/" + category
	prefix := scope + "/"
	first := !s.Bootstrapped[scope]

	// 空结果先攒够连续次数再当真,避免一次抖动清空整个分类。
	if len(products) == 0 {
		s.EmptyStreak[scope]++
		if s.EmptyStreak[scope] < emptyStreakThreshold {
			return nil
		}
	} else {
		delete(s.EmptyStreak, scope)
	}

	var events []Event
	seen := make(map[string]bool, len(products))

	for _, p := range products {
		k := p.Key()
		seen[k] = true

		prev, exists := s.Items[k]
		if !exists {
			events = append(events, Event{Kind: EventListed, Product: p})
			s.Items[k] = Entry{
				Region: p.Region, Category: p.Category, PartNumber: p.PartNumber,
				Title: p.Title, URL: p.URL, PriceCents: p.PriceCents, Currency: p.Currency,
				Dimensions: p.Dimensions, FirstSeen: now, LastSeen: now,
			}
			continue
		}

		// 只有降价才是事件;涨价静默更新基准,以免下次小幅回落又报一次降价。
		if p.PriceCents < prev.PriceCents {
			events = append(events, Event{
				Kind: EventPriceDrop, Product: p, OldPriceCents: prev.PriceCents,
			})
		}
		prev.Title = p.Title
		prev.URL = p.URL
		prev.PriceCents = p.PriceCents
		prev.Currency = p.Currency
		prev.Dimensions = p.Dimensions
		prev.LastSeen = now
		s.Items[k] = prev
	}

	for k, e := range s.Items {
		if !strings.HasPrefix(k, prefix) || seen[k] {
			continue
		}
		events = append(events, Event{Kind: EventDelisted, Product: e.Product()})
		delete(s.Items, k)
	}

	s.UpdatedAt = now

	// 该范围首次成功抓取:只落基线。事件已在上面被消费掉(状态已更新),这里丢弃即可。
	if first {
		s.Bootstrapped[scope] = true
		return nil
	}
	return events
}

// CountScope 返回某地区某分类当前记录的商品数。
func (s *State) CountScope(region, category string) int {
	prefix := region + "/" + category + "/"
	n := 0
	for k := range s.Items {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// ScopeEntries 返回某地区某分类当前记录的全部商品。
// 日报据此对在架商品重跑一遍规则,给出「现在有几件命中」——
// 这个数字在别处看不到,而规则写错(维度键名抄错、型号猜错)的表现
// 正是它长期为 0,否则用户只能靠「一直没收到推送」去猜。
func (s *State) ScopeEntries(region, category string) []Entry {
	prefix := region + "/" + category + "/"
	var out []Entry
	for k, e := range s.Items {
		if strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// CountEvents 把一批事件累计进各自 scope 的计数器。
//
// 必须在 Apply 之后、推送之前调用:计数器与基线同属一份状态,
// 推送全败回滚时它跟着一起退回,下一轮重新产生的同一批事件才不会被计两次。
func (s *State) CountEvents(events []Event) {
	for _, ev := range events {
		c := s.counter(ev.Product.Region, ev.Product.Category)
		switch ev.Kind {
		case EventListed:
			c.Listed++
		case EventPriceDrop:
			c.PriceDrop++
		case EventDelisted:
			c.Delisted++
		}
		s.setCounter(ev.Product.Region, ev.Product.Category, c)
	}
}

// CountPushed 记录已推送出去的事件(即通过了规则过滤的那些)。
func (s *State) CountPushed(events []Event) {
	for _, ev := range events {
		c := s.counter(ev.Product.Region, ev.Product.Category)
		c.Pushed++
		s.setCounter(ev.Product.Region, ev.Product.Category, c)
	}
}

// ScopeCounter 返回某地区某分类当前的累计值。
func (s *State) ScopeCounter(region, category string) Counter {
	return s.counter(region, category)
}

// ResetCounters 清空全部计数并把区间起点推到 now,在日报成功发出后调用。
func (s *State) ResetCounters(now time.Time) {
	s.Counters = make(map[string]Counter)
	s.CountersSince = now
}

func (s *State) counter(region, category string) Counter {
	return s.Counters[region+"/"+category]
}

func (s *State) setCounter(region, category string, c Counter) {
	// 从旧版状态文件加载时 counters 字段不存在,Load 不会替我们建好这个 map。
	if s.Counters == nil {
		s.Counters = make(map[string]Counter)
	}
	s.Counters[region+"/"+category] = c
}

// Clone 返回一份深拷贝,供调用方在推送失败时回滚整轮变更。
//
// Apply 是原地修改的:事件一旦算出,内存里的旧价格/旧条目就已经被覆盖或删除,
// 仅仅跳过落盘并不能让下一轮重新产生这批事件——常驻进程下这些变动会被永久吞掉。
// 因此推送前先留一份快照,全军覆没时整体还原。
func (s *State) Clone() *State {
	c := &State{
		Version:       s.Version,
		UpdatedAt:     s.UpdatedAt,
		CountersSince: s.CountersSince,
		LastSummaryAt: s.LastSummaryAt,
		Items:         make(map[string]Entry, len(s.Items)),
		EmptyStreak:   make(map[string]int, len(s.EmptyStreak)),
		Bootstrapped:  make(map[string]bool, len(s.Bootstrapped)),
		Counters:      make(map[string]Counter, len(s.Counters)),
	}
	for k, e := range s.Items {
		// Dimensions 同样复制:当前 Apply 只整体替换该 map 而不原地改写,
		// 但共享底层 map 会让「快照」这个名字随时可能变成谎言。
		if e.Dimensions != nil {
			d := make(map[string]string, len(e.Dimensions))
			for dk, dv := range e.Dimensions {
				d[dk] = dv
			}
			e.Dimensions = d
		}
		c.Items[k] = e
	}
	for k, v := range s.EmptyStreak {
		c.EmptyStreak[k] = v
	}
	for k, v := range s.Bootstrapped {
		c.Bootstrapped[k] = v
	}
	for k, v := range s.Counters {
		c.Counters[k] = v
	}
	return c
}

// Restore 把状态整体还原成 snapshot 的内容。
// 刻意做成原地覆盖而非返回新指针:State 指针在 Runner 之外还被持有,
// 换指针会让退出时落盘的仍是那份已被推进的脏状态。
func (s *State) Restore(snapshot *State) {
	*s = *snapshot
}
