package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	_ "golang.org/x/image/webp"

	"github.com/TyrEamon/xrw-album/publisher/internal/admin"
	"github.com/TyrEamon/xrw-album/publisher/internal/config"
	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	"github.com/TyrEamon/xrw-album/publisher/internal/store"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
	"github.com/TyrEamon/xrw-album/publisher/internal/veil"
)

var errNonContiguous = errors.New("gallery image ids are not contiguous")

const (
	telegramBatchSize = 10
)

type App struct {
	cfg      config.Config
	store    *store.Store
	veil     *veil.Client
	uploader *telegram.Client
	admin    *admin.Client
	logger   *slog.Logger
}

func New(cfg config.Config, database *store.Store, veilClient *veil.Client, uploader *telegram.Client, adminClient *admin.Client, logger *slog.Logger) *App {
	return &App{
		cfg: cfg, store: database, veil: veilClient,
		uploader: uploader, admin: adminClient, logger: logger,
	}
}

func (a *App) Discover(ctx context.Context, pageLimit, startOffset int) (int, error) {
	const pageSize = 100
	processPage := func(ctx context.Context, page int) (int, int, bool, error) {
		var items []model.GallerySummary
		var total int
		var hasNext bool
		for attempt := 1; ; attempt++ {
			var err error
			items, total, hasNext, err = a.veil.ListGalleries(ctx, pageSize, startOffset+page*pageSize)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return 0, 0, false, ctx.Err()
			}
			a.logger.Warn("discovery page retry", "page", page+1, "offset", startOffset+page*pageSize, "attempt", attempt, "error", err)
			timer := time.NewTimer(10 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 0, 0, false, ctx.Err()
			case <-timer.C:
			}
		}
		if err := a.store.UpsertGalleries(ctx, items); err != nil {
			return 0, 0, false, err
		}
		a.logger.Info("discovered gallery page", "page", page+1, "offset", startOffset+page*pageSize, "items", len(items), "total", total)
		return len(items), total, hasNext, nil
	}

	firstCount, total, hasNext, err := processPage(ctx, 0)
	if err != nil || !hasNext || firstCount == 0 || pageLimit == 1 {
		return firstCount, err
	}
	pageCount := (total - startOffset + pageSize - 1) / pageSize
	if pageLimit > 0 && pageLimit < pageCount {
		pageCount = pageLimit
	}
	if pageCount <= 1 {
		return firstCount, nil
	}

	discoveryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var discovered atomic.Int64
	discovered.Store(int64(firstCount))
	var firstErr error
	var errOnce sync.Once
	var workers sync.WaitGroup
	workerCount := max(a.cfg.GalleryWorkers, len(a.cfg.VeilProxies))
	workerCount = min(workerCount, pageCount-1)
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for page := range jobs {
				count, _, _, err := processPage(discoveryCtx, page)
				if err != nil {
					errOnce.Do(func() {
						firstErr = err
						cancel()
					})
					return
				}
				discovered.Add(int64(count))
			}
		}()
	}
	for page := 1; page < pageCount; page++ {
		select {
		case jobs <- page:
		case <-discoveryCtx.Done():
			page = pageCount
		}
	}
	close(jobs)
	workers.Wait()
	return int(discovered.Load()), firstErr
}

