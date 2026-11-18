// Command shimamura-telemetry は計算機サーバーのリソース情報を Shimamura IdP の監視 Worker へ送るエージェント。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/config"
	"github.com/ShimamuraLaboratory/shimamura-telemetry/internal/version"
)

const usage = `Usage: shimamura-telemetry <command> [options]

Commands:
  run       エージェントを起動する
  version   バージョンを表示する

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

	// TODO: collector（gopsutil / go-nvml）と transport（WebSocket）の実装ができたら
	// agent.New(collector, sender, cfg.Interval.Duration, logger).Run(ctx) を呼ぶ。
	_ = ctx
	logger.Error("collector and sender are not implemented yet")
	return 1
}
