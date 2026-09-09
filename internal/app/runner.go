package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hh/refurb-sentry/internal/apple"
	"github.com/hh/refurb-sentry/internal/config"
	"github.com/hh/refurb-sentry/internal/filter"
	"github.com/hh/refurb-sentry/internal/notify"
	"github.com/hh/refurb-sentry/internal/state"
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
	st     *state.State
	scopes []scope
	log    *slog.Logger
}

func NewRunner(cfg *config.Config, client *apple.Client, rules *filter.Set,
	notif *notify.Multi, st *state.State, log *slog.Logger) (*Runner, error) {

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
	return &Runner{cfg: cfg, client: client, rules: rules, notif: notif, st: st, scopes: scopes, log: log}, nil
}

// Run 阻塞执行监控循环,直到 ctx 被取消。
func (r *Runner) Run(ctx context.Context) error {
	r.log.Info("开始监控",
		"scopes", len(r.scopes), "interval", r.cfg.Interval.Std().String(),
		"channels", r.notif.Names(), "state", r.cfg.StatePath)

	if err := r.RunOnce(ctx); err != nil {
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
	firstRound := !r.st.Bootstrapped

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
				// 该地区根本没有这个分类,属配置错误,重试再多次也没用。
				if firstRound {
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
	for _, res := range results {
		events = append(events, r.st.Apply(res.scope.region.Code, res.scope.category, res.grid.Products, now)...)
	}

	if firstRound {
		r.st.Bootstrapped = true
		total := 0
		for _, res := range results {
			total += len(res.grid.Products)
		}
		// 首轮把在售商品全部记为基线。若此时推送,数百件在架商品会一次性涌出。
		r.log.Info("首轮基线已建立,本轮不发送通知", "products", total, "scopes", len(results))
		return r.save()
	}

	r.dispatch(ctx, events)
	return r.save()
}

// dispatch 过滤事件并推送。规则匹配放在 diff 之后:
// 状态库始终记录全部商品,这样日后放宽规则时,早已在架的商品不会被误报成新上架。
func (r *Runner) dispatch(ctx context.Context, events []state.Event) {
	if len(events) == 0 {
		r.log.Info("本轮无变化")
		return
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
		return
	}

	// 上架优先展示,其次降价,最后下架。
	sort.SliceStable(matched, func(i, j int) bool {
		return kindRank(matched[i].Kind) < kindRank(matched[j].Kind)
	})

	group := r.cfg.Notify.Group
	if len(matched) > r.cfg.Notify.DigestThreshold {
		if err := r.notif.Send(ctx, notify.RenderDigest(matched, group)); err != nil {
			r.log.Error("摘要推送存在失败渠道", "err", err)
		}
		return
	}
	for _, ev := range matched {
		if err := r.notif.Send(ctx, notify.RenderEvent(ev, group)); err != nil {
			r.log.Error("事件推送存在失败渠道", "part_number", ev.Product.PartNumber, "err", err)
		}
	}
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
			vs := sortedKeys(values[lg.Key])
			out(fmt.Sprintf("  %-24s %-10s %v", lg.Key, "("+lg.Legend+")", vs))
			delete(values, lg.Key)
		}
		for _, k := range sortedKeys(values) {
			out(fmt.Sprintf("  %-24s %-10s %v", k, "", sortedKeys(values[k])))
		}

		chips := map[string]bool{}
		for _, p := range grid.Products {
			if c := filter.ParseSpec(p.Title).Chip; c != "" {
				chips[c] = true
			}
		}
		if len(chips) > 0 {
			out(fmt.Sprintf("  %-24s %-10s %v", "chips", "(芯片)", sortedKeys(chips)))
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
