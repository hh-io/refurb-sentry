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
}

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
	return &Runner{
		cfg: cfg, client: opt.Client, rules: opt.Rules, notif: opt.Notifier,
		render: notify.NewRenderer(lang, cfg.Notify.Group),
		st:     opt.State, scopes: scopes, log: opt.Logger, dryRun: opt.DryRun,
		memCache: apple.NewMemoryCache(),
	}, nil
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

// settle 是 RunOnce 的第二阶段:比对、推送、落盘。
// 与抓取分离既是为了「先抓全再统一比对」,也让这段无网络依赖的逻辑可以被测试覆盖。
func (r *Runner) settle(ctx context.Context, results []fetched, now time.Time) error {
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

	if !r.dispatch(ctx, events) {
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
	return r.save()
}

// dispatch 过滤事件并推送。规则匹配放在 diff 之后:
// 状态库始终记录全部商品,这样日后放宽规则时,早已在架的商品不会被误报成新上架。
//
// 返回 false 表示有消息一个渠道都没送出去,调用方据此回滚基线。
// 逐条推送时只要有一条全败就算整轮失败:回滚是整轮粒度的,
// 下一轮同批事件会重新产生,已送达的那几条因此可能重复推送一次——
// 重复优于永久丢失。
func (r *Runner) dispatch(ctx context.Context, events []state.Event) bool {
	if len(events) == 0 {
		r.log.Info("本轮无变化")
		return true
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
		return true
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
		return sent > 0
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
		return false
	}
	return true
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
func (r *Runner) ListDimensions(ctx context.Context, out func(string)) error {
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

		out("\n  ----- 规则骨架(整段复制到配置的 rules: 下,再删掉不要的取值)-----")
		out(formatRuleSkeleton(sc, idx))
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
		fmt.Fprintf(&b, "    chips: [%s]\n", yamlFlowSeq(idx.chips))
	}
	// 这两项没有可枚举的取值,只能给注释行提示存在。YAML 注释粘贴过去照样合法。
	b.WriteString("    # min_cpu_cores: 12\n")
	b.WriteString("    # max_price: 20000")
	return b.String()
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
//   - 结果按货号缓存。同一货号配置固定,查一次就够;否则常驻进程每轮
//     都要重抓几十个详情页,把「一分类一请求」的设计彻底破坏掉。
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

	var filled, failed, skipped int
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

		mem, cached := r.memCache.Get(p.PartNumber)
		if !cached {
			if err := r.delay(ctx); err != nil {
				return err
			}
			var err error
			mem, err = r.client.FetchMemory(ctx, sc.region, p.URL, p.Dimensions[apple.CapacityDimension])
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// 空串同样入缓存:上游永远不给内存的机器不该每轮都被重试一遍。
				r.memCache.Put(p.PartNumber, "")
				r.log.Debug("详情页未能补齐内存", "scope", sc.String(), "part", p.PartNumber, "err", err)
				failed++
				continue
			}
			r.memCache.Put(p.PartNumber, mem)
		}
		if mem == "" {
			failed++
			continue
		}

		// 维度 map 直接来自解析结果,可能是 nil。
		if p.Dimensions == nil {
			p.Dimensions = make(map[string]string, 1)
		}
		p.Dimensions[apple.MemoryDimension] = mem
		filled++
	}

	if filled > 0 || failed > 0 || skipped > 0 {
		r.log.Debug("内存维度补齐完成", "scope", sc.String(),
			"filled", filled, "failed", failed, "skipped", skipped, "cached", r.memCache.Len())
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
