package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBarkServer 是 Bark 官方服务器,自建服务端可在配置里覆盖。
const DefaultBarkServer = "https://api.day.app"

// Bark 走 Bark 的 V2 REST 接口:POST {server}/push,JSON 提交。
// 字段名依据官方 API V2 文档(device_key / title / body / url / group / sound / isArchive)。
type Bark struct {
	name      string
	server    string
	deviceKey string
	sound     string
	hc        *http.Client
}

type BarkOptions struct {
	Name      string
	Server    string
	DeviceKey string
	Sound     string
	Timeout   time.Duration
}

func NewBark(opt BarkOptions) (*Bark, error) {
	if opt.DeviceKey == "" {
		return nil, fmt.Errorf("bark 渠道缺少 device_key")
	}
	if opt.Server == "" {
		opt.Server = DefaultBarkServer
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	if opt.Name == "" {
		opt.Name = "bark"
	}
	return &Bark{
		name:      opt.Name,
		server:    strings.TrimRight(opt.Server, "/"),
		deviceKey: opt.DeviceKey,
		sound:     opt.Sound,
		hc:        &http.Client{Timeout: opt.Timeout},
	}, nil
}

func (b *Bark) Name() string { return b.name }

func (b *Bark) Send(ctx context.Context, m Message) error {
	payload := map[string]any{
		"device_key": b.deviceKey,
		"title":      m.Title,
		"body":       m.Body,
		// isArchive 按官方文档是字符串 "1",让消息留存在 App 的历史里。
		"isArchive": "1",
	}
	if m.URL != "" {
		payload["url"] = m.URL
	}
	if m.Group != "" {
		payload["group"] = m.Group
	}
	if b.sound != "" {
		payload["sound"] = b.sound
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化 bark 请求: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.server+"/push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造 bark 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("请求 bark: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bark 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	// Bark 即使参数有误也可能返回 200,需要看响应体里的业务码。
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respBody, &r); err == nil && r.Code != 0 && r.Code != http.StatusOK {
		return fmt.Errorf("bark 业务错误 code=%d: %s", r.Code, r.Message)
	}
	return nil
}
