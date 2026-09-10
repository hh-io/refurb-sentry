package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/notify"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// stubNotifier 按 failFrom 决定从第几次发送开始失败,用于构造「部分事件送达」。
type stubNotifier struct {
	calls    int
	failFrom int
}

func (s *stubNotifier) Name() string { return "stub" }

func (s *stubNotifier) Send(context.Context, notify.Message) error {
	s.calls++
	if s.failFrom > 0 && s.calls >= s.failFrom {
		return fmt.Errorf("渠道不可用")
	}
	return nil
}

func prod(pn string, cents int64) apple.Product {
	return apple.Product{
		Region: "CN", Category: "mac", PartNumber: pn,
		Title: "翻新 " + pn, URL: "https://example.invalid/" + pn,
		PriceCents: cents, Currency: "CNY",
	}
}

// newTestRunner 构造一个不含 apple.Client 的 Runner:settle 阶段不发网络请求,
// 这正是把 RunOnce 拆成两段的用意。
func newTestRunner(t *testing.T, n notify.Notifier, st *state.State) *Runner {
	t.Helper()
	rules, err := filter.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Runner{
		cfg: &config.Config{
			StatePath: filepath.Join(t.TempDir(), "state.json"),
			Notify:    config.NotifyConfig{DigestThreshold: 100},
		},
		rules:  rules,
		notif:  notify.NewMulti([]notify.Notifier{n}, log),
		render: notify.NewRenderer(notify.LangZH, ""),
		st:     st,
		log:    log,
	}
}

func cnMac(t *testing.T, products []apple.Product) []fetched {
	t.Helper()
	region, err := apple.LookupRegion("CN")
	if err != nil {
		t.Fatal(err)
	}
	return []fetched{{
		scope: scope{region: region, category: "mac"},
		grid:  &apple.Grid{Products: products},
	}}
}

// 推送全败时必须把内存基线回滚,否则常驻进程下一轮比对不出这批事件,
// 「下一轮重试」的承诺就成了空话——变动被永久吞掉。
func TestFailedDispatchRollsBackBaseline(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())

	r := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, ok := st.Items["CN/mac/B/A"]; ok {
		t.Fatal("推送全败后不应把新商品留在基线里")
	}

	// 下一轮同一份数据必须重新产生上架事件并成功送达。
	ok := &stubNotifier{}
	r2 := newTestRunner(t, ok, st)
	if err := r2.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if ok.calls != 1 {
		t.Fatalf("回滚后下一轮应重新推送 1 条上架事件,实际发送 %d 次", ok.calls)
	}
	if _, exists := st.Items["CN/mac/B/A"]; !exists {
		t.Fatal("推送成功后应推进基线")
	}
}

// 逐条推送时,一条成功、其余全败不能算整轮成功:
// 那些没送达的事件既没落盘也不会重试,会被静默丢弃。
func TestPartialDeliveryIsNotSuccess(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())

	// 第 1 条成功,第 2、3 条失败。
	r := newTestRunner(t, &stubNotifier{failFrom: 2}, st)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000), prod("C/A", 300000), prod("D/A", 400000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"CN/mac/B/A", "CN/mac/C/A", "CN/mac/D/A"} {
		if _, ok := st.Items[k]; ok {
			t.Fatalf("存在未送达的事件时应整轮回滚,%s 不该进入基线", k)
		}
	}
}

// 回滚必须是原地覆盖:State 指针在 Runner 之外仍被持有,
// 换指针会让进程退出时落盘的还是那份已被推进的脏状态。
func TestRollbackKeepsStatePointer(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())
	held := st // 模拟 main.go 里另一处持有

	r := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := held.Items["CN/mac/B/A"]; ok {
		t.Fatal("外部持有的同一 State 也应看到回滚结果")
	}
}

// 推送成功才落盘;全败时状态文件不该被写出。
func TestFailedDispatchSkipsSave(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())

	r := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.Load(r.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Items) != 0 {
		t.Fatalf("推送全败时不应落盘,实际写出了 %d 条记录", len(loaded.Items))
	}
}

// 抓到空列表时 Apply 要攒够 emptyStreakThreshold 轮才真正建立基线,
// 这期间不能报「已建立基线」——那是假的,而且每轮都会重复一次。
func TestEmptyScopeDoesNotClaimBaseline(t *testing.T) {
	st := state.New()
	r := newTestRunner(t, &stubNotifier{}, st)

	if err := r.settle(context.Background(), cnMac(t, nil), time.Now()); err != nil {
		t.Fatal(err)
	}
	if st.IsBootstrapped("CN", "mac") {
		t.Fatal("空列表首轮不应建立基线")
	}
}

// 推送若是永久性失败(正文超长、webhook 恒返 400),无限回滚会让每一轮
// 都把同批事件里能送达的那几条再推一遍。攒够 maxRollbacks 轮必须放弃并推进。
func TestPermanentFailureStopsRollingBack(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())

	r := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	products := []apple.Product{prod("A/A", 100000), prod("B/A", 200000)}

	for i := 1; i <= maxRollbacks; i++ {
		if err := r.settle(context.Background(), cnMac(t, products), time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, ok := st.Items["CN/mac/B/A"]; ok {
			t.Fatalf("第 %d 轮仍在重试窗口内,不应推进基线", i)
		}
	}

	// 超过上限的这一轮放弃重试,强制推进。
	if err := r.settle(context.Background(), cnMac(t, products), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Items["CN/mac/B/A"]; !ok {
		t.Fatalf("连续 %d 轮失败后应放弃并推进基线,否则会无限重推", maxRollbacks)
	}
}

// 中途一次成功必须清零计数,否则零星的渠道抖动会攒够上限、误伤后面的真实重试。
func TestSuccessResetsRollbackCounter(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A/A", 100000)}, time.Now())

	failing := &stubNotifier{failFrom: 1}
	r := newTestRunner(t, failing, st)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.rollbacks != 1 {
		t.Fatalf("失败一轮后计数应为 1,实际 %d", r.rollbacks)
	}

	r.notif = notify.NewMulti([]notify.Notifier{&stubNotifier{}}, r.log)
	if err := r.settle(context.Background(), cnMac(t, []apple.Product{
		prod("A/A", 100000), prod("B/A", 200000),
	}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.rollbacks != 0 {
		t.Fatalf("推送成功后计数应清零,实际 %d", r.rollbacks)
	}
}
