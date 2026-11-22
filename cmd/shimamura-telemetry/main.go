// Command shimamura-telemetry は計算機サーバーのリソース情報を Shimamura IdP の監視 Worker へ送るエージェント。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/agent"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/config"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/logins"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/spec"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/transport"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/version"
)

const usage = `Usage: shimamura-telemetry <command> [options]

Commands:
  run            エージェントを起動する
  collect        リソース情報を 1 回収集して JSON で表示する (動作確認用)
  logins         ログイン中のユーザーと直近 24 時間のログイン・ログアウトを JSON で表示する (動作確認用)
  spec           ハードウェア情報 (CPU・メモリ・SSD・GPU) を JSON で表示する (動作確認用)
  check-config   設定ファイルを検証する
  version        バージョンを表示する

Run 'shimamura-telemetry run -h' for run options.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "run":
		return runAgent(args[1:], stderr)
	case "collect":
		return runCollect(stdout, stderr)
	case "check-config":
		return runCheckConfig(args[1:], stdout, stderr)
	case "logins":
		return runLogins(stdout, stderr)
	case "spec":
		return runSpec(stdout, stderr)
	case "version", "--version", "-v":
		_, _ = fmt.Fprintln(stdout, version.String())
		return 0
	case "help", "--help", "-h":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command: %s\n\n%s", args[0], usage)
		return 2
	}
}

// runCheckConfig は設定ファイルを読み込んで検証する。install.sh が新しい設定を配置する前に使う。
func runCheckConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "設定ファイルのパス")
	get := fs.String("get", "", "検証した上で、指定した項目（server_id・agent_token・url・interval）の値だけを表示する")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := config.CheckPermissions(*configPath); err != nil {
		_, _ = fmt.Fprintln(stderr, "warning:", err)
	}
	if *get == "" {
		_, _ = fmt.Fprintf(stdout, "%s: ok\n", *configPath)
		return 0
	}
	// install.sh が、省略されたオプションを既存の設定ファイルの値で補うために使う
	values := map[string]string{
		"server_id":   cfg.ServerID,
		"agent_token": cfg.AgentToken,
		"url":         cfg.URL,
		"interval":    cfg.Interval.String(),
	}
	value, ok := values[*get]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown key: %s\n", *get)
		return 2
	}
	_, _ = fmt.Fprintln(stdout, value)
	return 0
}

// runCollect はリソース情報を 1 回収集し、送信メッセージの measuredAt・metrics と同じ形で表示する。
// 設定ファイルは読まないため、インストール前の動作確認にも使える。
func runCollect(stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	snapshot, err := collector.NewSystem(logger).Collect(ctx)
	if err != nil {
		logger.Error("failed to collect", "error", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(struct {
		MeasuredAt int64             `json:"measuredAt"`
		Metrics    collector.Metrics `json:"metrics"`
	}{snapshot.CollectedAt.UnixMilli(), snapshot.Metrics}); err != nil {
		logger.Error("failed to encode", "error", err)
		return 1
	}
	return 0
}

func runAgent(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", config.DefaultPath, "設定ファイルのパス")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	if err := config.CheckPermissions(*configPath); err != nil {
		logger.Warn("config file permission check failed", "error", err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting shimamura-telemetry",
		"version", version.Version,
		"server_id", cfg.ServerID,
		"url", cfg.URL,
		"interval", cfg.Interval.Duration,
	)

	sender := transport.NewWebSocket(cfg.URL, cfg.AgentToken, logger)

	system := collector.NewSystem(logger)

	// ハードウェア情報とログイン履歴（utmp / wtmp は Linux のみ）は、メトリクスと同じ接続で別のメッセージとして送る。
	// 接続・再接続はメトリクスの送信が担い、これらは接続している間だけ送る
	var wg sync.WaitGroup
	wg.Go(func() { spec.NewReporter(sender, system, logger).Run(ctx) })
	if src := loginSource(logger); src != nil {
		wg.Go(func() { logins.NewReporter(sender, src, logger).Run(ctx) })
	}

	err = agent.New(system, sender, cfg.Interval.Duration, logger).Run(ctx)
	wg.Wait()
	if err != nil {
		logger.Error("agent stopped", "error", err)
		return 1
	}
	logger.Info("stopped shimamura-telemetry")
	return 0
}

// loginSource は utmp・wtmp を読む Source を返す。Linux 以外や utmp が無い場合は nil（ログイン履歴を送らない）。
func loginSource(logger *slog.Logger) *logins.Source {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := os.Stat(logins.DefaultUtmpPath); err != nil {
		logger.Info("login history is disabled", "reason", err)
		return nil
	}
	src, err := logins.NewSource(logins.DefaultUtmpPath, logins.DefaultWtmpPath)
	if err != nil {
		logger.Info("login history is disabled", "reason", err)
		return nil
	}
	return src
}

// runSpec はハードウェア情報を、送るメッセージの spec と同じ形で表示する。
func runSpec(stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := collector.NewSystem(logger).Spec(ctx)
	if err != nil {
		logger.Error("failed to read the hardware spec", "error", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		logger.Error("failed to encode", "error", err)
		return 1
	}
	return 0
}

// runLogins は今のセッションと直近 24 時間のイベントを、送るメッセージの形で表示する。
func runLogins(stdout, stderr io.Writer) int {
	if runtime.GOOS != "linux" {
		_, _ = fmt.Fprintln(stderr, "login history is only available on Linux")
		return 1
	}
	src, err := logins.NewSource(logins.DefaultUtmpPath, logins.DefaultWtmpPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	msgs, err := logins.Snapshot(src, time.Now())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(msgs); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
