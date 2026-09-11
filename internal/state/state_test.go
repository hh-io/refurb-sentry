package state

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	s.Bootstrapped["CN/mac"] = true
	evs = s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000), prod("C/A", 300000)}, now)
	if k := kinds(evs); k[EventListed] != 1 || len(evs) != 1 {
		t.Fatalf("期望恰好 1 条上架事件,实际 %v", k)
	}
}

func TestPriceDropAndRise(t *testing.T) {
	s := New()
	s.Bootstrapped["CN/mac"] = true
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
	s.Bootstrapped["CN/mac"] = true
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
	s.Bootstrapped["CN/mac"] = true
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
	s.Bootstrapped["CN/mac"] = true
	s.Bootstrapped["US/mac"] = true
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
	s.Bootstrapped["CN/mac"] = true
	now := time.Now().Truncate(time.Second)
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)

	if err := Save(path, s); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if !got.IsBootstrapped("CN", "mac") || got.CountScope("CN", "mac") != 1 {
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
	if s.IsBootstrapped("CN", "mac") {
		t.Fatal("全新状态不应标记为已 bootstrap")
	}
}

// 存量用户的状态文件里不会有新版本才加的字段,反序列化后那些 map 是 nil。
// Load 必须把每个 map 字段都补上——漏一个既不编译报错,其余测试也照样全绿
// (新装的用户走 New(),map 都是好的),只有存量用户升级后写入那个 map 的瞬间才 panic,
// 而开发与 CI 都碰不到这条路径。
//
// 与 TestCloneCoversEveryField 同样用反射遍历:新加的 map 字段自动纳入,不必手动登记。
func TestLoadInitializesEveryMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// 刻意只写 version,模拟一份缺了全部 map 字段的旧状态文件。
	// 版本号取自常量,将来升版本时这里会跟着走而不是莫名其妙地失败。
	raw := fmt.Sprintf(`{"version":%d}`, stateVersion)
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("写入测试状态文件失败: %v", err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	v := reflect.ValueOf(*s)
	for i := range v.NumField() {
		if v.Field(i).Kind() != reflect.Map {
			continue
		}
		if v.Field(i).IsNil() {
			t.Errorf("Load 未初始化 map 字段 %s,存量用户升级后写入它会 panic",
				v.Type().Field(i).Name)
		}
	}
}

// 给已在运行的部署新增地区或分类时,新范围必须静默建立基线。
// 用全局 bootstrapped 标志时,新增一个地区会把它数百件在架商品全报成「上架」。
func TestNewScopeBootstrapsSilently(t *testing.T) {
	s := New()
	now := time.Now()

	// CN 已在监控中,基线建好
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)
	if evs := s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}, now); len(evs) != 1 {
		t.Fatalf("CN 已建基线,新货号应报上架,实际 %+v", evs)
	}

	// 此时往配置里加了 US:它的首轮必须静默
	us := prod("X/A", 50000)
	us.Region, us.Currency = "US", "USD"
	us2 := prod("Y/A", 60000)
	us2.Region, us2.Currency = "US", "USD"
	if evs := s.Apply("US", "mac", []apple.Product{us, us2}, now); len(evs) != 0 {
		t.Fatalf("新增地区的首轮必须静默,实际推出 %d 条: %+v", len(evs), evs)
	}
	if s.CountScope("US", "mac") != 2 {
		t.Fatal("新增地区仍应建立基线")
	}
	// 第二轮起才报变化
	us3 := prod("Z/A", 70000)
	us3.Region, us3.Currency = "US", "USD"
	if evs := s.Apply("US", "mac", []apple.Product{us, us2, us3}, now); len(evs) != 1 {
		t.Fatalf("新增地区第二轮应报上架,实际 %+v", evs)
	}
}

// 首轮某个地区抓取失败(调用方跳过 Apply)时,不能因为别的地区成功
// 就把失败地区也当成已建基线——否则它下一轮会把全部在架商品报成上架。
func TestFailedScopeStaysUnbootstrapped(t *testing.T) {
	s := New()
	now := time.Now()

	// CN 成功,US 因网络失败,调用方不会为 US 调用 Apply
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)
	if s.IsBootstrapped("US", "mac") {
		t.Fatal("未成功抓取过的范围不应被标记为已建基线")
	}

	// US 下一轮成功,必须静默建基线
	us := prod("X/A", 50000)
	us.Region, us.Currency = "US", "USD"
	if evs := s.Apply("US", "mac", []apple.Product{us}, now); len(evs) != 0 {
		t.Fatalf("此前失败的范围首次成功时应静默,实际 %+v", evs)
	}
}

