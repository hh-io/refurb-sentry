package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/notify"
	"github.com/hh-io/refurb-sentry/internal/state"
)

type scope struct {
	region   apple.Region
	category string
}

func (s scope) String() string { return s.region.Code + "/" + s.category }

// fetched 是一个 scope 一次成功抓取的结果。
type fetched struct {
	scope scope
	grid  *apple.Grid
}

type Runner struct {
	cfg    *config.Config
	client *apple.Client
	rules  *filter.Set
	notif  *notify.Multi
	render *notify.Renderer
	st     *state.State
	scopes []scope
	log    *slog.Logger
	dryRun bool

	// memCache 缓存详情页补齐来的内存,仅在 fill_missing_memory 开启时使用。
	memCache *apple.MemoryCache

	// rollbacks 是连续回滚的轮数,用于在推送永久性失败时放弃重试。见 maxRollbacks。
	rollbacks int

	// summaryAt 是日报触发时刻相对当天零点的偏移,nil 表示未启用。
	// 用指针而不是 -1 之类的哨兵:零值必须等于关闭,而 0 本身是合法的 00:00。
	summaryAt *time.Duration
	// summaryAttempts 是当前这份日报已经尝试送达的轮数。见 maxSummaryAttempts。
	summaryAttempts int
	// summaryFor 是 summaryAttempts 正在计的那次汇总的触发时刻,用于跨天归零。
	summaryFor time.Time
}

// staleSince 把「是否陈旧」与「最后更新时刻」合成日报用的一个字段:
// 零值即表示不陈旧,省掉一个只在另一个字段为真时才有意义的布尔。
func staleSince(stale bool, lastSeen time.Time) time.Time {
	if !stale {
		return time.Time{}
	}
	return lastSeen
}

// maxSummaryAttempts 是同一份日报最多尝试送达多少轮。
// 理由与 maxRollbacks 相同:渠道恒定失败时,120s 一轮会让它一天重试几百次。
// 放弃时只推进 LastSummaryAt 而不清零计数器,那批变动会并进下一份日报,数字不丢。
const maxSummaryAttempts = 3

// maxRollbacks 是同一批变动最多连续回滚多少轮。
//
// 回滚本身假设失败是暂时的(渠道宕机),但有些失败重试多少次都不会好:
// 正文超出 Telegram 的 4096 字上限、webhook 对某个载荷恒返 400。
// 此时若无限回滚,每一轮都会把同批事件里能送达的那几条再推一遍——
// 用户每个 interval 收一次重复通知,基线永不推进,新商品也跟着一起卡住。
// 攒够这么多轮后强制推进并把丢失的事件记进 ERROR 日志:
// 丢一批通知是坏结果,无限刷屏是更坏的结果。
const maxRollbacks = 5

type Options struct {
	Config   *config.Config
	Client   *apple.Client
	Rules    *filter.Set
	Notifier *notify.Multi
	State    *state.State
	Logger   *slog.Logger
	// DryRun 时既不推送也不落盘。写状态同样是副作用:
	// 一次 dry-run 若恰好撞上真实降价,该降价会被吸收进基线,
	// 正式进程从此再也不会推送它。
	DryRun bool
}

func NewRunner(opt Options) (*Runner, error) {
	cfg := opt.Config
	scopes := make([]scope, 0, len(cfg.Regions)*len(cfg.Categories))
	for _, code := range cfg.Regions {
		r, err := apple.LookupRegion(code)
		if err != nil {
			return nil, err
		}
		for _, c := range cfg.Categories {
			scopes = append(scopes, scope{region: r, category: c})
		}
	}
	// 配置在 Validate 阶段已经归一化过语言,这里不会再失败。
	lang, err := notify.ParseLang(cfg.Notify.Lang)
	if err != nil {
		return nil, err
	}
	r := &Runner{
		cfg: cfg, client: opt.Client, rules: opt.Rules, notif: opt.Notifier,
		render: notify.NewRenderer(lang, cfg.Notify.Group),
		st:     opt.State, scopes: scopes, log: opt.Logger, dryRun: opt.DryRun,
		memCache: apple.NewMemoryCache(),
	}
	if at := cfg.Notify.DailySummary; at != "" {
		// 同样已在 Validate 阶段校验过格式。
		d, err := config.ParseDailySummary(at)
		if err != nil {
			return nil, err
		}
		r.summaryAt = &d
	}
	return r, nil
}

