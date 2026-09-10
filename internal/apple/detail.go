package apple

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"
)

// 三种失败必须分开报告。它们的排查方向完全不同:缺变量意味着上游改版或这类商品
// 没有概述栏,候选不唯一意味着解析判据需要调整。混成一句会让人查错方向——
// 实测法国站因不间断空格导致候选为 0,报的却是「未找到 Overview」,白查了一轮页面结构。
var (
	// ErrNoOverview:页面里没有 pageLevelData.Overview。与 ErrNoBootstrap 同理,
	// 调用方必须把它当作「这次没读到」而不是「这台机器没有内存」。
	ErrNoOverview = errors.New("详情页中未找到 pageLevelData.Overview")
	// ErrOverviewDecode:找到了变量但 JSON 解不开,通常是上游改了结构。
	ErrOverviewDecode = errors.New("概述数据无法解码")
	// ErrMemoryAmbiguous:排除存储后剩下的容量条目不是恰好一条,无法断定哪个是内存。
	ErrMemoryAmbiguous = errors.New("概述里的容量条目无法唯一确定内存")
	// ErrProductGone:详情页 404。翻新品常在抓列表与抓详情之间就被买走,
	// 这与「该地区不提供此分类」是完全不同的事,不能共用 ErrCategoryNotAvailable——
	// 那条错误信息会把运维引向 categories 配置。
	ErrProductGone = errors.New("详情页返回 404,商品可能已下架")
)

const overviewVar = "window.pageLevelData.Overview"

// MemoryDimension 是列表页给出内存时所用的维度键。补齐逻辑把解析结果写回同一个键,
// 这样过滤规则不必关心这台机器的内存是列表页给的还是详情页补的。
const MemoryDimension = "tsMemorySize"

// CapacityDimension 是存储容量维度键。它同时是内存解析的锚点,见 ParseOverviewMemory。
const CapacityDimension = "dimensionCapacity"

// overviewDoc 只声明用得到的字段。详情页的这棵树比列表页深得多,
// 但同样遵循「字段越少越不容易被上游改版打断」的原则。
type overviewDoc struct {
	Tiles struct {
		Groups struct {
			Items []struct {
				Value struct {
					// 上游会给某些分组塞 null,故用指针而非值。
					Selector *struct {
						AttributeList struct {
							Items []struct {
								Value string `json:"value"`
							} `json:"items"`
						} `json:"attributeList"`
					} `json:"mutiValueAttributeSelector"`
				} `json:"value"`
			} `json:"items"`
		} `json:"groups"`
	} `json:"tiles"`
}

// capacityRE 匹配概述条目里的容量。刻意不认「内存」「memory」这类指示词——
// 那需要为 19 个地区维护一张多语言表,而单位写法(GB/TB,法语 Go/To)只有寥寥几种。
// 区分内存与存储改用锚点法,见 ParseOverviewMemory。
//
// 数字与单位之间的空白不能只写 `\s`:Go 的 `\s` 只等价于 [\t\n\f\r ],
// **不含不间断空格**。实测法国站写的是 "24\u00a0Go",只写 `\s*` 会一条都匹配不上,
// 而失败方式是静默的——概述里找不到任何容量条目,整个地区的补齐悄悄失效。
// 与 filter 包的 dashNormalizer 同源的坑,那里的注释记着同一段教训。
// 见 unicodeSpaces。
var capacityRE = regexp.MustCompile(`(?i)\b(\d{1,4})` + unicodeSpaces + `*(GB|TB|Go|To)\b`)

// unicodeSpaces 覆盖各站点混用的空白。刻意用 `\p{Zs}`(Unicode 空格分隔符整类)
// 而不是手抄一张码位表:实测见过 U+00A0(法)、U+202F、U+2009、U+3000,
// 但 Zs 里还有 U+2002~U+2008、U+205F 等十来个码位,哪天某个站点换用其中之一,
// 手抄表就会漏掉,而失败方式是静默的——那个地区一条容量条目都匹配不上、补齐悄悄失效。
// `\s` 仍要保留:它含 \t\n\f\r,这些不属于 Zs。
const unicodeSpaces = `[\s\p{Zs}]`