// 快照必须与原状态完全脱钩:任何共享的底层 map 都会让回滚只还原一半。
func TestCloneIsolatesSnapshot(t *testing.T) {
	s := New()
	now := time.Now()
	p := prod("A/A", 100000)
	p.Dimensions = map[string]string{"tsMemorySize": "16GB"}
	s.Apply("CN", "mac", []apple.Product{p}, now)
	s.EmptyStreak["CN/watch"] = 2

	snap := s.Clone()

	// 在原状态上做一轮完整变更:降价、上架、下架、streak 归零。
	cheaper := prod("A/A", 90000)
	cheaper.Dimensions = map[string]string{"tsMemorySize": "24GB"}
	s.Apply("CN", "mac", []apple.Product{cheaper, prod("B/A", 200000)}, now)
	delete(s.EmptyStreak, "CN/watch")
	s.Bootstrapped["US/mac"] = true

	if got := snap.Items["CN/mac/A/A"].PriceCents; got != 100000 {
		t.Fatalf("快照价格被原状态改写: %d", got)
	}
	if got := snap.Items["CN/mac/A/A"].Dimensions["tsMemorySize"]; got != "16GB" {
		t.Fatalf("快照 Dimensions 与原状态共享: %q", got)
	}
	if _, ok := snap.Items["CN/mac/B/A"]; ok {
		t.Fatal("快照不应包含拍摄之后新增的商品")
	}
	if snap.EmptyStreak["CN/watch"] != 2 {
		t.Fatal("快照的 EmptyStreak 与原状态共享")
	}
	if snap.Bootstrapped["US/mac"] {
		t.Fatal("快照的 Bootstrapped 与原状态共享")
	}
}

// Restore 必须原地覆盖:换指针会让别处持有的 State 仍指向脏状态。
func TestRestoreOverwritesInPlace(t *testing.T) {
	s := New()
	now := time.Now()
	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, now)
	snap := s.Clone()

	s.Apply("CN", "mac", []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}, now)
	s.Restore(snap)

	if _, ok := s.Items["CN/mac/B/A"]; ok {
		t.Fatal("Restore 后不应残留回滚点之后的变更")
	}
	if s.CountScope("CN", "mac") != 1 {
		t.Fatalf("Restore 应还原到 1 条记录,实际 %d", s.CountScope("CN", "mac"))
	}
}

// Clone 是逐字段手写的,新增一个 map 字段却忘了在里面拷贝不会有任何编译错误,
// 失败方式却很难查:推送失败回滚时那个字段不退回,日报计数凭空翻倍。
// 这里用反射遍历 State 的每个字段,要求 map 字段既非空又不与原状态共享底层数组。
func TestCloneCoversEveryField(t *testing.T) {
	now := time.Now()
	s := New()
	s.Apply("CN", "mac", []apple.Product{prod("A", 100)}, now)
	s.EmptyStreak["CN/watch"] = 1
	s.CountEvents([]Event{{Kind: EventListed, Product: prod("B", 200)}})
	s.CountersSince = now.Add(-24 * time.Hour)
	s.LastSummaryAt = now.Add(-12 * time.Hour)

	c := s.Clone()
	v, cv := reflect.ValueOf(*s), reflect.ValueOf(*c)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		if v.Field(i).Kind() != reflect.Map {
			// 与下面的空 map 护栏对称:样本里是零值的话,原与副本都是零值,
			// DeepEqual 恒真,漏拷一个标量字段照样全绿。新增字段必须在上面的
			// 样本里显式赋一个非零值,这条断言才真的在守东西。
			if v.Field(i).IsZero() {
				t.Fatalf("测试样本里的 %s 是零值,守不住任何东西", name)
			}
			if !reflect.DeepEqual(v.Field(i).Interface(), cv.Field(i).Interface()) {
				t.Errorf("Clone 漏拷字段 %s: 原 %v,副本 %v", name, v.Field(i), cv.Field(i))
			}
			continue
		}
		// 样本本身得非空,否则这条断言什么也守不住。
		if v.Field(i).Len() == 0 {
			t.Fatalf("测试样本里的 %s 是空 map,守不住任何东西", name)
		}
		if cv.Field(i).Len() != v.Field(i).Len() {
			t.Errorf("Clone 漏拷字段 %s: 原 %d 项,副本 %d 项", name, v.Field(i).Len(), cv.Field(i).Len())
		}
		if cv.Field(i).Pointer() == v.Field(i).Pointer() {
			t.Errorf("Clone 的 %s 与原状态共享同一个 map,快照会被后续写入污染", name)
		}
	}
}

func TestCountEventsSeparatesScopes(t *testing.T) {
	s := New()
	watch := apple.Product{Region: "CN", Category: "watch", PartNumber: "W1"}
	s.CountEvents([]Event{
		{Kind: EventListed, Product: prod("A", 100)},
		{Kind: EventListed, Product: prod("B", 100)},
		{Kind: EventPriceDrop, Product: prod("A", 90)},
		{Kind: EventDelisted, Product: watch},
	})
	s.CountPushed([]Event{{Kind: EventListed, Product: prod("A", 100)}})

	if got := s.ScopeCounter("CN", "mac"); got != (Counter{Listed: 2, PriceDrop: 1, Pushed: 1}) {
		t.Errorf("CN/mac 计数不对: %+v", got)
	}
	if got := s.ScopeCounter("CN", "watch"); got != (Counter{Delisted: 1}) {
		t.Errorf("CN/watch 计数不对: %+v", got)
	}

	s.ResetCounters(time.Now())
	if got := s.ScopeCounter("CN", "mac"); got != (Counter{}) {
		t.Errorf("ResetCounters 之后仍有残留: %+v", got)
	}
}
