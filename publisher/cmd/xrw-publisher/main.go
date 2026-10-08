package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/admin"
	"github.com/TyrEamon/xrw-album/publisher/internal/app"
	"github.com/TyrEamon/xrw-album/publisher/internal/config"
	"github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
	"github.com/TyrEamon/xrw-album/publisher/internal/store"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
	"github.com/TyrEamon/xrw-album/publisher/internal/veil"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}
	database, err := store.Open(cfg.DatabasePath)
	if err != nil {
		logger.Error("open database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	veilClient, err := veil.New(
		cfg.VeilBaseURL, cfg.VeilProxies, cfg.VeilRequests, cfg.VeilWindow,
		cfg.VeilCooldown, cfg.HTTPTimeout, cfg.MaxImageBytes,
	)
	if err != nil {
		logger.Error("configure Veil client", "error", err)
		os.Exit(1)
	}
	application := app.New(
		cfg,
		database,
		veilClient,
		telegram.New(cfg.TGAPIBase, cfg.TGBotToken, cfg.ImagePublicBase, cfg.TGUploadInterval, cfg.TGGlobalInterval, cfg.TGMaxConcurrent, cfg.HTTPTimeout),
		admin.New(cfg.AdminURL, cfg.AdminToken, cfg.HTTPTimeout),
		logger,
	)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	switch os.Args[1] {
	case "discover":
		flags := flag.NewFlagSet("discover", flag.ExitOnError)
		pages := flags.Int("pages", 0, "pages to scan; 0 scans all pages")
		offset := flags.Int("offset", 0, "starting gallery-list offset")
		_ = flags.Parse(os.Args[2:])
		if *offset < 0 {
			logger.Error("offset must be non-negative")
			os.Exit(2)
		}
		count, err := application.Discover(ctx, *pages, *offset)
		fatalIf(logger, err)
		logger.Info("discovery complete", "galleries", count)
	case "run":
		flags := flag.NewFlagSet("run", flag.ExitOnError)
		maximum := flags.Int("max", 0, "maximum galleries to process; 0 processes the queue")
		_ = flags.Parse(os.Args[2:])
		fatalIf(logger, database.RecoverProcessing(ctx))
		fatalIf(logger, application.Run(ctx, *maximum))
	case "daemon":
		flags := flag.NewFlagSet("daemon", flag.ExitOnError)
		pages := flags.Int("pages", 5, "latest gallery pages to scan each cycle")
		batch := flags.Int("batch", 0, "galleries processed between discovery cycles; 0 drains the queue")
		fullScanInterval := flags.Duration("full-scan-interval", 24*time.Hour, "interval between full gallery rescans; 0 disables them")
		initialFullScanOffset := flags.Int("initial-full-scan-offset", 0, "starting offset for the first full rescan after process start")
		_ = flags.Parse(os.Args[2:])
		if *initialFullScanOffset < 0 {
			logger.Error("initial full scan offset must be non-negative")
			os.Exit(2)
		}
		fatalIf(logger, database.RecoverProcessing(ctx))
		var lastFullScan time.Time
		for ctx.Err() == nil {
			discoveryPages, fullScan := pagesForDiscovery(time.Now(), lastFullScan, *pages, *fullScanInterval)
			discoveryOffset := 0
			if fullScan && lastFullScan.IsZero() {
				discoveryOffset = *initialFullScanOffset
			}
			if fullScan {
				logger.Info("starting full gallery discovery", "offset", discoveryOffset)
			}
			if _, err := application.Discover(ctx, discoveryPages, discoveryOffset); err != nil {
				logger.Error("discovery cycle", "error", err)
			} else if fullScan {
				lastFullScan = time.Now()
				logger.Info("full gallery discovery complete")
			}
			if err := application.Run(ctx, *batch); err != nil {
				logger.Error("publisher cycle", "error", err)
			}
			timer := time.NewTimer(cfg.DiscoveryInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	case "status":
		stats, err := database.Stats(ctx)
		fatalIf(logger, err)
		fmt.Printf("galleries pending=%d processing=%d incomplete=%d waiting=%d ready=%d ok=%d failed=%d blocked=%d\n", stats.Pending, stats.Processing, stats.Incomplete, stats.Waiting, stats.Ready, stats.OK, stats.Failed, stats.Blocked)
		fmt.Printf("images uploaded=%d total=%d\n", stats.Uploaded, stats.Images)
		fmt.Printf("traffic source_images=%d bytes (%.3f GiB) telegram_files=%d bytes (%.3f GiB)\n",
			stats.SourceImageBytes, gib(stats.SourceImageBytes), stats.TelegramFileBytes, gib(stats.TelegramFileBytes))
	case "retry-failed":
		count, err := database.RetryFailed(ctx)
		fatalIf(logger, err)
		logger.Info("failed galleries returned to queue", "galleries", count)
	case "snapshot":
		flags := flag.NewFlagSet("snapshot", flag.ExitOnError)
		outDir := flags.String("out", "snapshot/batches", "directory for sanitized snapshot batches")
		maximum := flags.Int("max", 1000, "maximum galleries in one batch")
		reset := flags.Bool("reset", false, "re-export completed galleries using the current snapshot URL format")
		_ = flags.Parse(os.Args[2:])
		if *maximum < 1 {
			logger.Error("snapshot maximum must be positive")
			os.Exit(2)
		}
		if *reset {
			resetCount, err := database.ResetSnapshotExports(ctx)
			fatalIf(logger, err)
			logger.Info("snapshot export markers reset", "galleries", resetCount)
		}
		file, count, err := snapshot.Export(ctx, database, *outDir, *maximum, snapshot.Options{
			ImageBase: cfg.GitHubImageBase, SigningSecret: cfg.GitHubImageSecret,
		})
		fatalIf(logger, err)
		logger.Info("snapshot export complete", "galleries", count, "file", file)
	default:
		usage()
		os.Exit(2)
	}
}

func pagesForDiscovery(now, lastFullScan time.Time, latestPages int, fullScanInterval time.Duration) (int, bool) {
	if fullScanInterval > 0 && (lastFullScan.IsZero() || now.Sub(lastFullScan) >= fullScanInterval) {
		return 0, true
	}
	return latestPages, false
}

func gib(bytes int64) float64 {
	return float64(bytes) / (1024 * 1024 * 1024)
}

func fatalIf(logger *slog.Logger, err error) {
	if err == nil {
		return
	}
	logger.Error("command failed", "error", err)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: xrw-publisher <discover|run|daemon|status|retry-failed|snapshot> [flags]")
}
