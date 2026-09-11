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
type consoleHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	level  slog.Level
	attrs  []slog.Attr
	groups []string
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
	// Clip 之后再 append:不裁掉多余容量的话,两个派生 logger 会写进同一段底层数组。
	n.attrs = append(slices.Clip(h.attrs), as...)
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

	for _, a := range h.attrs {
		appendAttr(&b, h.groups, a)
	}
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
		for _, s := range sub {
			appendAttr(b, append(groups, a.Key), s)
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
// strconv.Quote 保留可打印的 Unicode,中文不会被转义成 \uXXXX。
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
