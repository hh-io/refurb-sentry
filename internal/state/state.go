package state

import (
	"strings"
	"time"

	"github.com/hh/refurb-sentry/internal/apple"
)

// stateVersion 用于将来变更磁盘格式时做迁移判断。
const stateVersion = 1

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

type State struct {
	Version   int              `json:"version"`
	UpdatedAt time.Time        `json:"updated_at"`
	Items     map[string]Entry `json:"items"`

	// EmptyStreak 按 "region/category" 记录连续抓到空列表的轮数。
	EmptyStreak map[string]int `json:"empty_streak,omitempty"`

	// Bootstrapped 为 false 表示这是首次运行:本轮只建立基线,不发任何通知,
	// 否则首轮会把全部数百件在售商品当成「新上架」一次性推出去。
	Bootstrapped bool `json:"bootstrapped"`
}

func New() *State {
	return &State{
		Version:     stateVersion,
		Items:       make(map[string]Entry),
		EmptyStreak: make(map[string]int),
	}
}

type EventKind string

const (
	EventListed    EventKind = "listed"
	EventPriceDrop EventKind = "price_drop"
	EventDelisted  EventKind = "delisted"
)

func (k EventKind) Label() string {
	switch k {
	case EventListed:
		return "上架"
	case EventPriceDrop:
		return "降价"
	case EventDelisted:
		return "下架"
	}
	return string(k)
}

type Event struct {
	Kind    EventKind
	Product apple.Product
	// OldPriceCents 仅在降价事件中有意义。
	OldPriceCents int64
	// Rules 是命中的规则名,用于在通知里说明推送原因。
	Rules []string
}

// Apply 用一次**成功**抓取的结果更新状态并计算事件。
// 调用方必须保证只在抓取成功时调用——抓取失败时调用会把整个分类误判为下架。
func (s *State) Apply(region, category string, products []apple.Product, now time.Time) []Event {
	scope := region + "/" + category
	prefix := scope + "/"

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

	// 首轮只落基线。事件已在上面被消费掉(状态已更新),这里丢弃即可。
	if !s.Bootstrapped {
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
