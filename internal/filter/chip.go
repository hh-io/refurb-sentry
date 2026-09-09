package filter

import (
	"regexp"
	"strconv"
	"strings"
)

// Spec 是从商品标题里抽出的规格。内存与存储不在此处解析——
// 它们已由页面的 dimensions 结构化提供,比解析文案可靠得多。
type Spec struct {
	Chip     string // 归一化芯片名,如 "M5 Pro"、"A18 Pro";空串表示未能识别
	CPUCores int    // 0 表示未能识别
	GPUCores int
}

// 标题是全项目唯一的非结构化数据源,各语言站点的语序差异极大(均为实测):
//
//	US  Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU
//	NL  Refurbished 13-inch MacBook Air Apple M4-chip met 10-core CPU en 10-core GPU
//	FR  Mac mini reconditionné avec puce Apple M4, CPU 10 cœurs, GPU 10 cœurs
//	IT  MacBook Air 13" ricondizionato con chip Apple M2, CPU 8-core e GPU 10-core
//	ES  iMac reacondicionado de 24 pulgadas con chip M4 de Apple, CPU de 8 núcleos
//	CN  翻新 Mac mini Apple M4 芯片 (配备 10 核中央处理器和 10 核图形处理器)
//	HK  翻新產品 14 吋 MacBook Pro Apple M5 晶片 (配備 10 核心 CPU 及 10 核心 GPU)
//	JP  14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ
//	KR  리퍼비쉬 MacBook Pro 14 Apple M5 Pro 칩 모델(15코어 CPU 및 16코어 GPU)
//
// 关键设计:芯片「指示词」与芯片「型号」解耦,不要求二者相邻。
// 早期版本要求 Apple 与 chip 紧挨着,在法/意/西/荷/韩五个站点识别率为 0——
// 那些语言里 puce/chip 在型号之前,或用连字符连成 M4-chip。
var (
	// chipHintRE 确认标题确实在描述一枚芯片,避免把无关的字母数字组合当成型号。
	chipHintRE = regexp.MustCompile(`Apple|[Cc]hip|puce|晶片|芯片|チップ|칩`)
	// chipRE 只认 M/A 系列型号本身,分隔符兼容空格与各式连字符。
	chipRE = regexp.MustCompile(`\b([MA]\d{1,2})(?:[\s-]+(Pro|Max|Ultra))?\b`)

	// coreUnit 覆盖各语言的「核心」量词。注意「核心」必须排在「核」之前:
	// Go 的正则交替是最左优先,顺序颠倒会让「核」先匹配而漏掉后面的「心」。
	coreUnit = `(?:[Cc]ores?|コア|코어|核心|核|c(?:œ|oe)urs?|n[úu]cleos?|nuclei|[Kk]ernen)`

	// 两种语序都要认:数字在量词之前(英/中/日/韩/荷),或在处理器名之后(法/意/西)。
	cpuREs = []*regexp.Regexp{
		regexp.MustCompile(`(\d+)\s*-?\s*` + coreUnit + `[\s-]*(?:CPU|中央处理器|中央處理器)`),
		regexp.MustCompile(`CPU\s*(?:de\s*|van\s*)?(\d+)\s*-?\s*` + coreUnit),
	}
	gpuREs = []*regexp.Regexp{
		regexp.MustCompile(`(\d+)\s*-?\s*` + coreUnit + `[\s-]*(?:GPU|图形处理器|圖形處理器)`),
		regexp.MustCompile(`GPU\s*(?:de\s*|van\s*)?(\d+)\s*-?\s*` + coreUnit),
	}
)

// dashNormalizer 把各站点混用的 Unicode 连字符与空格折成 ASCII。
//
// 一律使用 \u 转义而非字面字符:这些码位在编辑器、终端和补丁传输中极易被
// 悄悄替换成普通 ASCII,使替换规则退化成「空格换空格」而静默失效。
// 实测踩过一次——U+00A0 被写成 U+0020,导致西/意/法站的 "A18\u00a0Pro"
// 被截成 "A18",配了 chips: [M4 Pro] 的规则会莫名漏推。
var dashNormalizer = strings.NewReplacer(
	"\u2010", "-", // 连字符
	"\u2011", "-", // 非断行连字符(德国站)
	"\u2012", "-", // 数字连字符
	"\u2013", "-", // en dash
	"\u2014", "-", // em dash(澳洲站)
	"\u2015", "-", // horizontal bar
	"\u2212", "-", // 减号
	"\u00a0", " ", // 不间断空格(西/意/法站用它分隔芯片型号与 Pro/Max)
	"\u202f", " ", // 窄不间断空格
	"\u2009", " ", // thin space
	"\u3000", " ", // 全角空格
)

// NormalizeTitle 供解析与正则规则共用,保证用户写的 title_match 面对的是同一套字符。
func NormalizeTitle(s string) string {
	return dashNormalizer.Replace(s)
}

// ParseSpec 尽最大努力解析标题。任何一项识别失败都只是留空,绝不丢弃商品——
// Apple 随时可能调整文案,解析失败不该让一台机器从监控里消失。
func ParseSpec(title string) Spec {
	t := NormalizeTitle(title)
	var s Spec

	s.CPUCores = firstInt(t, cpuREs)
	s.GPUCores = firstInt(t, gpuREs)

	// 标题里已经出现 CPU/GPU 核心数,本身就足以说明它在描述一台带芯片的机器,
	// 此时不必再要求出现 Apple/chip 之类的指示词。
	// 西班牙站的 16 吋 MacBook Pro 正是这种写法——
	// "MacBook Pro reacondicionado de 16 pulgadas con M4 Pro, CPU de 14 núcleos y GPU de 20 núcleos"
	// 通篇没有 Apple 也没有 chip,只靠指示词会漏掉整整一档机型。
	if chipHintRE.MatchString(t) || s.CPUCores > 0 || s.GPUCores > 0 {
		if m := chipRE.FindStringSubmatch(t); m != nil {
			s.Chip = m[1]
			if m[2] != "" {
				s.Chip += " " + m[2]
			}
		}
	}
	return s
}

func firstInt(t string, res []*regexp.Regexp) int {
	for _, re := range res {
		if m := re.FindStringSubmatch(t); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return n
			}
		}
	}
	return 0
}
