package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// 整行格式是这个改动的全部意义所在,用精确匹配钉住它。
// 松一点的断言(只查关键字)会放过多打一个空格、级别没对齐这类回归,
// 而那正是换掉 TextHandler 想解决的问题。
func TestConsoleHandlerLineFormat(t *testing.T) {
	cases := []struct {
		name  string
		level slog.Level
		msg   string
		args  []any
		want  string
	}{
		{
			name: "无属性", level: slog.LevelInfo, msg: "本轮无变化",
			want: "2026-09-11 20:13:56 INFO  本轮无变化\n",
		},
		{
			name: "带属性", level: slog.LevelInfo, msg: "已为新范围建立基线",
			args: []any{"scopes", 1, "products", 203},
			want: "2026-09-11 20:13:56 INFO  已为新范围建立基线 scopes=1 products=203\n",
		},
		{
			// 级别左对齐到 5 位,消息才能在各级别之间竖直对齐。
			name: "WARN 对齐", level: slog.LevelWarn, msg: "页面中没有商品数据",
			args: []any{"scope", "US/mac"},
			want: "2026-09-11 20:13:56 WARN  页面中没有商品数据 scope=US/mac\n",
		},
		{
			name: "ERROR 对齐", level: slog.LevelError, msg: "本轮存在未送达的通知",
			args: []any{"rounds", 1, "max", 5},
			want: "2026-09-11 20:13:56 ERROR 本轮存在未送达的通知 rounds=1 max=5\n",
		},
		{
			// 值里有空格才加引号,否则 key=value 会被断错。
			name: "含空格的值加引号", level: slog.LevelInfo, msg: "推送成功",
			args: []any{"title", "翻新监控日报 · 2026-09-11"},
			want: "2026-09-11 20:13:56 INFO  推送成功 title=\"翻新监控日报 · 2026-09-11\"\n",
		},
		{
			name: "空值也要看得见", level: slog.LevelInfo, msg: "推送成功",
			args: []any{"sound", ""},
			want: "2026-09-11 20:13:56 INFO  推送成功 sound=\"\"\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			h := newConsoleHandler(&buf, slog.LevelDebug)
			r := slog.NewRecord(at(t, "2026-09-11 20:13:56"), c.level, c.msg, 0)
			r.Add(c.args...)
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != c.want {
				t.Errorf("\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// 中文日志是这个项目的既定约定,绝不能被转义成 \uXXXX 推到运维眼前。
// strconv.Quote 保留可打印的 Unicode,QuoteToASCII 不会——用错一个就全是天书。
func TestConsoleHandlerKeepsChineseReadable(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelDebug)
	r := slog.NewRecord(at(t, "2026-09-11 20:13:56"), slog.LevelWarn, "补齐失败", 0)
	r.Add("err", "详情页 解析失败") // 带空格,会走引号分支
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, `\u`) {
		t.Errorf("中文被转义了:\n%s", got)
	}
	if !strings.Contains(got, "详情页 解析失败") {
		t.Errorf("原文没有出现:\n%s", got)
	}
}

// 时间戳不该带毫秒或时区偏移:轮询间隔 120 秒,毫秒是噪音;
// 时区整个进程生命周期不变,逐行重复没有信息量,改在启动那一行打一次。
func TestConsoleHandlerTimestampIsTrimmed(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelDebug)
	ts := time.Date(2026, 9, 11, 20, 13, 56, 432_000_000, time.FixedZone("CST", 8*3600))
	r := slog.NewRecord(ts, slog.LevelInfo, "本轮无变化", 0)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, ".432") {
		t.Errorf("时间戳带上了毫秒:\n%s", got)
	}
	if strings.Contains(got, "+08:00") || strings.Contains(got, "CST") {
		t.Errorf("时间戳带上了时区:\n%s", got)
	}
	if !strings.HasPrefix(got, "2026-09-11 20:13:56 ") {
		t.Errorf("时间格式不对:\n%s", got)
	}
}

func TestConsoleHandlerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newConsoleHandler(&buf, slog.LevelWarn))
	log.Debug("调试")
	log.Info("信息")
	log.Warn("警告")
	if strings.Contains(buf.String(), "调试") || strings.Contains(buf.String(), "信息") {
		t.Errorf("低于阈值的日志没有被挡住:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "警告") {
		t.Errorf("WARN 应当输出:\n%s", buf.String())
	}
}

// 项目当前没有用 With/WithGroup,但 slog.Handler 的契约要求它们正确:
// 派生出的两个 logger 不能互相污染属性(append 共享底层数组是经典写法坑)。
func TestConsoleHandlerWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(newConsoleHandler(&buf, slog.LevelDebug)).With("scope", "CN/mac")

	a := base.With("a", 1)
	b := base.With("b", 2)
	a.Info("甲")
	b.Info("乙")

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应有两行,实际 %d:\n%s", len(lines), buf.String())
	}
	if !strings.HasSuffix(lines[0], "甲 scope=CN/mac a=1") {
		t.Errorf("第一行属性不对: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "乙 scope=CN/mac b=2") {
		t.Errorf("第二个 logger 被第一个污染了: %q", lines[1])
	}

	buf.Reset()
	slog.New(newConsoleHandler(&buf, slog.LevelDebug)).WithGroup("http").Info("请求", "status", 503)
	if !strings.Contains(buf.String(), "http.status=503") {
		t.Errorf("分组前缀没生效: %q", buf.String())
	}
}

// Handler 要能被多个 goroutine 同时写而不撕裂成半行。
func TestConsoleHandlerConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(newConsoleHandler(&buf, slog.LevelDebug))

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Info("并发", "i", i)
		}()
	}
	wg.Wait()

	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if !strings.Contains(line, "INFO  并发 i=") {
			t.Fatalf("出现了撕裂的行: %q", line)
		}
	}
}
