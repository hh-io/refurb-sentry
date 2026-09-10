package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/state"
)

func sampleEvent(kind state.EventKind) state.Event {
	return state.Event{
		Kind: kind,
		Product: apple.Product{
			Region: "CN", Category: "mac", PartNumber: "FHFA4CH/A",
			Title:      "翻新 14 英寸 MacBook Pro",
			URL:        "https://www.apple.com.cn/shop/product/fhfa4ch/a",
			PriceCents: 1419900, Currency: "CNY",
		},
		OldPriceCents: 1619900,
		Rules:         []string{"MBP 高配"},
	}
}

// zhRenderer/enRenderer 是测试里的默认渲染器,分组固定为 "g"。
func zhRenderer() *Renderer { return NewRenderer(LangZH, "g") }
func enRenderer() *Renderer { return NewRenderer(LangEN, "g") }

func TestRenderPriceDrop(t *testing.T) {
	m := zhRenderer().Event(sampleEvent(state.EventPriceDrop))
	if m.Title != "降价 · CN mac" {
		t.Errorf("标题有误: %q", m.Title)
	}
	for _, want := range []string{"RMB 16,199", "RMB 14,199", "降 RMB 2,000", "12.3%", "MBP 高配"} {
		if !strings.Contains(m.Body, want) {
			t.Errorf("正文缺少 %q:\n%s", want, m.Body)
		}
	}
}

func TestRenderDigestCounts(t *testing.T) {
	evs := []state.Event{
		sampleEvent(state.EventListed), sampleEvent(state.EventListed),
		sampleEvent(state.EventPriceDrop), sampleEvent(state.EventDelisted),
	}
	m := zhRenderer().Digest(evs)
	if !strings.Contains(m.Title, "上架 2") || !strings.Contains(m.Title, "降价 1") || !strings.Contains(m.Title, "下架 1") {
		t.Errorf("摘要标题未正确统计: %q", m.Title)
	}
}

// 标题里的引号和换行必须被转义,否则会破坏 JSON 载荷。
func TestWebhookEscapesJSON(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := NewWebhook(WebhookOptions{
		Name: "tg", URL: srv.URL,
		Body: `{"chat_id":"1","text":{{json .Text}}}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	ev := sampleEvent(state.EventListed)
	ev.Product.Title = "他说\"这很\"便宜\n换行"
	if err := wh.Send(context.Background(), zhRenderer().Event(ev)); err != nil {
		t.Fatalf("推送失败: %v", err)
	}

	var payload struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v\n%s", err, got)
	}
	if !strings.Contains(payload.Text, `他说"这很"便宜`) {
		t.Errorf("文本内容丢失: %q", payload.Text)
	}
}

func TestWebhookReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "chat not found", http.StatusBadRequest)
	}))
	defer srv.Close()

	wh, _ := NewWebhook(WebhookOptions{URL: srv.URL})
	err := wh.Send(context.Background(), zhRenderer().Event(sampleEvent(state.EventListed)))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("应报出 HTTP 400,实际: %v", err)
	}
}

func TestBarkPayload(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/push" {
			t.Errorf("Bark V2 应请求 /push,实际 %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer srv.Close()

	b, err := NewBark(BarkOptions{Server: srv.URL, DeviceKey: "kkk", Sound: "minuet"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Send(context.Background(), NewRenderer(LangZH, "grp").Event(sampleEvent(state.EventListed))); err != nil {
		t.Fatalf("推送失败: %v", err)
	}
	if got["device_key"] != "kkk" || got["group"] != "grp" || got["sound"] != "minuet" {
		t.Errorf("载荷字段有误: %+v", got)
	}
	if got["isArchive"] != "1" {
		t.Errorf("isArchive 按官方文档应为字符串 \"1\",实际 %v", got["isArchive"])
	}
}

// Bark 参数有误时可能返回 200 但带非零业务码。
func TestBarkBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":400,"message":"failed to get device token"}`))
	}))
	defer srv.Close()

	b, _ := NewBark(BarkOptions{Server: srv.URL, DeviceKey: "bad"})
	err := b.Send(context.Background(), zhRenderer().Event(sampleEvent(state.EventListed)))
	if err == nil || !strings.Contains(err.Error(), "failed to get device token") {
		t.Fatalf("应报出业务错误,实际: %v", err)
	}
}

