package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/hh/refurb-sentry/internal/apple"
	"github.com/hh/refurb-sentry/internal/state"
)

// defaultWebhookBody 是未指定模板时的通用 JSON 载荷。
const defaultWebhookBody = `{"title":{{json .Title}},"body":{{json .Body}},"url":{{json .URL}},"kind":{{json .Kind}},"count":{{.Count}}}`

// Webhook 是模板化的通用 HTTP 推送渠道。一个实现即可覆盖 Telegram、
// 飞书、Server 酱、Discord 等——差别只在 URL、请求头和 body 模板。
type Webhook struct {
	name     string
	url      string
	method   string
	headers  map[string]string
	bodyTmpl *template.Template
	hc       *http.Client
}

type WebhookOptions struct {
	Name    string
	URL     string
	Method  string
	Headers map[string]string
	// Body 是 text/template 模板,可用 {{json .X}} 安全地嵌入 JSON 字符串。
	Body    string
	Timeout time.Duration
}

var tmplFuncs = template.FuncMap{
	// json 把值序列化为合法的 JSON 字面量,避免标题里的引号和换行破坏载荷。
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
}

func NewWebhook(opt WebhookOptions) (*Webhook, error) {
	if opt.URL == "" {
		return nil, fmt.Errorf("webhook 渠道缺少 url")
	}
	if opt.Method == "" {
		opt.Method = http.MethodPost
	}
	if opt.Body == "" {
		opt.Body = defaultWebhookBody
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	if opt.Name == "" {
		opt.Name = "webhook"
	}

	t, err := template.New(opt.Name).Funcs(tmplFuncs).Parse(opt.Body)
	if err != nil {
		return nil, fmt.Errorf("webhook %q 的 body 模板不合法: %w", opt.Name, err)
	}

	headers := make(map[string]string, len(opt.Headers)+1)
	for k, v := range opt.Headers {
		headers[k] = v
	}
	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = "application/json; charset=utf-8"
	}

	return &Webhook{
		name:     opt.Name,
		url:      opt.URL,
		method:   strings.ToUpper(opt.Method),
		headers:  headers,
		bodyTmpl: t,
		hc:       &http.Client{Timeout: opt.Timeout},
	}, nil
}

func (w *Webhook) Name() string { return w.name }

// TemplateData 是 body 模板可访问的字段。摘要消息里的商品字段取首条事件。
type TemplateData struct {
	Title string
	Body  string
	URL   string
	Group string
	Text  string

	Kind      string
	KindLabel string
	Count     int

	Region        string
	Category      string
	PartNumber    string
	ProductTitle  string
	Currency      string
	Price         string
	PriceCents    int64
	OldPrice      string
	OldPriceCents int64
	Rules         []string
}

func buildTemplateData(m Message) TemplateData {
	d := TemplateData{
		Title: m.Title, Body: m.Body, URL: m.URL, Group: m.Group,
		Text: m.Text(), Count: len(m.Events),
	}
	if len(m.Events) == 0 {
		return d
	}
	ev := m.Events[0]
	p := ev.Product
	d.Kind = string(ev.Kind)
	d.KindLabel = ev.Kind.Label()
	d.Region, d.Category, d.PartNumber = p.Region, p.Category, p.PartNumber
	d.ProductTitle, d.Currency = p.Title, p.Currency
	d.Price, d.PriceCents = p.DisplayPrice(), p.PriceCents
	d.Rules = ev.Rules
	if ev.Kind == state.EventPriceDrop {
		d.OldPriceCents = ev.OldPriceCents
		d.OldPrice = apple.FormatPrice(ev.OldPriceCents, p.Currency)
	}
	return d
}

func (w *Webhook) Send(ctx context.Context, m Message) error {
	var buf bytes.Buffer
	if err := w.bodyTmpl.Execute(&buf, buildTemplateData(m)); err != nil {
		return fmt.Errorf("渲染 webhook 模板: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, w.method, w.url, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("构造 webhook 请求: %w", err)
	}
	for k, v := range w.headers {
		req.Header.Set(k, v)
	}

	resp, err := w.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 webhook: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
