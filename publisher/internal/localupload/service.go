package localupload

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "golang.org/x/image/webp"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	"github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

const uploadBatchSize = 10

type Options struct {
	SnapshotDir   string
	ImageBase     string
	SigningSecret string
	MaxImageBytes int64
	ChatIDs       []string
}

func (options Options) Validate() error {
	if len(options.ChatIDs) == 0 {
		return errors.New("TG_CHAT_IDS or TG_CHAT_ID is missing")
	}
	if options.MaxImageBytes < 1 {
		return errors.New("MAX_IMAGE_MB must be positive")
	}
	if strings.TrimSpace(options.SnapshotDir) == "" {
		return errors.New("snapshot directory is missing")
	}
	if strings.TrimSpace(options.ImageBase) == "" || strings.TrimSpace(options.SigningSecret) == "" {
		return errors.New("GIMG_PUBLIC_BASE and GIMG_SIGNING_SECRET are required")
	}
	return (snapshot.Options{ImageBase: options.ImageBase, SigningSecret: options.SigningSecret}).Validate()
}

type CreateRequest struct {
	Folder    string   `json:"folder"`
	Title     string   `json:"title"`
	Category  string   `json:"category"`
	Tags      []string `json:"tags"`
	ChannelID string   `json:"channel_id"`
}

type Service struct {
	store           *Store
	uploader        *telegram.Client
	options         Options
	logger          *slog.Logger
	wake            chan struct{}
	start           sync.Once
	readyHook       func(string) error
	publishSnapshot func(context.Context, string) error
}

func (s *Service) SetReadyHook(hook func(string) error) {
	s.readyHook = hook
}

func (s *Service) SetSnapshotPublisher(publish func(context.Context, string) error) {
	s.publishSnapshot = publish
}

func NewService(store *Store, uploader *telegram.Client, options Options, logger *slog.Logger) *Service {
	return &Service{
		store: store, uploader: uploader, options: options, logger: logger,
		wake: make(chan struct{}, 1),
	}
}

func (s *Service) Ready() error {
	if !s.uploader.Enabled() {
		return errors.New("TG_BOT_TOKEN and GIMG_PUBLIC_BASE are required")
	}
	return s.options.Validate()
}

func (s *Service) Start(ctx context.Context) {
	s.start.Do(func() {
		go s.loop(ctx)
		s.Notify()
	})
}

func (s *Service) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		for {
			if err := s.Ready(); err != nil {
				s.logger.Warn("local uploader is not configured", "error", err)
				break
			}
			job, found, err := s.store.ClaimNext(ctx)
			if err != nil {
				s.logger.Error("claim local upload", "error", err)
				break
			}
			if !found {
				break
			}
			if err := s.process(ctx, job); err != nil {
				_ = s.store.MarkFailed(context.Background(), job.ID, err)
				s.logger.Error("local upload failed", "job", job.ID, "error", err)
			}
		}
	}
}

func (s *Service) Create(ctx context.Context, request CreateRequest) (Job, error) {
	if err := s.Ready(); err != nil {
		return Job{}, err
	}
	folder, err := filepath.Abs(strings.TrimSpace(request.Folder))
	if err != nil {
		return Job{}, err
	}
	info, err := os.Stat(folder)
	if err != nil {
		return Job{}, fmt.Errorf("open photo folder: %w", err)
	}
	if !info.IsDir() {
		return Job{}, errors.New("selected path is not a folder")
	}
	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = filepath.Base(folder)
	}
	channelID := strings.TrimSpace(request.ChannelID)
	if !contains(s.options.ChatIDs, channelID) {
		return Job{}, errors.New("selected Telegram channel is not configured")
	}

	id, numericID, err := newJobID()
	if err != nil {
		return Job{}, err
	}
	files, totalBytes, err := scanFolder(folder, id, s.options.MaxImageBytes)
	if err != nil {
		return Job{}, err
	}
	now := time.Now().Unix()
	job := Job{
		ID: id, NumericID: numericID, Title: title,
		Category: strings.TrimSpace(request.Category), Tags: normalizeTags(request.Tags),
		Folder: folder, ChannelID: channelID, Status: "pending",
		TotalFiles: len(files), TotalBytes: totalBytes, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.Create(ctx, job, files); err != nil {
		return Job{}, err
	}
	s.Notify()
	return job, nil
}

func (s *Service) Retry(ctx context.Context, id string) error {
	if err := s.Ready(); err != nil {
		return err
	}
	if err := s.store.Retry(ctx, id); err != nil {
		return err
	}
	s.Notify()
	return nil
}

