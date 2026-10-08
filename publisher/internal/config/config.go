package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	VeilBaseURL       string
	VeilProxies       []string
	VeilRequests      int
	VeilWindow        time.Duration
	VeilCooldown      time.Duration
	DatabasePath      string
	WorkDir           string
	TGBotToken        string
	TGChatIDs         []string
	TGAPIBase         string
	ImagePublicBase   string
	GitHubImageBase   string
	GitHubImageSecret string
	TGUploadInterval  time.Duration
	TGGlobalInterval  time.Duration
	TGMaxConcurrent   int
	AdminURL          string
	AdminToken        string
	DiscoveryInterval time.Duration
	GalleryWorkers    int
	GalleryMaxRetries int
	HTTPTimeout       time.Duration
	MaxImageBytes     int64
	WordPressSites    []WordPressSite
	WordPressRefresh  time.Duration
}

// WordPressSite is one gallery site the local uploader can browse. ID is the
// stable handle used in URLs and as the on-disk cache directory name, so it must
// stay short and free of path separators; Name is only what the page displays.
type WordPressSite struct {
	ID      string
	Name    string
	BaseURL string
	// ImageSource selects where a gallery's images are read from, either
	// wordpressImageMedia (the default) or wordpressImageContent.
	ImageSource string
}

func Load() (Config, error) {
	sites, err := wordpressSites()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		VeilBaseURL:       env("VEIL_BASE_URL", "https://veil.ortlinde.com"),
		VeilProxies:       csv(os.Getenv("VEIL_PROXIES")),
		VeilRequests:      envInt("VEIL_REQUESTS_PER_WINDOW", 80),
		VeilWindow:        envDuration("VEIL_WINDOW", 300*time.Second),
		VeilCooldown:      envDuration("VEIL_COOLDOWN", 35*time.Minute),
		DatabasePath:      env("PUBLISHER_DB", "publisher.db"),
		WorkDir:           env("PUBLISHER_WORK_DIR", "work"),
		TGBotToken:        strings.TrimSpace(os.Getenv("TG_BOT_TOKEN")),
		TGChatIDs:         telegramChats(),
		TGAPIBase:         strings.TrimRight(env("TG_API_BASE", "https://api.telegram.org"), "/"),
		ImagePublicBase:   strings.TrimRight(strings.TrimSpace(os.Getenv("IMAGE_PUBLIC_BASE")), "/"),
		GitHubImageBase:   strings.TrimRight(strings.TrimSpace(os.Getenv("GIMG_PUBLIC_BASE")), "/"),
		GitHubImageSecret: strings.TrimSpace(os.Getenv("GIMG_SIGNING_SECRET")),
		TGUploadInterval:  envDuration("TG_UPLOAD_INTERVAL", 3500*time.Millisecond),
		TGGlobalInterval:  envDuration("TG_GLOBAL_INTERVAL", 500*time.Millisecond),
		TGMaxConcurrent:   envInt("TG_MAX_CONCURRENT", 3),
		AdminURL:          strings.TrimRight(strings.TrimSpace(os.Getenv("XRW_ADMIN_URL")), "/"),
		AdminToken:        os.Getenv("XRW_ADMIN_TOKEN"),
		DiscoveryInterval: envDuration("DISCOVERY_INTERVAL", 15*time.Minute),
		GalleryWorkers:    envInt("GALLERY_WORKERS", 2),
		GalleryMaxRetries: envInt("GALLERY_MAX_RETRIES", 5),
		HTTPTimeout:       envDuration("HTTP_TIMEOUT", 2*time.Minute),
		MaxImageBytes:     int64(envInt("MAX_IMAGE_MB", 20)) * 1024 * 1024,
		WordPressSites:    sites,
		WordPressRefresh:  envDuration("XRW_WP_REFRESH", 6*time.Hour),
	}

	if cfg.VeilRequests < 1 {
		return Config{}, fmt.Errorf("VEIL_REQUESTS_PER_WINDOW must be positive")
	}
	if cfg.GalleryWorkers < 1 {
		return Config{}, fmt.Errorf("GALLERY_WORKERS must be positive")
	}
	if cfg.GalleryMaxRetries < 1 {
		return Config{}, fmt.Errorf("GALLERY_MAX_RETRIES must be positive")
	}
	if cfg.TGMaxConcurrent < 1 {
		return Config{}, fmt.Errorf("TG_MAX_CONCURRENT must be positive")
	}
	if cfg.MaxImageBytes < 1 {
		return Config{}, fmt.Errorf("MAX_IMAGE_MB must be positive")
	}
	if cfg.MaxImageBytes > 20*1024*1024 {
		return Config{}, fmt.Errorf("MAX_IMAGE_MB cannot exceed Telegram Bot API's 20 MB download limit")
	}
	if cfg.AdminURL != "" && cfg.AdminToken == "" {
		return Config{}, fmt.Errorf("XRW_ADMIN_TOKEN is required when XRW_ADMIN_URL is set")
	}
	return cfg, nil
}

func (c Config) ChatForGallery(galleryID int64) string {
	if len(c.TGChatIDs) == 0 {
		return ""
	}
	if galleryID < 0 {
		galleryID = -galleryID
	}
	return c.TGChatIDs[galleryID%int64(len(c.TGChatIDs))]
}

