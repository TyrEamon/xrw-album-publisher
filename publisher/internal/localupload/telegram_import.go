package localupload

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxTelegramDraftFiles = 1000
	// KindTelegram marks a draft assembled from Telegram Web.
	KindTelegram = "tg"
	// KindWordPress marks a draft pulled from a gallery site's REST API.
	KindWordPress = "wp"
)

type TelegramImportStore struct {
	root          string
	maxImageBytes int64
	mu            sync.Mutex
}

type TelegramImportDraft struct {
	ID             string               `json:"id"`
	Kind           string               `json:"kind,omitempty"`
	SourceURL      string               `json:"source_url"`
	Title          string               `json:"title"`
	Category       string               `json:"category,omitempty"`
	Tags           []string             `json:"tags"`
	Files          []TelegramImportFile `json:"files"`
	CommittedJobID string               `json:"committed_job_id,omitempty"`
	CreatedAt      int64                `json:"created_at"`
	UpdatedAt      int64                `json:"updated_at"`
}

type TelegramImportFile struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContentType   string `json:"content_type"`
	Size          int64  `json:"size"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	SourceMessage string `json:"source_message,omitempty"`
	SHA256        string `json:"sha256"`
	Position      int    `json:"position"`
}

type TelegramImportSummary struct {
	ID             string   `json:"id"`
	Kind           string   `json:"kind,omitempty"`
	SourceURL      string   `json:"source_url"`
	Title          string   `json:"title"`
	Category       string   `json:"category,omitempty"`
	Tags           []string `json:"tags"`
	FileCount      int      `json:"file_count"`
	TotalBytes     int64    `json:"total_bytes"`
	CommittedJobID string   `json:"committed_job_id,omitempty"`
	CreatedAt      int64    `json:"created_at"`
	UpdatedAt      int64    `json:"updated_at"`
}

type CreateTelegramImportRequest struct {
	Kind      string   `json:"kind"`
	SourceURL string   `json:"source_url"`
	Title     string   `json:"title"`
	Category  string   `json:"category"`
	Tags      []string `json:"tags"`
}

// draftKind normalizes the caller's kind onto a known prefix.
func draftKind(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), KindWordPress) {
		return KindWordPress
	}
	return KindTelegram
}

func NewTelegramImportStore(root string, maxImageBytes int64) (*TelegramImportStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("Telegram import directory is missing")
	}
	if maxImageBytes < 1 {
		return nil, errors.New("Telegram import image limit must be positive")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &TelegramImportStore{root: root, maxImageBytes: maxImageBytes}, nil
}

func LoadOrCreateImportToken(path string) (string, error) {
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) < 32 {
			return "", errors.New("saved Telegram import token is invalid")
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

func (s *TelegramImportStore) Create(input CreateTelegramImportRequest) (TelegramImportDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	random, err := randomHex(4)
	if err != nil {
		return TelegramImportDraft{}, err
	}
	now := time.Now().Unix()
	kind := draftKind(input.Kind)
	draft := TelegramImportDraft{
		ID:        kind + "-" + time.Now().UTC().Format("20060102-150405") + "-" + random,
		Kind:      kind,
		SourceURL: strings.TrimSpace(input.SourceURL),
		Title:     strings.TrimSpace(input.Title),
		Category:  strings.TrimSpace(input.Category),
		Tags:      normalizeTags(input.Tags),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if draft.Title == "" {
		draft.Title = "Telegram 图集"
	}
	if err := os.MkdirAll(s.filesDir(draft.ID), 0o755); err != nil {
		return TelegramImportDraft{}, err
	}
	if err := s.writeDraft(draft); err != nil {
		return TelegramImportDraft{}, err
	}
	return draft, nil
}

func (s *TelegramImportStore) List() ([]TelegramImportSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	result := make([]TelegramImportSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !safeImportComponent(entry.Name()) {
			continue
		}
		draft, err := s.readDraft(entry.Name())
		if err != nil {
			continue
		}
		var total int64
		for _, file := range draft.Files {
			total += file.Size
		}
		result = append(result, TelegramImportSummary{
			ID: draft.ID, Kind: draft.Kind, SourceURL: draft.SourceURL, Title: draft.Title,
			Category: draft.Category, Tags: draft.Tags,
			FileCount: len(draft.Files), TotalBytes: total, CommittedJobID: draft.CommittedJobID,
			CreatedAt: draft.CreatedAt, UpdatedAt: draft.UpdatedAt,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].CreatedAt > result[right].CreatedAt })
	return result, nil
}

func (s *TelegramImportStore) Get(id string) (TelegramImportDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readDraft(id)
}

func (s *TelegramImportStore) AddFile(id, name, contentType, sourceMessage string, source io.Reader) (TelegramImportFile, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, err := s.readDraft(id)
	if err != nil {
		return TelegramImportFile{}, false, err
	}
	if draft.CommittedJobID != "" {
		return TelegramImportFile{}, false, errors.New("Telegram draft has already been committed")
	}
	if len(draft.Files) >= maxTelegramDraftFiles {
		return TelegramImportFile{}, false, fmt.Errorf("Telegram draft exceeds %d images", maxTelegramDraftFiles)
	}
	extension, err := importImageExtension(name, contentType)
	if err != nil {
		return TelegramImportFile{}, false, err
	}
	temporary, err := os.CreateTemp(s.filesDir(id), ".upload-*")
	if err != nil {
		return TelegramImportFile{}, false, err
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(source, s.maxImageBytes+1))
	if err != nil {
		return TelegramImportFile{}, false, err
	}
	if err := temporary.Close(); err != nil {
		return TelegramImportFile{}, false, err
	}
	if written < 1 {
		return TelegramImportFile{}, false, errors.New("Telegram image is empty")
	}
	if written > s.maxImageBytes {
		return TelegramImportFile{}, false, fmt.Errorf("Telegram image exceeds %d MB", s.maxImageBytes/(1024*1024))
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	for _, existing := range draft.Files {
		if existing.SHA256 == digest {
			return existing, true, nil
		}
	}
	width, height, err := imageDimensions(temporaryPath)
	if err != nil {
		return TelegramImportFile{}, false, fmt.Errorf("read Telegram image dimensions: %w", err)
	}
	fileID, err := randomHex(8)
	if err != nil {
		return TelegramImportFile{}, false, err
	}
	fileID = "media-" + fileID
	target := filepath.Join(s.filesDir(id), fileID+extension)
	if err := os.Rename(temporaryPath, target); err != nil {
		return TelegramImportFile{}, false, err
	}
	keep = true
	value := strings.TrimSpace(contentType)
	if value == "" {
		value = mime.TypeByExtension(extension)
	}
	file := TelegramImportFile{
		ID: fileID, Name: filepath.Base(strings.TrimSpace(name)), ContentType: value,
		Size: written, Width: width, Height: height, SourceMessage: strings.TrimSpace(sourceMessage),
		SHA256: digest, Position: len(draft.Files) + 1,
	}
	if file.Name == "." || file.Name == "" {
		file.Name = fileID + extension
	}
	draft.Files = append(draft.Files, file)
	draft.UpdatedAt = time.Now().Unix()
	if err := s.writeDraft(draft); err != nil {
		return TelegramImportFile{}, false, err
	}
	return file, false, nil
}

func (s *TelegramImportStore) FilePath(draftID, fileID string) (string, TelegramImportFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, err := s.readDraft(draftID)
	if err != nil {
		return "", TelegramImportFile{}, err
	}
	for _, file := range draft.Files {
		if file.ID == fileID {
			matches, err := filepath.Glob(filepath.Join(s.filesDir(draftID), file.ID+".*"))
			if err != nil || len(matches) != 1 {
				return "", TelegramImportFile{}, errors.New("Telegram import image is missing")
			}
			return matches[0], file, nil
		}
	}
	return "", TelegramImportFile{}, os.ErrNotExist
}

func (s *TelegramImportStore) Prepare(id string, fileIDs []string) (string, TelegramImportDraft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, err := s.readDraft(id)
	if err != nil {
		return "", TelegramImportDraft{}, err
	}
	if draft.CommittedJobID != "" {
		return "", TelegramImportDraft{}, errors.New("Telegram draft has already been committed")
	}
	if len(fileIDs) == 0 {
		return "", TelegramImportDraft{}, errors.New("至少保留一张图片")
	}
	byID := make(map[string]TelegramImportFile, len(draft.Files))
	for _, file := range draft.Files {
		byID[file.ID] = file
	}
	seen := make(map[string]struct{}, len(fileIDs))
	selected := make([]TelegramImportFile, 0, len(fileIDs))
	for _, fileID := range fileIDs {
		if _, exists := seen[fileID]; exists {
			return "", TelegramImportDraft{}, errors.New("图片顺序里出现重复项目")
		}
		file, exists := byID[fileID]
		if !exists {
			return "", TelegramImportDraft{}, fmt.Errorf("图片不存在：%s", fileID)
		}
		seen[fileID] = struct{}{}
		selected = append(selected, file)
	}
	random, err := randomHex(4)
	if err != nil {
		return "", TelegramImportDraft{}, err
	}
	folder := filepath.Join(s.draftDir(id), "selected-"+random)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", TelegramImportDraft{}, err
	}
	for index, file := range selected {
		matches, err := filepath.Glob(filepath.Join(s.filesDir(id), file.ID+".*"))
		if err != nil || len(matches) != 1 {
			return "", TelegramImportDraft{}, fmt.Errorf("图片文件不存在：%s", file.Name)
		}
		extension := strings.ToLower(filepath.Ext(matches[0]))
		target := filepath.Join(folder, fmt.Sprintf("%06d%s", index+1, extension))
		if err := linkOrCopy(matches[0], target); err != nil {
			return "", TelegramImportDraft{}, err
		}
	}
	return folder, draft, nil
}

func (s *TelegramImportStore) MarkCommitted(id, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, err := s.readDraft(id)
	if err != nil {
		return err
	}
	draft.CommittedJobID = jobID
	draft.UpdatedAt = time.Now().Unix()
	return s.writeDraft(draft)
}

func (s *TelegramImportStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, err := s.readDraft(id)
	if err != nil {
		return err
	}
	if draft.CommittedJobID != "" {
		return errors.New("已经建立上传任务的草稿不能删除")
	}
	return s.removeDraftDir(id)
}

func (s *TelegramImportStore) RemoveCommitted(id, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	draft, err := s.readDraft(id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" || draft.CommittedJobID != jobID {
		return errors.New("Telegram draft does not belong to the completed job")
	}
	return s.removeDraftDir(id)
}

func (s *TelegramImportStore) RemoveByJob(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return errors.New("completed job id is missing")
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safeImportComponent(entry.Name()) {
			continue
		}
		draft, err := s.readDraft(entry.Name())
		if err != nil || draft.CommittedJobID != jobID {
			continue
		}
		return s.removeDraftDir(draft.ID)
	}
	return nil
}

func (s *TelegramImportStore) removeDraftDir(id string) error {
	if !safeImportComponent(id) {
		return errors.New("invalid Telegram draft id")
	}
	path := filepath.Clean(s.draftDir(id))
	if filepath.Dir(path) != filepath.Clean(s.root) {
		return errors.New("invalid Telegram draft path")
	}
	return os.RemoveAll(path)
}

func (s *TelegramImportStore) draftDir(id string) string {
	return filepath.Join(s.root, id)
}

func (s *TelegramImportStore) filesDir(id string) string {
	return filepath.Join(s.draftDir(id), "files")
}

func (s *TelegramImportStore) manifestPath(id string) string {
	return filepath.Join(s.draftDir(id), "draft.json")
}

func (s *TelegramImportStore) readDraft(id string) (TelegramImportDraft, error) {
	if !safeImportComponent(id) {
		return TelegramImportDraft{}, errors.New("invalid Telegram draft id")
	}
	data, err := os.ReadFile(s.manifestPath(id))
	if err != nil {
		return TelegramImportDraft{}, err
	}
	var draft TelegramImportDraft
	if err := json.Unmarshal(data, &draft); err != nil {
		return TelegramImportDraft{}, err
	}
	return draft, nil
}

func (s *TelegramImportStore) writeDraft(draft TelegramImportDraft) error {
	data, err := json.MarshalIndent(draft, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.manifestPath(draft.ID), append(data, '\n'))
}

func safeImportComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value && !strings.ContainsAny(value, `/\\`)
}

func importImageExtension(name, contentType string) (string, error) {
	extension := strings.ToLower(filepath.Ext(strings.TrimSpace(name)))
	allowed := map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	if allowed[extension] {
		return extension, nil
	}
	mediaType, _, _ := mime.ParseMediaType(strings.TrimSpace(contentType))
	switch strings.ToLower(mediaType) {
	case "image/jpeg":
		return ".jpg", nil
	case "image/png":
		return ".png", nil
	case "image/gif":
		return ".gif", nil
	case "image/webp":
		return ".webp", nil
	default:
		return "", errors.New("只支持 JPG、PNG、GIF 或 WebP 图片")
	}
}

func linkOrCopy(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