// htmlTagRE 去掉概述条目里的 <b> 之类标签,上游在芯片名等条目上会加粗。
var htmlTagRE = regexp.MustCompile(`<[^>]+>`)

// ParseOverviewMemory 从详情页 HTML 解析内存,返回形如 "36gb" 的值,
// 与列表页 tsMemorySize 的取值格式一致。
//
// 概述栏里恰好有两个带容量单位的条目——内存与存储(实测五款机型均如此):
//
//	36GB 统一内存
//	2TB 固态硬盘²
//
// 而存储容量列表页已经给了。于是用它作锚点排除掉存储条目,剩下的唯一一条就是内存。
// 这个判据不依赖语言,不必为每个地区维护「统一内存 / unified memory / mémoire unifiée」
// 这样一张必然滞后于上游文案的表。
//
// knownCapacity 传列表页的 dimensionCapacity(如 "2tb")。剩余条目不是恰好一条时
// 报错而不是挑一个:猜错会让规则静默匹配到错误的机器,比读不到更糟。
//
// knownCapacity 为空时直接放弃:没有锚点就排除不掉存储条目,而只有一个容量条目的
// 页面会让那唯一一条被当成内存返回。实测 watch 详情页正是这样——28 件全部「解析出内存」,
// 那其实是存储容量。上层的分类闸(scopeHasMemory)只挡得住整类没有内存维度的分类,
// 挡不住同一分类里个别既无 tsMemorySize 也无 dimensionCapacity 的商品。
func ParseOverviewMemory(html []byte, knownCapacity string) (string, error) {
	i := bytes.Index(html, []byte(overviewVar))
	if i < 0 {
		return "", ErrNoOverview
	}
	j := bytes.IndexByte(html[i:], '{')
	if j < 0 {
		return "", ErrNoOverview
	}

	var doc overviewDoc
	// 与 ParseGrid 同样用流式解码:概述条目里出现 "</script>" 会截断正则,
	// 而 Decoder 在对象闭合处自然停止。
	if err := json.NewDecoder(bytes.NewReader(html[i+j:])).Decode(&doc); err != nil {
		return "", ErrOverviewDecode
	}

	want := normalizeCapacity(knownCapacity)
	if want == "" {
		return "", fmt.Errorf("%w: 列表页未给出 %s,缺少排除存储条目的锚点", ErrMemoryAmbiguous, CapacityDimension)
	}

	var candidates []string
	for _, g := range doc.Tiles.Groups.Items {
		if g.Value.Selector == nil {
			continue
		}
		for _, it := range g.Value.Selector.AttributeList.Items {
			text := htmlTagRE.ReplaceAllString(it.Value, "")
			// "460GB/s 内存带宽" 这类带宽条目也含容量单位,靠斜杠排除。
			if strings.Contains(text, "/") {
				continue
			}
			m := capacityRE.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			v := normalizeCapacity(m[1] + m[2])
			if v == want {
				continue // 这是存储条目
			}
			candidates = append(candidates, v)
		}
	}

	if len(candidates) != 1 {
		return "", fmt.Errorf("%w: 候选 %v(已知存储 %q)", ErrMemoryAmbiguous, candidates, knownCapacity)
	}
	return candidates[0], nil
}

// normalizeCapacity 把 "2 TB"、"2To"、"2tb" 统一成 "2tb",
// 以便与列表页维度值(小写无空格)直接比较。
func normalizeCapacity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// 去掉全部空白。unicode.IsSpace 已覆盖 U+00A0 与整个 Zs 类(法国站写的是 "24\u00a0Go"),
	// 不必再单列码位。
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	// 法语站用 Go/To 表示 GB/TB。
	switch {
	case strings.HasSuffix(s, "go"):
		s = strings.TrimSuffix(s, "go") + "gb"
	case strings.HasSuffix(s, "to"):
		s = strings.TrimSuffix(s, "to") + "tb"
	}
	return s
}