// Run 阻塞执行监控循环,直到 ctx 被取消。
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("开始监控",
		"scopes", len(r.scopes), "interval", r.cfg.Interval.Std().String(),
		"channels", r.notif.Names(), "state", r.cfg.StatePath)

	if err := r.RunOnce(ctx); err != nil {
		// 首轮尚未结束就收到退出信号时,与 ticker 分支保持一致:
		// 正常落盘退出,不要让 systemd 把它记成启动失败。
		if ctx.Err() != nil {
			return r.save()
		}
		return err
	}

	ticker := time.NewTicker(r.cfg.Interval.Std())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Info("收到退出信号,保存状态后退出")
			return r.save()
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return r.save()
				}
				// 单轮失败不终止长驻进程,下一轮继续。
				r.log.Error("本轮监控失败", "err", err)
			}
		}
	}
}

// RunOnce 执行一轮完整的抓取—比对—通知。
//
// 刻意分成「先抓全、再统一比对」两阶段:首轮若有任何地区/分类不可用,
// 必须在写入任何状态之前失败退出,否则会留下一份只覆盖部分范围的基线。
func (r *Runner) RunOnce(ctx context.Context) error {
	var results []fetched
	var fatal []error

	for i, sc := range r.scopes {
		if err := r.pause(ctx, i); err != nil {
			return err
		}

		grid, err := r.client.FetchGrid(ctx, sc.region, sc.category)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return ctx.Err()

			case errors.Is(err, apple.ErrCategoryNotAvailable):
				// 该地区根本没有这个分类,重试再多次也没用。
				// 从未成功抓取过则判定为配置写错,直接退出;
				// 曾经正常过则可能是 Apple 临时下线了分类,不该让长驻进程死掉。
				if !r.st.IsBootstrapped(sc.region.Code, sc.category) {
					fatal = append(fatal, fmt.Errorf("%s: 该地区不提供此分类,请从 categories 中移除或改用其他地区", sc))
					continue
				}
				r.log.Error("分类不可用,本轮跳过", "scope", sc.String())

			case errors.Is(err, apple.ErrNoBootstrap):
				// 页面结构变了或该分类当前为空。绝不能据此判定商品下架,
				// 因此这里直接跳过而不是当成「抓到空列表」。
				r.log.Warn("页面中没有商品数据,本轮跳过该分类(可能是上游改版)", "scope", sc.String())

			default:
				r.log.Error("抓取失败,本轮跳过该分类", "scope", sc.String(), "err", err)
			}
			continue
		}

		if grid.Skipped > 0 {
			r.log.Warn("部分商品价格无法解析已跳过", "scope", sc.String(), "skipped", grid.Skipped)
		}
		r.log.Debug("抓取完成", "scope", sc.String(), "products", len(grid.Products))
		if err := r.fillMissingMemory(ctx, sc, grid); err != nil {
			return err
		}
		results = append(results, fetched{scope: sc, grid: grid})
	}

	if len(fatal) > 0 {
		return fmt.Errorf("配置校验未通过:\n  %w", errors.Join(fatal...))
	}
	if len(results) == 0 {
		return fmt.Errorf("本轮所有地区/分类均抓取失败")
	}
	return r.settle(ctx, results, time.Now())
}

