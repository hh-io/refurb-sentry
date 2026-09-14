package history

import (
	"strings"
	"testing"
	"time"

	"github.com/hh-io/refurb-sentry/internal/filter"
)

func sale(pn, title string, cents int64, dims map[string]string, listed, delisted time.Time) Sale {
	return Sale{
		Region: "CN", Category: "mac", PartNumber: pn, Title: title, Currency: "CNY",
		Dimensions: dims, ListedAt: listed, DelistedAt: delisted,
		Prices: []PricePoint{{At: listed, Cents: cents}},
	}
}

func mustSet(t *testing.T, rules ...filter.Rule) *filter.Set {
	t.Helper()
	s, err := filter.New(rules)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func report(sales []Sale, f Filter, now time.Time) string {
	var b strings.Builder
	Report(sales, f, now, func(s string) {
		b.WriteString(s)
		b.WriteByte('\n')
	})
	return b.String()
}

const (
	proNano = "翻新 14 英寸 MacBook Pro Apple M5 Pro 芯片 (配备 15 核中央处理器和 16 核图形处理器) 和纳米纹理显示屏 - 银色"
	proStd  = "翻新 14 英寸 MacBook Pro Apple M5 Pro 芯片 (配备 15 核中央处理器和 16 核图形处理器) - 银色"
	air     = "翻新 13 英寸 MacBook Air Apple M4 芯片 (配备 10 核中央处理器和 10 核图形处理器) - 银色"
)

func TestReportGroupsSameConfig(t *testing.T) {
	d := func(day int) time.Time { return t0.AddDate(0, 0, day) }
	mem32 := map[string]string{"tsMemorySize": "32gb", "dimensionCapacity": "1tb"}
	sales := []Sale{
		sale("P1", proNano, 1349900, mem32, d(0), d(0).Add(105*time.Minute)),
		sale("P2", proNano, 1299900, mem32, d(30), time.Time{}),
		sale("P3", proStd, 1249900, mem32, d(5), d(6)),
		sale("A1", air, 700000, map[string]string{"tsMemorySize": "16gb", "dimensionCapacity": "512gb"}, d(1), d(2)),
	}
	f := Filter{Sets: []*filter.Set{mustSet(t, filter.Rule{
		Chips: []string{"M5 Pro"}, Dimensions: map[string][]string{"tsMemorySize": {"32gb"}},
	})}}
	out := report(sales, f, d(31))

	if strings.Contains(out, "MacBook Air") {
		t.Errorf("不匹配的商品不应出现:\n%s", out)
	}
	// 同配置两次售卖归为一组,纳米纹理与标准玻璃是两个配置。
	if !strings.Contains(out, "纳米纹理 · 共 2 次 · RMB 12,999 ~ RMB 13,499") {
		t.Errorf("纳米纹理那组应汇总 2 次售卖与价格区间:\n%s", out)
	}
	if !strings.Contains(out, "共 2 个配置、3 次售卖") {
		t.Errorf("汇总行不对:\n%s", out)
	}
	if !strings.Contains(out, "在架 1小时45分") || !strings.Contains(out, "在售") {
		t.Errorf("应给出在架时长,并标出仍在售的那次:\n%s", out)
	}
}

// -rule 与命令行条件是 AND;纳米纹理由查询单独判断。
func TestReportFilterCombination(t *testing.T) {
	mem := map[string]string{"tsMemorySize": "32gb"}
	sales := []Sale{
		sale("P1", proNano, 1, mem, t0, time.Time{}),
		sale("P2", proStd, 1, mem, t0, time.Time{}),
	}
	no := false
	f := Filter{
		Sets: []*filter.Set{
			mustSet(t, filter.Rule{Chips: []string{"M5 Pro"}}),
			mustSet(t, filter.Rule{Name: "配置里的规则", Dimensions: map[string][]string{"tsMemorySize": {"32gb"}}}),
		},
		NanoTexture: &no,
	}
	out := report(sales, f, t0)
	if strings.Contains(out, "P1") || !strings.Contains(out, "P2") {
		t.Errorf("-nano false 应只留下标准玻璃:\n%s", out)
	}

	f.Sets = append(f.Sets, mustSet(t, filter.Rule{Dimensions: map[string][]string{"tsMemorySize": {"64gb"}}}))
	if out := report(sales, f, t0); !strings.Contains(out, "没有匹配的历史记录") {
		t.Errorf("多组条件须同时满足:\n%s", out)
	}
}

// 按内存查时,缺内存维度的记录不能悄无声息地消失——那正是「内存 32GB 以上」类规则
// 静默漏掉整档机型的同一个坑,查询里至少要说出来。
func TestReportCountsUnknownMemory(t *testing.T) {
	sales := []Sale{
		sale("P1", proNano, 1, map[string]string{"tsMemorySize": "32gb"}, t0, time.Time{}),
		sale("P2", proNano, 1, map[string]string{"dimensionCapacity": "1tb"}, t0, time.Time{}),
		// 芯片就不对,缺不缺内存都不会匹配,不该算进去。
		sale("A1", air, 1, nil, t0, time.Time{}),
	}
	f := Filter{Sets: []*filter.Set{mustSet(t, filter.Rule{
		Chips: []string{"M5 Pro"}, Dimensions: map[string][]string{"tsMemorySize": {"32gb"}},
	})}}
	if out := report(sales, f, t0); !strings.Contains(out, "另有 1 次售卖缺少内存维度") {
		t.Errorf("应提示 1 次售卖因缺内存无法判断:\n%s", out)
	}
}

func TestReportApproximateDelisting(t *testing.T) {
	s := sale("P1", proNano, 1, nil, t0, t0.Add(3*time.Hour))
	s.DelistedApprox = true
	out := report([]Sale{s}, Filter{}, t0.Add(100*time.Hour))
	if !strings.Contains(out, "→≤") || !strings.Contains(out, "在架 ≤3小时0分") {
		t.Errorf("近似下架应标 ≤,在架时长为上限:\n%s", out)
	}
	s.ListedApprox = true
	if out := report([]Sale{s}, Filter{}, t0); !strings.Contains(out, "在架 未知") {
		t.Errorf("两头都近似时在架时长应为未知:\n%s", out)
	}
}
