package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// envRefRE 匹配 ${VAR} 与 ${VAR:-默认值}。
var envRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv 展开一段文本中的环境变量引用。
// 未定义且未给默认值的变量会直接报错——静默展开成空串会让 device_key 变空,
// 表现为「程序在跑但推送永远收不到」,这种失败方式最难排查。
func ExpandEnv(s string) (string, error) {
	var missing []string
	out := envRefRE.ReplaceAllStringFunc(s, func(m string) string {
		g := envRefRE.FindStringSubmatch(m)
		name, hasDefault, def := g[1], g[2] != "", g[3]

		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		if hasDefault {
			return def
		}
		missing = append(missing, name)
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("配置引用了未设置的环境变量: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// expandNode 在 YAML 节点树上展开环境变量,而不是在原始文本上做替换。
// 这样注释里出现的 ${VAR} 不会被误当成引用,展开后的值也由 YAML 序列化器
// 负责转义,含引号或换行的密钥不会撕裂配置结构。
//
// skip 中的子树不展开:那是被 enabled: false 关掉的通知渠道,
// 不该因为它配置里写着 ${TELEGRAM_BOT_TOKEN} 就要求用户去设这个变量。
func expandNode(n *yaml.Node, skip map[*yaml.Node]bool) error {
	if skip[n] {
		return nil
	}
	// 只处理字符串标量:数字与布尔标量不该被当作模板,
	// 且保持原 Tag 可以让 "123" 这类展开结果仍作为字符串解析。
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		v, err := ExpandEnv(n.Value)
		if err != nil {
			return err
		}
		n.Value = v
	}
	for _, c := range n.Content {
		if err := expandNode(c, skip); err != nil {
			return err
		}
	}
	return nil
}

// disabledChannelNodes 找出 channels 里 enabled: false 的条目子树。
func disabledChannelNodes(root *yaml.Node) map[*yaml.Node]bool {
	out := map[*yaml.Node]bool{}
	doc := root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "channels" {
			continue
		}
		seq := doc.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			continue
		}
		for _, ch := range seq.Content {
			if channelDisabled(ch) {
				out[ch] = true
			}
		}
	}
	return out
}

func channelDisabled(ch *yaml.Node) bool {
	if ch.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(ch.Content); i += 2 {
		if ch.Content[i].Value == "enabled" && ch.Content[i+1].Value == "false" {
			return true
		}
	}
	return false
}

// Load 读取并校验配置文件。
func Load(path string) (*Config, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取配置文件 %s: %w", path, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	if err := expandNode(&root, disabledChannelNodes(&root)); err != nil {
		return nil, nil, fmt.Errorf("配置文件 %s: %w", path, err)
	}

	// 回写再解析一遍,以便启用 KnownFields 严格模式:
	// 拼错的键会被明确报出,而不是静默忽略后让人以为设置已生效。
	expanded, err := yaml.Marshal(&root)
	if err != nil {
		return nil, nil, fmt.Errorf("配置文件 %s: 展开环境变量后重新序列化失败: %w", path, err)
	}

	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}

	if err := applyEnvOverrides(&cfg); err != nil {
		return nil, nil, err
	}

	warnings, err := cfg.Validate()
	if err != nil {
		return nil, nil, fmt.Errorf("配置文件 %s 校验失败: %w", path, err)
	}
	return &cfg, warnings, nil
}

// applyEnvOverrides 允许用环境变量覆盖少量常改的顶层项,
// 便于在容器或 systemd 单元里不改配置文件就调整行为。
//
// 取值非法时直接报错而不是跳过:静默沿用默认值会让人以为覆盖已经生效,
// 与 ExpandEnv 对缺失变量的处理保持同一种态度。
func applyEnvOverrides(c *Config) error {
	if v := os.Getenv("REFURB_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("环境变量 REFURB_INTERVAL=%q 不是合法时长(如 120s、2m): %w", v, err)
		}
		c.Interval = Duration(d)
	}
	if v := os.Getenv("REFURB_STATE_PATH"); v != "" {
		c.StatePath = v
	}
	if v := os.Getenv("REFURB_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("REFURB_PROXY"); v != "" {
		c.HTTP.Proxy = v
	}
	if v := os.Getenv("REFURB_REGIONS"); v != "" {
		c.Regions = splitList(v)
	}
	if v := os.Getenv("REFURB_CATEGORIES"); v != "" {
		c.Categories = splitList(v)
	}
	if v := os.Getenv("REFURB_DIGEST_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("环境变量 REFURB_DIGEST_THRESHOLD=%q 必须是正整数", v)
		}
		c.Notify.DigestThreshold = n
	}
	// 这一项用 LookupEnv 而非 Getenv:日报默认开启,空串是「关闭」这个有意义的取值,
	// 只有 LookupEnv 能把「显式设成空」与「根本没设」区分开。
	// 配置文件只读挂载(compose 就是这么挂的)时,这是唯一能关掉它的手段。
	if v, ok := os.LookupEnv("REFURB_DAILY_SUMMARY"); ok {
		if v != "" {
			// 在这里就校验,是为了让错误信息指对地方:交给 Validate 的话,
			// 报出来的是「配置文件校验失败: notify.daily_summary=...」,
			// 而那个键根本不在文件里,运维会照着去翻一份没问题的配置。
			if _, err := ParseDailySummary(v); err != nil {
				return fmt.Errorf("环境变量 REFURB_DAILY_SUMMARY=%q 必须是 HH:MM 形式的时刻", v)
			}
		}
		c.Notify.DailySummary = v
	}
	return nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
