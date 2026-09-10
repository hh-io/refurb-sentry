package notify

import (
	"fmt"
	"strings"

	"github.com/hh-io/refurb-sentry/internal/state"
)

// Lang 是推送文案的语言。只覆盖推给用户的通知内容:
// 日志与错误信息始终是中文,那是给运维和开发者看的,翻译它们没有收益。
type Lang string

const (
	LangZH Lang = "zh-CN"
	LangEN Lang = "en"
)

// DefaultLang 保持中文,以免升级后老用户手机上的推送突然换成英文。
const DefaultLang = LangZH

// ParseLang 校验语言代码。未知取值直接报错而不是回退默认值——
// 拼错的 lang 若静默失效,用户会以为设置已经生效。
func ParseLang(s string) (Lang, error) {
	switch l := Lang(strings.TrimSpace(s)); l {
	case "":
		return DefaultLang, nil
	case LangZH, LangEN:
		return l, nil
	default:
		return "", fmt.Errorf("未知的通知语言 %q,可选:%s、%s", s, LangZH, LangEN)
	}
}

// kindWords 是一种事件在某语言下的各种词形。
// label 用于标题,countOne/countMany 用于摘要里的计数——
// 英文的 "1 listing" 与 "6 listings" 词形不同,中文三者一致。
type kindWords struct {
	label     string
	countOne  string
	countMany string
}

// phrases 是一种语言的全部用户可见文案。标点也随语言走:
// 中文用全角括号与顿号,英文用半角。只换词不换标点,英文读起来会很别扭。
type phrases struct {
	kinds map[state.EventKind]kindWords

	eventTitle  string // 事件 · 地区 分类
	priceChange string // 旧价 → 新价(降幅、百分比)
	rulesOne    string
	rulesMany   string
	ruleSep     string

	digestTitle string
	// digestCount 用显式参数索引:中文是「上架 6」,英文是「6 listings」,词序相反。
	digestCount string
	digestLine  string
	digestMore  string

	consoleHeader string
}

var langPhrases = map[Lang]phrases{
	LangZH: {
		kinds: map[state.EventKind]kindWords{
			state.EventListed:    {label: "上架", countOne: "上架", countMany: "上架"},
			state.EventPriceDrop: {label: "降价", countOne: "降价", countMany: "降价"},
			state.EventDelisted:  {label: "下架", countOne: "下架", countMany: "下架"},
		},
		eventTitle:    "%s · %s %s",
		priceChange:   "%s → %s(降 %s,%.1f%%)",
		rulesOne:      "\n命中规则:%s",
		rulesMany:     "\n命中规则:%s",
		ruleSep:       "、",
		digestTitle:   "翻新监控 · %s",
		digestCount:   "%[1]s %[2]d",
		digestLine:    "[%s] %s %s",
		digestMore:    "…… 另有 %d 条",
		consoleHeader: "\n──────── 通知 ────────\n标题: %s\n%s\n链接: %s\n分组: %s\n",
	},
	LangEN: {
		kinds: map[state.EventKind]kindWords{
			state.EventListed:    {label: "Listed", countOne: "listing", countMany: "listings"},
			state.EventPriceDrop: {label: "Price drop", countOne: "price drop", countMany: "price drops"},
			state.EventDelisted:  {label: "Delisted", countOne: "delisting", countMany: "delistings"},
		},
		eventTitle:    "%s · %s %s",
		priceChange:   "%s → %s (down %s, %.1f%%)",
		rulesOne:      "\nMatched rule: %s",
		rulesMany:     "\nMatched rules: %s",
		ruleSep:       ", ",
		digestTitle:   "Refurb watch · %s",
		digestCount:   "%[2]d %[1]s",
		digestLine:    "[%s] %s %s",
		digestMore:    "…… and %d more",
		consoleHeader: "\n──────── notification ────────\nTitle: %s\n%s\nURL: %s\nGroup: %s\n",
	},
}

func phrasesFor(l Lang) phrases {
	if p, ok := langPhrases[l]; ok {
		return p
	}
	return langPhrases[DefaultLang]
}

// label 遇到未知事件类型时回退到其原始标识,而不是留空。
func (p phrases) label(k state.EventKind) string {
	if w, ok := p.kinds[k]; ok {
		return w.label
	}
	return string(k)
}

func (p phrases) count(k state.EventKind, n int) string {
	w, ok := p.kinds[k]
	if !ok {
		return fmt.Sprintf("%s %d", k, n)
	}
	word := w.countMany
	if n == 1 {
		word = w.countOne
	}
	return fmt.Sprintf(p.digestCount, word, n)
}
