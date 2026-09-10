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
}

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
	type result struct {
		scope scope
		grid  *apple.Grid
	}
	var results []result
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
		results = append(results, result{scope: sc, grid: grid})
	}

	if len(fatal) > 0 {
		return fmt.Errorf("配置校验未通过:\n  %w", errors.Join(fatal...))
	}
	if len(results) == 0 {
		return fmt.Errorf("本轮所有地区/分类均抓取失败")
	}

	now := time.Now()
	var events []state.Event
	baselined, baselinedItems := 0, 0
	for _, res := range results {
		region, category := res.scope.region.Code, res.scope.category
		// Apply 会自行判断该范围是否首次成功抓取,首次只落基线不产生事件。
		fresh := !r.st.IsBootstrapped(region, category)
		events = append(events, r.st.Apply(region, category, res.grid.Products, now)...)
		if fresh {
			baselined++
			baselinedItems += len(res.grid.Products)
		}
	}
	if baselined > 0 {
		r.log.Info("已为新范围建立基线,这些范围本轮不发送通知",
			"scopes", baselined, "products", baselinedItems)
	}

	if !r.dispatch(ctx, events) {
		// 一条都没送出去(例如 Bark 宕机)。此时不推进基线,
		// 让这批变动在下一轮重新产生并重试推送,而不是被永久吞掉。
		r.log.Error("本轮通知全部推送失败,暂不保存状态,下一轮将重试")
		return nil
	}
	return r.save()
}

// dispatch 过滤事件并推送。规则匹配放在 diff 之后:
// 状态库始终记录全部商品,这样日后放宽规则时,早已在架的商品不会被误报成新上架。
// dispatch 返回 false 表示有消息需要推送、但没有任何一个渠道成功。
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

	delivered := 0
	for _, ev := range matched {
		sent, err := r.notif.Send(ctx, r.render.Event(ev))
		if err != nil {
			r.log.Error("事件推送存在失败渠道", "part_number", ev.Product.PartNumber, "err", err)
		}
		delivered += sent
	}
	return delivered > 0
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
		values := map[string]map[string]bool{}
		for _, p := range grid.Products {
			for k, v := range p.Dimensions {
				if values[k] == nil {
					values[k] = map[string]bool{}
				}
				values[k][v] = true
			}
		}
		for _, lg := range grid.Legends {
			out(formatDimension(lg.Key, lg.Legend, sortedKeys(values[lg.Key])))
			delete(values, lg.Key)
		}
		for _, k := range sortedKeys(values) {
			out(formatDimension(k, "", sortedKeys(values[k])))
		}

		chips := map[string]bool{}
		for _, p := range grid.Products {
			if c := filter.ParseSpec(p.Title).Chip; c != "" {
				chips[c] = true
			}
		}
		if len(chips) > 0 {
			out(formatDimension("chips", "芯片", sortedKeys(chips)))
		}
	}
	return nil
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