func (s *Service) ImageURL(fileID string) (string, error) {
	return snapshot.SignedTelegramURL(s.options.ImageBase, s.options.SigningSecret, fileID)
}

func (s *Service) process(ctx context.Context, job Job) error {
	for {
		files, err := s.store.PendingFiles(ctx, job.ID, uploadBatchSize)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			break
		}
		items := make([]telegram.UploadItem, len(files))
		for index, file := range files {
			if info, err := os.Stat(file.Path); err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
				if err == nil {
					err = errors.New("file size changed after the task was created")
				}
				return fmt.Errorf("%s: %w", file.Name, err)
			}
			items[index] = telegram.UploadItem{
				Path: file.Path, ContentType: file.ContentType, PublicKey: file.PublicKey,
			}
		}
		caption := ""
		if files[0].Position == 1 {
			caption = galleryCaption(job.Title, job.Tags)
		}
		results, err := s.uploadWithRetry(ctx, job.ChannelID, items, caption)
		if err != nil {
			return err
		}
		if err := s.store.MarkUploaded(ctx, job.ID, files, results); err != nil {
			return err
		}
		s.logger.Info("local upload group complete", "job", job.ID,
			"first", files[0].Position, "count", len(files))
	}

	job, err := s.store.Get(ctx, job.ID)
	if err != nil {
		return err
	}
	files, err := s.store.Files(ctx, job.ID)
	if err != nil {
		return err
	}
	path, err := s.writeSnapshot(job, files)
	if err != nil {
		return err
	}
	if s.publishSnapshot != nil {
		if err := s.publishSnapshot(ctx, path); err != nil {
			return fmt.Errorf("publish snapshot to GitHub: %w", err)
		}
	}
	if err := s.store.MarkReady(ctx, job.ID, path); err != nil {
		return err
	}
	if s.readyHook != nil {
		if err := s.readyHook(job.ID); err != nil {
			s.logger.Error("clean completed local upload files", "job", job.ID, "error", err)
		}
	}
	s.logger.Info("local gallery ready", "job", job.ID, "snapshot", path)
	return nil
}

func (s *Service) uploadWithRetry(ctx context.Context, chatID string, items []telegram.UploadItem, caption string) ([]telegram.Result, error) {
	for attempt := 1; attempt <= 5; attempt++ {
		results, err := s.uploader.UploadGroup(ctx, chatID, items, caption)
		if err == nil {
			return results, nil
		}
		var retry *telegram.RetryAfterError
		if !errors.As(err, &retry) || attempt == 5 {
			return nil, err
		}
		delay := retry.Duration
		if delay < time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("Telegram upload retries exhausted")
}

func (s *Service) writeSnapshot(job Job, files []File) (string, error) {
	if len(files) != job.TotalFiles {
		return "", fmt.Errorf("job expects %d files but database contains %d", job.TotalFiles, len(files))
	}
	photos := make([]model.Image, len(files))
	for index, file := range files {
		if file.Status != "uploaded" || file.FileID == "" {
			return "", fmt.Errorf("image %d is not uploaded", file.Position)
		}
		imageURL, err := snapshot.SignedTelegramURL(s.options.ImageBase, s.options.SigningSecret, file.FileID)
		if err != nil {
			return "", err
		}
		photos[index] = model.Image{
			SourceImageID: int64(file.Position), SortOrder: file.Position,
			Width: file.Width, Height: file.Height, TGURL: imageURL,
		}
	}
	payload := model.PublishPayload{
		ID: job.ID, Source: "manual", SourceGalleryID: job.NumericID,
		SourceUpdatedAt: time.Unix(job.CreatedAt, 0).UTC().Format(time.RFC3339),
		Title:           job.Title, Category: job.Category, Tags: job.Tags,
		Count: len(photos), Cover: photos[0].TGURL, Href: "/album/" + job.ID,
		Status: "ok", Photos: photos,
	}
	now := time.Now().UTC()
	batch := snapshot.Batch{
		Version: 1, ExportedAt: now.Format(time.RFC3339Nano),
		Galleries: []model.PublishPayload{payload},
	}
	encoded, err := json.Marshal(batch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.options.SnapshotDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("manual-snapshot-%s-%s.json", now.Format("20060102T150405.000000000Z"), job.ID)
	target := filepath.Join(s.options.SnapshotDir, name)
	if err := writeAtomic(target, append(encoded, '\n')); err != nil {
		return "", err
	}
	return target, nil
}

func scanFolder(folder, jobID string, maximum int64) ([]File, int64, error) {
	type candidate struct {
		path string
		name string
	}
	var candidates []candidate
	err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if !contains([]string{".jpg", ".jpeg", ".png", ".gif", ".webp"}, extension) {
			return nil
		}
		relative, err := filepath.Rel(folder, path)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{path: path, name: relative})
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("scan photo folder: %w", err)
	}
	sort.Slice(candidates, func(left, right int) bool {
		return naturalLess(candidates[left].name, candidates[right].name)
	})
	if len(candidates) == 0 {
		return nil, 0, errors.New("文件夹里没有 JPG、PNG、GIF 或 WebP 图片")
	}

	files := make([]File, 0, len(candidates))
	var total int64
	for index, candidate := range candidates {
		info, err := os.Stat(candidate.path)
		if err != nil {
			return nil, 0, err
		}
		if info.Size() <= 0 {
			return nil, 0, fmt.Errorf("图片为空：%s", candidate.name)
		}
		if info.Size() > maximum {
			return nil, 0, fmt.Errorf("图片超过 %d MB：%s", maximum/(1024*1024), candidate.name)
		}
		width, height, err := imageDimensions(candidate.path)
		if err != nil {
			return nil, 0, fmt.Errorf("读取图片尺寸 %s：%w", candidate.name, err)
		}
		position := index + 1
		files = append(files, File{
			JobID: jobID, Position: position, Path: candidate.path, Name: candidate.name,
			ContentType: contentType(candidate.path), Size: info.Size(), Width: width, Height: height,
			PublicKey: fmt.Sprintf("%s-%06d", jobID, position), Status: "pending",
		})
		total += info.Size()
	}
	return files, total, nil
}