// settle 是 RunOnce 的第二阶段:比对、推送、落盘,最后视情况发出日报。
// 与抓取分离既是为了「先抓全再统一比对」,也让这段无网络依赖的逻辑可以被测试覆盖。
//
// 日报刻意放在本轮推送成败之外:dispatch 失败最典型的场景是「某条事件的载荷被拒」
// (正文超长、webhook 对该载荷恒返 400,正是 maxRollbacks 存在的理由),
// 这时渠道本身是好的,短小的日报照样发得出去——而那恰恰是最需要它的时候。
// 把它挂在成功路径上,会让系统出问题的那几轮正好没有活性信号。
func (r *Runner) settle(ctx context.Context, results []fetched, now time.Time) error {
	err := r.reconcile(ctx, results, now)
	if sErr := r.maybeDailySummary(ctx, now); err == nil {
		err = sErr
	}
	return err
}

func (r *Runner) reconcile(ctx context.Context, results []fetched, now time.Time) error {
	// Apply 会原地推进内存基线,推送失败后仅仅跳过落盘并不能让下一轮重新产生这批事件。
	// 因此先留一份快照,推送没能全部送达时整体回滚。
	snapshot := r.st.Clone()

	var events []state.Event
	baselined, baselinedItems := 0, 0
	for _, res := range results {
		region, category := res.scope.region.Code, res.scope.category
		// Apply 会自行判断该范围是否首次成功抓取,首次只落基线不产生事件。
		fresh := !r.st.IsBootstrapped(region, category)
		events = append(events, r.st.Apply(region, category, res.grid.Products, now)...)
		// 以 Apply 之后的实际状态为准:抓到空列表时它会攒够 emptyStreakThreshold
		// 轮才真正建立基线,此前报「已建立基线」是假的。
		if fresh && r.st.IsBootstrapped(region, category) {
			baselined++
			baselinedItems += len(res.grid.Products)
		}
	}
	if baselined > 0 {
		r.log.Info("已为新范围建立基线,这些范围本轮不发送通知",
			"scopes", baselined, "products", baselinedItems)
	}

	// 计数放在 Apply 之后、推送之前:它与基线同属一份状态,
	// 推送全败回滚时一并退回,下一轮重新产生的同一批事件才不会被计两次。
	r.st.CountEvents(events)

	matched, delivered := r.dispatch(ctx, events)
	if !delivered {
		r.rollbacks++
		if r.rollbacks > maxRollbacks {
			// 连续回滚这么多轮,失败几乎不可能是暂时的。强制推进,
			// 否则会无限重推同一批事件。丢掉的通知记进日志,让运维查得到。
			r.log.Error("连续多轮未能送达,判定为永久性失败,放弃这批通知并推进基线",
				"rounds", r.rollbacks, "dropped", len(events))
			r.rollbacks = 0
			return r.save()
		}
		// 有事件一条渠道都没送出去(例如 Bark 宕机)。回滚到本轮开始前的基线,
		// 让这批变动下一轮重新产生并重试推送,而不是被永久吞掉。
		r.st.Restore(snapshot)
		r.log.Error("本轮存在未送达的通知,已回滚基线,下一轮将重试",
			"rounds", r.rollbacks, "max", maxRollbacks)
		return nil
	}
	r.rollbacks = 0
	r.st.CountPushed(matched)
	return r.save()
}

// sameDay 判断两个时刻是否落在 loc 时区的同一天。
func sameDay(a, b time.Time, loc *time.Location) bool {
	x, y := a.In(loc), b.In(loc)
	return x.Year() == y.Year() && x.YearDay() == y.YearDay()
}

// staleAfter 是「该范围的数据多久没更新就算陈旧」。取三轮,与 emptyStreakThreshold
// 同样的理由:单轮抓取失败多半是抖动,连续三轮没拿到才值得在日报里说。
func (r *Runner) staleAfter() time.Duration {
	iv := r.cfg.Interval.Std()
	if iv <= 0 {
		iv = 120 * time.Second
	}
	return 3 * iv
}

