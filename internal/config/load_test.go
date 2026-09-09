package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 注释里出现的 ${VAR} 不应被当成真实引用——这是文本层替换方案会踩的坑。
func TestEnvRefInCommentIsIgnored(t *testing.T) {
	p := writeConfig(t, `
# 所有 ${VAR} 都会被展开,${MISSING_IN_COMMENT} 也一样
regions: [CN]
categories: [mac]
`)
	if _, _, err := Load(p); err != nil {
		t.Fatalf("注释中的环境变量引用不应导致失败: %v", err)
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("TEST_BARK_KEY", "abc123")
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
channels:
  - type: bark
    device_key: ${TEST_BARK_KEY}
    server: ${TEST_BARK_SERVER:-https://api.day.app}
`)
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Channels[0].DeviceKey != "abc123" {
		t.Errorf("device_key 未展开: %q", cfg.Channels[0].DeviceKey)
	}
	if cfg.Channels[0].Server != "https://api.day.app" {
		t.Errorf("默认值未生效: %q", cfg.Channels[0].Server)
	}
}

// 未定义变量必须报错:静默展开成空串会让推送永远失败且难以排查。
func TestMissingEnvIsFatal(t *testing.T) {
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
channels:
  - type: bark
    device_key: ${DEFINITELY_NOT_SET_XYZ}
`)
	_, _, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_XYZ") {
		t.Fatalf("期望报出未设置的环境变量,实际: %v", err)
	}
}

// 含引号的密钥不应撕裂 YAML 结构。
func TestEnvValueWithSpecialChars(t *testing.T) {
	t.Setenv("TEST_TRICKY", `a"b: c#d`)
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
channels:
  - type: bark
    device_key: ${TEST_TRICKY}
`)
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatalf("含特殊字符的值应被安全处理: %v", err)
	}
	if cfg.Channels[0].DeviceKey != `a"b: c#d` {
		t.Errorf("值未正确保留: %q", cfg.Channels[0].DeviceKey)
	}
}

// 拼错的键必须报错,而不是静默忽略后让人以为设置生效了。
func TestUnknownFieldIsRejected(t *testing.T) {
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
intervall: 60s
`)
	if _, _, err := Load(p); err == nil {
		t.Fatal("拼错的配置键应当报错")
	}
}

func TestValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"未知地区", "regions: [ZZ]\ncategories: [mac]\n", "未知地区"},
		{"未知分类", "regions: [CN]\ncategories: [banana]\n", "未知分类"},
		{"间隔过短", "regions: [CN]\ncategories: [mac]\ninterval: 5s\n", "低于下限"},
		{"渠道类型错误", "regions: [CN]\ncategories: [mac]\nchannels:\n  - type: carrier-pigeon\n", "不支持"},
		{"bark 缺 key", "regions: [CN]\ncategories: [mac]\nchannels:\n  - type: bark\n", "device_key"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("期望错误含 %q,实际: %v", c.wantErr, err)
			}
		})
	}
}

// 间隔短于 CDN 缓存是合法的,但要给出警告。
func TestShortIntervalWarns(t *testing.T) {
	cfg, warnings, err := Load(writeConfig(t, "regions: [CN]\ncategories: [mac]\ninterval: 60s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval.Std().Seconds() != 60 {
		t.Errorf("间隔应保持用户设置的 60s")
	}
	if len(warnings) == 0 {
		t.Error("间隔短于 120s CDN 缓存时应给出警告")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("REFURB_REGIONS", "US, HK")
	t.Setenv("REFURB_INTERVAL", "300s")
	cfg, _, err := Load(writeConfig(t, "regions: [CN]\ncategories: [mac]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Regions) != 2 || cfg.Regions[0] != "US" || cfg.Regions[1] != "HK" {
		t.Errorf("REFURB_REGIONS 未覆盖: %v", cfg.Regions)
	}
	if cfg.Interval.Std().Seconds() != 300 {
		t.Errorf("REFURB_INTERVAL 未覆盖: %v", cfg.Interval.Std())
	}
}

// 被 enabled: false 关掉的渠道不该要求它的密钥,否则示例配置开箱即挂:
// 用户照 README 只 export BARK_KEY,却被未启用的 telegram 渠道拦住。
func TestDisabledChannelSkipsEnvExpansion(t *testing.T) {
	t.Setenv("TEST_ONLY_BARK", "bark-key")
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
channels:
  - type: bark
    enabled: true
    device_key: ${TEST_ONLY_BARK}
  - type: webhook
    name: telegram
    enabled: false
    url: https://api.telegram.org/bot${NEVER_SET_TOKEN}/sendMessage
    body: |
      {"chat_id":{{json "${NEVER_SET_CHAT_ID}"}},"text":{{json .Text}}}
`)
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatalf("未启用的渠道不该要求其环境变量: %v", err)
	}
	if cfg.Channels[0].DeviceKey != "bark-key" {
		t.Errorf("启用渠道的变量仍应展开: %q", cfg.Channels[0].DeviceKey)
	}
}

// 反过来:启用的渠道缺变量必须报错,不能因为上面的放宽而漏掉。
func TestEnabledChannelStillRequiresEnv(t *testing.T) {
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
channels:
  - type: webhook
    enabled: true
    url: https://example.invalid/${NEVER_SET_TOKEN_2}
`)
	_, _, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "NEVER_SET_TOKEN_2") {
		t.Fatalf("启用渠道缺少环境变量时应报错,实际: %v", err)
	}
}

// 环境变量覆盖取值非法时必须报错,而不是默默沿用默认值让人以为覆盖生效了。
func TestInvalidEnvOverrideIsFatal(t *testing.T) {
	for _, c := range []struct{ key, val, want string }{
		{"REFURB_INTERVAL", "5min", "REFURB_INTERVAL"},
		{"REFURB_DIGEST_THRESHOLD", "abc", "REFURB_DIGEST_THRESHOLD"},
		{"REFURB_DIGEST_THRESHOLD", "0", "REFURB_DIGEST_THRESHOLD"},
	} {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			t.Setenv(c.key, c.val)
			_, _, err := Load(writeConfig(t, "regions: [CN]\ncategories: [mac]\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("期望报出 %s 的错误,实际: %v", c.want, err)
			}
		})
	}
}

// 地区与分类的大小写应与规则里的写法一致地宽容,并归一化为规范形式。
func TestRegionCategoryCaseInsensitive(t *testing.T) {
	t.Setenv("REFURB_REGIONS", "cn, us")
	cfg, _, err := Load(writeConfig(t, "regions: [CN]\ncategories: [MAC]\n"))
	if err != nil {
		t.Fatalf("大小写不应导致失败: %v", err)
	}
	if cfg.Regions[0] != "CN" || cfg.Regions[1] != "US" {
		t.Errorf("地区应归一化为大写: %v", cfg.Regions)
	}
	if cfg.Categories[0] != "mac" {
		t.Errorf("分类应归一化为小写: %v", cfg.Categories)
	}
}
