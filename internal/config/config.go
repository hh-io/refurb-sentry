package config

import (
	"fmt"
	"path/filepath"
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
	History  HistoryConfig   `yaml:"history"`
	Channels []ChannelConfig `yaml:"channels"`
	Rules    []filter.Rule   `yaml:"rules"`
}

// HistoryConfig 控制历史档案:每次上架、调价、下架各追加一行,供 -history 查询。
type HistoryConfig struct {
	// Enabled 默认开启(nil 即开启)。档案无法事后补录,等到想查某个配置以前卖多少钱时
	// 才去打开,之前的几个月就已经丢了;而代价只是数据目录下多一个几 MB 的文件。
	// 用 *bool 而不是沿用 daily_summary「空串即关闭」的约定:那种约定需要一整套
	// null / 空白 / 环境变量的特判才守得住,开关就该长得像开关。
	Enabled *bool `yaml:"enabled"`
	// Path 留空时放在 state_path 的同一目录下,见 Validate。
	Path string `yaml:"path"`
	// Categories 是只归档、不推送的额外分类。它们照常抓取、建立基线、写入档案,
	// 但产生的事件绝不进入推送与日报。已在顶层 categories 里的分类无需重复列出。
	Categories []string `yaml:"categories"`
}

func (h HistoryConfig) IsEnabled() bool { return h.Enabled == nil || *h.Enabled }

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
	// DailySummary 是每日汇总的触发时刻,格式 "HH:MM"。
	// 默认 DefaultDailySummary,显式写成空串即关闭。
	//
	// 汇总本身不产生新的抓取,只是把已有状态读一遍。它的用途是把「手机很安静」
	// 这个二义信号变成单义:条到了说明抓取与推送链路都通,条没到就是系统出了问题。
	// 其中「命中规则数」还能暴露规则写错——维度键名抄错、型号猜错的表现
	// 正是它长期为 0,而那类错误在别处不会有任何报错。
	DailySummary string `yaml:"daily_summary"`
}

// DefaultDailySummary 是每日汇总的默认时刻。默认开启而不是关闭:
// 这个工具的常态是配一条窄规则等上几个月,期间「手机安静」既可能是没货,
// 也可能是进程挂了或规则写错了永远不命中——三者需要的行动完全不同。
// 首次启动那一份还会立刻告诉用户规则当前命中几件,
// 把「规则写错」从几个月后的困惑提前到第一分钟。
const DefaultDailySummary = "09:00"

// ParseDailySummary 把 "HH:MM" 解析成当天零点起的偏移。
func ParseDailySummary(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("notify.daily_summary=%q 不是合法的 HH:MM 时刻", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
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
		Notify: NotifyConfig{
			Group: "refurb-sentry", DigestThreshold: 5, Lang: string(notify.DefaultLang),
			DailySummary: DefaultDailySummary,
		},
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
	// 无条件写回归一化后的值:只在非空分支里写回的话,daily_summary: " "
	// 会以「校验通过且已关闭」的姿态离开 Validate,却在 NewRunner 里
	// 因为 at != "" 而去解析空串,让进程拒绝启动。
	//
	// 这里绝不能像 interval 那样把空值回填成默认时刻:默认值由 Default()
	// 提供,用户显式写 daily_summary: "" 正是唯一的关闭手段,一回填就再也关不掉——
	// 而这是个会往用户手机上推东西的功能。
	// TestDailySummaryEveryDisablingForm 覆盖五种关闭写法与两种该保持默认的写法。
	c.Notify.DailySummary = strings.TrimSpace(c.Notify.DailySummary)
	if c.Notify.DailySummary != "" {
		if _, err := ParseDailySummary(c.Notify.DailySummary); err != nil {
			return nil, err
		}
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

	// 默认跟状态文件放在同一目录,而不是相对工作目录的 data/:systemd 单元的工作目录
	// 是只读的 /opt/refurb-sentry,状态写在 StateDirectory 里,固定相对路径会让档案
	// 每一轮都写失败;compose 挂的卷也只覆盖状态所在的那个目录。
	if c.History.Path == "" {
		c.History.Path = filepath.Join(filepath.Dir(c.StatePath), "history.jsonl")
	}
	extra, err := c.archiveOnlyCategories()
	if err != nil {
		return nil, err
	}
	c.History.Categories = extra
	if !c.History.IsEnabled() && len(extra) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"history.enabled 为 false,history.categories 中的 %v 不会被抓取", extra))
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
	// 档案开启时补齐为档案服务,不再以规则为前提,见 Runner.fillMissingMemory。
	if c.HTTP.FillMissingMemory && !c.History.IsEnabled() &&
		!filter.RulesUseDimension(c.Rules, apple.MemoryDimension) {
		warnings = append(warnings, fmt.Sprintf(
			"fill_missing_memory 已开启,但没有任何规则约束 %s,补齐不会改变推送结果,已跳过",
			apple.MemoryDimension))
	}
	return warnings, nil
}

// archiveOnlyCategories 校验并归一化 history.categories,去掉已在顶层 categories 里的项。
// 重复列出是无害的写法(「这些我都要归档」),不值得报错或告警。
func (c *Config) archiveOnlyCategories() ([]string, error) {
	pushed := make(map[string]bool, len(c.Categories))
	for _, cat := range c.Categories {
		pushed[cat] = true
	}
	var out []string
	for _, cat := range c.History.Categories {
		if !apple.ValidCategory(cat) {
			return nil, fmt.Errorf("history.categories 中有未知分类 %q,可选:%v", cat, apple.Categories)
		}
		cat = strings.ToLower(strings.TrimSpace(cat))
		if pushed[cat] {
			continue
		}
		pushed[cat] = true
		out = append(out, cat)
	}
	return out, nil
}
