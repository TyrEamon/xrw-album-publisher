package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/config"
	"github.com/TyrEamon/xrw-album/publisher/internal/localupload"
	"github.com/TyrEamon/xrw-album/publisher/internal/sitealbum"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

func main() {
	executable, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	baseDir := filepath.Dir(executable)
	defaultConfig := filepath.Join(baseDir, "local-uploader.env")
	configPath := flag.String("config", defaultConfig, "path to the local uploader environment file")
	noBrowser := flag.Bool("no-browser", false, "do not open the browser automatically")
	flag.Parse()

	if err := loadEnvFile(*configPath); err != nil {
		fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	dataDir := environment("LOCAL_UPLOADER_DATA_DIR", filepath.Join(baseDir, "local-uploader-data"))
	snapshotDir := environment("LOCAL_SNAPSHOT_DIR", filepath.Join(dataDir, "batches"))
	gitRepository := environment("LOCAL_GIT_REPOSITORY", filepath.Clean(filepath.Join(baseDir, "..", "..")))
	address := environment("LOCAL_UPLOADER_ADDR", "127.0.0.1:8765")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fatal(err)
	}
	importToken, err := localupload.LoadOrCreateImportToken(filepath.Join(dataDir, "telegram-import-token"))
	if err != nil {
		fatal(err)
	}
	imports, err := localupload.NewTelegramImportStore(filepath.Join(dataDir, "telegram-imports"), cfg.MaxImageBytes)
	if err != nil {
		fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	proxyRequest, err := http.NewRequest(http.MethodGet, cfg.TGAPIBase, nil)
	if err != nil {
		fatal(err)
	}
	proxyURL, err := http.ProxyFromEnvironment(proxyRequest)
	if err != nil {
		fatal(fmt.Errorf("invalid HTTPS_PROXY; use http://127.0.0.1:your-proxy-port"))
	}
	proxyHost := "direct"
	if proxyURL != nil {
		proxyHost = proxyURL.Host
	}
	logger.Info("Telegram network", "proxy", proxyHost)
	database, err := localupload.OpenStore(filepath.Join(dataDir, "local-uploader.db"))
	if err != nil {
		fatal(err)
	}
	defer database.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := database.RedactErrors(ctx, cfg.TGBotToken); err != nil {
		fatal(err)
	}
	if err := database.Recover(ctx); err != nil {
		fatal(err)
	}

	// The gallery-site browser is optional. Without it the uploader still runs,
	// it just hides the region.
	var siteRegistry *sitealbum.Registry
	var siteImporter *localupload.SiteImporter
	if cfg.WordPressEnabled() {
		sites := make([]sitealbum.Site, 0, len(cfg.WordPressSites))
		for _, site := range cfg.WordPressSites {
			sites = append(sites, sitealbum.Site{ID: site.ID, Name: site.Name, BaseURL: site.BaseURL, ImageSource: site.ImageSource})
		}
		siteRegistry, err = sitealbum.NewRegistry(filepath.Join(dataDir, "site-albums"), sites, logger)
		if err != nil {
			fatal(err)
		}
		siteImporter, err = localupload.NewSiteImporter(siteRegistry, imports,
			filepath.Join(dataDir, "site-import"), cfg.MaxImageBytes, logger)
		if err != nil {
			fatal(err)
		}
		siteRegistry.Bind(ctx)
		siteImporter.Bind(ctx)
		for _, site := range siteRegistry.Sites() {
			logger.Info("gallery site browser", "id", site.ID, "site", site.BaseURL)
		}
	}

	publicBase := cfg.ImagePublicBase
	if publicBase == "" {
		publicBase = cfg.GitHubImageBase
	}
	uploader := telegram.New(cfg.TGAPIBase, cfg.TGBotToken, publicBase,
		cfg.TGUploadInterval, cfg.TGGlobalInterval, cfg.TGMaxConcurrent, cfg.HTTPTimeout)
	service := localupload.NewService(database, uploader, localupload.Options{
		SnapshotDir: snapshotDir, ImageBase: cfg.GitHubImageBase,
		SigningSecret: cfg.GitHubImageSecret, MaxImageBytes: cfg.MaxImageBytes,
		ChatIDs: cfg.TGChatIDs,
	}, logger)
	if localupload.DisabledGitRepository(gitRepository) {
		logger.Warn("snapshot git publishing is off; snapshots stay on disk", "dir", snapshotDir)
	} else {
		snapshotPublisher, err := localupload.NewGitSnapshotPublisher(gitRepository)
		if err != nil {
			fatal(err)
		}
		service.SetSnapshotPublisher(snapshotPublisher.Publish)
	}
	service.SetReadyHook(imports.RemoveByJob)
	server, err := localupload.NewServer(database, service, localupload.ServerOptions{
		ChatIDs: cfg.TGChatIDs, SnapshotDir: snapshotDir,
		Imports: imports, ImportToken: importToken,
		Sites: siteRegistry, SiteImport: siteImporter,
	}, logger)
	if err != nil {
		fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		fatal(err)
	}
	httpServer := &http.Server{
		Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 2 * time.Minute,
	}
	service.Start(ctx)
	if siteRegistry != nil {
		// An empty index walks the whole site; a populated one only re-reads the
		// galleries that changed since the last build.
		siteRegistry.StartRefreshAll(false)
		startSiteRefreshLoop(ctx, siteRegistry, cfg.WordPressRefresh, logger)
	}
	url := "http://" + listener.Addr().String() + "/"
	logger.Info("local uploader started", "url", url, "data", dataDir, "snapshots", snapshotDir)
	if !*noBrowser {
		go func() {
			time.Sleep(250 * time.Millisecond)
			if err := openBrowser(url); err != nil {
				logger.Warn("open browser", "error", err)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
}

// startSiteRefreshLoop keeps every cached site index current. An incremental pass
// costs one request per page of changed galleries, so a short interval is cheap.
func startSiteRefreshLoop(ctx context.Context, sites *sitealbum.Registry, interval time.Duration, logger *slog.Logger) {
	if interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if started := sites.StartRefreshAll(false); started < len(sites.Sites()) {
					logger.Info("some site album refreshes were still running, skipping those", "started", started)
				}
			}
		}
	}()
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, found := strings.Cut(text, "=")
		if !found || strings.TrimSpace(name) == "" {
			return fmt.Errorf("%s:%d: expected NAME=value (each variable must fit on one line)", path, line)
		}
		name = strings.TrimSpace(name)
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if _, exists := os.LookupEnv(name); !exists {
			if err := os.Setenv(name, value); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

func environment(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func openBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		command = exec.Command("open", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	return command.Start()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "xrw-local-uploader:", err)
	os.Exit(1)
}
