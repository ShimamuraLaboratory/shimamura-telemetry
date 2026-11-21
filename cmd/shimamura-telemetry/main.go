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
	"syscall"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/agent"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/collector"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/config"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/transport"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/version"
)

const usage = `Usage: shimamura-telemetry <command> [options]

Commands:
  run            エージェントを起動する
  collect        リソース情報を 1 回収集して JSON で表示する (動作確認用)
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
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if _, err := config.Load(*configPath); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := config.CheckPermissions(*configPath); err != nil {
		_, _ = fmt.Fprintln(stderr, "warning:", err)
	}
	_, _ = fmt.Fprintf(stdout, "%s: ok\n", *configPath)
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
	if err := agent.New(collector.NewSystem(logger), sender, cfg.Interval.Duration, logger).Run(ctx); err != nil {
		logger.Error("agent stopped", "error", err)
		return 1
	}
	logger.Info("stopped shimamura-telemetry")
	return 0
}
