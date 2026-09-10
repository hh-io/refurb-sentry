// Command refurb-sentry 监控 Apple 官网翻新store的上架、降价与下架并实时推送。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hh-io/refurb-sentry/internal/app"
	"github.com/hh-io/refurb-sentry/internal/apple"
	"github.com/hh-io/refurb-sentry/internal/config"
	"github.com/hh-io/refurb-sentry/internal/filter"
	"github.com/hh-io/refurb-sentry/internal/notify"
	"github.com/hh-io/refurb-sentry/internal/state"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "configs/config.yaml", "配置文件路径")
		once        = flag.Bool("once", false, "只执行一轮后退出")
		dryRun      = flag.Bool("dry-run", false, "试运行:通知打印到标准输出,且不写入状态文件")
		listDims    = flag.Bool("list-dims", false, "列出各地区/分类当前可用的过滤维度与取值后退出")
		skeleton    = flag.Bool("skeleton", false, "配合 -list-dims:额外打印可直接粘贴到 rules: 下的规则骨架")
		showVersion = flag.Bool("version", false, "打印版本后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("refurb-sentry", version)
		return
	}

	if err := run(*configPath, *once, *dryRun, *listDims, *skeleton); err != nil {
		// 用 stderr 而非 slog:配置尚未加载时 logger 可能还不存在。
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(configPath string, once, dryRun, listDims, skeleton bool) error {
	cfg, warnings, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	for _, w := range warnings {
		log.Warn(w)
	}

	client, err := apple.NewClient(apple.ClientOptions{
		Proxy:      cfg.HTTP.Proxy,
		UserAgent:  cfg.HTTP.UserAgent,
		Timeout:    cfg.HTTP.Timeout.Std(),
		MaxRetries: cfg.HTTP.MaxRetries,
		Logger:     log,
	})
	if err != nil {
		return err
	}

	rules, err := filter.New(cfg.Rules)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM 触发 context 取消,由 Runner 负责落盘后再退出。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// -list-dims 只查询上游,既不读写状态也不推送,因此跳过渠道与状态库的准备。
	// -skeleton 单独给出时隐含 -list-dims:骨架本就由维度数据生成,不必要求两个参数一起写。
	if listDims || skeleton {
		runner, err := app.NewRunner(app.Options{
			Config: cfg, Client: client, Rules: rules,
			Notifier: notify.NewMulti(nil, log), State: state.New(), Logger: log,
		})
		if err != nil {
			return err
		}
		return runner.ListDimensions(ctx, skeleton, func(s string) { fmt.Println(s) })
	}

	notifiers, err := buildNotifiers(cfg, dryRun)
	if err != nil {
		return err
	}
	if len(notifiers) == 0 {
		return errors.New("没有可用的通知渠道:请在配置的 channels 中至少启用一个,或加 -dry-run 试运行")
	}

	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return err
	}

	runner, err := app.NewRunner(app.Options{
		Config: cfg, Client: client, Rules: rules,
		Notifier: notify.NewMulti(notifiers, log), State: st, Logger: log, DryRun: dryRun,
	})
	if err != nil {
		return err
	}

	if once {
		return runner.RunOnce(ctx)
	}
	return runner.Run(ctx)
}

func buildNotifiers(cfg *config.Config, dryRun bool) ([]notify.Notifier, error) {
	if dryRun {
		lang, err := notify.ParseLang(cfg.Notify.Lang)
		if err != nil {
			return nil, err
		}
		return []notify.Notifier{notify.NewConsole(os.Stdout, lang)}, nil
	}

	var out []notify.Notifier
	for i, ch := range cfg.Channels {
		if !ch.IsEnabled() {
			continue
		}
		switch ch.Type {
		case "bark":
			n, err := notify.NewBark(notify.BarkOptions{
				Name: ch.Name, Server: ch.Server, DeviceKey: ch.DeviceKey,
				Sound: ch.Sound, Icon: ch.Icon, Timeout: ch.Timeout.Std(),
			})
			if err != nil {
				return nil, fmt.Errorf("channels[%d]: %w", i, err)
			}
			out = append(out, n)
		case "webhook":
			n, err := notify.NewWebhook(notify.WebhookOptions{
				Name: ch.Name, URL: ch.URL, Method: ch.Method,
				Headers: ch.Headers, Body: ch.Body, Timeout: ch.Timeout.Std(),
			})
			if err != nil {
				return nil, fmt.Errorf("channels[%d]: %w", i, err)
			}
			out = append(out, n)
		}
	}
	return out, nil
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}
