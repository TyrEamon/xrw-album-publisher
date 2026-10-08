package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/config"
	"github.com/TyrEamon/xrw-album/publisher/internal/legacy"
	"github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.Load()
	fatalIf(logger, err)
	databasePath := env("LEGACY_DB", "/var/lib/xrw-publisher/legacy.db")
	workDir := env("LEGACY_WORK_DIR", "/var/lib/xrw-publisher/legacy-work")
	sourcePath := env("LEGACY_SOURCE", "/var/lib/xrw-publisher/linuxdo-85w.txt")
	database, err := legacy.Open(databasePath)
	fatalIf(logger, err)
	defer database.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "init":
		flags := flag.NewFlagSet("init", flag.ExitOnError)
		source := flags.String("source", sourcePath, "linuxdo-85w source text")
		_ = flags.Parse(os.Args[2:])
		albums, images, err := database.SyncSource(ctx, *source, cfg.TGChatIDs)
		fatalIf(logger, err)
		logger.Info("legacy source synchronized", "albums", albums, "images", images)
	case "run":
		flags := flag.NewFlagSet("run", flag.ExitOnError)
		maximum := flags.Int("max", 0, "maximum albums; 0 drains the queue")
		workers := flags.Int("workers", envInt("LEGACY_WORKERS", 1), "parallel album workers")
		_ = flags.Parse(os.Args[2:])
		fatalIf(logger, validate(*workers))
		fatalIf(logger, database.RecoverProcessing(ctx))
		fatalIf(logger, runner(cfg, database, workDir, *workers, logger).Run(ctx, *maximum))
	case "daemon":
		flags := flag.NewFlagSet("daemon", flag.ExitOnError)
		workers := flags.Int("workers", envInt("LEGACY_WORKERS", 1), "parallel album workers")
		idle := flags.Duration("idle", envDuration("LEGACY_IDLE_INTERVAL", 5*time.Minute), "wait when no album is ready")
		_ = flags.Parse(os.Args[2:])
		fatalIf(logger, validate(*workers))
		fatalIf(logger, database.RecoverProcessing(ctx))
		for ctx.Err() == nil {
			if err := runner(cfg, database, workDir, *workers, logger).Run(ctx, 0); err != nil && ctx.Err() == nil {
				logger.Error("legacy cycle", "error", err)
			}
			timer := time.NewTimer(*idle)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	case "status":
		stats, err := database.Stats(ctx)
		fatalIf(logger, err)
		fmt.Printf("albums pending=%d processing=%d incomplete=%d ready=%d invalid=%d\n",
			stats.Pending, stats.Processing, stats.Incomplete, stats.Ready, stats.Invalid)
		fmt.Printf("images uploaded=%d downloaded=%d dead=%d total=%d\n",
			stats.Uploaded, stats.Downloaded, stats.Dead, stats.Images)
		fmt.Printf("traffic source_images=%d bytes (%.3f GiB) telegram_files=%d bytes (%.3f GiB)\n",
			stats.SourceBytes, gib(stats.SourceBytes), stats.TelegramBytes, gib(stats.TelegramBytes))
	case "invalid-report":
		flags := flag.NewFlagSet("invalid-report", flag.ExitOnError)
		out := flags.String("out", filepath.Join(workDir, "invalid-albums.json"), "output report")
		_ = flags.Parse(os.Args[2:])
		count, err := database.InvalidReport(ctx, *out)
		fatalIf(logger, err)
		logger.Info("invalid report written", "albums", count, "file", *out)
	case "snapshot":
		flags := flag.NewFlagSet("snapshot", flag.ExitOnError)
		out := flags.String("out", "/var/lib/xrw-publisher/github-snapshot/batches", "snapshot batch directory")
		maximum := flags.Int("max", 100, "maximum ready or invalid albums")
		_ = flags.Parse(os.Args[2:])
		file, galleries, removed, err := legacy.ExportSnapshot(ctx, database, *out, *maximum, snapshot.Options{
			ImageBase: cfg.GitHubImageBase, SigningSecret: cfg.GitHubImageSecret,
		})
		fatalIf(logger, err)
		logger.Info("legacy snapshot export complete", "galleries", galleries, "removed", removed, "file", file)
	default:
		usage()
		os.Exit(2)
	}
}

func runner(cfg config.Config, database *legacy.Store, workDir string, workers int, logger *slog.Logger) *legacy.Runner {
	return legacy.NewRunner(database,
		telegram.New(cfg.TGAPIBase, cfg.TGBotToken, cfg.ImagePublicBase,
			cfg.TGUploadInterval, cfg.TGGlobalInterval, cfg.TGMaxConcurrent, cfg.HTTPTimeout),
		workDir, cfg.MaxImageBytes, envInt("LEGACY_SOURCE_RETRIES", 3), workers,
		cfg.HTTPTimeout, envDuration("LEGACY_RETRY_DELAY", 30*time.Minute), logger)
}

func validate(workers int) error {
	if workers < 1 {
		return fmt.Errorf("workers must be positive")
	}
	return nil
}

func fatalIf(logger *slog.Logger, err error) {
	if err == nil {
		return
	}
	logger.Error("command failed", "error", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: xrw-legacy <init|run|daemon|status|invalid-report|snapshot> [flags]")
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

func gib(bytes int64) float64 { return float64(bytes) / (1024 * 1024 * 1024) }
