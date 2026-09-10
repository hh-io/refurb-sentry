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
// 数字与单位之间的空白必须显式列出各个 Unicode 码位,不能只写 `\s`:
// Go 的 `\s` 只等价于 [\t\n\f\r ],**不含不间断空格**。
// 实测法国站写的是 "24\u00a0Go",只写 `\s*` 会一条都匹配不上,
// 而失败方式是静默的——概述里找不到任何容量条目,整个地区的补齐悄悄失效。
// 与 filter 包的 dashNormalizer 同源的坑,那里的注释记着同一段教训。
var capacityRE = regexp.MustCompile(`(?i)\b(\d{1,4})` + unicodeSpaces + `*(GB|TB|Go|To)\b`)

// unicodeSpaces 是各站点混用的空白码位。一律用 \x{...} 转义而非字面字符:
// 这些码位在编辑器与补丁传输中极易被悄悄替换成普通 ASCII 空格,
// 使规则退化成「只认普通空格」而静默失效。
const unicodeSpaces = `[\s\x{00a0}\x{202f}\x{2009}\x{3000}]`

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
			if want != "" && v == want {
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
	// 去掉全部空白,含不间断空格——法国站写的是 "24\u00a0Go"。
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '\u00a0' {
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

// FetchMemory 抓取商品详情页并解析内存。
func (c *Client) FetchMemory(ctx context.Context, region Region, productURL, knownCapacity string) (string, error) {
	body, err := c.get(ctx, region, productURL)
	if err != nil {
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
}

func NewMemoryCache() *MemoryCache {
	return &MemoryCache{m: make(map[string]string)}
}

// Get 返回缓存值。第二个返回值区分「查过且没查到」与「没查过」:
// 前者不该再发请求,否则一台上游永远不给内存的机器会每轮都被重试。
func (c *MemoryCache) Get(partNumber string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[partNumber]
	return v, ok
}

// Put 记录结果。查到但解析失败时存空串,表示「查过了,别再查」。
func (c *MemoryCache) Put(partNumber, memory string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[partNumber] = memory
}

func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