// maybeDailySummary 在到点且今天尚未汇总时发出日报。
//
// 它不做任何抓取,只把已有状态读一遍。用途是把「手机很安静」这个二义信号
// 变成单义:日报到了说明抓取与推送链路都通,没到就是系统坏了。
// 其中的命中规则数还能暴露规则写错——维度键名抄错、型号猜错的表现正是它长期为 0。
//
// 送达失败不回滚基线:日报是派生信息,为它回滚会让本轮的真实事件
// 下一轮重复推送一遍。失败只是不推进 LastSummaryAt,下一轮再试。
func (r *Runner) maybeDailySummary(ctx context.Context, now time.Time) error {
	// dry-run 连这份状态也不能碰:推进 LastSummaryAt 会让正式进程当天不再汇总,
	// 清零计数则会把那批变动从下一份日报里抹掉——都属于约束「dry-run 无副作用」。
	if r.summaryAt == nil || r.dryRun {
		return nil
	}
	loc := now.Location()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	due := midnight.Add(*r.summaryAt)

	// 判重按自然日而不是与 due 比大小:首份日报可能发在 due 之前(见下),
	// 那之后再拿 due 判断就会在同一天里发出第二份。
	if !r.st.LastSummaryAt.IsZero() {
		if sameDay(r.st.LastSummaryAt, now, loc) {
			return nil
		}
		if now.Before(due) {
			return nil
		}
	}
	// LastSummaryAt 为零表示从未汇总过,此时不等到点就发一份:
	// 它是「装好了,确实在跑」的确认,也让人第一眼看到规则当前命中几件。
	// 部署在当天 due 之前时,靠上面的自然日判重保证当天不会再发第二份。

	// 重试计数是针对「某一次汇总」的。不按 due 归零的话,昨天失败一次的余额
	// 会留给今天,今天就只剩两次机会——渠道抖一下就被判定为永久失败。
	if !r.summaryFor.Equal(due) {
		r.summaryFor = due
		r.summaryAttempts = 0
	}

	scopes := make([]notify.SummaryScope, 0, len(r.scopes))
	for _, sc := range r.scopes {
		region, category := sc.region.Code, sc.category
		entries := r.st.ScopeEntries(region, category)
		matches := 0
		var lastSeen time.Time
		for _, e := range entries {
			p := e.Product()
			if ok, _ := r.rules.Match(p, filter.ParseSpec(p.Title)); ok {
				matches++
			}
			if e.LastSeen.After(lastSeen) {
				lastSeen = e.LastSeen
			}
		}
		// 抓取失败的范围会被 RunOnce 跳过,它的商品因此原样留在状态里。
		// 不标出来的话,一个连着几天抓不到的范围在日报里与「一切正常但没变动」
		// 一模一样——那正是这个功能要消除的二义,不能在范围粒度上又放回来。
		stale := !lastSeen.IsZero() && now.Sub(lastSeen) > r.staleAfter()
		scopes = append(scopes, notify.SummaryScope{
			Region: region, Category: category,
			InStock:     len(entries),
			Counter:     r.st.ScopeCounter(region, category),
			RuleMatches: matches,
			StaleSince:  staleSince(stale, lastSeen),
		})
	}

	sent, err := r.notif.Send(ctx, r.render.DailySummary(now, r.st.CountersSince, scopes))
	if err != nil {
		r.log.Warn("日报推送存在失败渠道", "err", err)
	}
	if sent == 0 {
		r.summaryAttempts++
		if r.summaryAttempts < maxSummaryAttempts {
			r.log.Warn("日报未送达任何渠道,下一轮重试",
				"attempts", r.summaryAttempts, "max", maxSummaryAttempts)
			return nil
		}
		// 放弃本次汇总。计数器刻意不清零:这批变动会并进下一份日报,
		// 「自上次汇总以来」的口径靠 CountersSince 保持准确。
		r.log.Error("日报连续未送达,放弃本次汇总,计数并入下一份",
			"attempts", r.summaryAttempts)
		r.summaryAttempts = 0
		r.st.LastSummaryAt = now
		return r.save()
	}

	r.summaryAttempts = 0
	r.st.LastSummaryAt = now
	r.st.ResetCounters(now)
	r.log.Info("已发送日报", "scopes", len(scopes))
	return r.save()
}