func (a *App) Run(ctx context.Context, maximum int) error {
	if !a.uploader.Enabled() {
		return fmt.Errorf("TG_BOT_TOKEN and IMAGE_PUBLIC_BASE are required to process galleries")
	}
	if len(a.cfg.TGChatIDs) == 0 {
		return fmt.Errorf("TG_CHAT_IDS or TG_CHAT_ID is required to process galleries")
	}
	if err := a.store.BackfillUploadedChannels(ctx, a.cfg.TGChatIDs[0]); err != nil {
		return fmt.Errorf("backfill historical Telegram channel: %w", err)
	}
	var claimed atomic.Int64
	var wg sync.WaitGroup
	errChannel := make(chan error, a.cfg.GalleryWorkers)

	for worker := 0; worker < a.cfg.GalleryWorkers; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				if maximum > 0 && claimed.Load() >= int64(maximum) {
					return
				}
				gallery, found, err := a.store.ClaimNext(ctx, a.admin.Enabled())
				if err != nil {
					errChannel <- err
					return
				}
				if !found {
					return
				}
				current := claimed.Add(1)
				if maximum > 0 && current > int64(maximum) {
					_ = a.store.MarkGallery(ctx, gallery.SourceGalleryID, gallery.Status, gallery.LastError, 0)
					return
				}
				if isUnsupportedVideo(gallery) {
					const reason = "unsupported video entry: no image or attachment payload"
					_ = a.store.MarkGallery(ctx, gallery.SourceGalleryID, "blocked", reason, 0)
					a.logger.Info("video gallery skipped", "gallery", gallery.SourceGalleryID, "title", gallery.Title)
					continue
				}
				a.logger.Info("processing gallery", "worker", workerID, "gallery", gallery.SourceGalleryID, "title", gallery.Title)
				if err := a.processGallery(ctx, gallery); err != nil {
					if ctx.Err() != nil {
						return
					}
					if errors.Is(err, errNonContiguous) {
						_ = a.store.MarkGallery(ctx, gallery.SourceGalleryID, "blocked", err.Error(), 0)
						a.logger.Error("gallery blocked", "gallery", gallery.SourceGalleryID, "error", err)
						continue
					}
					quarantined, markErr := a.store.MarkGalleryFailure(ctx, gallery.SourceGalleryID, err.Error(), 5*time.Minute, a.cfg.GalleryMaxRetries)
					if markErr != nil {
						errChannel <- markErr
						return
					}
					if quarantined {
						a.logger.Error("gallery quarantined after repeated failures", "gallery", gallery.SourceGalleryID, "error", err)
					} else {
						a.logger.Error("gallery failed; retry scheduled", "gallery", gallery.SourceGalleryID, "error", err)
					}
				}
			}
		}(worker + 1)
	}
	wg.Wait()
	close(errChannel)
	for err := range errChannel {
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) processGallery(ctx context.Context, gallery model.Gallery) error {
	targetChat, err := a.store.AssignTargetChat(ctx, gallery.SourceGalleryID, a.cfg.ChatForGallery(gallery.SourceGalleryID))
	if err != nil {
		return fmt.Errorf("assign Telegram channel: %w", err)
	}
	gallery.TargetChatID = targetChat
	if a.admin.Enabled() {
		check, err := a.admin.Check(ctx, gallery.AlbumID)
		if err != nil {
			return fmt.Errorf("check D1 state: %w", err)
		}
		if check.Status == "ok" &&
			check.SourceUpdatedAt == gallery.SourceUpdatedAt &&
			check.ExpectedCount == gallery.ImageCount &&
			check.PublishedCount == gallery.ImageCount {
			return a.store.MarkGallery(ctx, gallery.SourceGalleryID, "ok", "", 0)
		}
	}

	if gallery.Status != "ready" {
		detail, err := a.veil.Gallery(ctx, gallery.SourceGalleryID)
		if err != nil {
			return fmt.Errorf("fetch gallery detail: %w", err)
		}
		if detail.ID != gallery.SourceGalleryID {
			return fmt.Errorf("gallery detail id %d does not match requested id %d", detail.ID, gallery.SourceGalleryID)
		}
		normalizeDetail(&detail)
		if err := validateDetail(detail); err != nil {
			return err
		}
		gallery.Title = detail.Title
		gallery.Category = detail.Category
		gallery.ImageCount = detail.ImageCount
		gallery.CoverImageID = detail.CoverImageID
		gallery.SourceUpdatedAt = detail.UpdatedAt
		gallery.Tags = append([]string(nil), detail.Tags...)
		refs := make(map[int]model.Image, len(detail.Images))
		for _, ref := range detail.Images {
			refs[ref.SortOrder] = model.Image{
				SourceImageID: ref.ID,
				GalleryID:     gallery.SourceGalleryID,
				SortOrder:     ref.SortOrder,
				Width:         ref.Width,
				Height:        ref.Height,
			}
		}
		if err := a.store.ApplyDetail(ctx, gallery, refs); err != nil {
			return err
		}

		for {
			items, err := a.store.NextImages(ctx, gallery.SourceGalleryID, telegramBatchSize)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				break
			}
			if err := a.processImageGroup(ctx, gallery, items); err != nil {
				return err
			}
		}
	}

	payload, err := a.store.Payload(ctx, gallery.SourceGalleryID, gallery.TargetChatID)
	if err != nil {
		return err
	}
	if err := a.writeOutbox(payload); err != nil {
		return err
	}
	if !a.admin.Enabled() {
		a.logger.Info("gallery ready in outbox", "gallery", gallery.SourceGalleryID)
		return a.store.MarkGallery(ctx, gallery.SourceGalleryID, "ready", "", 0)
	}
	if err := a.admin.Publish(ctx, payload); err != nil {
		return fmt.Errorf("publish D1 snapshot: %w", err)
	}
	a.logger.Info("gallery published", "gallery", gallery.SourceGalleryID, "images", len(payload.Photos))
	return a.store.MarkGallery(ctx, gallery.SourceGalleryID, "ok", "", 0)
}