// IsPermanentMemoryFailure 区分「这台机器的内存永远读不出来」与「这次没读到」。
//
// 前者是页面本身的事实(没有概述栏、结构变了、容量条目无法唯一确定内存),
// 重试多少轮都是同一个结果,值得记进缓存不再查;404 同理——商品已经下架,
// 它下一轮本就会从列表里消失。
// 后者是超时、5xx、连接重置这类传输故障,**绝不能**记进缓存:
// 一次抖动就让该货号在整个进程生命周期里不再被补齐,
// 按内存过滤的规则会从此静默漏掉它,而这正是补齐功能要消除的问题。
func IsPermanentMemoryFailure(err error) bool {
	return errors.Is(err, ErrNoOverview) ||
		errors.Is(err, ErrOverviewDecode) ||
		errors.Is(err, ErrMemoryAmbiguous) ||
		errors.Is(err, ErrProductGone)
}

// FetchMemory 抓取商品详情页并解析内存。
func (c *Client) FetchMemory(ctx context.Context, region Region, productURL, knownCapacity string) (string, error) {
	body, err := c.get(ctx, region, productURL)
	if err != nil {
		// c.get 的 404 分支是为列表页写的,一律译成 ErrCategoryNotAvailable。
		// 详情页 404 的含义不同,换一条错误免得把人引向 categories 配置。
		if errors.Is(err, ErrCategoryNotAvailable) {
			return "", fmt.Errorf("%s: %w", productURL, ErrProductGone)
		}
		return "", err
	}
	mem, err := ParseOverviewMemory(body, knownCapacity)
	if err != nil {
		return "", fmt.Errorf("%s: %w", productURL, err)
	}
	return mem, nil
}

// MemoryCache 缓存 partNumber 到内存的映射。同一货号对应固定配置,
// 查过一次就不必再查——否则常驻进程每轮都要为同一批机器重抓几十个详情页。
//
// 只存在于进程生命周期内:重启后重新查一遍是可接受的一次性开销,
// 换取状态文件格式不必为此变更。
type MemoryCache struct {
	mu sync.Mutex
	m  map[string]string
	// fails 记录同一货号连续遇到多少轮可重试的失败,见 maxMemoryAttempts。
	fails map[string]int
}

// maxMemoryAttempts 是同一货号在可重试故障下最多尝试多少轮。
//
// 与 runner 的 maxRollbacks 同一个道理:重试假设故障是暂时的,但若上游持续 5xx
// 或把详情页整个封了,每轮都为这批商品重发几十个请求既补不到内存,
// 又白白加重上游负担。攒够这么多轮就当永久失败记进缓存,并由调用方在日志里说清楚。
const maxMemoryAttempts = 3

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{m: make(map[string]string), fails: make(map[string]int)}
}

// Get 返回缓存值。第二个返回值区分「查过且没查到」与「没查过」:
// 前者不该再发请求,否则一台上游永远不给内存的机器会每轮都被重试。
func (c *MemoryCache) Get(partNumber string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[partNumber]
	return v, ok
}

// Put 记录结果。解析失败时存空串,表示「查过了,别再查」。
// 只有**永久性**失败才该走这条路,见 IsPermanentMemoryFailure:
// 把一次超时或 5xx 记成空串,会让这台机器在整个进程生命周期里再也不被补齐,
// 而那正是本功能要消除的「规则静默漏掉一档机型」。
func (c *MemoryCache) Put(partNumber, memory string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[partNumber] = memory
	delete(c.fails, partNumber)
}

// Fail 记一次可重试的失败,返回该货号是否已用尽重试机会。
// 用尽后写入空串,后续轮次直接命中缓存,不再发请求。
func (c *MemoryCache) Fail(partNumber string) (exhausted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fails[partNumber]++
	if c.fails[partNumber] < maxMemoryAttempts {
		return false
	}
	c.m[partNumber] = ""
	delete(c.fails, partNumber)
	return true
}

func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