func imageDimensions(path string) (int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	config, _, err := image.DecodeConfig(file)
	if err != nil {
		return 0, 0, err
	}
	if config.Width < 1 || config.Height < 1 {
		return 0, 0, errors.New("invalid image dimensions")
	}
	return config.Width, config.Height, nil
}

func contentType(path string) string {
	value := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if separator := strings.IndexByte(value, ';'); separator >= 0 {
		value = value[:separator]
	}
	if value == "" {
		value = "application/octet-stream"
	}
	return value
}

func newJobID() (string, int64, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", 0, err
	}
	id := "manual-" + time.Now().UTC().Format("20060102") + "-" + hex.EncodeToString(random[:4])
	numeric := -int64(binary.BigEndian.Uint32(random[4:])) - 1
	return id, numeric, nil
}

func normalizeTags(values []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.TrimLeft(value, "#"))
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func galleryCaption(title string, tags []string) string {
	var hashtags []string
	for _, tag := range tags {
		if tag = normalizeHashtag(tag); tag != "" {
			hashtags = append(hashtags, "#"+tag)
		}
	}
	line := strings.Join(hashtags, " ")
	maximum := 900 - len([]rune(line))
	if line != "" {
		maximum--
	}
	if maximum < 1 {
		maximum = 1
	}
	quote := "<blockquote>" + html.EscapeString(truncateRunes(strings.TrimSpace(title), maximum)) + "</blockquote>"
	if line == "" {
		return quote
	}
	return line + "\n" + quote
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

func naturalLess(left, right string) bool {
	left = strings.ToLower(filepath.ToSlash(left))
	right = strings.ToLower(filepath.ToSlash(right))
	for len(left) > 0 && len(right) > 0 {
		leftDigit := left[0] >= '0' && left[0] <= '9'
		rightDigit := right[0] >= '0' && right[0] <= '9'
		if leftDigit && rightDigit {
			leftEnd, rightEnd := 0, 0
			for leftEnd < len(left) && left[leftEnd] >= '0' && left[leftEnd] <= '9' {
				leftEnd++
			}
			for rightEnd < len(right) && right[rightEnd] >= '0' && right[rightEnd] <= '9' {
				rightEnd++
			}
			leftNumber, _ := strconv.ParseUint(strings.TrimLeft(left[:leftEnd], "0"), 10, 64)
			rightNumber, _ := strconv.ParseUint(strings.TrimLeft(right[:rightEnd], "0"), 10, 64)
			if leftNumber != rightNumber {
				return leftNumber < rightNumber
			}
			if leftEnd != rightEnd {
				return leftEnd < rightEnd
			}
			left, right = left[leftEnd:], right[rightEnd:]
			continue
		}
		if left[0] != right[0] {
			return left[0] < right[0]
		}
		left, right = left[1:], right[1:]
	}
	return len(left) < len(right)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func writeAtomic(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".local-upload-*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