// dispatch 过滤事件并推送。规则匹配放在 diff 之后:
// 状态库始终记录全部商品,这样日后放宽规则时,早已在架的商品不会被误报成新上架。
//
// 返回实际推送出去的事件,以及是否全部送达;后者为 false 表示有消息
// 一个渠道都没送出去,调用方据此回滚基线。
// 逐条推送时只要有一条全败就算整轮失败:回滚是整轮粒度的,
// 下一轮同批事件会重新产生,已送达的那几条因此可能重复推送一次——
// 重复优于永久丢失。
func (r *Runner) dispatch(ctx context.Context, events []state.Event) ([]state.Event, bool) {
	if len(events) == 0 {
		r.log.Info("本轮无变化")
		return nil, true
	}

	matched := make([]state.Event, 0, len(events))
	for _, ev := range events {
		spec := filter.ParseSpec(ev.Product.Title)
		ok, names := r.rules.Match(ev.Product, spec)
		if !ok {
			continue
		}
		ev.Rules = names
		matched = append(matched, ev)
	}

	r.log.Info("检测到变化", "events", len(events), "matched", len(matched))
	if len(matched) == 0 {
		return nil, true
	}

	// 上架优先展示,其次降价,最后下架。
	sort.SliceStable(matched, func(i, j int) bool {
		return kindRank(matched[i].Kind) < kindRank(matched[j].Kind)
	})

	if len(matched) > r.cfg.Notify.DigestThreshold {
		sent, err := r.notif.Send(ctx, r.render.Digest(matched))
		if err != nil {
			r.log.Error("摘要推送存在失败渠道", "err", err)
		}
		return matched, sent > 0
	}

	undelivered := 0
	for _, ev := range matched {
		sent, err := r.notif.Send(ctx, r.render.Event(ev))
		if err != nil {
			r.log.Error("事件推送存在失败渠道", "part_number", ev.Product.PartNumber, "err", err)
		}
		if sent == 0 {
			undelivered++
		}
	}
	if undelivered > 0 {
		r.log.Error("部分事件未送达任何渠道", "undelivered", undelivered, "total", len(matched))
		return matched, false
	}
	return matched, true
}

func kindRank(k state.EventKind) int {
	switch k {
	case state.EventListed:
		return 0
	case state.EventPriceDrop:
		return 1
	default:
		return 2
	}
}

// pause 在同一轮的相邻请求之间插入随机停顿,避免十几个请求在同一瞬间齐发。
func (r *Runner) pause(ctx context.Context, i int) error {
	if i == 0 {
		return nil
	}
	return r.delay(ctx)
}

