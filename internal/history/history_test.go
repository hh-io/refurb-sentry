package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/state"
)

var t0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

func prod(pn string, cents int64, dims map[string]string) apple.Product {
	return apple.Product{
		Region: "CN", Category: "mac", PartNumber: pn,
		Title: "翻新 14 英寸 MacBook Pro Apple M5 Pro 芯片 (配备 15 核中央处理器和 16 核图形处理器) 和纳米纹理显示屏 - 银色",
		URL:   "https://www.apple.com.cn/shop/product/" + pn + "?fnode=a&b", PriceCents: cents, Currency: "CNY",
		Dimensions: dims,
	}
}

// apply 模拟 runner 的一轮:先留快照再 Apply,返回快照。
func apply(st *state.State, now time.Time, ps ...apple.Product) *state.State {
	before := st.Clone()
	st.Apply("CN", "mac", ps, now)
	return before
}

func kinds(rs []Record) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.PartNumber+":"+string(r.Kind))
	}
	return out
}

func TestDiff(t *testing.T) {
	st := state.New()

	// 首轮建立基线:Apply 丢弃了事件,档案仍须记下这些在架商品。
	before := apply(st, t0, prod("A", 100, nil), prod("B", 200, nil))
	got := kinds(Diff(before, st, t0))
	if strings.Join(got, ",") != "A:baseline,B:baseline" {
		t.Fatalf("首轮应记 baseline,实际 %v", got)
	}

	// 涨价(Apply 不为它产生事件)、上架、下架都要记。
	t1 := t0.Add(time.Hour)
	before = apply(st, t1, prod("A", 150, nil), prod("C", 300, nil))
	rs := Diff(before, st, t1)
	if strings.Join(kinds(rs), ",") != "A:price,B:delisted,C:listed" {
		t.Fatalf("实际 %v", kinds(rs))
	}
	if rs[0].OldPriceCents != 100 || rs[0].PriceCents != 150 {
		t.Fatalf("调价记录应带新旧价格,实际 %+v", rs[0])
	}
	if !rs[2].Spec.NanoTexture || rs[2].Spec.Chip != "M5 Pro" {
		t.Fatalf("应附带从标题解析的规格,实际 %+v", rs[2].Spec)
	}

	// 空结果未攒够阈值时 Apply 不删条目,档案也绝不能记出下架。
	before = apply(st, t1.Add(time.Hour))
	if rs := Diff(before, st, t1.Add(time.Hour)); len(rs) != 0 {
		t.Fatalf("单次空结果不应产生任何档案记录,实际 %v", kinds(rs))
	}
}

func TestSeedUsesFirstSeen(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A", 100, nil)}, t0)
	rs := Seed(st, t0.Add(48*time.Hour))
	if len(rs) != 1 || rs[0].Kind != KindBaseline || !rs[0].FirstSeen.Equal(t0) {
		t.Fatalf("打底记录应为 baseline 且保留状态库的首次发现时刻,实际 %+v", rs)
	}
}

func TestAppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "history.jsonl")
	if need, err := NeedsSeed(path); err != nil || !need {
		t.Fatalf("文件不存在时应需要打底: %v %v", need, err)
	}

	r := newRecord(KindListed, state.Entry{Region: "CN", Category: "mac", PartNumber: "A",
		Title: "t", URL: "https://x/?a=1&b=2", PriceCents: 100, Currency: "CNY", FirstSeen: t0}, t0)
	if err := Append(path, []Record{r}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	// 写成 "\\u" 拼接而非字面转义串:后者在补丁传输中会被还原成 &,断言就恒真了。
	if strings.Contains(string(raw), "\\u"+"0026") || !strings.Contains(string(raw), "a=1&b=2") {
		t.Fatal("链接里的 & 不应被转义")
	}

	// 模拟断电留下的残行:下一批的第一条不能被它连累。
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"ts":"2026-09-01T10:00:00Z","kind":"lis`)
	f.Close()
	r2 := r
	r2.PartNumber = "B"
	if err := Append(path, []Record{r2}); err != nil {
		t.Fatal(err)
	}

	recs, bad, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 1 || len(recs) != 2 || recs[1].PartNumber != "B" {
		t.Fatalf("应跳过 1 条残行并读出 2 条记录,实际 bad=%d recs=%v", bad, kinds(recs))
	}
	if need, _ := NeedsSeed(path); need {
		t.Fatal("已有内容的档案不应再打底")
	}
}

func TestNeedsSeedOnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if need, _ := NeedsSeed(path); !need {
		t.Fatal("空文件(创建后、写入前被 kill)应视为需要打底")
	}
}

func rec(kind Kind, pn string, at time.Time, cents int64, dims map[string]string) Record {
	return Record{Time: at, Kind: kind, Region: "CN", Category: "mac", PartNumber: pn,
		PriceCents: cents, Currency: "CNY", Dimensions: dims, FirstSeen: at}
}

func TestFold(t *testing.T) {
	h := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }
	price := rec(KindPrice, "A", h(2), 90, nil)
	price.OldPriceCents = 100
	recs := []Record{
		rec(KindListed, "A", h(0), 100, map[string]string{"dimensionCapacity": "1tb"}),
		// 写完档案、落状态前被 kill 导致的重复上架行,须并入同一次售卖。
		rec(KindListed, "A", h(1), 100, nil),
		price,
		rec(KindDelisted, "A", h(3), 90, map[string]string{"tsMemorySize": "32gb", "dimensionCapacity": "1tb"}),
		// 同一货号再次上架是另一次售卖。
		rec(KindListed, "A", h(10), 95, nil),
	}
	sales := Fold(recs)
	if len(sales) != 2 {
		t.Fatalf("应折叠为 2 次售卖,实际 %d", len(sales))
	}
	s := sales[0]
	if !s.ListedAt.Equal(h(0)) || !s.DelistedAt.Equal(h(3)) || s.ListedApprox {
		t.Fatalf("第一次售卖时间不对: %+v", s)
	}
	if len(s.Prices) != 2 || s.Prices[0].Cents != 100 || s.Prices[1].Cents != 90 {
		t.Fatalf("价格序列应为 100→90,实际 %+v", s.Prices)
	}
	if s.Dimensions["tsMemorySize"] != "32gb" {
		t.Fatal("上架时缺失的内存应由之后的记录补上")
	}
	if !sales[1].DelistedAt.IsZero() || sales[1].Price() != 95 {
		t.Fatalf("第二次售卖应仍在架且价格为 95: %+v", sales[1])
	}
}

func TestFoldApproximateListing(t *testing.T) {
	base := rec(KindBaseline, "A", t0.Add(48*time.Hour), 100, nil)
	base.FirstSeen = t0
	price := rec(KindPrice, "B", t0.Add(time.Hour), 80, nil)
	price.OldPriceCents = 100

	sales := Fold([]Record{base, price})
	if len(sales) != 2 {
		t.Fatalf("实际 %d 次售卖", len(sales))
	}
	for _, s := range sales {
		if !s.ListedApprox {
			t.Fatalf("%s 缺少上架记录,上架时刻应标为近似", s.PartNumber)
		}
	}
	if !sales[0].ListedAt.Equal(t0) {
		t.Fatalf("baseline 的上架时刻应取 first_seen,实际 %v", sales[0].ListedAt)
	}
	if sales[1].Prices[0].Cents != 100 {
		t.Fatalf("以调价记录开头时,上架价应取调价前的价格,实际 %+v", sales[1].Prices)
	}
}
