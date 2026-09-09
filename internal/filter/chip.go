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

// 标题是全项目唯一的非结构化数据源,各地区文案差异极大(实测):
//
//	US  Refurbished 14-inch MacBook Pro Apple M5 Pro chip with 12-Core CPU and 16-Core GPU
//	DE  Refurbished 14" MacBook Pro mit Apple M5 Chip, 10‑Core CPU und 10‑Core GPU
//	CN  翻新 Mac mini Apple M4 芯片 (配备 10 核中央处理器和 10 核图形处理器)
//	HK  翻新產品 14 吋 MacBook Pro Apple M5 晶片 (配備 10 核心 CPU 及 10 核心 GPU)
//	JP  14インチMacBook Pro [整備済製品] 10コアCPUと10コアGPUを搭載したApple M5チップ
//
// 这里刻意不按地区分派正则,而是把所有语言的模式都试一遍:各语言的关键词
// 互不冲突,全量匹配对新增地区和上游改文案都更宽容。
var (
	chipRE = regexp.MustCompile(`Apple\s*([MA]\d+)(?:\s*(Pro|Max|Ultra))?\s*(?:[Cc]hip|芯片|晶片|チップ)`)

	cpuREs = []*regexp.Regexp{
		regexp.MustCompile(`(\d+)[-\s]?Core\s*CPU`), // en / de
		regexp.MustCompile(`(\d+)\s*核中央处理器`),        // zh-Hans
		regexp.MustCompile(`(\d+)\s*核心\s*CPU`),      // zh-Hant
		regexp.MustCompile(`(\d+)\s*コア\s*CPU`),      // ja
	}
	gpuREs = []*regexp.Regexp{
		regexp.MustCompile(`(\d+)[-\s]?Core\s*GPU`),
		regexp.MustCompile(`(\d+)\s*核图形处理器`),
		regexp.MustCompile(`(\d+)\s*核心\s*GPU`),
		regexp.MustCompile(`(\d+)\s*コア\s*GPU`),
	}
)

// dashNormalizer 把各站点混用的 Unicode 连字符与不换行空格折成 ASCII。
// 实测德国站用 U+2011(非断行连字符)、澳洲站用 U+2014,朴素正则会因此漏匹配。
var dashNormalizer = strings.NewReplacer(
	"‐", "-", "‑", "-", "‒", "-", "–", "-",
	"—", "-", "―", "-", "−", "-",
	" ", " ", "　", " ",
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

	if m := chipRE.FindStringSubmatch(t); m != nil {
		s.Chip = m[1]
		if m[2] != "" {
			s.Chip += " " + m[2]
		}
	}
	s.CPUCores = firstInt(t, cpuREs)
	s.GPUCores = firstInt(t, gpuREs)
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