// WordPressEnabled reports whether the gallery-site browser is configured.
// Setting XRW_WP_BASE to off, none or - hides it for someone who only wants the
// Telegram draft flow.
func (c Config) WordPressEnabled() bool {
	return len(c.WordPressSites) > 0
}

// defaultWordPressBase is the site the uploader browses when nothing is set.
const defaultWordPressBase = "https://cosplaytele.com"

// The image-source values XRW_WP_SITES accepts. They mirror
// legacy.ImageSourceMedia, legacy.ImageSourceContent and legacy.EngineAcgmhn;
// config deliberately does not import legacy, so the spellings are kept in step
// by hand.
const (
	wordpressImageMedia   = "media"
	wordpressImageContent = "content"
	siteEngineAcgmhn      = "acgmhn"
)

// wordpressSites resolves the browsable gallery sites. XRW_WP_SITES accepts one
// entry per line, or several separated by commas, each written as
// `url`, `id|url`, `id|name|url` or `id|name|url|image-source`. A single
// XRW_WP_BASE keeps working for someone who only browses one site, and either
// variable set to off/none/- disables the whole region.
func wordpressSites() ([]WordPressSite, error) {
	entries := splitList(os.Getenv("XRW_WP_SITES"))
	if len(entries) == 1 && disabledWordPressValue(entries[0]) {
		return nil, nil
	}
	if len(entries) == 0 {
		base := strings.TrimSpace(os.Getenv("XRW_WP_BASE"))
		if base == "" {
			base = defaultWordPressBase
		}
		if disabledWordPressValue(base) {
			return nil, nil
		}
		entries = []string{base}
	}

	var sites []WordPressSite
	seen := map[string]int{}
	for _, entry := range entries {
		site, err := parseWordPressSite(entry)
		if err != nil {
			return nil, err
		}
		// Two sites may legitimately share a host (staging next to production),
		// so a collision gets a numeric suffix instead of dropping the entry.
		if count := seen[site.ID]; count > 0 {
			seen[site.ID] = count + 1
			site.ID = fmt.Sprintf("%s-%d", site.ID, count+1)
		} else {
			seen[site.ID] = 1
		}
		sites = append(sites, site)
	}
	return sites, nil
}

func parseWordPressSite(entry string) (WordPressSite, error) {
	parts := []string{}
	for _, part := range strings.Split(entry, "|") {
		parts = append(parts, strings.TrimSpace(part))
	}
	// The URL is found by content rather than by position so the optional
	// image-source field can follow it without changing any older entry.
	at := -1
	for index, part := range parts {
		if strings.Contains(part, "://") {
			at = index
			break
		}
	}
	if at < 0 {
		return WordPressSite{}, fmt.Errorf("XRW_WP_SITES entry %q is missing a site URL", entry)
	}
	if at > 3 || len(parts) > at+2 {
		return WordPressSite{}, fmt.Errorf("XRW_WP_SITES entry %q has too many fields", entry)
	}
	base, err := normalizeSiteBase(parts[at])
	if err != nil {
		return WordPressSite{}, err
	}
	site := WordPressSite{BaseURL: base, ImageSource: wordpressImageMedia}
	if at > 0 {
		site.ID = siteSlug(parts[0])
	}
	if at > 1 {
		site.Name = parts[1]
	}
	// The optional image source is whatever follows the URL, so `id|url|content`
	// and `id|name|url|content` both work.
	if at+1 < len(parts) {
		source := strings.ToLower(parts[at+1])
		if source != wordpressImageMedia && source != wordpressImageContent && source != siteEngineAcgmhn {
			return WordPressSite{}, fmt.Errorf(
				"XRW_WP_SITES entry %q has an unknown image source %q (use %s, %s or %s)",
				entry, parts[at+1], wordpressImageMedia, wordpressImageContent, siteEngineAcgmhn)
		}
		site.ImageSource = source
	}
	if site.ID == "" {
		site.ID = siteSlug(hostOf(base))
	}
	if site.ID == "" {
		return WordPressSite{}, fmt.Errorf("XRW_WP_SITES entry %q has no usable site id", entry)
	}
	if site.Name == "" {
		site.Name = site.ID
	}
	return site, nil
}

// normalizeSiteBase keeps only scheme and host: a path would silently point the
// REST client at the wrong endpoint, and a trailing slash breaks joining.
func normalizeSiteBase(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("XRW_WP_SITES entry %q is not a valid URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("XRW_WP_SITES entry %q must use http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("XRW_WP_SITES entry %q has no host", raw)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

func hostOf(base string) string {
	if parsed, err := url.Parse(base); err == nil {
		return parsed.Hostname()
	}
	return base
}

// siteSlug turns a display value into an identifier safe to use as a directory
// name and a query parameter.
func siteSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "www.")
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteRune(char)
			lastDash = false
		case !lastDash && builder.Len() > 0:
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func disabledWordPressValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "off", "none", "-":
		return true
	default:
		return false
	}
}

// splitList accepts the same value written on one line with commas or across
// several lines, because both are natural in an env file.
func splitList(value string) []string {
	var values []string
	for _, line := range strings.Split(value, "\n") {
		for _, item := range strings.Split(line, ",") {
			if item = strings.TrimSpace(item); item != "" {
				values = append(values, item)
			}
		}
	}
	return values
}

func telegramChats() []string {
	if chats := csv(os.Getenv("TG_CHAT_IDS")); len(chats) > 0 {
		return chats
	}
	return csv(os.Getenv("TG_CHAT_ID"))
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func csv(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}