func (r *Runner) delay(ctx context.Context) error {
	base := r.cfg.HTTP.DelayMin.Std()
	spread := r.cfg.HTTP.DelayMax.Std() - base
	d := r.client.Jitter(base, spread)
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (r *Runner) save() error {
	if r.dryRun {
		r.log.Info("dry-run:跳过状态落盘")
		return nil
	}
	if err := state.Save(r.cfg.StatePath, r.st); err != nil {
		return fmt.Errorf("保存状态: %w", err)
	}
	return nil
}

// ListDimensions 打印各地区/分类当前可用的过滤维度键与取值,供用户编写规则。
//
// skeleton 控制是否附带可粘贴的规则骨架。默认不打印:这个命令的日常用途是
// 「现在有哪些取值」与「芯片解析还正常吗」,骨架对这两件事都是噪音,
// 而且每个 scope 十几行,地区一多就把维度表淹没了。骨架只在初次写规则时有用。
func (r *Runner) ListDimensions(ctx context.Context, skeleton bool, out func(string)) error {
	for i, sc := range r.scopes {
		if err := r.pause(ctx, i); err != nil {
			return err
		}
		grid, err := r.client.FetchGrid(ctx, sc.region, sc.category)
		if err != nil {
			out(fmt.Sprintf("\n===== %s =====\n  抓取失败: %v", sc, err))
			continue
		}

		out(fmt.Sprintf("\n===== %s(%d 件)=====", sc, len(grid.Products)))
		idx := indexDimensions(grid)
		for _, k := range idx.keys {
			out(formatDimension(k, idx.legend[k], idx.values[k]))
		}
		if len(idx.chips) > 0 {
			out(formatDimension("chips", "芯片", idx.chips))
		}

		if skeleton {
			out("\n  ----- 规则骨架(整段复制到配置的 rules: 下,再删掉不要的取值)-----")
			out(formatRuleSkeleton(sc, idx))
		}
	}
	// 不打印骨架时提示它的存在,否则这个功能没人会发现。
	if !skeleton {
		out("\n(加 -skeleton 可附带打印可直接粘贴到 rules: 下的规则骨架)")
	}
	return nil
}

// dimensionIndex 是一个 scope 内全部商品的维度取值汇总。
// 上面的表格与下面的规则骨架同源于它,免得两处各自遍历一遍商品而列出不同的取值。
type dimensionIndex struct {
	// keys 是维度键的展示顺序。含 legends 声明了但当前无商品命中的键——
	// 表格照常列出它(信息是「有这个维度,只是眼下没货」),但骨架会跳过。
	keys   []string
	legend map[string]string
	values map[string][]string
	chips  []string
}

// indexDimensions 汇总维度取值。键的顺序照搬上游 legends 的展示顺序,
// legends 未声明的键按字母序补在后面。
func indexDimensions(grid *apple.Grid) dimensionIndex {
	sets := map[string]map[string]bool{}
	for _, p := range grid.Products {
		for k, v := range p.Dimensions {
			if sets[k] == nil {
				sets[k] = map[string]bool{}
			}
			sets[k][v] = true
		}
	}

	idx := dimensionIndex{
		legend: make(map[string]string, len(grid.Legends)),
		values: make(map[string][]string, len(sets)),
	}
	seen := map[string]bool{}
	for _, lg := range grid.Legends {
		if seen[lg.Key] {
			continue
		}
		seen[lg.Key] = true
		idx.legend[lg.Key] = lg.Legend
		idx.keys = append(idx.keys, lg.Key)
	}
	for _, k := range sortedKeys(sets) {
		if !seen[k] {
			idx.keys = append(idx.keys, k)
		}
	}
	for _, k := range idx.keys {
		idx.values[k] = sortedKeys(sets[k])
	}

	chips := map[string]bool{}
	for _, p := range grid.Products {
		if c := filter.ParseSpec(p.Title).Chip; c != "" {
			chips[c] = true
		}
	}
	idx.chips = sortedKeys(chips)
	return idx
}

// formatRuleSkeleton 把当前在售的取值拼成可直接粘贴到配置 rules: 下的片段。
// 上面的表格是给人读的,这段是给人复制的——否则用户得对着表格把几十个取值手工誊一遍,
// 而维度键名(refurbClearModel、dimensionCaseMaterial)恰恰是最容易抄错的东西。
//
// 骨架列出全部取值,等价于「不过滤」:留给用户做的是删减,不是补全。
func formatRuleSkeleton(sc scope, idx dimensionIndex) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %s %s\n", sc.region.Code, sc.category)
	fmt.Fprintf(&b, "    regions: [%s]\n", sc.region.Code)
	fmt.Fprintf(&b, "    categories: [%s]\n", sc.category)

	var dims []string
	for _, k := range idx.keys {
		if len(idx.values[k]) > 0 {
			dims = append(dims, k)
		}
	}
	if len(dims) > 0 {
		b.WriteString("    dimensions:\n")
		for _, k := range dims {
			fmt.Fprintf(&b, "      %s: [%s]\n", k, yamlFlowSeq(idx.values[k]))
		}
	}
	if len(idx.chips) > 0 {
		fmt.Fprintf(&b, "    chips: [%s]", yamlFlowSeq(idx.chips))
	}
	// 刻意不再附 "# min_cpu_cores: 12"、"# max_price: 20000" 这类提示行:
	// 骨架其余每一行都来自当前真实在售的商品,读者会合理地认为整段都是这个性质,
	// 而那两个数字与当前数据、与用户的需求都毫无关系,纯粹是从示例配置抄来的。
	// 实测已经误导过用户(「为什么会有个最大价格 2 万」),更糟的是顺手去掉 # 之后,
	// max_price: 20000 会把这个项目最值得监控的高配机型静默挡在门外。
	// 这些字段的说明属于 README 的规则字段速查表——文档里写「这个字段存在」是说明,
	// 数据输出里写「= 20000」则像是个结论。
	//
	// 去掉末尾换行:每个维度行都自带 \n,调用方 out() 还会再加一个,
	// 不修掉的话每段骨架后面会多出一个空行。
	return strings.TrimRight(b.String(), "\n")
}

