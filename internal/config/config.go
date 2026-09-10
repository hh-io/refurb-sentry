package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/notify"
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

	// FillMissingMemory 开启后,对列表页没给内存维度的商品补抓一次详情页。
	// 实测上游对 16 英寸 MacBook Pro 的 M5 Pro / M5 Max 不给 tsMemorySize,
	// 不补的话按内存过滤的规则会静默漏掉这一整档机型。
	// 默认关闭:它打破了「一分类一请求」的设计,只有按内存过滤时才值得付这个代价。
	FillMissingMemory bool `yaml:"fill_missing_memory"`
}

type NotifyConfig struct {
	Group string `yaml:"group"`
	// Lang 只影响推送文案(zh-CN / en);日志与错误信息始终是中文。
	// 商品标题的语言由抓取的地区决定,不受这里影响。
	Lang string `yaml:"lang"`
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
	Icon      string `yaml:"icon"`

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
		Notify: NotifyConfig{Group: "refurb-sentry", DigestThreshold: 5, Lang: string(notify.DefaultLang)},
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
	// 归一化后存回,让后续取用不必再关心大小写与空白。
	lang, e := notify.ParseLang(c.Notify.Lang)
	if e != nil {
		return nil, e
	}
	c.Notify.Lang = string(lang)

	if len(c.Regions) == 0 {
		return nil, fmt.Errorf("regions 不能为空,可选:%v", apple.RegionCodes())
	}
	// 归一化后存回,让状态库的键与日志里的地区码始终是规范大写形式。
	for i, r := range c.Regions {
		reg, e := apple.LookupRegion(r)
		if e != nil {
			return nil, e
		}
		c.Regions[i] = reg.Code
	}
	if len(c.Categories) == 0 {
		return nil, fmt.Errorf("categories 不能为空,可选:%v", apple.Categories)
	}
	for i, cat := range c.Categories {
		if !apple.ValidCategory(cat) {
			return nil, fmt.Errorf("未知分类 %q,可选:%v", cat, apple.Categories)
		}
		c.Categories[i] = strings.ToLower(strings.TrimSpace(cat))
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
	// 开了开关却没有规则用到该维度时,补齐逻辑会整个跳过——这是对的(补了也改变不了
	// 推送结果),但沉默会让人以为在生效,等到「怎么还是漏机型」时无从下手。
	if c.HTTP.FillMissingMemory && !rulesUseDimension(c.Rules, apple.MemoryDimension) {
		warnings = append(warnings, fmt.Sprintf(
			"fill_missing_memory 已开启,但没有任何规则约束 %s,补齐不会改变推送结果,已跳过",
			apple.MemoryDimension))
	}
	return warnings, nil
}

// rulesUseDimension 报告是否有规则约束了这个维度键。大小写敏感,与 filter 的匹配一致:
// 维度键要与上游字面一致才匹配得上,这里跟着严格比较,才不会把写错大小写的规则
// 报成「已在用」。
func rulesUseDimension(rules []filter.Rule, key string) bool {
	for _, r := range rules {
		if _, ok := r.Dimensions[key]; ok {
			return true
		}
	}
	return false
}
