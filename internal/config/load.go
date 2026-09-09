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
func expandNode(n *yaml.Node) error {
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
		if err := expandNode(c); err != nil {
			return err
		}
	}
	return nil
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
	if err := expandNode(&root); err != nil {
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

	applyEnvOverrides(&cfg)

	warnings, err := cfg.Validate()
	if err != nil {
		return nil, nil, fmt.Errorf("配置文件 %s 校验失败: %w", path, err)
	}
	return &cfg, warnings, nil
}

// applyEnvOverrides 允许用环境变量覆盖少量常改的顶层项,
// 便于在容器或 systemd 单元里不改配置文件就调整行为。
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("REFURB_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			c.Interval = Duration(d)
		}
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
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Notify.DigestThreshold = n
		}
	}
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
