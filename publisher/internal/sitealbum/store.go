// Package sitealbum keeps a local index of the galleries published on a remote
// WordPress gallery site, so the local uploader can browse and search them
// without re-walking the origin on every page view. Only metadata is cached:
// cover images stay hotlinked from the site, so no image is stored locally
// before the user picks an album.
package sitealbum

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/legacy"
)

const (
	// indexFileName is the cache file inside the configured directory.
	indexFileName = "index.json"
	// refreshPerPage is the page size used while walking the site.
	refreshPerPage = 100
	// publishEveryPages is how often a long walk writes out the albums it has so
	// far, so a site with tens of thousands of posts can be browsed while the rest
	// of it is still being read. The first page is always published, because that
	// is the one a user is waiting for.
	publishEveryPages = 25
	// maxWalkPages stops a walk whose site reports no page count, or reports one
	// that keeps growing. It has to clear the largest real section (acgmhn.com
	// publishes just over a thousand listing pages).
	maxWalkPages = 5000
	// defaultPageSize is used when a caller does not ask for a page size.
	defaultPageSize = 48
	// maxPageSize bounds one response so a bad query cannot return the whole site.
	maxPageSize = 200
)

// Album is one gallery as it appears in the site's list view.
type Album struct {
	ID         int    `json:"id"`
	Slug       string `json:"slug"`
	Link       string `json:"link"`
	Title      string `json:"title"`
	Photos     int    `json:"photos"`
	Videos     int    `json:"videos"`
	CoverID    int    `json:"cover_id,omitempty"`
	CoverURL   string `json:"cover_url,omitempty"`
	Categories []int  `json:"categories,omitempty"`
	Tags       []int  `json:"tags,omitempty"`
	Date       string `json:"date,omitempty"`
	// Actual is how many images the post really carries. Photos is the number the
	// title advertises, which on some sites is much higher because the rest of the
	// set sits behind a download link. Zero means nobody has measured it yet.
	Actual int `json:"actual,omitempty"`
}

// Term is a category or tag the browser can filter by.
type Term struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Index is the cached snapshot of the site.
type Index struct {
	BuiltAt     time.Time `json:"built_at"`
	RefreshedAt time.Time `json:"refreshed_at,omitempty"`
	Albums      []Album   `json:"albums"`
	Categories  []Term    `json:"categories,omitempty"`
	Tags        []Term    `json:"tags,omitempty"`
	// Partial marks an index that a walk published while it was still running and
	// never finished. It cannot be trusted as a starting point, so the next
	// refresh walks the whole site instead of only asking for newer posts.
	Partial bool `json:"partial,omitempty"`
}

