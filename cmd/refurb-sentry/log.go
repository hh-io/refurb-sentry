package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// logTimeLayout 精确到秒。轮询间隔是 120 秒,毫秒精度在这里只是噪音;
// 时区也不逐行重复,改为在启动那一行打一次(见 tzLabel)。
const logTimeLayout = "2006-01-02 15:04:05"

// consoleHandler 把日志打成人读的一行:
//
//	2026-09-11 20:13:56 INFO  本轮无变化 scope=CN/mac
//
// 不用 slog 自带的 TextHandler:它固定输出 time=/level=/msg= 这些键名,
// 实测「本轮无变化」那一行 65 个字符里有 49 个是这类样板。
// 而这些日志是给人看的——本项目不输出 JSON,没有任何机器在解析它。
//
// TestConsoleHandlerLineFormat 用整行精确匹配钉住格式:只查关键字会放过
// 多一个空格、级别没对齐这类回归。TestConsoleHandlerGroupOnlyQualifiesLaterAttrs
// 逐字对照 TextHandler 的分组语义——只测不交错的场景等于给了一个虚假的契约合规保证。
type consoleHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Level
	// preformatted 是 WithAttrs 累积下来、已经带好各自分组前缀的属性文本。
	// 必须在 WithAttrs 的当下就格式化:slog 的契约是只有 WithGroup 之后添加的
	// 属性才归入该组,留着原始 attr 到 Handle 再统一套前缀,会把此前添加的
	// 也一并套进去(实测 TextHandler 给的是 scope=CN/mac http.status=503,
	// 而那种写法会打成 http.scope=CN/mac)。
	preformatted string
	groups       []string
}

func newConsoleHandler(w io.Writer, level slog.Level) *consoleHandler {
	return &consoleHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *consoleHandler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	n := *h
	var b strings.Builder
	b.WriteString(h.preformatted)
	for _, a := range as {
		appendAttr(&b, h.groups, a)
	}
	n.preformatted = b.String()
	return &n
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.groups = append(slices.Clip(h.groups), name)
	return &n
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(logTimeLayout))
	b.WriteByte(' ')
	// 级别左对齐到 5 位(DEBUG/ERROR 最长),让消息在各级别之间竖直对齐。
	fmt.Fprintf(&b, "%-5s ", r.Level.String())
	b.WriteString(r.Message)

	b.WriteString(h.preformatted)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.groups, a)
		return true
	})
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func appendAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		sub := a.Value.Group()
		if len(sub) == 0 {
			return
		}
		// slog 规定空 key 的 Group 要内联,不加前缀——否则会打出 "g..k=1"。
		if a.Key != "" {
			// 这里的 Clip 是防御性的,与 WithGroup 里那处不同(那处不加就是真 bug:
			// 两个派生 handler 会长期持有并写进同一段底层数组)。递归是深度优先,
			// 每个兄弟分支用完 groups 才轮到下一个,当前写法不会串键;
			// 留着是因为一旦有人把遍历改成并发或延迟消费,失败方式是前缀静默出错。
			groups = append(slices.Clip(groups), a.Key)
		}
		for _, s := range sub {
			appendAttr(b, groups, s)
		}
		return
	}

	b.WriteByte(' ')
	for _, g := range groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(quoteIfNeeded(a.Value.String()))
}

// quoteIfNeeded 只在值本身会破坏 key=value 断句时才加引号。
// 必须是 strconv.Quote 而不是 QuoteToASCII:后者会把中文转义成 \uXXXX,
// 而本项目的日志一律中文,转了就全是天书。
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsFunc(s, func(r rune) bool {
		return r == ' ' || r == '"' || r == '=' || r < 0x20 || r == 0x7f
	}) {
		return strconv.Quote(s)
	}
	return s
}
