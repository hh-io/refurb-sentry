package filter

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/hh-io/refurb-sentry/internal/apple"
)

// Rule 是一条过滤规则。各字段之间是 AND;字段内的候选值之间是 OR;
// 留空的字段不施加任何约束。
type Rule struct {
	Name       string   `yaml:"name"`
	Regions    []string `yaml:"regions"`
	Categories []string `yaml:"categories"`

	// Dimensions 直接匹配页面给出的结构化维度。key 集合随分类而变
	// (mac 有 tsMemorySize/dimensionCapacity,watch 有 dimensionCaseSize 等),
	// 故此处是开放的 map 而非固定字段。可用 -list-dims 查询当前有哪些键。
	Dimensions map[string][]string `yaml:"dimensions"`

	// 芯片与核心数只能从标题解析,是全项目最易受上游文案变动影响的条件。
	Chips       []string `yaml:"chips"`
	MinCPUCores int      `yaml:"min_cpu_cores"`
	MinGPUCores int      `yaml:"min_gpu_cores"`

	TitleMatch string  `yaml:"title_match"`
	MinPrice   float64 `yaml:"min_price"`
	MaxPrice   float64 `yaml:"max_price"`

	titleRE      *regexp.Regexp
	minCents     int64
	maxCents     int64
	regionSet    map[string]bool
	categorySet  map[string]bool
	chipSet      map[string]bool
	dimensionSet map[string]map[string]bool
}

// Set 是编译后的规则集合。
type Set struct {
	rules []Rule
}

// New 校验并预编译规则。规则集为空时匹配全部商品——即「监控整个翻新店」。
func New(rules []Rule) (*Set, error) {
	compiled := make([]Rule, 0, len(rules))
	for i, r := range rules {
		if r.Name == "" {
			r.Name = fmt.Sprintf("rule#%d", i+1)
		}
		if r.TitleMatch != "" {
			re, err := regexp.Compile(r.TitleMatch)
			if err != nil {
				return nil, fmt.Errorf("规则 %q 的 title_match 不是合法正则: %w", r.Name, err)
			}
			r.titleRE = re
		}
		for _, code := range r.Regions {
			if _, err := apple.LookupRegion(code); err != nil {
				return nil, fmt.Errorf("规则 %q: %w", r.Name, err)
			}
		}
		for _, c := range r.Categories {
			if !apple.ValidCategory(c) {
				return nil, fmt.Errorf("规则 %q 引用了未知分类 %q,可选:%v", r.Name, c, apple.Categories)
			}
		}
		if r.MinPrice < 0 || r.MaxPrice < 0 {
			return nil, fmt.Errorf("规则 %q: 价格不能为负", r.Name)
		}
		if r.MaxPrice > 0 && r.MinPrice > r.MaxPrice {
			return nil, fmt.Errorf("规则 %q: min_price(%.2f) 大于 max_price(%.2f)", r.Name, r.MinPrice, r.MaxPrice)
		}

		r.minCents = int64(math.Round(r.MinPrice * 100))
		r.maxCents = int64(math.Round(r.MaxPrice * 100))
		r.regionSet = toSet(r.Regions)
		r.categorySet = toSet(r.Categories)
		r.chipSet = toSet(r.Chips)
		r.dimensionSet = make(map[string]map[string]bool, len(r.Dimensions))
		for k, vs := range r.Dimensions {
			if len(vs) == 0 {
				return nil, fmt.Errorf("规则 %q 的维度 %q 没有给出候选值", r.Name, k)
			}
			r.dimensionSet[k] = toSet(vs)
		}
		compiled = append(compiled, r)
	}
	return &Set{rules: compiled}, nil
}

// Match 返回商品是否命中,以及命中的规则名(用于在通知里说明为什么推给你)。
func (s *Set) Match(p apple.Product, spec Spec) (bool, []string) {
	if len(s.rules) == 0 {
		return true, nil
	}
	var hit []string
	for _, r := range s.rules {
		if r.matches(p, spec) {
			hit = append(hit, r.Name)
		}
	}
	return len(hit) > 0, hit
}

// Empty 表示未配置任何规则,即放行全部商品。
func (s *Set) Empty() bool { return len(s.rules) == 0 }

// UsesDimension 报告是否有规则约束了这个维度。
// 没有任何规则用到它时,补齐它不会改变任何推送结果,那些请求就都是白发的。
func (s *Set) UsesDimension(key string) bool {
	for _, r := range s.rules {
		if _, ok := r.dimensionSet[key]; ok {
			return true
		}
	}
	return false
}

// MayMatchWithout 判断在忽略某个维度的前提下,商品是否还有可能命中某条规则。
//
// 用途是决定值不值得为这件商品多发一个请求去补齐该维度:机型、芯片、价格这些
// 列表页就已经给全了,凭它们已能判定不可能命中的商品,再去看详情页也是白看。
//
// 只考察**用到了该维度**的规则:其余规则的结论不依赖它,补与不补结果一样。
func (s *Set) MayMatchWithout(p apple.Product, spec Spec, ignore string) bool {
	for _, r := range s.rules {
		if _, ok := r.dimensionSet[ignore]; !ok {
			continue
		}
		if r.matchesIgnoring(p, spec, ignore) {
			return true
		}
	}
	return false
}

func (r *Rule) matches(p apple.Product, spec Spec) bool {
	return r.matchesIgnoring(p, spec, "")
}

// matchesIgnoring 跳过 ignore 指定的维度键做匹配。ignore 为空串时等同于完整匹配——
// 维度键为空串的规则在 New 里就没有意义,不必额外设哨兵。
func (r *Rule) matchesIgnoring(p apple.Product, spec Spec, ignore string) bool {
	if len(r.regionSet) > 0 && !r.regionSet[strings.ToLower(p.Region)] {
		return false
	}
	if len(r.categorySet) > 0 && !r.categorySet[strings.ToLower(p.Category)] {
		return false
	}
	if r.minCents > 0 && p.PriceCents < r.minCents {
		return false
	}
	if r.maxCents > 0 && p.PriceCents > r.maxCents {
		return false
	}
	for key, allowed := range r.dimensionSet {
		if key == ignore {
			continue
		}
		// 商品缺少该维度时视为不匹配:规则明确要求了这个条件。
		if !allowed[strings.ToLower(p.Dimensions[key])] {
			return false
		}
	}
	if len(r.chipSet) > 0 && !r.chipSet[strings.ToLower(spec.Chip)] {
		return false
	}
	// 核心数未能解析(0)时不满足任何下限要求,避免把未知当作符合条件。
	if r.MinCPUCores > 0 && spec.CPUCores < r.MinCPUCores {
		return false
	}
	if r.MinGPUCores > 0 && spec.GPUCores < r.MinGPUCores {
		return false
	}
	if r.titleRE != nil && !r.titleRE.MatchString(NormalizeTitle(p.Title)) {
		return false
	}
	return true
}

// toSet 统一转小写:页面维度值本身是小写(如 "24gb"、"macbookpro"),
// 但配置是人手写的,大小写不该成为匹配失败的原因。
func toSet(vs []string) map[string]bool {
	if len(vs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(vs))
	for _, v := range vs {
		m[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return m
}