// Status describes the cache for the web UI. The site's own identity lives on
// Registry's Site, so one site's status never carries a second copy of its URL.
type Status struct {
	Total       int    `json:"total"`
	BuiltAt     int64  `json:"built_at"`
	RefreshedAt int64  `json:"refreshed_at"`
	Building    bool   `json:"building"`
	Progress    string `json:"progress,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Filter selects a page of cached albums.
type Filter struct {
	Query      string
	CategoryID int
	Page       int
	PerPage    int
}

// Source is the reading strategy behind one site: how to list its galleries,
// where their covers are, how to read one gallery's images, and what taxonomies
// it publishes. Two very different sites implement it — a WordPress REST API and
// a hand-rolled HTML site — which is why the store holds an interface rather
// than a concrete type.
type Source interface {
	// Base returns the site root, used for logging and for the page's image
	// allowlist.
	Base() string
	// Posts reads one page of the gallery listing.
	Posts(ctx context.Context, options legacy.WPPostsOptions) ([]legacy.WPPost, legacy.WPPage, error)
	// Covers maps album ids to their cover URL.
	Covers(ctx context.Context, ids []int) (map[int]string, error)
	// Terms reads one taxonomy, most-used first.
	Terms(ctx context.Context, taxonomy, orderBy string) ([]legacy.WPTerm, error)
	// AlbumImages reads the image list of one gallery.
	AlbumImages(ctx context.Context, postID int) ([]legacy.WPMedia, error)
}

// Measurer is an optional Source capability: reporting how many images each
// album really carries. The listing only carries the count the title advertises,
// which on some sites is a truncated preview, so the browser asks about the page
// it is showing. A source without a bulk way to measure simply leaves the badge
// off, because one request per album would be worse than the badge is worth.
type Measurer interface {
	Measure(ctx context.Context, ids []int) (map[int]int, error)
}

// Store caches the site index on disk and refreshes it on demand.
type Store struct {
	path   string
	source Source
	logger *slog.Logger
	base   context.Context

	mu       sync.RWMutex
	index    Index
	loaded   bool
	building bool
	progress string
	lastErr  string
}

// NewStore prepares a cache rooted at directory. The index file is read lazily on
// the first access so a corrupt or missing file cannot stop the uploader booting.
func NewStore(directory string, source Source, logger *slog.Logger) (*Store, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return nil, errors.New("site album directory is missing")
	}
	if source == nil {
		return nil, errors.New("site album source is missing")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create site album directory: %w", err)
	}
	return &Store{
		path:   filepath.Join(directory, indexFileName),
		source: source,
		logger: logger,
		base:   context.Background(),
	}, nil
}

// Bind sets the context used for background refreshes. Pass the process context,
// not a request context: a refresh outlives the request that started it.
func (s *Store) Bind(ctx context.Context) {
	if ctx != nil {
		s.base = ctx
	}
}

// BaseURL reports the site this cache mirrors.
func (s *Store) BaseURL() string {
	return s.source.Base()
}

// Status reports the cache state for the web UI.
func (s *Store) Status() Status {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	status := Status{
		Total:    len(s.index.Albums),
		Building: s.building,
		Progress: s.progress,
		Error:    s.lastErr,
	}
	if !s.index.BuiltAt.IsZero() {
		status.BuiltAt = s.index.BuiltAt.Unix()
	}
	if !s.index.RefreshedAt.IsZero() {
		status.RefreshedAt = s.index.RefreshedAt.Unix()
	}
	return status
}

// Terms returns the cached categories and tags.
func (s *Store) Terms() ([]Term, []Term) {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Term(nil), s.index.Categories...), append([]Term(nil), s.index.Tags...)
}

// Get returns one cached album.
func (s *Store) Get(id int) (Album, error) {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, album := range s.index.Albums {
		if album.ID == id {
			return album, nil
		}
	}
	return Album{}, fmt.Errorf("site album %d is not in the local index", id)
}

// List returns one page of cached albums plus the total number of matches.
func (s *Store) List(filter Filter) ([]Album, int, error) {
	s.ensureLoaded()
	albums := s.albums()
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	matched := make([]Album, 0, len(albums))
	for _, album := range albums {
		if filter.CategoryID > 0 && !containsInt(album.Categories, filter.CategoryID) {
			continue
		}
		if query != "" && !albumMatches(album, query) {
			continue
		}
		matched = append(matched, album)
	}
	total := len(matched)
	perPage := filter.PerPage
	if perPage < 1 || perPage > maxPageSize {
		perPage = defaultPageSize
	}
	page := filter.Page
	if page < 1 {
		page = 1
	}
	start := (page - 1) * perPage
	if start >= total {
		return []Album{}, total, nil
	}
	end := start + perPage
	if end > total {
		end = total
	}
	return append([]Album(nil), matched[start:end]...), total, nil
}

// Verify measures how many images each album really carries and remembers the
// numbers. The walk only sees the list view, which advertises the title's count
// and cannot tell a complete gallery from a preview, so the browser asks for the
// page it is showing. Only ids already in the index are kept; unknown ones are
// dropped from the answer.
func (s *Store) Verify(ctx context.Context, ids []int) (map[int]int, error) {
	s.ensureLoaded()
	measurer, ok := s.source.(Measurer)
	if !ok {
		return map[int]int{}, nil
	}
	counts, err := measurer.Measure(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(counts) == 0 {
		return map[int]int{}, nil
	}
	if err := s.recordActual(counts); err != nil {
		return counts, err
	}
	return counts, nil
}

// recordActual writes measured image counts into the cache. Ids that are not in
// the index are ignored, so a stale browser tab cannot add albums to it.
func (s *Store) recordActual(counts map[int]int) error {
	if len(counts) == 0 {
		return nil
	}
	s.mu.Lock()
	albums := make([]Album, len(s.index.Albums))
	copy(albums, s.index.Albums)
	recorded := 0
	for index := range albums {
		if actual, ok := counts[albums[index].ID]; ok {
			albums[index].Actual = actual
			recorded++
		}
	}
	if recorded == 0 {
		s.mu.Unlock()
		return nil
	}
	index := s.index
	index.Albums = albums
	index.RefreshedAt = time.Now().UTC()
	s.index = index
	s.mu.Unlock()
	return s.save(index)
}

// carryActual keeps measured image counts across a walk. A walk rebuilds every
// album from the list view, which knows nothing about the post's real image
// count, so without this the numbers would be thrown away and measured again on
// the next visit. A title whose count changed is measured afresh.
func carryActual(fresh Album, previous map[int]Album) Album {
	if fresh.Actual > 0 {
		return fresh
	}
	if old, ok := previous[fresh.ID]; ok && old.Actual > 0 && old.Photos == fresh.Photos {
		fresh.Actual = old.Actual
	}
	return fresh
}

// Refresh walks the site synchronously and reports the outcome. Tests and the
// very first boot use it; the web UI uses StartRefresh so a click never blocks a
// request for the length of a 57 page walk.
func (s *Store) Refresh(ctx context.Context, full bool) error {
	if !s.begin() {
		return errors.New("图包列表正在更新中")
	}
	err := s.refresh(ctx, full)
	s.finish(err)
	return err
}

// StartRefresh launches a background refresh and reports whether it started. A
// refresh already running is left alone, so repeated clicks cannot stack walks.
func (s *Store) StartRefresh(full bool) bool {
	if !s.begin() {
		return false
	}
	go func() { s.finish(s.refresh(s.base, full)) }()
	return true
}

// begin claims the single refresh slot.
func (s *Store) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.building {
		return false
	}
	s.building = true
	s.progress = "准备中"
	s.lastErr = ""
	return true
}

func (s *Store) finish(err error) {
	s.mu.Lock()
	s.building = false
	s.progress = ""
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()
	if err != nil {
		s.logger.Error("refresh site album index", "site", s.source.Base(), "error", err)
		return
	}
	s.logger.Info("site album index refreshed", "site", s.source.Base(), "albums", s.Status().Total)
}

// refresh walks the site and rewrites the cache. A full refresh rebuilds the
// index from scratch; an incremental one only re-reads posts modified since the
// last build, which is what the periodic timer uses.
//
// A walk over a site with tens of thousands of posts takes minutes, so it
// publishes what it has along the way and the page stays browsable meanwhile. A
// walk that dies is not rolled back: every publish is a union with the previous
// index, so nothing is lost, and the partial mark makes the next refresh read
// the whole site again instead of only looking for newer posts.
func (s *Store) refresh(ctx context.Context, full bool) error {
	s.ensureLoaded()
	s.mu.Lock()
	previous := s.index
	s.mu.Unlock()

	byID := map[int]Album{}
	previousByID := make(map[int]Album, len(previous.Albums))
	for _, album := range previous.Albums {
		previousByID[album.ID] = album
	}
	// A partial index came from a walk that was cut short, so the only way to
	// fill its gaps is to read the whole site again.
	incremental := !full && !previous.Partial && !previous.BuiltAt.IsZero()
	if incremental {
		for id, album := range previousByID {
			byID[id] = album
		}
	}
	options := legacy.WPPostsOptions{OrderBy: "id", Order: "desc"}
	var known map[int]struct{}
	if incremental {
		options.ModifiedAfter = previous.BuiltAt
		options.OrderBy = "modified"
		// A site with no modification filter cannot be queried for what changed,
		// so it is told what is already indexed and stops when a page holds
		// nothing new.
		known = make(map[int]struct{}, len(previousByID))
		for id := range previousByID {
			known[id] = struct{}{}
		}
	}
	totalPages := 0
	for page := 1; page <= maxWalkPages; page++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if totalPages > 0 {
			s.setProgress(fmt.Sprintf("正在读取第 %d/%d 页图包列表", page, totalPages))
		} else {
			s.setProgress(fmt.Sprintf("正在读取第 %d 页图包列表", page))
		}
		posts, info, err := s.source.Posts(ctx, legacy.WPPostsOptions{
			Page: page, PerPage: refreshPerPage,
			OrderBy: options.OrderBy, Order: options.Order,
			ModifiedAfter: options.ModifiedAfter,
			Known:         known,
		})
		if err != nil {
			return err
		}
		if info.TotalPages > 0 && totalPages == 0 {
			// The size is taken from the first page that reports one. A pager near
			// the end of a long listing can name a page that does not exist yet,
			// and following that would walk past the site.
			totalPages = info.TotalPages
		}
		if len(posts) > 0 {
			covers, err := s.source.Covers(ctx, legacy.FeaturedIDs(posts))
			if err != nil {
				return err
			}
			for _, post := range posts {
				album := albumFromPost(post, covers[post.FeaturedMedia])
				byID[post.ID] = carryActual(album, previousByID)
			}
		}
		if page == 1 || page%publishEveryPages == 0 {
			if err := s.publish(byID, previous); err != nil {
				return err
			}
		}
		if len(posts) == 0 && !info.More {
			break
		}
		if totalPages > 0 && page >= totalPages {
			break
		}
	}

	s.setProgress("正在读取分类与标签")
	categories, err := s.source.Terms(ctx, "categories", "count")
	if err != nil {
		return err
	}
	tags, err := s.source.Terms(ctx, "tags", "count")
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	s.mu.Lock()
	index := Index{
		BuiltAt: now, RefreshedAt: now, Albums: sortAlbums(byID),
		Categories: termList(categories), Tags: termList(tags),
	}
	if incremental {
		index.BuiltAt = previous.BuiltAt
	}
	s.index = index
	s.loaded = true
	s.mu.Unlock()

	if err := s.save(index); err != nil {
		return err
	}
	return nil
}

// publish writes the albums read so far. A rebuild walks a big site for minutes,
// so the albums it has not reached yet are carried over from the previous index:
// the list then only ever grows, instead of collapsing to the handful of pages
// read so far and hiding everything the user was looking at. Categories and tags
// only arrive once the walk is over, so a partial index keeps the ones it had.
func (s *Store) publish(byID map[int]Album, previous Index) error {
	merged := make(map[int]Album, len(previous.Albums)+len(byID))
	for _, album := range previous.Albums {
		merged[album.ID] = album
	}
	for id, album := range byID {
		merged[id] = album
	}
	s.mu.Lock()
	index := Index{
		BuiltAt:     previous.BuiltAt,
		RefreshedAt: time.Now().UTC(),
		Albums:      sortAlbums(merged),
		Categories:  previous.Categories,
		Tags:        previous.Tags,
		Partial:     true,
	}
	if index.BuiltAt.IsZero() {
		index.BuiltAt = index.RefreshedAt
	}
	s.index = index
	s.loaded = true
	s.mu.Unlock()
	return s.save(index)
}

// sortAlbums orders the cache newest first, which is the order the site lists in.
func sortAlbums(byID map[int]Album) []Album {
	albums := make([]Album, 0, len(byID))
	for _, album := range byID {
		albums = append(albums, album)
	}
	sort.Slice(albums, func(left, right int) bool { return albums[left].ID > albums[right].ID })
	return albums
}

// save writes the index atomically so a crash mid-write cannot truncate the cache.
func (s *Store) save(index Index) error {
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	temporary := s.path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, s.path)
}

// ensureLoaded reads the cache file once. A missing file is normal on first run.
func (s *Store) ensureLoaded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Warn("read site album index", "path", s.path, "error", err)
		}
		return
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		s.logger.Warn("site album index is unreadable, a refresh will rebuild it", "path", s.path, "error", err)
		return
	}
	s.index = index
}

func (s *Store) albums() []Album {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Album(nil), s.index.Albums...)
}

func (s *Store) setProgress(value string) {
	s.mu.Lock()
	s.progress = value
	s.mu.Unlock()
}

func albumFromPost(post legacy.WPPost, cover string) Album {
	return Album{
		ID: post.ID, Slug: post.Slug, Link: post.Link, Title: post.Title,
		Photos: post.Photos, Videos: post.Videos,
		CoverID: post.FeaturedMedia, CoverURL: cover,
		Categories: append([]int(nil), post.Categories...),
		Tags:       append([]int(nil), post.Tags...),
		Date:       post.Date,
	}
}

// termList copies the taxonomy terms the source package returns into this
// package's own type so the cached index does not depend on legacy's structs.
func termList(terms []legacy.WPTerm) []Term {
	if len(terms) == 0 {
		return nil
	}
	result := make([]Term, 0, len(terms))
	for _, term := range terms {
		result = append(result, Term{ID: term.ID, Slug: term.Slug, Name: term.Name, Count: term.Count})
	}
	return result
}

func albumMatches(album Album, query string) bool {
	if strings.Contains(strings.ToLower(album.Title), query) {
		return true
	}
	if strings.Contains(strings.ToLower(album.Slug), query) {
		return true
	}
	return strings.Contains(strings.ToLower(album.Link), query)
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