// yamlFlowSeq 把取值拼成 flow 序列。取值直接来自上游,含逗号或冒号时裸写会改变
// YAML 语义,而这段是给人整段复制的——吐出解析不了的片段等于白给。
func yamlFlowSeq(values []string) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = yamlFlowScalar(v)
	}
	return strings.Join(quoted, ", ")
}

// yamlFlowScalar 只在裸写会有歧义时加引号。一律加引号也正确,
// 但骨架是给人读和改的,满屏的 "macbookpro" 是白白多出来的噪声。
func yamlFlowScalar(v string) string {
	// & * ! | > % @ ` 仅在标量开头才有特殊含义,这里不做位置区分:
	// 维度取值是 slug,误判的代价只是多一对引号,漏判的代价是片段解析失败。
	if v == "" || v != strings.TrimSpace(v) || strings.ContainsAny(v, ",[]{}:#&*!|>'\"%@"+"`") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
	}
	return v
}

// formatDimension 用逗号分隔取值。取值本身可能含空格(如芯片名 "M4 Max"),
// 直接用 %v 打印切片会让人分不清那是一个值还是两个。
func formatDimension(key, legend string, values []string) string {
	label := ""
	if legend != "" {
		label = "(" + legend + ")"
	}
	return fmt.Sprintf("  %-22s %-10s %s", key, label, strings.Join(values, ", "))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fillMissingMemory 为列表页没给内存维度的商品补抓详情页。
//
// 上游数据不一致:同一批 MacBook Pro 里 14 英寸机型带 tsMemorySize,
// 16 英寸的 M5 Pro / M5 Max 不带(实测 CN 站 106 件里有 37 件如此)。
// 而过滤规则把维度缺失判为不匹配,不补的话「内存 32GB 以上」这类规则
// 会静默漏掉整整一档机型——用户看不到任何异常,只是永远收不到通知。
//
// 三条边界:
//   - 详情页失败绝不影响商品本身。补不到就保持维度缺失,商品照常参与 diff,
//     否则一次详情页 5xx 会让几十台机器凭空「下架」。
//   - 结果按货号缓存,但**只缓存永久性失败**。同一货号配置固定,查到了就不必再查;
//     解析失败也不必再查。而超时、5xx 这类传输故障必须留到下一轮重试:
//     把一次抖动记成「查过没查到」,会让这台机器在整个进程生命周期里再也不被补齐,
//     按内存过滤的规则从此静默漏掉它——正是本功能要消除的那个问题。
//   - 只有 ctx 取消才向上返回错误,与抓取列表页时的处理保持一致。
func (r *Runner) fillMissingMemory(ctx context.Context, sc scope, grid *apple.Grid) error {
	if !r.cfg.HTTP.FillMissingMemory {
		return nil
	}
	// 没有任何规则按内存过滤时,补齐不会改变任何推送结果,这些请求全是白发的。
	if !r.rules.UsesDimension(apple.MemoryDimension) {
		return nil
	}
	// 只在这个分类本来就有内存维度、仅个别商品缺失时才补。
	// 否则 watch 这种压根没有内存概念的分类会让每一件商品都白抓一次详情页。
	if !scopeHasMemory(grid.Products) {
		return nil
	}

	// unreadable 是本轮判定为「永久读不出」的件数,retryable 是暂时失败、下一轮会重试的件数。
	// 分开计数才能让日志只在情况有变时说话:known 那批每轮都会命中缓存,
	// 若把它们也算进告警,常驻进程会每个 interval 重复喊一遍同样的话。
	var filled, unreadable, retryable, known, skipped int
	for i := range grid.Products {
		p := &grid.Products[i]
		if p.Dimensions[apple.MemoryDimension] != "" {
			continue
		}
		// 机型、芯片、容量、价格列表页都已给全,凭它们就能判定不可能命中的商品,
		// 再去看详情页也是白看:内存是它唯一还没定的条件,而其余条件已经否决了它。
		// 实测这一步把 CN mac 的补齐请求从 45 个降到 20 个。
		if !r.rules.MayMatchWithout(*p, filter.ParseSpec(p.Title), apple.MemoryDimension) {
			skipped++
			continue
		}
		// 没有容量锚点就无从把存储条目排除,详情页拿回来也解析不出内存
		// (见 ParseOverviewMemory),这个请求注定失败。实测 CN mac 的 13 件
		// Studio Display 正属此类:内存与容量两个维度它都没有。
		if p.Dimensions[apple.CapacityDimension] == "" {
			skipped++
			continue
		}

		mem, cached := r.memCache.Get(p.PartNumber)
		if cached {
			if mem == "" {
				// 之前已判定为永久读不出,不再发请求也不再告警。
				known++
				continue
			}
		} else {
			if err := r.delay(ctx); err != nil {
				return err
			}
			var err error
			mem, err = r.client.FetchMemory(ctx, sc.region, p.URL, p.Dimensions[apple.CapacityDimension])
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				switch {
				case apple.IsPermanentMemoryFailure(err):
					// 页面本身读不出内存,重试多少轮都是同一个结果,记住别再查。
					r.memCache.Put(p.PartNumber, "")
					unreadable++
				case r.memCache.Fail(p.PartNumber):
					// 连续多轮都是可重试故障,再试下去也只是白发请求。
					r.log.Warn("详情页连续多轮抓取失败,放弃补齐该商品的内存",
						"scope", sc.String(), "part", p.PartNumber, "err", err)
					unreadable++
				default:
					// 超时、5xx、连接重置:不入缓存,下一轮重试。
					retryable++
				}
				r.log.Debug("详情页未能补齐内存", "scope", sc.String(), "part", p.PartNumber, "err", err)
				continue
			}
			r.memCache.Put(p.PartNumber, mem)
		}

		// 维度 map 直接来自解析结果,可能是 nil。
		if p.Dimensions == nil {
			p.Dimensions = make(map[string]string, 1)
		}
		p.Dimensions[apple.MemoryDimension] = mem
		filled++
	}

	// 补不到就等于回到「按内存过滤的规则静默漏掉这档机型」,而这正是本功能要消除的问题,
	// 因此必须在默认日志级别(info)下看得见,不能只留在 Debug 里。
	// 只对本轮新出现的失败告警:known 那批每轮都命中缓存,一起算会变成每个 interval 刷一遍。
	if unreadable > 0 || retryable > 0 {
		r.log.Warn("部分商品的内存维度未能补齐,按内存过滤的规则会漏掉它们",
			"scope", sc.String(), "unreadable", unreadable, "retryable", retryable)
	}
	if filled > 0 || known > 0 || skipped > 0 {
		r.log.Debug("内存维度补齐完成", "scope", sc.String(), "filled", filled,
			"unreadable", unreadable, "retryable", retryable, "known", known,
			"skipped", skipped, "cached", r.memCache.Len())
	}
	return nil
}

// scopeHasMemory 判断这个分类是否使用内存维度。
// 判据是「同批里至少有一件带这个维度」,而不是硬编码分类白名单:
// 上游哪天给 watch 加上内存、或给 mac 换个键名,这里都不必跟着改。
func scopeHasMemory(products []apple.Product) bool {
	for _, p := range products {
		if p.Dimensions[apple.MemoryDimension] != "" {
			return true
		}
	}
	return false
}