// 一个渠道失败不应影响其它渠道。
func TestMultiIsolatesFailures(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	var okCalled bool
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	w1, _ := NewWebhook(WebhookOptions{Name: "bad", URL: bad.URL})
	w2, _ := NewWebhook(WebhookOptions{Name: "good", URL: good.URL})

	sent, err := NewMulti([]Notifier{w1, w2}, discardLogger()).Send(
		context.Background(), zhRenderer().Event(sampleEvent(state.EventListed)))
	if err == nil {
		t.Error("应回报失败渠道的错误")
	}
	if !okCalled {
		t.Error("前一个渠道失败不应阻断后续渠道")
	}
	if sent != 1 {
		t.Errorf("应有 1 个渠道送达成功,实际 %d", sent)
	}
}

// 全部渠道失败时 sent 必须为 0——调用方据此决定不推进状态基线。
func TestMultiReportsTotalFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	w1, _ := NewWebhook(WebhookOptions{Name: "bad1", URL: bad.URL})
	w2, _ := NewWebhook(WebhookOptions{Name: "bad2", URL: bad.URL})
	sent, err := NewMulti([]Notifier{w1, w2}, discardLogger()).Send(
		context.Background(), zhRenderer().Event(sampleEvent(state.EventListed)))
	if sent != 0 || err == nil {
		t.Fatalf("全部失败时应返回 sent=0 与错误,实际 sent=%d err=%v", sent, err)
	}
}

// 英文文案不能只换词不换标点:全角括号与顿号混进英文里会很别扭。
// 英文摘要正文此前不在标点检查范围内,digestMore 的中文省略号因此漏了很久。
func TestEnglishDigestUsesASCIIPunctuation(t *testing.T) {
	evs := make([]state.Event, 20)
	for i := range evs {
		evs[i] = sampleEvent(state.EventListed)
	}
	body := enRenderer().Digest(evs).Body

	// 全角标点写成 \u 转义,理由同 TestRenderEnglishUsesASCIIPunctuation。
	for _, bad := range []string{"\u2026", "\uff08", "\uff09", "\uff0c", "\u3001"} {
		if strings.Contains(body, bad) {
			t.Errorf("英文摘要残留全角标点 %q:\n%s", bad, body)
		}
	}
	if !strings.Contains(body, "... and 8 more") {
		t.Errorf("英文摘要的省略行应使用半角:\n%s", body)
	}
}

