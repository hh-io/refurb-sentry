package config

import (
	"fmt"
	"time"

	"github.com/hh/refurb-sentry/internal/apple"
	"github.com/hh/refurb-sentry/internal/filter"
)

// Duration 让 YAML 里可以直接写 "120s"、"2m" 这类可读时长。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return fmt.Errorf("时长必须是带单位的字符串,如 \"120s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	// Interval 是轮询间隔。列表页的 CDN 缓存为 120 秒,设得更短拿到的是同一份副本。
	Interval  Duration `yaml:"interval"`
	StatePath string   `yaml:"state_path"`
	LogLevel  string   `yaml:"log_level"`

	Regions    []string `yaml:"regions"`
	Categories []string `yaml:"categories"`

	HTTP     HTTPConfig      `yaml:"http"`
	Notify   NotifyConfig    `yaml:"notify"`
	Channels []ChannelConfig `yaml:"channels"`
	Rules    []filter.Rule   `yaml:"rules"`
}

type HTTPConfig struct {
	// Proxy 支持 http/https/socks5,用途是修正出口地区而非隐藏身份。
	Proxy      string   `yaml:"proxy"`
	UserAgent  string   `yaml:"user_agent"`
	Timeout    Duration `yaml:"timeout"`
	MaxRetries int      `yaml:"max_retries"`
	// DelayMin/DelayMax 是同一轮内相邻请求的随机间隔,用来打散请求节奏。
	DelayMin Duration `yaml:"delay_min"`
	DelayMax Duration `yaml:"delay_max"`
}

type NotifyConfig struct {
	Group string `yaml:"group"`
	// DigestThreshold:一轮内匹配事件超过此数量就合并为一条摘要,
	// 避免 Apple 批量上架时几十条推送刷屏。
	DigestThreshold int `yaml:"digest_threshold"`
}

type ChannelConfig struct {
	Type    string `yaml:"type"` // bark | webhook
	Name    string `yaml:"name"`
	Enabled *bool  `yaml:"enabled"`

	// bark
	Server    string `yaml:"server"`
	DeviceKey string `yaml:"device_key"`
	Sound     string `yaml:"sound"`

	// webhook
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`

	Timeout Duration `yaml:"timeout"`
}

func (c ChannelConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// Default 返回一份可直接运行的默认配置。
func Default() Config {
	return Config{
		Interval:   Duration(120 * time.Second),
		StatePath:  "data/state.json",
		LogLevel:   "info",
		Regions:    []string{"CN"},
		Categories: []string{"mac"},
		HTTP: HTTPConfig{
			Timeout:    Duration(20 * time.Second),
			MaxRetries: 3,
			DelayMin:   Duration(1 * time.Second),
			DelayMax:   Duration(3 * time.Second),
		},
		Notify: NotifyConfig{Group: "refurb-sentry", DigestThreshold: 5},
	}
}

// minInterval 是硬下限。列表页 CDN 缓存 120 秒,低于此值除了徒增请求量没有任何收益。
const minInterval = 30 * time.Second

// Validate 校验配置并回填缺省值。返回的 warnings 由调用方以 WARN 级别输出。
func (c *Config) Validate() (warnings []string, err error) {
	if c.Interval.Std() <= 0 {
		c.Interval = Duration(120 * time.Second)
	}
	if c.Interval.Std() < minInterval {
		return nil, fmt.Errorf("interval 为 %s,低于下限 %s", c.Interval.Std(), minInterval)
	}
	if c.Interval.Std() < 120*time.Second {
		warnings = append(warnings, fmt.Sprintf(
			"interval=%s 短于列表页 120s 的 CDN 缓存,不会更早发现新品,只会增加请求量", c.Interval.Std()))
	}

	if c.StatePath == "" {
		c.StatePath = "data/state.json"
	}
	if c.HTTP.Timeout.Std() <= 0 {
		c.HTTP.Timeout = Duration(20 * time.Second)
	}
	if c.HTTP.MaxRetries <= 0 {
		c.HTTP.MaxRetries = 3
	}
	if c.HTTP.DelayMax.Std() < c.HTTP.DelayMin.Std() {
		return nil, fmt.Errorf("delay_max(%s) 不能小于 delay_min(%s)", c.HTTP.DelayMax.Std(), c.HTTP.DelayMin.Std())
	}
	if c.Notify.DigestThreshold <= 0 {
		c.Notify.DigestThreshold = 5
	}
	if c.Notify.Group == "" {
		c.Notify.Group = "refurb-sentry"
	}

	if len(c.Regions) == 0 {
		return nil, fmt.Errorf("regions 不能为空,可选:%v", apple.RegionCodes())
	}
	for _, r := range c.Regions {
		if _, e := apple.LookupRegion(r); e != nil {
			return nil, e
		}
	}
	if len(c.Categories) == 0 {
		return nil, fmt.Errorf("categories 不能为空,可选:%v", apple.Categories)
	}
	for _, cat := range c.Categories {
		if !apple.ValidCategory(cat) {
			return nil, fmt.Errorf("未知分类 %q,可选:%v", cat, apple.Categories)
		}
	}

	for i, ch := range c.Channels {
		if !ch.IsEnabled() {
			continue
		}
		switch ch.Type {
		case "bark":
			if ch.DeviceKey == "" {
				return nil, fmt.Errorf("channels[%d](bark)缺少 device_key", i)
			}
		case "webhook":
			if ch.URL == "" {
				return nil, fmt.Errorf("channels[%d](webhook)缺少 url", i)
			}
		case "":
			return nil, fmt.Errorf("channels[%d] 缺少 type 字段(bark 或 webhook)", i)
		default:
			return nil, fmt.Errorf("channels[%d] 的 type %q 不支持,可选:bark、webhook", i, ch.Type)
		}
	}

	if len(c.Rules) == 0 {
		warnings = append(warnings, "未配置任何过滤规则,将监控所选地区/分类下的全部商品")
	}
	return warnings, nil
}
