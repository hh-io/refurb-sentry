package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/history"
)

// historyFlags 是 -history 的查询参数。多个取值用逗号分隔,之间是 OR,与规则的写法一致。
type historyFlags struct {
	rule     *string
	region   *string
	category *string
	model    *string
	chip     *string
	memory   *string
	storage  *string
	color    *string
	nano     *string
	title    *string
}

func registerHistoryFlags() historyFlags {
	return historyFlags{
		rule:     flag.String("rule", "", "配合 -history:按配置里同名规则过滤"),
		region:   flag.String("region", "", "配合 -history:地区,如 CN,HK"),
		category: flag.String("category", "", "配合 -history:分类,如 mac,ipad"),
		model:    flag.String("model", "", "配合 -history:机型(refurbClearModel),如 macbookpro"),
		chip:     flag.String("chip", "", "配合 -history:芯片,如 \"M5 Pro,M5 Max\""),
		memory:   flag.String("memory", "", "配合 -history:内存,如 32gb,36gb"),
		storage:  flag.String("storage", "", "配合 -history:存储,如 1tb"),
		color:    flag.String("color", "", "配合 -history:颜色(dimensionColor),如 silver"),
		nano:     flag.String("nano", "", "配合 -history:true 只看纳米纹理,false 只看标准玻璃"),
		title:    flag.String("title", "", "配合 -history:标题正则"),
	}
}

// used 报告是否给了任何查询参数。它们脱离 -history 时毫无作用,
// 静默忽略会让人以为 -once -chip "M5 Pro" 这种写法真的在过滤什么。
func (h historyFlags) used() bool {
	for _, v := range []*string{h.rule, h.region, h.category, h.model, h.chip,
		h.memory, h.storage, h.color, h.nano, h.title} {
		if *v != "" {
			return true
		}
	}
	return false
}

func (h historyFlags) buildFilter(cfg *config.Config) (history.Filter, error) {
	adhoc := filter.Rule{
		Name:       "-history",
		Regions:    splitFlag(*h.region),
		Categories: splitFlag(*h.category),
		Chips:      splitFlag(*h.chip),
		TitleMatch: *h.title,
		Dimensions: map[string][]string{},
	}
	// 维度键名是上游页面的原样拼写,见 -list-dims 的输出。
	for key, v := range map[string]string{
		"refurbClearModel":      *h.model,
		apple.MemoryDimension:   *h.memory,
		apple.CapacityDimension: *h.storage,
		"dimensionColor":        *h.color,
	} {
		if vs := splitFlag(v); len(vs) > 0 {
			adhoc.Dimensions[key] = vs
		}
	}
	set, err := filter.New([]filter.Rule{adhoc})
	if err != nil {
		return history.Filter{}, err
	}
	f := history.Filter{Sets: []*filter.Set{set}}

	if *h.rule != "" {
		var found []filter.Rule
		for _, r := range cfg.Rules {
			if r.Name == *h.rule {
				found = append(found, r)
			}
		}
		if len(found) == 0 {
			names := make([]string, 0, len(cfg.Rules))
			for _, r := range cfg.Rules {
				names = append(names, r.Name)
			}
			return history.Filter{}, fmt.Errorf("配置里没有名为 %q 的规则,现有规则:%q", *h.rule, names)
		}
		named, err := filter.New(found)
		if err != nil {
			return history.Filter{}, err
		}
		f.Sets = append(f.Sets, named)
	}

	if *h.nano != "" {
		v, err := strconv.ParseBool(*h.nano)
		if err != nil {
			return history.Filter{}, fmt.Errorf("-nano=%q 只能是 true 或 false", *h.nano)
		}
		f.NanoTexture = &v
	}
	return f, nil
}

// runHistory 读档案并打印查询结果。它不联网、不读写状态,因此不需要通知渠道。
func runHistory(cfg *config.Config, log *slog.Logger, h historyFlags, out func(string)) error {
	f, err := h.buildFilter(cfg)
	if err != nil {
		return err
	}
	recs, bad, err := history.Read(cfg.History.Path)
	if errors.Is(err, fs.ErrNotExist) {
		if !cfg.History.IsEnabled() {
			return fmt.Errorf("历史档案 %s 不存在,且配置里 history.enabled 为 false", cfg.History.Path)
		}
		return fmt.Errorf("历史档案 %s 还不存在:监控跑完第一轮后才会生成", cfg.History.Path)
	}
	if err != nil {
		return err
	}
	if bad > 0 {
		log.Warn("历史档案中有无法解析的行,已跳过", "path", cfg.History.Path, "lines", bad)
	}
	history.Report(history.Fold(recs), f, time.Now(), out)
	return nil
}

func splitFlag(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