func TestRenderEnglishUsesASCIIPunctuation(t *testing.T) {
	ev := sampleEvent(state.EventPriceDrop)
	ev.Rules = []string{"MBP high-end", "cheap mini"}
	m := enRenderer().Event(ev)

	if m.Title != "Price drop · CN mac" {
		t.Errorf("标题有误: %q", m.Title)
	}
	if !strings.Contains(m.Body, "(down RMB 2,000, 12.3%)") {
		t.Errorf("降价句应使用半角标点:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "Matched rules: MBP high-end, cheap mini") {
		t.Errorf("多条规则应使用复数与半角分隔:\n%s", m.Body)
	}
	// 全角标点写成 \u 转义:字面全角字符在编辑过程中一旦退化成半角,
	// 这里就变成「半角里不含半角」的恒假检查而静默失效,
	// 与 dashNormalizer 当年栽的是同一个跟头。
	for _, bad := range []string{"\uff08", "\uff09", "\uff0c", "\u3001", "降"} {
		if strings.Contains(m.Body, bad) {
			t.Errorf("英文正文残留中文文案或全角标点 %q:\n%s", bad, m.Body)
		}
	}
}

// 单条规则用单数,多条用复数。
func TestRenderEnglishRuleSingular(t *testing.T) {
	m := enRenderer().Event(sampleEvent(state.EventListed))
	if !strings.Contains(m.Body, "Matched rule: MBP 高配") {
		t.Errorf("单条规则应使用单数:\n%s", m.Body)
	}
}

// 摘要计数的词序中英相反(「上架 2」/「2 listings」),且英文要区分单复数。
func TestDigestEnglishPluralises(t *testing.T) {
	evs := []state.Event{
		sampleEvent(state.EventListed), sampleEvent(state.EventListed),
		sampleEvent(state.EventPriceDrop),
	}
	m := enRenderer().Digest(evs)
	if !strings.Contains(m.Title, "2 listings") {
		t.Errorf("复数计数有误: %q", m.Title)
	}
	if !strings.Contains(m.Title, "1 price drop") || strings.Contains(m.Title, "1 price drops") {
		t.Errorf("单数计数有误: %q", m.Title)
	}
	if !strings.HasPrefix(m.Title, "Refurb watch · ") {
		t.Errorf("摘要标题未本地化: %q", m.Title)
	}
}

// 摘要正文有行数上限,但 Events 必须携带全部事件——
// webhook 模板要能 range 出自己的完整列表,而不是迁就内置排版。
func TestDigestExposesAllEventsToTemplates(t *testing.T) {
	const total = 20
	evs := make([]state.Event, 0, total)
	for i := 0; i < total; i++ {
		evs = append(evs, sampleEvent(state.EventListed))
	}
	m := zhRenderer().Digest(evs)

	if len(m.Events) != total {
		t.Errorf("Events 应含全部 %d 条事件,实际 %d 条", total, len(m.Events))
	}
	if !strings.Contains(m.Body, "另有 8 条") {
		t.Errorf("正文应折叠超出部分:\n%s", m.Body)
	}
	if n := strings.Count(m.Body, "[上架]"); n != 12 {
		t.Errorf("正文应只列 12 条,实际 %d 条", n)
	}
}

// Kind 保持语言中立,模板可据此自行分派任意语言的文案。
func TestEventViewKindIsLanguageNeutral(t *testing.T) {
	m := enRenderer().Event(sampleEvent(state.EventPriceDrop))
	if m.Events[0].Kind != "price_drop" {
		t.Errorf("Kind 应为语言中立标识,实际 %q", m.Events[0].Kind)
	}
	if m.Events[0].KindLabel != "Price drop" {
		t.Errorf("KindLabel 应已本地化,实际 %q", m.Events[0].KindLabel)
	}
}

func TestWebhookTemplateCanRangeOverEvents(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh, err := NewWebhook(WebhookOptions{
		Name: "discord", URL: srv.URL,
		Body: `{"count":{{.Count}},"first":{{json .PartNumber}},"items":[{{range $i, $e := .Events}}{{if $i}},{{end}}{{json $e.ProductTitle}}{{end}}]}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	evs := []state.Event{sampleEvent(state.EventListed), sampleEvent(state.EventPriceDrop)}
	if err := wh.Send(context.Background(), zhRenderer().Digest(evs)); err != nil {
		t.Fatalf("推送失败: %v", err)
	}

	var payload struct {
		Count int      `json:"count"`
		First string   `json:"first"`
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("载荷不是合法 JSON: %v\n%s", err, got)
	}
	if payload.Count != 2 || len(payload.Items) != 2 {
		t.Errorf("模板未拿到全部事件: %+v", payload)
	}
	// 顶层扁平字段仍取首条事件,保持既有模板可用。
	if payload.First != "FHFA4CH/A" {
		t.Errorf("顶层字段应展开首条事件,实际 %q", payload.First)
	}
}

func TestParseLangRejectsUnknown(t *testing.T) {
	if _, err := ParseLang("jp"); err == nil {
		t.Error("未知语言应报错,而不是静默回退默认值")
	}
	if l, err := ParseLang(""); err != nil || l != DefaultLang {
		t.Errorf("留空应回退默认语言,实际 %q err=%v", l, err)
	}
	if l, _ := ParseLang(" en "); l != LangEN {
		t.Errorf("应容忍首尾空白,实际 %q", l)
	}
	// 配置里的 regions/categories 都是大小写不敏感的,lang 没理由单独挑剔;
	// 且 Validate 会把返回值存回配置,必须是规范形式而不是用户的原始拼写。
	for _, in := range []string{"EN", "En"} {
		if l, err := ParseLang(in); err != nil || l != LangEN {
			t.Errorf("ParseLang(%q) 应归一化为 %q,实际 %q err=%v", in, LangEN, l, err)
		}
	}
	for _, in := range []string{"zh-cn", "ZH-CN", "Zh-CN"} {
		if l, err := ParseLang(in); err != nil || l != LangZH {
			t.Errorf("ParseLang(%q) 应归一化为 %q,实际 %q err=%v", in, LangZH, l, err)
		}
	}
}

// console 的 dry-run 输出同样跟随语言。
func TestConsoleFollowsLang(t *testing.T) {
	var buf bytes.Buffer
	if err := NewConsole(&buf, LangEN).Send(context.Background(),
		enRenderer().Event(sampleEvent(state.EventListed))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Title: Listed · CN mac") {
		t.Errorf("dry-run 输出未本地化:\n%s", buf.String())
	}
}

// phrases 的字段漏填不会编译报错,只会得到空串,推送出去就是残缺文案。
// 这里逐字段扫一遍,新增语言或新增文案时漏掉哪一项都会在这里失败。
func TestAllLanguagesDefineEveryPhrase(t *testing.T) {
	kinds := []state.EventKind{state.EventListed, state.EventPriceDrop, state.EventDelisted}
	for lang, p := range langPhrases {
		v := reflect.ValueOf(p)
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.Type.Kind() != reflect.String {
				continue
			}
			if v.Field(i).String() == "" {
				t.Errorf("语言 %s 缺少文案字段 %s", lang, f.Name)
			}
		}
		for _, k := range kinds {
			w, ok := p.kinds[k]
			if !ok {
				t.Errorf("语言 %s 缺少事件 %s 的词形", lang, k)
				continue
			}
			if w.label == "" || w.countOne == "" || w.countMany == "" {
				t.Errorf("语言 %s 的事件 %s 词形不完整: %+v", lang, k, w)
			}
		}
	}
}

// 每种语言都必须能渲染出非空的标题与正文,防止格式串占位符与参数数量对不上。
func TestEveryLanguageRendersAllKinds(t *testing.T) {
	for lang := range langPhrases {
		r := NewRenderer(lang, "g")
		for _, k := range []state.EventKind{state.EventListed, state.EventPriceDrop, state.EventDelisted} {
			m := r.Event(sampleEvent(k))
			if m.Title == "" || m.Body == "" {
				t.Errorf("语言 %s 的 %s 事件渲染为空", lang, k)
			}
			for _, bad := range []string{"%!", "%s", "%d", "EXTRA", "MISSING"} {
				if strings.Contains(m.Title+m.Body, bad) {
					t.Errorf("语言 %s 的 %s 事件格式串有误(含 %q):\n%s\n%s", lang, k, bad, m.Title, m.Body)
				}
			}
		}
		d := r.Digest([]state.Event{sampleEvent(state.EventListed), sampleEvent(state.EventPriceDrop)})
		if strings.Contains(d.Title+d.Body, "%!") {
			t.Errorf("语言 %s 的摘要格式串有误:\n%s\n%s", lang, d.Title, d.Body)
		}

		// consoleHeader 的占位符个数漏写不会编译报错,只会在 dry-run 输出里
		// 静默印出 %!(EXTRA ...)。它不走 Renderer,得单独渲染一次才能覆盖到。
		var buf bytes.Buffer
		if err := NewConsole(&buf, lang).Send(context.Background(), d); err != nil {
			t.Fatalf("语言 %s 的 console 渲染失败: %v", lang, err)
		}
		if strings.Contains(buf.String(), "%!") {
			t.Errorf("语言 %s 的 consoleHeader 占位符与实参不匹配:\n%s", lang, buf.String())
		}
	}
}
