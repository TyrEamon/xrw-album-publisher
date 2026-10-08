package localupload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/legacy"
	"github.com/TyrEamon/xrw-album/publisher/internal/sitealbum"
)

const (
	// siteImportWorkers is how many album images are fetched at the same time.
	// The origin is a CDN, so a handful of parallel GETs is both polite and
	// several times faster than walking a 170-image album one request at a time.
	siteImportWorkers = 4
	// siteImportAlbums is how many galleries are pulled at the same time. Two
	// keeps the origin calm while still making a batch of picks feel immediate.
	siteImportAlbums = 2
	// siteImportUserAgent identifies the mirror when fetching originals.
	siteImportUserAgent = "xrw-local-uploader/1.0"
)

// SiteImportRequest asks the importer to pull one site gallery into a draft.
type SiteImportRequest struct {
	SiteID   string   `json:"site_id"`
	PostID   int      `json:"post_id"`
	Title    string   `json:"title"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
}

// SiteImporter turns a gallery published on a remote site into a local draft.
// From there the existing draft editor and upload pipeline take over unchanged:
// the sites are just another producer of drafts, not a second publishing path.
type SiteImporter struct {
	sites         *sitealbum.Registry
	drafts        *TelegramImportStore
	client        *http.Client
	workDir       string
	maxImageBytes int64
	logger        *slog.Logger

	mu      sync.Mutex
	active  map[string]struct{}
	failed  map[string]string
	filling map[string]int
	slots   chan struct{}
	base    context.Context
}

// NewSiteImporter prepares an importer. The registry may be nil, in which case
// the server simply does not offer the site browser.
func NewSiteImporter(sites *sitealbum.Registry, drafts *TelegramImportStore, workDir string, maxImageBytes int64, logger *slog.Logger) (*SiteImporter, error) {
	if sites == nil || drafts == nil {
		return nil, errors.New("site importer needs a site registry and a draft store")
	}
	if maxImageBytes < 1 {
		return nil, errors.New("site importer image limit must be positive")
	}
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil, errors.New("site importer work directory is missing")
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("create site importer work directory: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &SiteImporter{
		sites: sites, drafts: drafts,
		client:  &http.Client{Timeout: 2 * time.Minute},
		workDir: workDir, maxImageBytes: maxImageBytes, logger: logger,
		active:  map[string]struct{}{},
		failed:  map[string]string{},
		filling: map[string]int{},
		slots:   make(chan struct{}, siteImportAlbums),
		base:    context.Background(),
	}, nil
}

// Bind attaches the process lifetime to queued imports. Background imports must
// not inherit a request context, which is cancelled the moment the response is
// written.
func (s *SiteImporter) Bind(ctx context.Context) {
	if ctx == nil {
		return
	}
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
}

// siteImportKey scopes a gallery to its site: two sites number their posts
// independently, so a bare post id would let one site's gallery block another's.
func siteImportKey(siteID string, postID int) string {
	return strings.TrimSpace(siteID) + "\x00" + strconv.Itoa(postID)
}

// Active reports how many album imports are running across every site.
func (s *SiteImporter) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// ActiveFor reports how many album imports are running for one site.
func (s *SiteImporter) ActiveFor(siteID string) int {
	prefix := strings.TrimSpace(siteID) + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for key := range s.active {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count
}

// IsActive reports whether one gallery is currently being pulled.
func (s *SiteImporter) IsActive(siteID string, postID int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, busy := s.active[siteImportKey(siteID, postID)]
	return busy
}

// Failure returns the last import error for a gallery, if any.
func (s *SiteImporter) Failure(siteID string, postID int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed[siteImportKey(siteID, postID)]
}

// Filling reports whether a draft is still being filled by a site import, along
// with the number of images the gallery advertises. Until the last image lands
// the draft really does hold zero files, which would otherwise look on the page
// like an import that produced nothing.
func (s *SiteImporter) Filling(draftID string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, busy := s.filling[draftID]
	return target, busy
}

func (s *SiteImporter) markFilling(draftID string, target int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filling[draftID] = target
}

func (s *SiteImporter) unmarkFilling(draftID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.filling, draftID)
}

// Queue starts background imports and returns how many galleries were accepted.
// Galleries that are already running are skipped, so a double click is harmless.
func (s *SiteImporter) Queue(siteID string, ids []int) int {
	accepted := 0
	for _, postID := range ids {
		if postID <= 0 || !s.claim(siteID, postID) {
			continue
		}
		accepted++
		go func(siteID string, postID int) {
			key := siteImportKey(siteID, postID)
			defer s.release(key)
			select {
			case s.slots <- struct{}{}:
			case <-s.base.Done():
				return
			}
			defer func() { <-s.slots }()
			request := SiteImportRequest{SiteID: siteID, PostID: postID}
			if _, err := s.runImport(s.base, request); err != nil {
				s.mu.Lock()
				s.failed[key] = err.Error()
				s.mu.Unlock()
				s.logger.Warn("import gallery", "site", siteID, "post", postID, "error", err)
				return
			}
			s.mu.Lock()
			delete(s.failed, key)
			s.mu.Unlock()
		}(siteID, postID)
	}
	return accepted
}

// claim reserves a gallery so two imports cannot write the same draft.
func (s *SiteImporter) claim(siteID string, postID int) bool {
	key := siteImportKey(siteID, postID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.active[key]; busy {
		return false
	}
	s.active[key] = struct{}{}
	delete(s.failed, key)
	return true
}

func (s *SiteImporter) release(key string) {
	s.mu.Lock()
	delete(s.active, key)
	s.mu.Unlock()
}

// Import downloads every image of a site gallery into a fresh draft and returns
// it. A failed import removes its draft, so a retry always starts from a clean
// slate instead of leaving a half-filled album behind.
func (s *SiteImporter) Import(ctx context.Context, request SiteImportRequest) (TelegramImportDraft, error) {
	if request.PostID <= 0 {
		return TelegramImportDraft{}, errors.New("图包编号无效")
	}
	if !s.claim(request.SiteID, request.PostID) {
		return TelegramImportDraft{}, errors.New("这个图包正在导入中，请稍候")
	}
	defer s.release(siteImportKey(request.SiteID, request.PostID))
	return s.runImport(ctx, request)
}

// runImport does the work. The caller must already hold the gallery claim.
func (s *SiteImporter) runImport(ctx context.Context, request SiteImportRequest) (TelegramImportDraft, error) {
	target, err := s.sites.Target(request.SiteID)
	if err != nil {
		return TelegramImportDraft{}, err
	}
	album, err := target.Store.Get(request.PostID)
	if err != nil {
		return TelegramImportDraft{}, err
	}

	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = strings.TrimSpace(album.Title)
	}
	if title == "" {
		title = album.Slug
	}
	draft, err := s.drafts.Create(CreateTelegramImportRequest{
		Kind: KindWordPress, SourceURL: album.Link, Title: title,
		Category: request.Category, Tags: request.Tags,
	})
	if err != nil {
		return TelegramImportDraft{}, err
	}
	// The draft is visible from the moment it is created, but stays empty until
	// the last image is staged. Mark it so the page can say "抓取中" instead of
	// showing an empty draft that looks like a failure.
	s.markFilling(draft.ID, album.Photos)
	defer s.unmarkFilling(draft.ID)
	if err := s.fill(ctx, draft.ID, target, album); err != nil {
		if removeErr := s.drafts.Delete(draft.ID); removeErr != nil {
			s.logger.Warn("discard failed site import draft", "draft", draft.ID, "error", removeErr)
		}
		return TelegramImportDraft{}, err
	}
	return s.drafts.Get(draft.ID)
}

// fill resolves the album's images and streams each one into the draft in
// display order.
func (s *SiteImporter) fill(ctx context.Context, draftID string, target sitealbum.Target, album sitealbum.Album) error {
	images, err := target.Source.AlbumImages(ctx, album.ID)
	if err != nil {
		return fmt.Errorf("读取源站图片列表失败：%w", err)
	}
	if len(images) == 0 {
		return errors.New("这个图包在源站没有可下载的图片")
	}
	if len(images) > maxTelegramDraftFiles {
		return fmt.Errorf("图包有 %d 张图片，超过 %d 张上限", len(images), maxTelegramDraftFiles)
	}
	staging, err := os.MkdirTemp(s.workDir, "site-import-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	staged, err := s.stage(ctx, staging, target, images)
	if err != nil {
		return err
	}
	for _, item := range staged {
		handle, err := os.Open(item.path)
		if err != nil {
			return err
		}
		_, _, addErr := s.drafts.AddFile(draftID, item.name, item.contentType, item.sourceURL, handle)
		closeErr := handle.Close()
		if addErr != nil {
			return addErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// stagedImage is one downloaded original waiting to enter a draft. Downloads run
// in parallel but are handed to the draft store strictly in gallery order,
// because AddFile numbers images by arrival.
type stagedImage struct {
	path        string
	name        string
	contentType string
	sourceURL   string
}

func (s *SiteImporter) stage(ctx context.Context, directory string, target sitealbum.Target, images []legacy.WPMedia) ([]stagedImage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	staged := make([]stagedImage, len(images))
	slots := make(chan struct{}, siteImportWorkers)
	var wait sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for index, image := range images {
		wait.Add(1)
		go func(index int, image legacy.WPMedia) {
			defer wait.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()

			mu.Lock()
			aborted := firstErr != nil
			mu.Unlock()
			if aborted {
				return
			}
			item, err := s.download(ctx, directory, target, index, image)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			staged[index] = item
		}(index, image)
	}
	wait.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return staged, nil
}

// download fetches one original into the staging directory. The originals are
// temporary: they are deleted as soon as the draft has copied them.
func (s *SiteImporter) download(ctx context.Context, directory string, site sitealbum.Target, index int, image legacy.WPMedia) (stagedImage, error) {
	extension := siteImageExtension(image)
	name := strings.TrimSpace(image.Slug)
	if name == "" {
		name = fmt.Sprintf("%06d", index+1)
	}
	target := filepath.Join(directory, fmt.Sprintf("%06d%s", index+1, extension))

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, image.URL, nil)
	if err != nil {
		return stagedImage{}, err
	}
	request.Header.Set("Accept", "image/*,*/*;q=0.8")
	request.Header.Set("User-Agent", siteImportUserAgent)
	request.Header.Set("Referer", site.Site.BaseURL+"/")
	response, err := s.client.Do(request)
	if err != nil {
		return stagedImage{}, fmt.Errorf("下载第 %d 张图片失败：%w", index+1, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return stagedImage{}, fmt.Errorf("下载第 %d 张图片失败：源站返回 HTTP %d", index+1, response.StatusCode)
	}
	handle, err := os.Create(target)
	if err != nil {
		return stagedImage{}, err
	}
	written, copyErr := io.Copy(handle, io.LimitReader(response.Body, s.maxImageBytes+1))
	closeErr := handle.Close()
	if copyErr != nil {
		return stagedImage{}, fmt.Errorf("下载第 %d 张图片失败：%w", index+1, copyErr)
	}
	if closeErr != nil {
		return stagedImage{}, closeErr
	}
	if written < 1 {
		return stagedImage{}, fmt.Errorf("第 %d 张图片是空文件", index+1)
	}
	if written > s.maxImageBytes {
		return stagedImage{}, fmt.Errorf("第 %d 张图片超过 %d MB", index+1, s.maxImageBytes/(1024*1024))
	}
	return stagedImage{
		path:        target,
		name:        name + extension,
		contentType: siteImageContentType(response.Header.Get("Content-Type"), extension),
		sourceURL:   image.URL,
	}, nil
}

// siteImageExtension picks a file extension the draft store accepts. The site
// publishes WebP, so the URL is authoritative; the fallback keeps an unnamed
// attachment usable.
func siteImageExtension(image legacy.WPMedia) string {
	extension := strings.ToLower(filepath.Ext(strings.TrimSpace(image.URL)))
	switch extension {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return extension
	}
	switch strings.ToLower(strings.TrimSpace(image.MimeType)) {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

// siteImageContentType prefers the origin's own header and falls back to the
// extension, so the draft store always receives a concrete image type.
func siteImageContentType(header, extension string) string {
	if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(header)); err == nil {
		if strings.HasPrefix(mediaType, "image/") {
			return mediaType
		}
	}
	if value := mime.TypeByExtension(extension); strings.HasPrefix(value, "image/") {
		return value
	}
	return "image/jpeg"
}
