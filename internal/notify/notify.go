package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// Message 是渲染后的、与具体推送渠道无关的通知内容。
type Message struct {
	Title string
	Body  string
	URL   string
	Group string

	// Events 是逐条事件的展示数据,供 webhook 模板自行拼装格式。
	// 摘要消息里含全部事件,单条消息里只有一条。
	Events []EventView
}

// EventView 是单条事件在模板里可见的展示数据,文案部分已按语言本地化。
// Kind 保持语言中立(listed / price_drop / delisted),
// 模板可据此自己分派任意语言的文案,不受 lang 配置约束。
type EventView struct {
	Kind      string
	KindLabel string

	Region       string
	Category     string
	PartNumber   string
	ProductTitle string
	URL          string

	Currency      string
	Price         string
	PriceCents    int64
	OldPrice      string
	OldPriceCents int64

	Rules []string
}

// Text 是 Title/Body/URL 的合并纯文本,供只接受单一文本字段的渠道使用。
func (m Message) Text() string {
	var b strings.Builder
	b.WriteString(m.Title)
	if m.Body != "" {
		b.WriteString("\n")
		b.WriteString(m.Body)
	}
	if m.URL != "" {
		b.WriteString("\n")
		b.WriteString(m.URL)
	}
	return b.String()
}

type Notifier interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Multi 把消息分发给所有渠道。单个渠道失败不影响其它渠道——
// Telegram 挂了不应该让 Bark 也收不到。
type Multi struct {
	notifiers []Notifier
	log       *slog.Logger
}

func NewMulti(ns []Notifier, log *slog.Logger) *Multi {
	if log == nil {
		log = slog.Default()
	}
	return &Multi{notifiers: ns, log: log}
}

func (m *Multi) Names() []string {
	out := make([]string, 0, len(m.notifiers))
	for _, n := range m.notifiers {
		out = append(out, n.Name())
	}
	return out
}

// Send 把消息发给所有渠道,返回成功送达的渠道数与合并后的错误。
// 调用方据成功数判断消息是否至少送出去了一份——全军覆没时不应推进状态基线,
// 否则这批变动会被永久吞掉。
func (m *Multi) Send(ctx context.Context, msg Message) (int, error) {
	var errs []error
	sent := 0
	for _, n := range m.notifiers {
		if err := n.Send(ctx, msg); err != nil {
			m.log.Error("推送失败", "channel", n.Name(), "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
			continue
		}
		sent++
		m.log.Info("推送成功", "channel", n.Name(), "title", msg.Title)
	}
	return sent, errors.Join(errs...)
}

// Renderer 按配置的语言渲染通知。语言与分组在一轮内都不变,
// 因此收在一个对象里,而不是给每个渲染函数追加两个 string 参数——
// 相邻的同类型参数很容易传反。
type Renderer struct {
	p     phrases
	group string
}

func NewRenderer(lang Lang, group string) *Renderer {
	return &Renderer{p: phrasesFor(lang), group: group}
}

// view 把事件转成模板可见的展示数据。
func (r *Renderer) view(ev state.Event) EventView {
	p := ev.Product
	v := EventView{
		Kind:         string(ev.Kind),
		KindLabel:    r.p.label(ev.Kind),
		Region:       p.Region,
		Category:     p.Category,
		PartNumber:   p.PartNumber,
		ProductTitle: p.Title,
		URL:          p.URL,
		Currency:     p.Currency,
		Price:        p.DisplayPrice(),
		PriceCents:   p.PriceCents,
		Rules:        ev.Rules,
	}
	if ev.Kind == state.EventPriceDrop {
		v.OldPriceCents = ev.OldPriceCents
		v.OldPrice = apple.FormatPrice(ev.OldPriceCents, p.Currency)
	}
	return v
}

// Event 把单条事件渲染成一条通知。
func (r *Renderer) Event(ev state.Event) Message {
	p := ev.Product
	title := fmt.Sprintf(r.p.eventTitle, r.p.label(ev.Kind), p.Region, p.Category)

	var b strings.Builder
	b.WriteString(p.Title)
	b.WriteString("\n")

	switch ev.Kind {
	case state.EventPriceDrop:
		drop := ev.OldPriceCents - p.PriceCents
		pct := float64(drop) / float64(ev.OldPriceCents) * 100
		fmt.Fprintf(&b, r.p.priceChange,
			apple.FormatPrice(ev.OldPriceCents, p.Currency),
			p.DisplayPrice(),
			apple.FormatPrice(drop, p.Currency),
			pct)
	default:
		b.WriteString(p.DisplayPrice())
	}

	if len(ev.Rules) > 0 {
		format := r.p.rulesOne
		if len(ev.Rules) > 1 {
			format = r.p.rulesMany
		}
		fmt.Fprintf(&b, format, strings.Join(ev.Rules, r.p.ruleSep))
	}

	return Message{
		Title:  title,
		Body:   b.String(),
		URL:    p.URL,
		Group:  r.group,
		Events: []EventView{r.view(ev)},
	}
}

// Digest 把一批事件合成一条摘要。Apple 偶尔会批量上架,
// 逐条推送会在手机上刷屏,超过阈值时改用摘要。
func (r *Renderer) Digest(evs []state.Event) Message {
	counts := map[state.EventKind]int{}
	for _, ev := range evs {
		counts[ev.Kind]++
	}
	var parts []string
	for _, k := range []state.EventKind{state.EventListed, state.EventPriceDrop, state.EventDelisted} {
		if counts[k] > 0 {
			parts = append(parts, r.p.count(k, counts[k]))
		}
	}
	title := fmt.Sprintf(r.p.digestTitle, strings.Join(parts, " / "))

	// 正文只列前 maxLines 条,但 Events 始终携带全部事件,
	// 需要完整列表的 webhook 模板可以自己遍历。
	const maxLines = 12
	var b strings.Builder
	views := make([]EventView, 0, len(evs))
	for i, ev := range evs {
		views = append(views, r.view(ev))
		if i > maxLines {
			continue
		}
		if i == maxLines {
			fmt.Fprintf(&b, r.p.digestMore, len(evs)-maxLines)
			continue
		}
		p := ev.Product
		fmt.Fprintf(&b, r.p.digestLine, r.p.label(ev.Kind), p.Region, p.Title)
		if ev.Kind == state.EventPriceDrop {
			fmt.Fprintf(&b, " %s → %s", apple.FormatPrice(ev.OldPriceCents, p.Currency), p.DisplayPrice())
		} else {
			fmt.Fprintf(&b, " %s", p.DisplayPrice())
		}
		b.WriteString("\n")
	}

	// 摘要含多个商品,链接只取第一条,避免误导性跳转。
	url := ""
	if len(evs) > 0 {
		url = evs[0].Product.URL
	}
	return Message{
		Title:  title,
		Body:   strings.TrimRight(b.String(), "\n"),
		URL:    url,
		Group:  r.group,
		Events: views,
	}
}
