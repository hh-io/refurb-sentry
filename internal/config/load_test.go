package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// 拼错的 lang 必须报错:静默回退默认语言会让人以为设置已生效。
func TestInvalidNotifyLangIsFatal(t *testing.T) {
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
notify:
  lang: jp
`)
	_, _, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "lang") && !strings.Contains(err.Error(), "语言") {
		t.Fatalf("应报出未知语言,实际: %v", err)
	}
}

func TestNotifyLangDefaultsToChinese(t *testing.T) {
	p := writeConfig(t, `
regions: [CN]
categories: [mac]
`)
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Lang != "zh-CN" {
		t.Errorf("未配置时应保持中文以免升级后推送换语言,实际 %q", cfg.Notify.Lang)
	}
}

// 开了 fill_missing_memory 却没有规则约束内存时,补齐会被整个跳过。
// 这是对的,但必须说出来——否则用户会以为它在生效,等到「怎么还是漏机型」时无从下手。
func TestFillMissingMemoryWithoutMemoryRuleWarns(t *testing.T) {
	const body = "regions: [CN]\ncategories: [mac]\n" +
		"http:\n  fill_missing_memory: true\n" +
		"rules:\n  - name: 只按机型\n    dimensions:\n      refurbClearModel: [macbookpro]\n"
	_, warnings, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(warnings, "fill_missing_memory") {
		t.Errorf("应当警告补齐不会生效,实际警告:%v", warnings)
	}

	const withMemory = "regions: [CN]\ncategories: [mac]\n" +
		"http:\n  fill_missing_memory: true\n" +
		"rules:\n  - name: 按内存\n    dimensions:\n      tsMemorySize: [36gb]\n"
	_, warnings, err = Load(writeConfig(t, withMemory))
	if err != nil {
		t.Fatal(err)
	}
	if hasWarning(warnings, "fill_missing_memory") {
		t.Errorf("有规则按内存过滤时不该警告,实际警告:%v", warnings)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// 最小配置是 README 快速开始里让用户直接 cp 的那一份,它靠默认值补全绝大多数字段。
// 哪天某个字段不再有默认值、或是这里写错了一个键名,失败方式是新用户第一次运行
// 就撞上报错,而仓库里没有任何东西会先一步发现。顺带确认它不带 warning:
// 起手第一次运行就看到 WARN 会让人以为自己配错了。
func TestExampleConfigIsMinimalAndRunnable(t *testing.T) {
	t.Setenv("BARK_KEY", "example-device-key")

	cfg, warnings, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("加载最小配置: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("最小配置不应产生 warning: %v", warnings)
	}
	if cfg.Interval.Std() != 120*time.Second || cfg.StatePath != "data/state.json" {
		t.Errorf("默认值未回填: interval=%v state_path=%q", cfg.Interval.Std(), cfg.StatePath)
	}
	if len(cfg.Channels) != 1 || cfg.Channels[0].Type != "bark" {
		t.Errorf("最小配置应当只有一个 bark 渠道,实际 %+v", cfg.Channels)
	}
}

// daily_summary 拼错必须直接报错退出。静默回退等于「配了个日报却永远收不到」,
// 而日报没到正是用户判断系统坏了的信号——那会让人去查一个根本不存在的故障。
func TestInvalidDailySummaryIsFatal(t *testing.T) {
	for _, bad := range []string{"9:00am", "25:00", "09:60", "早上九点"} {
		p := writeConfig(t, "regions: [CN]\ncategories: [mac]\nnotify:\n  daily_summary: \""+bad+"\"\n")
		if _, _, err := Load(p); err == nil {
			t.Errorf("daily_summary=%q 应当报错", bad)
		}
	}

	p := writeConfig(t, "regions: [CN]\ncategories: [mac]\nnotify:\n  daily_summary: \"09:05\"\n")
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatalf("合法的 daily_summary 被拒: %v", err)
	}
	d, err := ParseDailySummary(cfg.Notify.DailySummary)
	if err != nil || d != 9*time.Hour+5*time.Minute {
		t.Errorf("解析结果不对: %v %v", d, err)
	}
}

// daily_summary 写成纯空白时,Validate 认定「未配置」,而 NewRunner 判断的是
// at != "",会拿着那个空格去解析并拒绝启动。同一个值,两处必须得出同一个结论。
func TestBlankDailySummaryIsDisabled(t *testing.T) {
	p := writeConfig(t, "regions: [CN]\ncategories: [mac]\nnotify:\n  daily_summary: \"   \"\n")
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatalf("纯空白的 daily_summary 应视为未配置: %v", err)
	}
	if cfg.Notify.DailySummary != "" {
		t.Errorf("归一化后应为空串,实际 %q——NewRunner 会据此尝试解析并启动失败", cfg.Notify.DailySummary)
	}
}

// 环境变量写错时,错误信息必须指向环境变量。报成「配置文件校验失败:
// notify.daily_summary=...」会让人去翻一份根本没有这个键的配置。
func TestInvalidDailySummaryEnvNamesTheVariable(t *testing.T) {
	t.Setenv("REFURB_DAILY_SUMMARY", "9am")
	p := writeConfig(t, "regions: [CN]\ncategories: [mac]\n")
	_, _, err := Load(p)
	if err == nil {
		t.Fatal("非法的 REFURB_DAILY_SUMMARY 应当报错")
	}
	if !strings.Contains(err.Error(), "REFURB_DAILY_SUMMARY") {
		t.Errorf("错误信息没指向环境变量,会把人引向配置文件: %v", err)
	}
}

// 日报默认开启。不写这一项就该拿到默认时刻——回归时这里最先红:
// 一旦有人在 Validate 里给它加了「空值回填成默认」的分支,
// 下面那个显式关闭的用例会跟着红,两条一起守住这对相反的语义。
func TestDailySummaryDefaultsToEnabled(t *testing.T) {
	p := writeConfig(t, "regions: [CN]\ncategories: [mac]\n")
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.DailySummary != DefaultDailySummary {
		t.Errorf("默认应开启并取 %q,实际 %q", DefaultDailySummary, cfg.Notify.DailySummary)
	}

	// 写了 notify 节点但没提 daily_summary,同样保持默认。
	p = writeConfig(t, "regions: [CN]\ncategories: [mac]\nnotify:\n  lang: en\n")
	cfg, _, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.DailySummary != DefaultDailySummary {
		t.Errorf("只配了 lang 也该保留默认时刻,实际 %q", cfg.Notify.DailySummary)
	}
}

// 显式写成空串是唯一的关闭手段。Validate 若把空值回填成默认时刻,
// 用户就再也关不掉这个功能了——而它是会往手机上推东西的。
func TestDailySummaryExplicitlyDisabled(t *testing.T) {
	p := writeConfig(t, "regions: [CN]\ncategories: [mac]\nnotify:\n  daily_summary: \"\"\n")
	cfg, _, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.DailySummary != "" {
		t.Errorf("显式空串应关闭日报,实际被回填成 %q", cfg.Notify.DailySummary)
	}
}

// 配置文件常常是只读挂载的(compose 就是这么挂的),那时环境变量是唯一的开关。
// 用 Getenv 判空会让「设成空串」与「没设」不可区分,于是关不掉。
func TestDailySummaryEnvCanDisableAndOverride(t *testing.T) {
	p := writeConfig(t, "regions: [CN]\ncategories: [mac]\nnotify:\n  daily_summary: \"09:00\"\n")

	t.Run("显式设空即关闭", func(t *testing.T) {
		t.Setenv("REFURB_DAILY_SUMMARY", "")
		cfg, _, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Notify.DailySummary != "" {
			t.Errorf("REFURB_DAILY_SUMMARY= 应关闭日报,实际 %q", cfg.Notify.DailySummary)
		}
	})

	t.Run("给了时刻即覆盖", func(t *testing.T) {
		t.Setenv("REFURB_DAILY_SUMMARY", "21:30")
		cfg, _, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Notify.DailySummary != "21:30" {
			t.Errorf("环境变量未覆盖配置文件,实际 %q", cfg.Notify.DailySummary)
		}
	})
}
