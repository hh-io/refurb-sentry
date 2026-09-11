package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/notify"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// captureNotifier 留下发出去的消息,供断言日报正文。
type captureNotifier struct {
	msgs []notify.Message
	fail bool
}

func (c *captureNotifier) Name() string { return "capture" }

func (c *captureNotifier) Send(_ context.Context, m notify.Message) error {
	if c.fail {
		return fmt.Errorf("渠道不可用")
	}
	c.msgs = append(c.msgs, m)
	return nil
}

func withSummary(t *testing.T, r *Runner, at string) {
	t.Helper()
	d, err := config.ParseDailySummary(at)
	if err != nil {
		t.Fatal(err)
	}
	r.summaryAt = &d

	// newTestRunner 只为 settle 准备,不填 scopes;日报要遍历它才给得出每范围一行。
	region, err := apple.LookupRegion("CN")
	if err != nil {
		t.Fatal(err)
	}
	r.scopes = []scope{{region: region, category: "mac"}}
}

// day 是测试用的固定日期,按本地时区——触发判断本身就是按本地时区做的。
func day(hours float64) time.Time {
	base := time.Date(2026, 9, 12, 0, 0, 0, 0, time.Local)
	return base.Add(time.Duration(hours * float64(time.Hour)))
}

// 已经汇总过之后,日报必须到点才发、当天只发一次、次日重新发。
// 常驻进程每 interval 就会走一遍这段,判重错了就是每两分钟推一条。
func TestDailySummarySendsOncePerDay(t *testing.T) {
	st := state.New()
	st.LastSummaryAt = day(-15) // 昨天 09:00 已经发过
	n := &captureNotifier{}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")
	ctx := context.Background()

	if err := r.maybeDailySummary(ctx, day(8.9)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 0 {
		t.Fatalf("未到点就发了日报: %d 条", len(n.msgs))
	}

	for _, h := range []float64{9, 9.05, 12, 23.9} {
		if err := r.maybeDailySummary(ctx, day(h)); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.msgs) != 1 {
		t.Fatalf("同一天应只发一条日报,实际 %d 条", len(n.msgs))
	}

	if err := r.maybeDailySummary(ctx, day(33)); err != nil { // 次日 09:00
		t.Fatal(err)
	}
	if len(n.msgs) != 2 {
		t.Fatalf("次日应再发一条,实际累计 %d 条", len(n.msgs))
	}
}

// 从未汇总过时不等到点就发一份:两版 README 都把它作为「装好了确实在跑」的
// 安装确认写给了用户,08:00 装好却要等到 09:00 会让人去排查一个没坏的部署。
// 发完之后当天不能再发第二份——判重因此按自然日,不能拿 due 比大小。
func TestFirstSummarySendsImmediatelyThenOncePerDay(t *testing.T) {
	n := &captureNotifier{}
	r := newTestRunner(t, n, state.New())
	withSummary(t, r, "09:00")
	ctx := context.Background()

	if err := r.maybeDailySummary(ctx, day(8)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("首次运行应立刻发一份,实际 %d 条", len(n.msgs))
	}

	// 当天真正到点时不能再发一份。
	for _, h := range []float64{9, 9.05, 20} {
		if err := r.maybeDailySummary(ctx, day(h)); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.msgs) != 1 {
		t.Fatalf("首份之后当天又发了,累计 %d 条", len(n.msgs))
	}

	if err := r.maybeDailySummary(ctx, day(33)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 2 {
		t.Fatalf("次日应正常发出,实际累计 %d 条", len(n.msgs))
	}
}

// 重试计数是针对某一次汇总的。不跨天归零的话,昨天失败一次的余额会留给今天,
// 今天就只剩两次机会,渠道抖一下就被判成永久失败、当天再也不汇总。
func TestSummaryAttemptsResetAcrossDays(t *testing.T) {
	st := state.New()
	st.LastSummaryAt = day(-15)
	n := &captureNotifier{fail: true}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")
	ctx := context.Background()

	// 第一天在放弃之前只失败了一次(之后的轮次假设没走到这里)。
	if err := r.maybeDailySummary(ctx, day(9)); err != nil {
		t.Fatal(err)
	}
	if r.summaryAttempts != 1 {
		t.Fatalf("第一天应记为尝试 1 次,实际 %d", r.summaryAttempts)
	}

	// 次日重新开始,该有完整的 maxSummaryAttempts 次机会。
	if err := r.maybeDailySummary(ctx, day(33)); err != nil {
		t.Fatal(err)
	}
	if r.summaryAttempts != 1 {
		t.Errorf("次日的重试计数未归零,实际 %d(昨天的余额被带了过来)", r.summaryAttempts)
	}
}

// 抓取失败的范围会被 RunOnce 跳过,商品原样留在状态里。不标出来的话,
// 一个连着几天抓不到的范围在日报里与「一切正常但没变动」一模一样——
// 那正是这个功能要消除的二义,不能在范围粒度上又放回来。
func TestStaleScopeIsMarkedInSummary(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A", 100)}, day(9).Add(-time.Hour))

	n := &captureNotifier{}
	r := newTestRunner(t, n, st)
	r.cfg.Interval = config.Duration(120 * time.Second)
	withSummary(t, r, "09:00")

	if err := r.maybeDailySummary(context.Background(), day(9)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("应发出一条日报,实际 %d 条", len(n.msgs))
	}
	if !strings.Contains(n.msgs[0].Body, "数据陈旧") {
		t.Errorf("一小时没更新的范围没有被标成陈旧:\n%s", n.msgs[0].Body)
	}

	// 刚抓过的范围不该被标。
	st2 := state.New()
	st2.Apply("CN", "mac", []apple.Product{prod("A", 100)}, day(9))
	n2 := &captureNotifier{}
	r2 := newTestRunner(t, n2, st2)
	r2.cfg.Interval = config.Duration(120 * time.Second)
	withSummary(t, r2, "09:00")
	if err := r2.maybeDailySummary(context.Background(), day(9)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(n2.msgs[0].Body, "数据陈旧") {
		t.Errorf("刚更新过的范围被误标成陈旧:\n%s", n2.msgs[0].Body)
	}
}

// 未配置 daily_summary 时整个功能不存在:零值不能等于「00:00 启用」。
func TestDailySummaryDisabledByDefault(t *testing.T) {
	n := &captureNotifier{}
	r := newTestRunner(t, n, state.New())

	if err := r.maybeDailySummary(context.Background(), day(12)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 0 {
		t.Fatalf("未配置日报却发了 %d 条", len(n.msgs))
	}
}

// dry-run 不得有任何副作用:发日报是副作用,推进 LastSummaryAt 同样是——
// 它会让正式进程当天不再汇总,清零计数则会把那批变动从下一份日报里抹掉。
func TestDailySummarySkippedInDryRun(t *testing.T) {
	st := state.New()
	st.CountEvents([]state.Event{{Kind: state.EventListed, Product: prod("A", 100)}})

	n := &captureNotifier{}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")
	r.dryRun = true

	if err := r.maybeDailySummary(context.Background(), day(10)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 0 {
		t.Errorf("dry-run 下发出了 %d 条日报", len(n.msgs))
	}
	if !r.st.LastSummaryAt.IsZero() {
		t.Errorf("dry-run 下推进了 LastSummaryAt: %v", r.st.LastSummaryAt)
	}
	if got := r.st.ScopeCounter("CN", "mac"); got.Listed != 1 {
		t.Errorf("dry-run 下动了计数器: %+v", got)
	}
}

// 日报送达失败时不能推进 LastSummaryAt,否则这一天就永远没有日报了——
// 而「日报没到」正是用户判断系统出问题的唯一信号。
func TestDailySummaryRetriesUntilDelivered(t *testing.T) {
	n := &captureNotifier{fail: true}
	r := newTestRunner(t, n, state.New())
	withSummary(t, r, "09:00")
	ctx := context.Background()

	if err := r.maybeDailySummary(ctx, day(9)); err != nil {
		t.Fatal(err)
	}
	if !r.st.LastSummaryAt.IsZero() {
		t.Fatal("日报一条都没送达,却推进了 LastSummaryAt")
	}

	// 渠道恢复,同一天的下一轮应当补上。
	n.fail = false
	if err := r.maybeDailySummary(ctx, day(9.1)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("渠道恢复后应补发一条,实际 %d 条", len(n.msgs))
	}
}

// 但重试必须有上限:渠道恒定失败时,120s 一轮会让它一天重试几百次。
// 放弃时计数器刻意不清零,那批变动并进下一份日报,数字不丢。
func TestDailySummaryStopsRetryingButKeepsCounters(t *testing.T) {
	st := state.New()
	st.CountEvents([]state.Event{
		{Kind: state.EventListed, Product: prod("A", 100)},
		{Kind: state.EventPriceDrop, Product: prod("B", 90)},
	})

	n := &captureNotifier{fail: true}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")
	ctx := context.Background()

	for i := range maxSummaryAttempts {
		if err := r.maybeDailySummary(ctx, day(9+0.1*float64(i))); err != nil {
			t.Fatal(err)
		}
	}
	if r.st.LastSummaryAt.IsZero() {
		t.Errorf("连续 %d 轮未送达后仍未放弃,会无限重试", maxSummaryAttempts)
	}
	if got := r.st.ScopeCounter("CN", "mac"); got.Listed != 1 || got.PriceDrop != 1 {
		t.Errorf("放弃汇总时不该清零计数,实际 %+v", got)
	}

	// 放弃之后当天不再重试。
	before := len(n.msgs)
	n.fail = false
	if err := r.maybeDailySummary(ctx, day(20)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != before {
		t.Errorf("放弃本次汇总后当天又发了一条")
	}
}

// 成功送达后才清零,并把区间起点推到本次时刻——
// 「自上次汇总以来」的口径靠它保持准确。
func TestDailySummaryDeliveryResetsCounters(t *testing.T) {
	st := state.New()
	st.CountEvents([]state.Event{{Kind: state.EventListed, Product: prod("A", 100)}})

	n := &captureNotifier{}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")

	at := day(9)
	if err := r.maybeDailySummary(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if got := r.st.ScopeCounter("CN", "mac"); got != (state.Counter{}) {
		t.Errorf("送达后计数器应清零,实际 %+v", got)
	}
	if !r.st.CountersSince.Equal(at) {
		t.Errorf("CountersSince 应推到本次汇总时刻,实际 %v", r.st.CountersSince)
	}
}

// 日报的核心价值之一是把「规则写错」变成可见的:维度键名抄错、型号猜错
// 都表现为长期 0 命中,而那类错误在别处不会有任何报错。
func TestDailySummaryReportsRuleMatches(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{
		prod("CHEAP", 500000),
		prod("ALSO-CHEAP", 550000),
		prod("PRICEY", 2100000),
	}, time.Now())

	rules, err := filter.New([]filter.Rule{{Name: "捡漏", MaxPrice: 6000}})
	if err != nil {
		t.Fatal(err)
	}
	n := &captureNotifier{}
	r := newTestRunner(t, n, st)
	r.rules = rules
	withSummary(t, r, "09:00")

	if err := r.maybeDailySummary(context.Background(), day(9)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("应发出一条日报,实际 %d 条", len(n.msgs))
	}
	body := n.msgs[0].Body
	if !strings.Contains(body, "在架 3") {
		t.Errorf("日报里没有正确的在架数:\n%s", body)
	}
	if !strings.Contains(body, "命中规则 2") {
		t.Errorf("日报里的命中规则数不对:\n%s", body)
	}
}

// 推送全败会回滚整轮基线,计数器与基线同属一份状态,必须一起退回——
// 否则下一轮重新产生的同一批事件会被计第二次,日报数字凭空翻倍。
func TestRollbackAlsoRollsBackCounters(t *testing.T) {
	st := state.New()
	r := newTestRunner(t, &stubNotifier{}, st)
	ctx := context.Background()

	// 首轮建基线,不产生事件。
	if err := r.settle(ctx, cnMac(t, []apple.Product{prod("A", 100)}), time.Now()); err != nil {
		t.Fatal(err)
	}

	// 第二轮有新商品,但一条都送不出去。
	r2 := newTestRunner(t, &stubNotifier{failFrom: 1}, st)
	if err := r2.settle(ctx, cnMac(t, []apple.Product{prod("A", 100), prod("B", 200)}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := st.ScopeCounter("CN", "mac"); got != (state.Counter{}) {
		t.Errorf("回滚后计数器仍有残留 %+v,下一轮会把同一批事件计两次", got)
	}

	// 渠道恢复后同一批事件重新产生,这时才该计数。
	r3 := newTestRunner(t, &stubNotifier{}, st)
	if err := r3.settle(ctx, cnMac(t, []apple.Product{prod("A", 100), prod("B", 200)}), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := st.ScopeCounter("CN", "mac"); got.Listed != 1 || got.Pushed != 1 {
		t.Errorf("重试成功后应计一次上架与一次推送,实际 %+v", got)
	}
}

// 日报是派生信息,它送不出去绝不能牵连基线回滚——
// 那会让本轮已经送达的真实事件下一轮再推一遍。
func TestFailedSummaryDoesNotRollBackBaseline(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A", 100)}, time.Now())

	// 第一次 Send 成功(事件),第二次开始失败(日报)。
	n := &stubNotifier{failFrom: 2}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")

	if err := r.settle(context.Background(),
		cnMac(t, []apple.Product{prod("A", 100), prod("B", 200)}), day(10)); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Items["CN/mac/B"]; !ok {
		t.Error("日报失败把基线一起回滚了,已推送的上架事件下一轮会重复推送")
	}
	if r.rollbacks != 0 {
		t.Errorf("日报失败不该记进回滚计数,实际 %d", r.rollbacks)
	}
}

// pickyNotifier 只拒绝事件消息,日报照收:模拟「某条事件的载荷被拒」
// (正文超长、webhook 对该载荷恒返 400),此时渠道本身是好的。
type pickyNotifier struct{ msgs []notify.Message }

func (p *pickyNotifier) Name() string { return "picky" }

func (p *pickyNotifier) Send(_ context.Context, m notify.Message) error {
	if !strings.Contains(m.Title, "日报") {
		return fmt.Errorf("载荷被拒")
	}
	p.msgs = append(p.msgs, m)
	return nil
}

// 日报必须独立于本轮推送的成败。挂在成功路径上的话,恰恰是系统出问题的那几轮
// (每轮都因同一条载荷回滚)完全没有日报,而那正是用户要靠它察觉故障的时候。
func TestSummarySentEvenWhenDispatchFails(t *testing.T) {
	st := state.New()
	st.Apply("CN", "mac", []apple.Product{prod("A", 100)}, day(8))
	st.LastSummaryAt = day(-15) // 昨天已汇总,今天到点该发

	n := &pickyNotifier{}
	r := newTestRunner(t, n, st)
	withSummary(t, r, "09:00")

	// 本轮有新商品,但它的推送被渠道拒掉,reconcile 会回滚基线。
	if err := r.settle(context.Background(),
		cnMac(t, []apple.Product{prod("A", 100), prod("B", 200)}), day(9)); err != nil {
		t.Fatal(err)
	}
	if len(n.msgs) != 1 {
		t.Fatalf("推送失败的那一轮没有发出日报(收到 %d 条),故障期间正好没有活性信号", len(n.msgs))
	}
	if _, ok := st.Items["CN/mac/B"]; ok {
		t.Error("事件推送失败却没有回滚基线")
	}
}

// 没设 TZ 时 Location() 是无信息量的 "Local",而时区正是日报的触发依据:
// 容器里默认 UTC 会让配好的 09:00 在别的时刻送达,启动日志必须带上实际偏移,
// 否则「为什么 09:00 没收到」只能靠猜。
func TestTZLabelAlwaysCarriesOffset(t *testing.T) {
	cases := map[string]string{
		"Asia/Shanghai":    "Asia/Shanghai(+08:00)",
		"America/New_York": "America/New_York(-05:00)",
		"UTC":              "UTC(+00:00)",
	}
	for zone, want := range cases {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("系统缺少时区库 %s: %v", zone, err)
		}
		// 取一个不在夏令时里的日期,免得偏移随季节变化。
		if got := tzLabel(time.Date(2026, 1, 15, 12, 0, 0, 0, loc)); got != want {
			t.Errorf("时区 %s -> %q,期望 %q", zone, got, want)
		}
	}

	// 未设 TZ 的进程拿到的是 time.Local,名字是 "Local" 这种没信息量的字符串,
	// 但偏移必须照样打出来。
	if got := tzLabel(time.Now()); !strings.Contains(got, "(") || !strings.HasSuffix(got, ")") {
		t.Errorf("本地时区没带上偏移: %q", got)
	}
}
