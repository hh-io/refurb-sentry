package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestRenderPriceDrop(t *testing.T) {
	m := RenderEvent(sampleEvent(state.EventPriceDrop), "g")
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
	m := RenderDigest(evs, "g")
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
	if err := wh.Send(context.Background(), RenderEvent(ev, "g")); err != nil {
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
	err := wh.Send(context.Background(), RenderEvent(sampleEvent(state.EventListed), "g"))
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
	if err := b.Send(context.Background(), RenderEvent(sampleEvent(state.EventListed), "grp")); err != nil {
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
	err := b.Send(context.Background(), RenderEvent(sampleEvent(state.EventListed), "g"))
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
		context.Background(), RenderEvent(sampleEvent(state.EventListed), "g"))
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
		context.Background(), RenderEvent(sampleEvent(state.EventListed), "g"))
	if sent != 0 || err == nil {
		t.Fatalf("全部失败时应返回 sent=0 与错误,实际 sent=%d err=%v", sent, err)
	}
}
