package legacy

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
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

const uploadGroupSize = 10

type Uploader interface {
	UploadGroup(context.Context, string, []telegram.UploadItem, string) ([]telegram.Result, error)
}

type Runner struct {
	store        *Store
	uploader     Uploader
	client       *http.Client
	workDir      string
	maxImageSize int64
	maxRetries   int
	retryDelay   time.Duration
	workers      int
	logger       *slog.Logger
}

func NewRunner(store *Store, uploader Uploader, workDir string, maxImageSize int64, maxRetries, workers int, timeout, retryDelay time.Duration, logger *slog.Logger) *Runner {
	return &Runner{
		store: store, uploader: uploader, client: &http.Client{Timeout: timeout},
		workDir: workDir, maxImageSize: maxImageSize, maxRetries: maxRetries,
		retryDelay: retryDelay, workers: workers, logger: logger,
	}
}

func (r *Runner) Run(ctx context.Context, maximum int) error {
	var claimed atomic.Int64
	errChannel := make(chan error, r.workers)
	var wait sync.WaitGroup
	for worker := 1; worker <= r.workers; worker++ {
		wait.Add(1)
		go func(workerID int) {
			defer wait.Done()
			for ctx.Err() == nil {
				if maximum > 0 && claimed.Load() >= int64(maximum) {
					return
				}
				album, found, err := r.store.ClaimNext(ctx)
				if err != nil {
					errChannel <- err
					return
				}
				if !found {
					return
				}
				current := claimed.Add(1)
				if maximum > 0 && current > int64(maximum) {
					_ = r.store.MarkUploadFailure(ctx, album.ID, "returned to queue", 0)
					return
				}
				r.logger.Info("processing legacy album", "worker", workerID, "album", album.ID, "title", album.Title)
				if err := r.processAlbum(ctx, album); err != nil && ctx.Err() == nil {
					r.logger.Error("legacy album deferred", "album", album.ID, "error", err)
				}
			}
		}(worker)
	}
	wait.Wait()
	close(errChannel)
	for err := range errChannel {
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) processAlbum(ctx context.Context, album Album) error {
	items, err := r.store.Images(ctx, album.ID)
	if err != nil {
		return err
	}
	if len(items) != album.Expected {
		message := fmt.Sprintf("source contains %d images, expected %d", len(items), album.Expected)
		_ = r.store.MarkUploadFailure(ctx, album.ID, message, r.retryDelay)
		return errors.New(message)
	}

	// Validate every source image before publishing the first Telegram group. This
	// keeps a permanently broken album from leaving a partial post in the channel.
	for index := range items {
		item := &items[index]
		if item.Status == "uploaded" {
			continue
		}
		if item.LocalPath != "" {
			if info, err := os.Stat(item.LocalPath); err == nil && info.Size() == item.Bytes && item.Width > 0 && item.Height > 0 {
				continue
			}
		}
		downloaded, status, err := r.download(ctx, album.ID, *item)
		if err != nil {
			invalid, markErr := r.store.MarkSourceFailure(ctx, album.ID, item.SortOrder, status, r.maxRetries, err.Error(), r.retryDelay)
			if markErr != nil {
				return markErr
			}
			if invalid {
				_ = os.RemoveAll(filepath.Join(r.workDir, album.ID))
				r.logger.Warn("legacy album marked invalid", "album", album.ID, "image", item.SortOrder, "error", err)
			}
			return err
		}
		*item = downloaded
		if err := r.store.MarkDownloaded(ctx, *item); err != nil {
			return err
		}
		if err := r.store.AddTraffic(ctx, "source_image_bytes", item.Bytes); err != nil {
			return err
		}
	}

	for start := 0; start < len(items); {
		for start < len(items) && items[start].Status == "uploaded" {
			start++
		}
		if start >= len(items) {
			break
		}
		end := start
		for end < len(items) && end-start < uploadGroupSize && items[end].Status != "uploaded" {
			end++
		}
		group := items[start:end]
		uploadItems := make([]telegram.UploadItem, len(group))
		for index, item := range group {
			uploadItems[index] = telegram.UploadItem{
				Path: item.LocalPath, ContentType: item.ContentType,
				PublicKey: fmt.Sprintf("legacy-%s-%d", album.ID, item.SortOrder),
			}
		}
		caption := ""
		if group[0].SortOrder == 1 {
			caption = "<blockquote>" + html.EscapeString(album.Title) + "</blockquote>"
		}
		results, err := r.uploader.UploadGroup(ctx, album.TargetChatID, uploadItems, caption)
		if err != nil {
			_ = r.store.MarkUploadFailure(ctx, album.ID, err.Error(), r.retryDelay)
			return err
		}
		var uploadedBytes int64
		checkpoints := make([]Image, len(group))
		for index, result := range results {
			item := group[index]
			item.TGURL = result.URL
			item.TGFileID = result.FileID
			item.TGFileUnique = result.FileUniqueID
			item.TGMessageID = result.MessageID
			item.TGPublicKey = result.PublicKey
			item.Status = "uploaded"
			checkpoints[index] = item
			uploadedBytes += item.Bytes
		}
		if err := r.store.MarkUploaded(ctx, checkpoints); err != nil {
			return err
		}
		if err := r.store.AddTraffic(ctx, "telegram_file_bytes", uploadedBytes); err != nil {
			return err
		}
		for index := range group {
			items[start+index] = checkpoints[index]
			if err := os.Remove(group[index].LocalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				r.logger.Warn("remove legacy upload", "path", group[index].LocalPath, "error", err)
			}
		}
		r.logger.Info("uploaded legacy image group", "album", album.ID, "first_order", group[0].SortOrder, "images", len(group))
		start = end
	}

	if err := r.writeOutbox(album, items); err != nil {
		_ = r.store.MarkUploadFailure(ctx, album.ID, err.Error(), r.retryDelay)
		return err
	}
	if err := r.store.MarkReady(ctx, album.ID); err != nil {
		return err
	}
	_ = os.RemoveAll(filepath.Join(r.workDir, album.ID))
	r.logger.Info("legacy album ready", "album", album.ID, "images", len(items))
	return nil
}

func (r *Runner) download(ctx context.Context, albumID string, item Image) (Image, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, item.SourceURL, nil)
	if err != nil {
		return item, 0, err
	}
	request.Header.Set("Accept", "image/*,*/*;q=0.8")
	request.Header.Set("User-Agent", "xrw-legacy-mirror/1.0")
	response, err := r.client.Do(request)
	if err != nil {
		return item, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return item, response.StatusCode, fmt.Errorf("source HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if response.ContentLength > r.maxImageSize {
		return item, response.StatusCode, fmt.Errorf("source image exceeds %d bytes", r.maxImageSize)
	}

	directory := filepath.Join(r.workDir, albumID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return item, response.StatusCode, err
	}
	temporary := filepath.Join(directory, fmt.Sprintf("%06d.part", item.SortOrder))
	file, err := os.Create(temporary)
	if err != nil {
		return item, response.StatusCode, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, r.maxImageSize+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written > r.maxImageSize {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return item, response.StatusCode, copyErr
		}
		if closeErr != nil {
			return item, response.StatusCode, closeErr
		}
		return item, response.StatusCode, fmt.Errorf("source image exceeds %d bytes", r.maxImageSize)
	}
	width, height, format := decodeConfig(temporary)
	if width <= 0 || height <= 0 {
		_ = os.Remove(temporary)
		return item, response.StatusCode, errors.New("source response is not a supported image")
	}
	contentType, extension := imageFormat(format)
	finalPath := filepath.Join(directory, fmt.Sprintf("%06d%s", item.SortOrder, extension))
	_ = os.Remove(finalPath)
	if err := os.Rename(temporary, finalPath); err != nil {
		_ = os.Remove(temporary)
		return item, response.StatusCode, err
	}
	item.Width, item.Height, item.Bytes = width, height, written
	item.ContentType, item.LocalPath, item.Status = contentType, finalPath, "downloaded"
	return item, response.StatusCode, nil
}

func decodeConfig(path string) (int, int, string) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, ""
	}
	defer file.Close()
	config, format, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0, ""
	}
	return config.Width, config.Height, format
}

