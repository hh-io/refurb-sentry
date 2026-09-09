package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hh/refurb-sentry/internal/apple"
	"github.com/hh/refurb-sentry/internal/state"
)

// Message 是渲染后的、与具体推送渠道无关的通知内容。
type Message struct {
	Title string
	Body  string
	URL   string
	Group string

	// Events 保留原始事件,供 webhook 模板取用更细的字段。
	Events []state.Event
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

func (m *Multi) Len() int { return len(m.notifiers) }

func (m *Multi) Names() []string {
	out := make([]string, 0, len(m.notifiers))
	for _, n := range m.notifiers {
		out = append(out, n.Name())
	}
	return out
}

func (m *Multi) Send(ctx context.Context, msg Message) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.Send(ctx, msg); err != nil {
			m.log.Error("推送失败", "channel", n.Name(), "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", n.Name(), err))
			continue
		}
		m.log.Info("推送成功", "channel", n.Name(), "title", msg.Title)
	}
	return errors.Join(errs...)
}

// RenderEvent 把单条事件渲染成一条通知。
func RenderEvent(ev state.Event, group string) Message {
	p := ev.Product
	title := fmt.Sprintf("%s · %s %s", ev.Kind.Label(), p.Region, p.Category)

	var b strings.Builder
	b.WriteString(p.Title)
	b.WriteString("\n")

	switch ev.Kind {
	case state.EventPriceDrop:
		drop := ev.OldPriceCents - p.PriceCents
		pct := float64(drop) / float64(ev.OldPriceCents) * 100
		fmt.Fprintf(&b, "%s → %s(降 %s,%.1f%%)",
			apple.FormatPrice(ev.OldPriceCents, p.Currency),
			p.DisplayPrice(),
			apple.FormatPrice(drop, p.Currency),
			pct)
	default:
		b.WriteString(p.DisplayPrice())
	}

	if len(ev.Rules) > 0 {
		fmt.Fprintf(&b, "\n命中规则:%s", strings.Join(ev.Rules, "、"))
	}

	return Message{Title: title, Body: b.String(), URL: p.URL, Group: group, Events: []state.Event{ev}}
}

// RenderDigest 把一批事件合成一条摘要。Apple 偶尔会批量上架,
// 逐条推送会在手机上刷屏,超过阈值时改用摘要。
func RenderDigest(evs []state.Event, group string) Message {
	counts := map[state.EventKind]int{}
	for _, ev := range evs {
		counts[ev.Kind]++
	}
	var parts []string
	for _, k := range []state.EventKind{state.EventListed, state.EventPriceDrop, state.EventDelisted} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k.Label(), counts[k]))
		}
	}
	title := fmt.Sprintf("翻新监控 · %s", strings.Join(parts, " / "))

	const maxLines = 12
	var b strings.Builder
	for i, ev := range evs {
		if i == maxLines {
			fmt.Fprintf(&b, "…… 另有 %d 条", len(evs)-maxLines)
			break
		}
		p := ev.Product
		fmt.Fprintf(&b, "[%s] %s %s", ev.Kind.Label(), p.Region, p.Title)
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
	return Message{Title: title, Body: strings.TrimRight(b.String(), "\n"), URL: url, Group: group, Events: evs}
}
