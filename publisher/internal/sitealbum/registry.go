package sitealbum

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/TyrEamon/xrw-album/publisher/internal/legacy"
)

// ImageSourceContent marks a site whose galleries hotlink their images from a
// CDN instead of publishing them as attachments. It is re-exported so callers
// that only handle a Site do not have to reach into legacy.
const ImageSourceContent = legacy.ImageSourceContent

// EngineAcgmhn marks a site that exposes no API at all: its galleries are
// paginated HTML. Also re-exported for callers that only see a Site.
const EngineAcgmhn = legacy.EngineAcgmhn

// ImageSourceMedia marks a site whose galleries come from the WordPress media
// endpoint, which is where its covers live too.
const ImageSourceMedia = legacy.ImageSourceMedia

// ImagesOutsideBase reports whether a site's covers can live on hosts its base URL
// never names, which the browser has to allow with a bare `https:` source. The
// media endpoint keeps its images on the site itself; the other strategies
// hotlink from a CDN or a separate image host.
func ImagesOutsideBase(imageSource string) bool {
	switch strings.ToLower(strings.TrimSpace(imageSource)) {
	case "", ImageSourceMedia:
		return false
	default:
		return true
	}
}

// Site describes one browsable gallery site. ID is what the page sends back in
// its requests and what names the cache directory, so it is also what keeps two
// sites from ever sharing an index.
type Site struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// ImageSource says how to read this site: the WordPress media endpoint
	// (empty or "media"), the images inlined in a WordPress post ("content"), or
	// a hand-rolled HTML site ("acgmhn"). See legacy.WordPressSource.ImageSource
	// and legacy.NewAcgmhnSource.
	ImageSource string `json:"image_source,omitempty"`
}

// Target bundles everything needed to browse and mirror one site, so a caller
// never has to pair a store with the wrong source.
type Target struct {
	Site   Site
	Store  *Store
	Source Source
}

// SiteStatus is one site's cache state as the page needs it. Site is embedded so
// the JSON is a flat object the front end can index by id.
type SiteStatus struct {
	Site
	Status
}

// Registry keeps one index cache per configured site. Each site gets its own
// directory, its own index file and its own refresh slot, so a slow or failing
// site cannot stall the others.
type Registry struct {
	entries []Target
	byID    map[string]int
	logger  *slog.Logger
	base    context.Context
}

// NewRegistry prepares a cache directory per site under directory.
func NewRegistry(directory string, sites []Site, logger *slog.Logger) (*Registry, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return nil, errors.New("site album directory is missing")
	}
	if len(sites) == 0 {
		return nil, errors.New("site album registry needs at least one site")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create site album directory: %w", err)
	}

	registry := &Registry{byID: map[string]int{}, logger: logger, base: context.Background()}
	for _, site := range sites {
		id := strings.TrimSpace(site.ID)
		if id == "" {
			return nil, errors.New("site album registry needs a site id")
		}
		if _, exists := registry.byID[id]; exists {
			return nil, fmt.Errorf("site album id %q is configured twice", id)
		}
		base := strings.TrimSpace(site.BaseURL)
		if base == "" {
			return nil, fmt.Errorf("site album %q has no base URL", id)
		}
		source := newSource(site.ImageSource, base)
		store, err := NewStore(filepath.Join(directory, id), source, logger)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(site.Name)
		if name == "" {
			name = id
		}
		registry.byID[id] = len(registry.entries)
		registry.entries = append(registry.entries, Target{
			Site:   Site{ID: id, Name: name, BaseURL: source.Base(), ImageSource: site.ImageSource},
			Store:  store,
			Source: source,
		})
	}
	registry.adoptLegacyIndex(directory)
	return registry, nil
}

// newSource builds the reading strategy a site asked for. An unknown engine falls
// back to WordPress, which is what every site used before engines existed.
func newSource(engine, base string) Source {
	if strings.EqualFold(strings.TrimSpace(engine), EngineAcgmhn) {
		return legacy.NewAcgmhnSource(base, nil)
	}
	source := legacy.NewWordPressSource(base, nil)
	source.ImageSource = strings.TrimSpace(engine)
	return source
}

// adoptLegacyIndex moves the flat index written before sites had ids into the
// only site's directory. Without it the first boot after the upgrade would
// re-walk every page of a 5000 album site for no reason.
func (r *Registry) adoptLegacyIndex(directory string) {
	if len(r.entries) != 1 {
		return
	}
	previous := filepath.Join(directory, indexFileName)
	target := r.entries[0].Store.path
	if _, err := os.Stat(target); err == nil {
		return
	}
	if _, err := os.Stat(previous); err != nil {
		return
	}
	if err := os.Rename(previous, target); err != nil {
		r.logger.Warn("adopt the previous site album index", "path", previous, "error", err)
		return
	}
	r.logger.Info("adopted the previous site album index", "site", r.entries[0].Site.ID)
}

// Bind attaches the process context used by background refreshes.
func (r *Registry) Bind(ctx context.Context) {
	if ctx == nil {
		return
	}
	r.base = ctx
	for _, entry := range r.entries {
		entry.Store.Bind(ctx)
	}
}

// Sites lists the configured sites in configuration order.
func (r *Registry) Sites() []Site {
	sites := make([]Site, 0, len(r.entries))
	for _, entry := range r.entries {
		sites = append(sites, entry.Site)
	}
	return sites
}

// Target returns one site's store and source.
func (r *Registry) Target(id string) (Target, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return r.Default()
	}
	index, ok := r.byID[id]
	if !ok {
		return Target{}, fmt.Errorf("没有配置名为 %q 的站点", id)
	}
	return r.entries[index], nil
}

// Default returns the first configured site, which is what a request without an
// explicit site id means.
func (r *Registry) Default() (Target, error) {
	if len(r.entries) == 0 {
		return Target{}, errors.New("没有配置站点")
	}
	return r.entries[0], nil
}

// Status reports every site's cache state in configuration order.
func (r *Registry) Status() []SiteStatus {
	statuses := make([]SiteStatus, 0, len(r.entries))
	for _, entry := range r.entries {
		statuses = append(statuses, SiteStatus{Site: entry.Site, Status: entry.Store.Status()})
	}
	return statuses
}

// StartRefreshAll launches a background refresh per site and reports how many
// actually started. A site already rebuilding is skipped, so a double click on
// "更新列表" cannot stack walks.
func (r *Registry) StartRefreshAll(full bool) int {
	started := 0
	for _, entry := range r.entries {
		if entry.Store.StartRefresh(full) {
			started++
		}
	}
	return started
}

// RefreshAll walks every site synchronously. Tests and the first boot use it.
func (r *Registry) RefreshAll(ctx context.Context, full bool) error {
	var failures []error
	for _, entry := range r.entries {
		if err := entry.Store.Refresh(ctx, full); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", entry.Site.ID, err))
		}
	}
	return errors.Join(failures...)
}
