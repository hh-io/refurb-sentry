// 外部测试包:config 依赖 notify,内部测试包引用 config 会成环。
package notify_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/notify"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// exampleWebhookChannels 是示例配置里必须存在的 webhook 模板。
// 少一个就说明模板被误删,用户复制粘贴的起点没了。
var exampleWebhookChannels = []string{"telegram", "feishu", "wecom", "dingtalk", "serverchan"}

const wantTitle = "翻新 14 英寸 MacBook Pro"

// 示例配置里的 body 模板是用户复制粘贴的起点,写错了既不编译报错也不会被
// 其它测试碰到,只会在真收到事件那天静默返回 400。这里把每个模板真发一遍
// 到本地服务器,按 Content-Type 校验载荷确实是合法的 JSON / form 表单,
// 并确认商品标题真的落进了载荷里,而不是渲染出一个空壳。
func TestExampleConfigWebhookTemplatesRender(t *testing.T) {
	// bark 渠道是启用状态,引用的环境变量必须有值才能加载。
	t.Setenv("BARK_KEY", "example-device-key")

	cfg, _, err := config.Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("加载示例配置: %v", err)
	}

	var gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	msg := notify.NewRenderer(notify.LangZH, "g").Event(state.Event{
		Kind: state.EventPriceDrop,
		Product: apple.Product{
			Region: "CN", Category: "mac", PartNumber: "FHFA4CH/A",
			Title:      wantTitle,
			URL:        "https://www.apple.com.cn/shop/product/fhfa4ch/a",
			PriceCents: 1419900, Currency: "CNY",
		},
		OldPriceCents: 1619900,
		Rules:         []string{"MBP 高配"},
	})

	seen := map[string]bool{}
	for _, ch := range cfg.Channels {
		if ch.Type != "webhook" {
			continue
		}
		seen[ch.Name] = true

		// URL 指向本地服务器,其余照搬示例——被测的正是 headers 与 body 模板。
		wh, err := notify.NewWebhook(notify.WebhookOptions{
			Name: ch.Name, URL: srv.URL, Method: ch.Method,
			Headers: ch.Headers, Body: ch.Body,
		})
		if err != nil {
			t.Errorf("%s 的模板不合法: %v", ch.Name, err)
			continue
		}
		if err := wh.Send(context.Background(), msg); err != nil {
			t.Errorf("%s 发送失败: %v", ch.Name, err)
			continue
		}

		switch {
		case strings.HasPrefix(gotType, "application/json"):
			if !json.Valid(gotBody) {
				t.Errorf("%s 渲染出的不是合法 JSON:\n%s", ch.Name, gotBody)
			}
			// Go 的 json 编码保留 UTF-8,中文标题应原样出现在载荷里。
			if !strings.Contains(string(gotBody), wantTitle) {
				t.Errorf("%s 的载荷里没有商品标题:\n%s", ch.Name, gotBody)
			}
		case strings.HasPrefix(gotType, "application/x-www-form-urlencoded"):
			form, err := url.ParseQuery(string(gotBody))
			if err != nil {
				t.Errorf("%s 渲染出的不是合法表单: %v\n%s", ch.Name, err, gotBody)
				continue
			}
			// 解析回来必须与原文逐字相同,漏用 urlquery 会在这里露出来。
			if form.Get("title") != msg.Title {
				t.Errorf("%s 的 title 转义有误: %q", ch.Name, form.Get("title"))
			}
			if form.Get("desp") != msg.Text() {
				t.Errorf("%s 的 desp 转义有误: %q", ch.Name, form.Get("desp"))
			}
		default:
			t.Errorf("%s 的 Content-Type 无法识别: %q", ch.Name, gotType)
		}
	}

	for _, name := range exampleWebhookChannels {
		if !seen[name] {
			t.Errorf("示例配置缺少 %s 的 webhook 模板", name)
		}
	}
}
