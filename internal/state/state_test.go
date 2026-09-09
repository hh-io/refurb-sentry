package state

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
)

func prod(pn string, cents int64) apple.Product {
	return apple.Product{
		Region: "CN", Category: "mac", PartNumber: pn,
		Title: "翻新 " + pn, URL: "https://example.invalid/" + pn,
		PriceCents: cents, Currency: "CNY",
	}
}

func kinds(evs []Event) map[EventKind]int {
	m := map[EventKind]int{}
	for _, e := range evs {
		m[e.Kind]++
	}
	return m
}

// 首轮必须静默:否则数百件在售商品会一次性全部作为「上架」推送出去。
func TestColdStartIsSilent(t *testing.T) {
	s := New()
	now := time.Now()

	evs := s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}, now)
	if len(evs) != 0 {
		t.Fatalf("冷启动应无事件,实际 %d 条", len(evs))
	}
	if got := s.CountScope("CN", "mac"); got != 2 {
		t.Fatalf("冷启动仍应建立基线,期望 2 条记录,实际 %d", got)
	}

	// 基线建立后进入正常模式,新货号才应报「上架」
	s.Bootstrapped = true
	evs = s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000), prod("C/A", 300000)}, now)
	if k := kinds(evs); k[EventListed] != 1 || len(evs) != 1 {
		t.Fatalf("期望恰好 1 条上架事件,实际 %v", k)
	}
}

func TestPriceDropAndRise(t *testing.T) {
	s := New()
	s.Bootstrapped = true
	now := time.Now()
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)

	evs := s.Apply("CN", "mac", []apple.Product{prod("A/A", 90000)}, now)
	if len(evs) != 1 || evs[0].Kind != EventPriceDrop {
		t.Fatalf("降价应产生 1 条 price_drop,实际 %+v", evs)
	}
	if evs[0].OldPriceCents != 100000 {
		t.Fatalf("原价应为 100000,实际 %d", evs[0].OldPriceCents)
	}

	// 涨价静默,但要把基准抬上去
	if evs := s.Apply("CN", "mac", []apple.Product{prod("A/A", 95000)}, now); len(evs) != 0 {
		t.Fatalf("涨价不应产生事件,实际 %+v", evs)
	}
	// 回落到 90000 相对新基准 95000 仍是降价
	if evs := s.Apply("CN", "mac", []apple.Product{prod("A/A", 90000)}, now); len(evs) != 1 {
		t.Fatalf("相对新基准的回落应报降价,实际 %+v", evs)
	}
}

func TestDelistedRequiresSustainedEmpty(t *testing.T) {
	s := New()
	s.Bootstrapped = true
	now := time.Now()
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}, now)

	// 前两轮空结果视为上游抖动,不得判下架
	for i := 1; i < emptyStreakThreshold; i++ {
		if evs := s.Apply("CN", "mac", nil, now); len(evs) != 0 {
			t.Fatalf("第 %d 轮空结果不应产生事件,实际 %+v", i, evs)
		}
		if s.CountScope("CN", "mac") != 2 {
			t.Fatalf("第 %d 轮空结果不应删除记录", i)
		}
	}
	// 连续第 3 轮才认定真的清空
	evs := s.Apply("CN", "mac", nil, now)
	if k := kinds(evs); k[EventDelisted] != 2 {
		t.Fatalf("持续为空后应报 2 条下架,实际 %v", k)
	}
	if s.CountScope("CN", "mac") != 0 {
		t.Fatal("下架后记录应被清除")
	}
}

// 单件商品消失(列表非空)应立即判下架,不受空结果阈值影响。
func TestSingleItemDelistedImmediately(t *testing.T) {
	s := New()
	s.Bootstrapped = true
	now := time.Now()
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}, now)

	evs := s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)
	if len(evs) != 1 || evs[0].Kind != EventDelisted || evs[0].Product.PartNumber != "B/A" {
		t.Fatalf("期望 B/A 下架,实际 %+v", evs)
	}
}

// 不同地区/分类互不干扰:抓 US 不能把 CN 的记录判为下架。
func TestScopeIsolation(t *testing.T) {
	s := New()
	s.Bootstrapped = true
	now := time.Now()
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)

	us := prod("X/A", 50000)
	us.Region = "US"
	us.Currency = "USD"
	if evs := s.Apply("US", "mac", []apple.Product{us}, now); len(evs) != 1 || evs[0].Kind != EventListed {
		t.Fatalf("US 新商品应报上架,实际 %+v", evs)
	}
	if s.CountScope("CN", "mac") != 1 {
		t.Fatal("抓取 US 不应影响 CN 的记录")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")

	s := New()
	s.Bootstrapped = true
	now := time.Now().Truncate(time.Second)
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)

	if err := Save(path, s); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !got.Bootstrapped || got.CountScope("CN", "mac") != 1 {
		t.Fatalf("往返后状态不一致: %+v", got)
	}
	if got.Items["CN/mac/A/A"].PriceCents != 100000 {
		t.Fatalf("价格未正确往返: %+v", got.Items)
	}
}

// 状态文件不存在时必须是「未 bootstrap」,这是冷启动静默的前提。
func TestLoadMissingFile(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("文件缺失不应报错: %v", err)
	}
	if s.Bootstrapped {
		t.Fatal("全新状态不应标记为已 bootstrap")
	}
}
