package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/notify"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// detailServer 提供详情页 fixture,并统计被请求了多少次。
func detailServer(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "apple", "testdata", "detail.html"))
	if err != nil {
		t.Fatalf("读取详情页 fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/missing" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMemoryRunner 的规则集刻意按内存过滤:补齐只为按内存过滤的规则服务,
// 空规则集会让整个补齐被跳过(见 TestFillMissingMemorySkipsWhenNoRuleUsesMemory)。
func newMemoryRunner(t *testing.T, fill bool) *Runner {
	t.Helper()
	rules, err := filter.New([]filter.Rule{{
		Name:       "按内存过滤",
		Dimensions: map[string][]string{"tsMemorySize": {"36gb", "48gb"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	client, err := apple.NewClient(apple.ClientOptions{MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Runner{
		cfg: &config.Config{
			StatePath: filepath.Join(t.TempDir(), "state.json"),
			HTTP:      config.HTTPConfig{FillMissingMemory: fill},
			Notify:    config.NotifyConfig{DigestThreshold: 100},
		},
		client:   client,
		rules:    rules,
		notif:    notify.NewMulti(nil, log),
		render:   notify.NewRenderer(notify.LangZH, ""),
		st:       state.New(),
		log:      log,
		memCache: apple.NewMemoryCache(),
	}
}

func macScope(t *testing.T) scope {
	t.Helper()
	region, err := apple.LookupRegion("CN")
	if err != nil {
		t.Fatal(err)
	}
	return scope{region: region, category: "mac"}
}

// 列表页缺内存维度的商品应当被详情页补齐,写回的键与列表页自带的一致。
func TestFillMissingMemory(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "A", URL: srv.URL + "/a", Dimensions: map[string]string{
			"dimensionCapacity": "1tb", "tsMemorySize": "24gb"}},
		{PartNumber: "B", URL: srv.URL + "/b", Dimensions: map[string]string{
			"dimensionCapacity": "2tb"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatal(err)
	}

	if got := grid.Products[1].Dimensions["tsMemorySize"]; got != "36gb" {
		t.Errorf("缺内存的商品应被补成 36gb,实际 %q", got)
	}
	if got := grid.Products[0].Dimensions["tsMemorySize"]; got != "24gb" {
		t.Errorf("列表页已给内存的商品不该被改动,实际 %q", got)
	}
	if hits.Load() != 1 {
		t.Errorf("只该为缺失的那一件发一个请求,实际 %d 个", hits.Load())
	}
}

// 关闭开关时一个详情页都不该抓——这是默认行为,不能让所有用户白白付出请求量。
func TestFillMissingMemoryDisabled(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, false)

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "A", URL: srv.URL + "/a", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
		{PartNumber: "B", URL: srv.URL + "/b", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Errorf("开关关闭时不该发任何请求,实际 %d 个", hits.Load())
	}
}

// watch 这类整个分类都没有内存维度的,一件也不该抓——
// 否则每轮会为几十件手表各发一个毫无意义的详情页请求。
func TestFillMissingMemorySkipsCategoryWithoutMemory(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "W1", URL: srv.URL + "/w1", Dimensions: map[string]string{"dimensionCaseSize": "49mm"}},
		{PartNumber: "W2", URL: srv.URL + "/w2", Dimensions: map[string]string{"dimensionCaseSize": "46mm"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Errorf("分类没有内存维度时不该发请求,实际 %d 个", hits.Load())
	}
}

// 详情页失败绝不能让商品消失或维度变脏:它必须原样留在 grid 里参与 diff,
// 否则一次上游 5xx 会把几十台机器误判成下架。
func TestFillMissingMemoryFailureKeepsProduct(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "OK", URL: srv.URL + "/ok", Dimensions: map[string]string{"dimensionCapacity": "1tb", "tsMemorySize": "24gb"}},
		{PartNumber: "BAD", URL: srv.URL + "/missing", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatalf("详情页失败不该向上返回错误: %v", err)
	}
	if len(grid.Products) != 2 {
		t.Fatalf("商品数不该变化,实际 %d", len(grid.Products))
	}
	if _, ok := grid.Products[1].Dimensions["tsMemorySize"]; ok {
		t.Error("补不到时不该写入内存维度,留空才能让规则照常判为不匹配")
	}
}

// 同一货号只查一次。常驻进程每轮都重查会把「一分类一请求」的设计彻底破坏掉,
// 查过没查到的也要记住,否则上游永远不给内存的机器会被无限重试。
func TestFillMissingMemoryCachesAcrossRounds(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)
	sc := macScope(t)

	newGrid := func() *apple.Grid {
		return &apple.Grid{Products: []apple.Product{
			{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
			{PartNumber: "B", URL: srv.URL + "/b", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
			{PartNumber: "BAD", URL: srv.URL + "/missing", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
		}}
	}

	for round := 1; round <= 3; round++ {
		g := newGrid()
		if err := r.fillMissingMemory(context.Background(), sc, g); err != nil {
			t.Fatal(err)
		}
		if got := g.Products[1].Dimensions["tsMemorySize"]; got != "36gb" {
			t.Errorf("第 %d 轮:缓存命中后仍应写回 36gb,实际 %q", round, got)
		}
	}
	// 两件缺内存的商品各查一次(一成一败),后两轮全部走缓存。
	if hits.Load() != 2 {
		t.Errorf("三轮下来只该发 2 个请求,实际 %d 个", hits.Load())
	}
}

// 没有任何规则按内存过滤时,补齐改变不了任何推送结果,一个请求都不该发。
func TestFillMissingMemorySkipsWhenNoRuleUsesMemory(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)
	rules, err := filter.New([]filter.Rule{{
		Name:       "只按机型",
		Dimensions: map[string][]string{"refurbClearModel": {"macbookpro"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.rules = rules

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
		{PartNumber: "B", URL: srv.URL + "/b", Dimensions: map[string]string{
			"refurbClearModel": "macbookpro", "dimensionCapacity": "2tb"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Errorf("没有规则按内存过滤时不该发请求,实际 %d 个", hits.Load())
	}
}

// 懒加载:列表页已经能判定不可能命中的商品,不必再为它抓详情页。
// 内存是它唯一还没定的条件时才值得查,其余条件已经否决它时查了也是白查。
func TestFillMissingMemorySkipsProductsRuledOutByGrid(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)
	rules, err := filter.New([]filter.Rule{{
		Name: "MacBook Pro 高配",
		Dimensions: map[string][]string{
			"refurbClearModel": {"macbookpro"},
			"tsMemorySize":     {"36gb", "48gb"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r.rules = rules

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
		// 机型对得上,内存未知——值得查。
		{PartNumber: "PRO", URL: srv.URL + "/pro", Dimensions: map[string]string{
			"refurbClearModel": "macbookpro", "dimensionCapacity": "2tb"}},
		// 机型就不对,内存再合适也不会命中——不该查。
		{PartNumber: "DISPLAY", URL: srv.URL + "/display", Dimensions: map[string]string{
			"refurbClearModel": "display", "dimensionCapacity": "2tb"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatal(err)
	}

	if got := grid.Products[1].Dimensions["tsMemorySize"]; got != "36gb" {
		t.Errorf("可能命中的商品应被补齐,实际 %q", got)
	}
	if _, ok := grid.Products[2].Dimensions["tsMemorySize"]; ok {
		t.Error("列表页已否决的商品不该被补齐")
	}
	if hits.Load() != 1 {
		t.Errorf("只该为可能命中的那一件发请求,实际 %d 个", hits.Load())
	}
}

// 传输故障(超时、5xx)绝不能被记成「查过没查到」:一次抖动就让这台机器在整个
// 进程生命周期里再也不被补齐,按内存过滤的规则会从此静默漏掉它——
// 而消除这种静默漏掉正是补齐功能存在的理由。
func TestFillMissingMemoryRetriesTransientFailure(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "apple", "testdata", "detail.html"))
	if err != nil {
		t.Fatalf("读取详情页 fixture: %v", err)
	}
	var down atomic.Bool
	down.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "upstream hiccup", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	r := newMemoryRunner(t, true)
	sc := macScope(t)
	newGrid := func() *apple.Grid {
		return &apple.Grid{Products: []apple.Product{
			{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
			{PartNumber: "B", URL: srv.URL + "/b", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
		}}
	}

	g := newGrid()
	if err := r.fillMissingMemory(context.Background(), sc, g); err != nil {
		t.Fatalf("详情页 5xx 不该向上返回错误: %v", err)
	}
	if _, ok := g.Products[1].Dimensions["tsMemorySize"]; ok {
		t.Error("补不到时不该写入内存维度")
	}

	down.Store(false)
	g = newGrid()
	if err := r.fillMissingMemory(context.Background(), sc, g); err != nil {
		t.Fatal(err)
	}
	if got := g.Products[1].Dimensions["tsMemorySize"]; got != "36gb" {
		t.Errorf("上游恢复后应当补齐成 36gb,实际 %q——传输故障被误当成永久失败缓存了", got)
	}
}

// 解析层面的失败反过来必须只查一次:上游永远不给内存的机器不该每轮都被重抓一遍。
func TestFillMissingMemoryCachesUnreadablePage(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body>这个页面没有概述栏</body></html>"))
	}))
	t.Cleanup(srv.Close)

	r := newMemoryRunner(t, true)
	sc := macScope(t)
	for round := 1; round <= 3; round++ {
		g := &apple.Grid{Products: []apple.Product{
			{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
			{PartNumber: "NOOVERVIEW", URL: srv.URL + "/n", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
		}}
		if err := r.fillMissingMemory(context.Background(), sc, g); err != nil {
			t.Fatalf("第 %d 轮: %v", round, err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("解析失败是永久的,三轮下来只该发 1 个请求,实际 %d 个", hits.Load())
	}
}

// 重试也要有上限:上游持续 5xx 或把详情页整个封了,每轮为同一批商品重发几十个请求
// 既补不到内存,又白白加重上游负担。与 maxRollbacks 同一个道理。
func TestFillMissingMemoryStopsRetryingAfterRepeatedFailure(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 403 而不是 5xx:同样属于「不是页面读不出内存」的可重试故障(WAF 拦截会这样),
		// 但 c.get 不会为它退避重试,测试因此不必等几轮指数退避。
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	r := newMemoryRunner(t, true)
	sc := macScope(t)
	for round := 1; round <= 6; round++ {
		g := &apple.Grid{Products: []apple.Product{
			{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
			{PartNumber: "DOWN", URL: srv.URL + "/down", Dimensions: map[string]string{"dimensionCapacity": "2tb"}},
		}}
		if err := r.fillMissingMemory(context.Background(), sc, g); err != nil {
			t.Fatalf("第 %d 轮: %v", round, err)
		}
	}
	// 403 不退避重试,故每轮 1 次;攒够 3 轮后放弃,后 3 轮走缓存不再发请求。
	if got := hits.Load(); got != 3 {
		t.Errorf("攒够 3 轮后应当放弃重试,期望 3 次请求,实际 %d 次", got)
	}
}

// 缺容量锚点的商品解析必然失败(无从排除存储条目),不该为它发这个请求。
// 实测 CN mac 的 13 件 Studio Display 内存与容量两个维度都没有。
func TestFillMissingMemorySkipsProductsWithoutCapacityAnchor(t *testing.T) {
	var hits atomic.Int64
	srv := detailServer(t, &hits)
	r := newMemoryRunner(t, true)

	grid := &apple.Grid{Products: []apple.Product{
		{PartNumber: "SEED", URL: srv.URL + "/seed", Dimensions: map[string]string{"tsMemorySize": "24gb"}},
		{PartNumber: "NOCAP", URL: srv.URL + "/nocap", Dimensions: map[string]string{"dimensionColor": "silver"}},
	}}
	if err := r.fillMissingMemory(context.Background(), macScope(t), grid); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Errorf("没有容量锚点时不该发请求,实际 %d 个", hits.Load())
	}
	if _, ok := grid.Products[1].Dimensions["tsMemorySize"]; ok {
		t.Error("不该写入内存维度")
	}
}