type preparedImage struct {
	item        model.Image
	path        string
	contentType string
	width       int
	height      int
	bytes       int64
}

func (a *App) processImageGroup(ctx context.Context, gallery model.Gallery, items []model.Image) error {
	prepared := make([]preparedImage, 0, len(items))
	for _, item := range items {
		image, err := a.prepareImage(ctx, gallery, item)
		if err != nil {
			_ = a.store.MarkImageFailed(ctx, item.SourceImageID, err.Error())
			return fmt.Errorf("image %d: %w", item.SourceImageID, err)
		}
		prepared = append(prepared, image)
	}

	caption := ""
	if prepared[0].item.SortOrder == 1 {
		caption = galleryCaption(gallery.Title, gallery.Tags)
	}

	uploadItems := make([]telegram.UploadItem, len(prepared))
	for index, image := range prepared {
		uploadItems[index] = telegram.UploadItem{
			Path: image.path, ContentType: image.contentType,
			PublicKey: fmt.Sprintf("veil-%d", image.item.SourceImageID),
		}
	}
	uploaded, err := a.uploader.UploadGroup(ctx, gallery.TargetChatID, uploadItems, caption)
	if err != nil {
		failed := prepared[0].item.SourceImageID
		_ = a.store.MarkImageFailed(ctx, failed, err.Error())
		return fmt.Errorf("store Telegram documents from image %d: %w", failed, err)
	}
	checkpoints := make([]model.Image, len(uploaded))
	for index, result := range uploaded {
		image := &prepared[index]
		image.item.TGURL = result.URL
		image.item.TGFileID = result.FileID
		image.item.TGFileUnique = result.FileUniqueID
		image.item.TGMessageID = result.MessageID
		image.item.TGPublicKey = result.PublicKey
		image.item.TGMimeType = result.ContentType
		image.item.Width = image.width
		image.item.Height = image.height
		checkpoints[index] = image.item
	}
	if err := a.store.MarkImagesUploaded(ctx, checkpoints); err != nil {
		return err
	}
	var uploadedBytes int64
	for _, image := range prepared {
		uploadedBytes += image.bytes
	}
	if err := a.store.AddTraffic(ctx, "telegram_file_bytes", uploadedBytes); err != nil {
		return err
	}

	for _, image := range prepared {
		if err := os.Remove(image.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.logger.Warn("remove uploaded image", "path", image.path, "error", err)
		}
	}
	a.logger.Info("uploaded image group", "gallery", gallery.SourceGalleryID,
		"first_order", prepared[0].item.SortOrder, "images", len(prepared))
	return nil
}

