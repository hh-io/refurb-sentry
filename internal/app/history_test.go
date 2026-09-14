package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/history"
	"github.com/hh-io/refurb-sentry/internal/state"
)

func readHistory(t *testing.T, r *Runner) []history.Record {
	t.Helper()
	if !fileExists(r.cfg.History.Path) {
		return nil
	}
	recs, bad, err := history.Read(r.cfg.History.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("档案里有 %d 行无法解析", bad)
	}
	return recs
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func countKind(recs []history.Record, pn string, kind history.Kind) int {
	n := 0
	for _, r := range recs {
		if r.PartNumber == pn && r.Kind == kind {
			n++
		}
	}
	return n
}

// 回滚的那一轮绝不能写档案:下一轮会重新产生同一批变动,写了就是重复记录。
// 重试成功的那一轮才写,且只写一次。
func TestHistoryWrittenOnlyOnCommit(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())
	products := []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}

	r := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	if err := r.settle(context.Background(), cnMac(t, products), time.Now()); err != nil {
		t.Fatal(err)
	}
	if fileExists(r.cfg.History.Path) {
		t.Fatal("推送全败回滚的那一轮不应写档案")
	}

	r.notif = newTestRunner(t, &stubNotifier{}, st).notif
	if err := r.settle(context.Background(), cnMac(t, products), time.Now()); err != nil {
		t.Fatal(err)
	}
	recs := readHistory(t, r)
	if countKind(recs, "B/A", history.KindListed) != 1 {
		t.Fatalf("重试成功后 B/A 应恰好有 1 条上架记录,实际 %+v", recs)
	}
	// 档案首次写入时,之前已在状态库里的 A/A 须打底为 baseline。
	if countKind(recs, "A/A", history.KindBaseline) != 1 {
		t.Fatalf("A/A 应被打底为 baseline,实际 %+v", recs)
	}
}

// 放弃重试、强制推进的那一轮同样是提交,变动必须进档案,否则这批商品在档案里凭空消失。
func TestHistoryWrittenWhenGivingUpRetries(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())
	r := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	products := []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}

	for i := 0; i <= maxRollbacks; i++ {
		if err := r.settle(context.Background(), cnMac(t, products), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if n := countKind(readHistory(t, r), "B/A", history.KindListed); n != 1 {
		t.Fatalf("强制推进后 B/A 应恰好有 1 条上架记录,实际 %d", n)
	}
}

func TestHistoryDryRunWritesNothing(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())
	r := newTestRunner(t, &stubNotifier{}, st)
	r.dryRun = true
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 90000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if fileExists(r.cfg.History.Path) {
		t.Fatal("-dry-run 不得写历史档案")
	}
}

func TestHistoryDisabled(t *testing.T) {
	r := newTestRunner(t, &stubNotifier{}, state.New())
	r.cfg.History.Enabled = new(bool)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{prod("A/A", 1)}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if fileExists(r.cfg.History.Path) {
		t.Fatal("history.enabled: false 时不应写档案")
	}
}

// 只归档的分类即使规则集为空(放行全部)也绝不能推送,但必须进档案、也不进日报计数。
func TestArchiveOnlyScopeIsNeverPushed(t *testing.T) {
	region, err := apple.LookupRegion("CN")
	if err != nil {
		t.Fatal(err)
	}
	ipad := scope{region: region, category: "ipad", archiveOnly: true}
	ipadProd := func(pn string) apple.Product {
		p := prod(pn, 50000)
		p.Category = "ipad"
		return p
	}

	st := state.New()
	st.Apply("CN", "ipad", []apple.Product{ipadProd("P1")}, time.Now())
	n := &stubNotifier{}
	r := newTestRunner(t, n, st)
	r.scopes = []scope{ipad}

	if err := r.settle(context.Background(), []fetched{{
		scope: ipad,
		grid:  &apple.Grid{Products: []apple.Product{ipadProd("P1"), ipadProd("P2")}},
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n.calls != 0 {
		t.Fatalf("只归档的分类不应推送,实际发送 %d 次", n.calls)
	}
	if c := st.ScopeCounter("CN", "ipad"); c.Listed != 0 {
		t.Fatalf("只归档的分类不应计入日报,实际 %+v", c)
	}
	if countKind(readHistory(t, r), "P2", history.KindListed) != 1 {
		t.Fatal("只归档的分类的上架应写进档案")
	}
}

// 档案里仍在架、状态库里却没有的商品(档案关闭期间或状态重建前就下架了)
// 要在本进程第一次提交时补记下架;每次启动只对一次账,不能每轮都补。
func TestHistoryClosesStaleSalesOnce(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())
	r := newTestRunner(t, &stubNotifier{}, st)

	old := time.Now().Add(-72 * time.Hour)
	if err := history.Append(r.cfg.History.Path, []history.Record{
		{Time: old, Kind: history.KindListed, Region: "CN", Category: "mac", PartNumber: "A/A", PriceCents: 100000, FirstSeen: old},
		{Time: old, Kind: history.KindListed, Region: "CN", Category: "mac", PartNumber: "GONE/A", PriceCents: 50000, FirstSeen: old},
	}); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := r.settle(context.Background(), cnMac(t, []apple.Product{prod("A/A", 100000)}), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	recs := readHistory(t, r)
	if n := countKind(recs, "GONE/A", history.KindDelisted); n != 1 {
		t.Fatalf("GONE/A 应恰好补记 1 条下架,实际 %d", n)
	}
	if countKind(recs, "A/A", history.KindDelisted) != 0 {
		t.Fatal("仍在状态库里的 A/A 不应被关闭")
	}
}

// 只归档的分类在某地区不存在时,报错要指向 history.categories,而不是 categories。
func TestUnavailableArchiveScopeErrorPointsToHistoryConfig(t *testing.T) {
	region, err := apple.LookupRegion("CN")
	if err != nil {
		t.Fatal(err)
	}
	if msg := unavailableScopeError(scope{region: region, category: "iphone", archiveOnly: true}).Error(); !strings.Contains(msg, "history.categories") {
		t.Errorf("只归档分类的报错应指向 history.categories,实际 %q", msg)
	}
	if msg := unavailableScopeError(scope{region: region, category: "iphone"}).Error(); strings.Contains(msg, "history.") {
		t.Errorf("普通分类的报错不应提 history,实际 %q", msg)
	}
}