func imageFormat(format string) (string, string) {
	switch strings.ToLower(format) {
	case "png":
		return "image/png", ".png"
	case "gif":
		return "image/gif", ".gif"
	case "webp":
		return "image/webp", ".webp"
	default:
		return "image/jpeg", ".jpg"
	}
}

func (r *Runner) writeOutbox(album Album, items []Image) error {
	type photo struct {
		ID     int    `json:"id"`
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	type tgFile struct {
		PublicKey   string `json:"public_key"`
		URL         string `json:"url"`
		FileID      string `json:"file_id"`
		FileUnique  string `json:"file_unique_id,omitempty"`
		MessageID   int64  `json:"message_id,omitempty"`
		ChannelID   string `json:"channel_id"`
		ContentType string `json:"content_type"`
	}
	photos := make([]photo, len(items))
	files := make([]tgFile, len(items))
	for index, item := range items {
		photos[index] = photo{ID: item.SortOrder, URL: item.TGURL, Width: item.Width, Height: item.Height}
		files[index] = tgFile{PublicKey: item.TGPublicKey, URL: item.TGURL, FileID: item.TGFileID,
			FileUnique: item.TGFileUnique, MessageID: item.TGMessageID, ChannelID: album.TargetChatID,
			ContentType: item.ContentType}
	}
	cover := ""
	if len(photos) > 0 {
		cover = photos[0].URL
	}
	source := album.Source
	if source == "" {
		source = SourceLinuxDO85W
	}
	payload := map[string]any{
		"id": album.ID, "source": source, "title": album.Title,
		"count": len(items), "cover": cover, "href": "/album/" + album.ID,
		"status": "ok", "photos": photos, "tg_files": files,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Join(r.workDir, "outbox")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(directory, album.ID+".json"), append(data, '\n'))
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