func (a *App) prepareImage(ctx context.Context, gallery model.Gallery, item model.Image) (preparedImage, error) {
	directory := filepath.Join(a.cfg.WorkDir, fmt.Sprintf("%d", gallery.SourceGalleryID))
	basePath := filepath.Join(directory, fmt.Sprintf("%d", item.SourceImageID))
	path, contentType := existingImage(basePath)
	if path == "" {
		download, err := a.veil.DownloadImage(ctx, item.SourceImageID, gallery.SourceGalleryID, basePath)
		if err != nil {
			return preparedImage{}, err
		}
		path, contentType = download.Path, download.ContentType
		if err := a.store.AddTraffic(ctx, "source_image_bytes", download.Bytes); err != nil {
			return preparedImage{}, err
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return preparedImage{}, err
	}
	width, height := item.Width, item.Height
	if width <= 0 || height <= 0 {
		width, height = imageSize(path)
	}
	if width <= 0 || height <= 0 {
		return preparedImage{}, fmt.Errorf("cannot determine image dimensions for %s", contentType)
	}
	return preparedImage{item: item, path: path, contentType: contentType, width: width, height: height, bytes: info.Size()}, nil
}

func galleryCaption(title string, tags []string) string {
	const maxCaptionRunes = 900
	var hashtags []string
	seen := make(map[string]struct{})
	used := 0
	for _, tag := range tags {
		hashtag := normalizeHashtag(tag)
		if hashtag == "" {
			continue
		}
		key := strings.ToLower(hashtag)
		if _, exists := seen[key]; exists {
			continue
		}
		addition := len([]rune(hashtag)) + 1
		if used+addition > 500 {
			break
		}
		seen[key] = struct{}{}
		hashtags = append(hashtags, "#"+hashtag)
		used += addition
	}
	tagLine := strings.Join(hashtags, " ")
	remaining := maxCaptionRunes - len([]rune(tagLine))
	if tagLine != "" {
		remaining--
	}
	if remaining < 1 {
		remaining = 1
	}
	title = truncateRunes(strings.TrimSpace(title), remaining)
	quote := "<blockquote>" + html.EscapeString(title) + "</blockquote>"
	if tagLine == "" {
		return quote
	}
	return tagLine + "\n" + quote
}

func normalizeHashtag(value string) string {
	value = strings.TrimSpace(strings.TrimLeft(value, "#"))
	var result []rune
	underscore := false
	for _, char := range value {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '_' {
			result = append(result, char)
			underscore = char == '_'
		} else if len(result) > 0 && !underscore {
			result = append(result, '_')
			underscore = true
		}
		if len(result) >= 64 {
			break
		}
	}
	return strings.Trim(string(result), "_")
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	if maximum <= 1 {
		return string(runes[:maximum])
	}
	return string(runes[:maximum-1]) + "…"
}

func isUnsupportedVideo(gallery model.Gallery) bool {
	return strings.EqualFold(strings.TrimSpace(gallery.Category), "video")
}

func normalizeDetail(detail *veil.GalleryDetail) {
	if detail.CoverImageID != 0 {
		return
	}
	for _, ref := range detail.Images {
		if ref.SortOrder == 1 {
			detail.CoverImageID = ref.ID
			return
		}
	}
}

func validateDetail(detail veil.GalleryDetail) error {
	if detail.ID == 0 || detail.ImageCount <= 0 || detail.CoverImageID == 0 {
		return fmt.Errorf("gallery detail misses id, image_count, or cover_image_id")
	}
	if len(detail.Images) == 0 {
		return fmt.Errorf("gallery detail returned no image references")
	}
	for _, ref := range detail.Images {
		if ref.SortOrder <= 0 || ref.ID != detail.CoverImageID+int64(ref.SortOrder-1) {
			return fmt.Errorf("%w: image %d at order %d", errNonContiguous, ref.ID, ref.SortOrder)
		}
	}
	return nil
}

func existingImage(basePath string) (string, string) {
	for _, extension := range []string{".jpg", ".png", ".gif", ".webp"} {
		path := basePath + extension
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return path, mime.TypeByExtension(extension)
		}
	}
	return "", ""
}

func imageSize(path string) (int, int) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
}

func (a *App) writeOutbox(payload model.PublishPayload) error {
	directory := filepath.Join(a.cfg.WorkDir, "outbox")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := filepath.Join(directory, payload.ID+".json")
	if err := writeAtomic(jsonPath, append(data, '\n')); err != nil {
		return err
	}
	var text strings.Builder
	text.WriteString(payload.Title)
	text.WriteByte('\n')
	for _, photo := range payload.Photos {
		text.WriteString(photo.TGURL)
		text.WriteByte('\n')
	}
	return writeAtomic(filepath.Join(directory, payload.ID+".txt"), []byte(text.String()))
}

func writeAtomic(path string, data []byte) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}
