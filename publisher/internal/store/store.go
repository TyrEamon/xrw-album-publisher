package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Stats struct {
	Pending           int
	Processing        int
	Incomplete        int
	Waiting           int
	Ready             int
	OK                int
	Failed            int
	Blocked           int
	Uploaded          int
	Images            int
	SourceImageBytes  int64
	TelegramFileBytes int64
}

type SnapshotCandidate struct {
	GalleryID int64
	ChannelID string
	UpdatedAt int64
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS galleries (
  source_gallery_id INTEGER PRIMARY KEY,
  album_id TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL,
  category TEXT NOT NULL DEFAULT '',
  image_count INTEGER NOT NULL,
  source_uploaded_images INTEGER NOT NULL DEFAULT 0,
  cover_image_id INTEGER NOT NULL,
  source_updated_at TEXT NOT NULL DEFAULT '',
  tags_json TEXT NOT NULL DEFAULT '[]',
  target_chat_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  retry_count INTEGER NOT NULL DEFAULT 0,
  next_retry_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS images (
  source_image_id INTEGER PRIMARY KEY,
  source_gallery_id INTEGER NOT NULL,
  sort_order INTEGER NOT NULL,
  width INTEGER NOT NULL DEFAULT 0,
  height INTEGER NOT NULL DEFAULT 0,
  tg_url TEXT NOT NULL DEFAULT '',
  tg_file_id TEXT NOT NULL DEFAULT '',
  tg_file_unique_id TEXT NOT NULL DEFAULT '',
  tg_message_id INTEGER NOT NULL DEFAULT 0,
  tg_public_key TEXT NOT NULL DEFAULT '',
  tg_mime_type TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  retry_count INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  UNIQUE(source_gallery_id, sort_order),
  FOREIGN KEY (source_gallery_id) REFERENCES galleries(source_gallery_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_galleries_queue
  ON galleries(status, next_retry_at, source_gallery_id DESC);
CREATE INDEX IF NOT EXISTS idx_images_queue
  ON images(source_gallery_id, status, sort_order);

CREATE TABLE IF NOT EXISTS traffic_counters (
  name TEXT PRIMARY KEY,
  bytes INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS snapshot_exports (
  source_gallery_id INTEGER PRIMARY KEY,
  gallery_updated_at INTEGER NOT NULL,
  exported_at INTEGER NOT NULL,
  FOREIGN KEY (source_gallery_id) REFERENCES galleries(source_gallery_id) ON DELETE CASCADE
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate publisher database: %w", err)
	}
	if err := ensureColumn(db, "galleries", "tags_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate publisher tags: %w", err)
	}
	if err := ensureColumn(db, "galleries", "source_uploaded_images", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate publisher uploaded image count: %w", err)
	}
	if err := ensureColumn(db, "galleries", "target_chat_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate publisher target chat: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) RecoverProcessing(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE galleries SET status = 'incomplete', updated_at = ? WHERE status = 'processing'`,
		time.Now().Unix(),
	)
	return err
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

func (s *Store) UpsertGallery(ctx context.Context, item model.GallerySummary) error {
	_, err := s.db.ExecContext(ctx, upsertGallerySQL, galleryUpsertArgs(item)...)
	return err
}

func (s *Store) UpsertGalleries(ctx context.Context, items []model.GallerySummary) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, upsertGallerySQL, galleryUpsertArgs(item)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const upsertGallerySQL = `
INSERT INTO galleries (
  source_gallery_id, album_id, title, category, image_count, source_uploaded_images, cover_image_id,
  source_updated_at, status, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_gallery_id) DO UPDATE SET
  album_id = excluded.album_id,
  title = excluded.title,
  category = excluded.category,
  image_count = excluded.image_count,
  source_uploaded_images = excluded.source_uploaded_images,
  cover_image_id = excluded.cover_image_id,
  status = CASE
    WHEN galleries.status = 'processing'
    THEN 'processing'
    WHEN excluded.source_uploaded_images < excluded.image_count
    THEN 'waiting'
    WHEN galleries.status = 'waiting'
    THEN 'pending'
    WHEN galleries.source_updated_at <> excluded.source_updated_at
      OR galleries.image_count <> excluded.image_count
      OR galleries.cover_image_id <> excluded.cover_image_id
    THEN 'pending'
    ELSE galleries.status
  END,
  source_updated_at = excluded.source_updated_at,
  updated_at = excluded.updated_at
`

func galleryUpsertArgs(item model.GallerySummary) []any {
	now := time.Now().Unix()
	queueStatus := "waiting"
	if item.ImageCount > 0 && item.UploadedImages >= item.ImageCount {
		queueStatus = "pending"
	}
	return []any{item.ID, item.AlbumID(), item.Title, item.Category, item.ImageCount,
		item.UploadedImages, item.CoverImageID, item.UpdatedAt, queueStatus, now, now}
}

func (s *Store) ClaimNext(ctx context.Context, includeReady bool) (model.Gallery, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Gallery{}, false, err
	}
	defer tx.Rollback()
	statuses := "'pending', 'incomplete'"
	if includeReady {
		statuses += ", 'ready'"
	}
	row := tx.QueryRowContext(ctx, `
SELECT source_gallery_id, album_id, title, category, image_count, cover_image_id,
       source_updated_at, tags_json, target_chat_id, status, last_error
FROM galleries
WHERE status IN (`+statuses+`)
  AND next_retry_at <= ?
ORDER BY CASE status WHEN 'ready' THEN 0 ELSE 1 END, source_gallery_id DESC
LIMIT 1
`, time.Now().Unix())
	var gallery model.Gallery
	var tagsJSON string
	if err := row.Scan(
		&gallery.SourceGalleryID, &gallery.AlbumID, &gallery.Title, &gallery.Category,
		&gallery.ImageCount, &gallery.CoverImageID, &gallery.SourceUpdatedAt, &tagsJSON, &gallery.TargetChatID,
		&gallery.Status, &gallery.LastError,
	); err != nil {
		if err == sql.ErrNoRows {
			return model.Gallery{}, false, nil
		}
		return model.Gallery{}, false, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &gallery.Tags)
	result, err := tx.ExecContext(ctx, `
UPDATE galleries SET status = 'processing', updated_at = ?
WHERE source_gallery_id = ? AND status = ?
`, time.Now().Unix(), gallery.SourceGalleryID, gallery.Status)
	if err != nil {
		return model.Gallery{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return model.Gallery{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Gallery{}, false, err
	}
	return gallery, true, nil
}

func (s *Store) BackfillUploadedChannels(ctx context.Context, legacyChatID string) error {
	if legacyChatID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE galleries SET target_chat_id = ?, updated_at = ?
WHERE target_chat_id = ''
  AND EXISTS (
    SELECT 1 FROM images
    WHERE images.source_gallery_id = galleries.source_gallery_id
      AND images.status = 'uploaded'
  )
`, legacyChatID, time.Now().Unix())
	return err
}

func (s *Store) AssignTargetChat(ctx context.Context, galleryID int64, chatID string) (string, error) {
	if chatID == "" {
		return "", fmt.Errorf("target Telegram chat is empty")
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE galleries SET target_chat_id = ?, updated_at = ?
WHERE source_gallery_id = ? AND target_chat_id = ''
`, chatID, time.Now().Unix(), galleryID); err != nil {
		return "", err
	}
	var assigned string
	if err := s.db.QueryRowContext(ctx, `
SELECT target_chat_id FROM galleries WHERE source_gallery_id = ?
`, galleryID).Scan(&assigned); err != nil {
		return "", err
	}
	return assigned, nil
}

func (s *Store) ApplyDetail(ctx context.Context, gallery model.Gallery, refs map[int]model.Image) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	tagsJSON, err := json.Marshal(gallery.Tags)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE galleries SET title = ?, category = ?, image_count = ?, cover_image_id = ?,
  source_updated_at = ?, tags_json = ?, last_error = '', updated_at = ?
WHERE source_gallery_id = ?
`, gallery.Title, gallery.Category, gallery.ImageCount, gallery.CoverImageID, gallery.SourceUpdatedAt, string(tagsJSON), now, gallery.SourceGalleryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
DELETE FROM images
WHERE source_gallery_id = ?
  AND (sort_order > ? OR source_image_id <> ? + sort_order - 1)
`, gallery.SourceGalleryID, gallery.ImageCount, gallery.CoverImageID); err != nil {
		return err
	}
	for order := 1; order <= gallery.ImageCount; order++ {
		image := refs[order]
		if image.SourceImageID == 0 {
			image.SourceImageID = gallery.CoverImageID + int64(order-1)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO images (source_image_id, source_gallery_id, sort_order, width, height, status, updated_at)
VALUES (?, ?, ?, ?, ?, 'pending', ?)
ON CONFLICT(source_image_id) DO UPDATE SET
  source_gallery_id = excluded.source_gallery_id,
  sort_order = excluded.sort_order,
  width = CASE WHEN excluded.width > 0 THEN excluded.width ELSE images.width END,
  height = CASE WHEN excluded.height > 0 THEN excluded.height ELSE images.height END,
  updated_at = excluded.updated_at
`, image.SourceImageID, gallery.SourceGalleryID, order, image.Width, image.Height, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) NextImage(ctx context.Context, galleryID int64) (model.Image, bool, error) {
	images, err := s.NextImages(ctx, galleryID, 1)
	if err != nil || len(images) == 0 {
		return model.Image{}, false, err
	}
	return images[0], true, nil
}

func (s *Store) NextImages(ctx context.Context, galleryID int64, limit int) ([]model.Image, error) {
	if limit < 1 {
		return nil, fmt.Errorf("image batch limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT source_image_id, source_gallery_id, sort_order, width, height, tg_url,
       tg_file_id, tg_file_unique_id, tg_message_id, tg_public_key, tg_mime_type, status, retry_count
FROM images
WHERE source_gallery_id = ? AND status <> 'uploaded'
ORDER BY sort_order
LIMIT ?
`, galleryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var images []model.Image
	for rows.Next() {
		var image model.Image
		if err := rows.Scan(
			&image.SourceImageID, &image.GalleryID, &image.SortOrder, &image.Width, &image.Height,
			&image.TGURL, &image.TGFileID, &image.TGFileUnique, &image.TGMessageID,
			&image.TGPublicKey, &image.TGMimeType, &image.Status, &image.RetryCount,
		); err != nil {
			return nil, err
		}
		images = append(images, image)
	}
	return images, rows.Err()
}

func (s *Store) MarkImageUploaded(ctx context.Context, imageID int64, tgURL, fileID, fileUniqueID, publicKey, mimeType string, messageID int64, width, height int) error {
	return s.MarkImagesUploaded(ctx, []model.Image{{
		SourceImageID: imageID, TGURL: tgURL, TGFileID: fileID, TGFileUnique: fileUniqueID,
		TGPublicKey: publicKey, TGMimeType: mimeType, TGMessageID: messageID,
		Width: width, Height: height,
	}})
}

func (s *Store) MarkImagesUploaded(ctx context.Context, images []model.Image) error {
	if len(images) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, image := range images {
		result, err := tx.ExecContext(ctx, `
UPDATE images SET tg_url = ?, tg_file_id = ?, tg_file_unique_id = ?, tg_message_id = ?,
  tg_public_key = ?, tg_mime_type = ?, width = ?, height = ?, status = 'uploaded',
  last_error = '', updated_at = ? WHERE source_image_id = ?
`, image.TGURL, image.TGFileID, image.TGFileUnique, image.TGMessageID,
			image.TGPublicKey, image.TGMimeType, image.Width, image.Height, now, image.SourceImageID)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return fmt.Errorf("image %d does not exist", image.SourceImageID)
		}
	}
	return tx.Commit()
}

func (s *Store) AddTraffic(ctx context.Context, name string, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO traffic_counters (name, bytes) VALUES (?, ?)
ON CONFLICT(name) DO UPDATE SET bytes = traffic_counters.bytes + excluded.bytes
`, name, bytes)
	return err
}

func (s *Store) MarkImageFailed(ctx context.Context, imageID int64, message string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE images SET status = 'failed', retry_count = retry_count + 1,
  last_error = ?, updated_at = ? WHERE source_image_id = ?
`, message, time.Now().Unix(), imageID)
	return err
}

func (s *Store) MarkGallery(ctx context.Context, galleryID int64, status, message string, retryAfter time.Duration) error {
	nextRetry := int64(0)
	if retryAfter > 0 {
		nextRetry = time.Now().Add(retryAfter).Unix()
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE galleries SET status = ?, last_error = ?,
  retry_count = CASE WHEN ? IN ('failed', 'incomplete') THEN retry_count + 1 ELSE retry_count END,
  next_retry_at = ?, updated_at = ? WHERE source_gallery_id = ?
`, status, message, status, nextRetry, time.Now().Unix(), galleryID)
	return err
}

func (s *Store) MarkGalleryFailure(ctx context.Context, galleryID int64, message string, retryAfter time.Duration, maxRetries int) (bool, error) {
	nextRetry := time.Now().Add(retryAfter).Unix()
	var status string
	err := s.db.QueryRowContext(ctx, `
UPDATE galleries SET
  status = CASE WHEN retry_count + 1 >= ? THEN 'failed' ELSE 'incomplete' END,
  retry_count = retry_count + 1,
  last_error = ?,
  next_retry_at = CASE WHEN retry_count + 1 >= ? THEN 0 ELSE ? END,
  updated_at = ?
WHERE source_gallery_id = ?
RETURNING status
`, maxRetries, message, maxRetries, nextRetry, time.Now().Unix(), galleryID).Scan(&status)
	if err != nil {
		return false, err
	}
	return status == "failed", nil
}

func (s *Store) RetryFailed(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE galleries SET status = 'pending', retry_count = 0, next_retry_at = 0,
  last_error = '', updated_at = ? WHERE status = 'failed'
`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) Payload(ctx context.Context, galleryID int64, channelID string) (model.PublishPayload, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT source_gallery_id, album_id, title, category, image_count, source_updated_at
     , tags_json FROM galleries WHERE source_gallery_id = ?
`, galleryID)
	var payload model.PublishPayload
	var tagsJSON string
	if err := row.Scan(&payload.SourceGalleryID, &payload.ID, &payload.Title, &payload.Category, &payload.Count, &payload.SourceUpdatedAt, &tagsJSON); err != nil {
		return model.PublishPayload{}, err
	}
	if err := json.Unmarshal([]byte(tagsJSON), &payload.Tags); err != nil {
		return model.PublishPayload{}, fmt.Errorf("decode gallery tags: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT source_image_id, source_gallery_id, sort_order, width, height, tg_url,
       tg_file_id, tg_file_unique_id, tg_message_id, tg_public_key, tg_mime_type, status, retry_count
FROM images WHERE source_gallery_id = ? ORDER BY sort_order
`, galleryID)
	if err != nil {
		return model.PublishPayload{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var image model.Image
		if err := rows.Scan(
			&image.SourceImageID, &image.GalleryID, &image.SortOrder, &image.Width, &image.Height,
			&image.TGURL, &image.TGFileID, &image.TGFileUnique, &image.TGMessageID,
			&image.TGPublicKey, &image.TGMimeType, &image.Status, &image.RetryCount,
		); err != nil {
			return model.PublishPayload{}, err
		}
		if image.Status != "uploaded" || image.TGURL == "" {
			return model.PublishPayload{}, fmt.Errorf("image %d is not uploaded", image.SourceImageID)
		}
		payload.Photos = append(payload.Photos, image)
		payload.TGFiles = append(payload.TGFiles, model.TGFile{
			PublicKey: image.TGPublicKey, URL: image.TGURL, FileID: image.TGFileID,
			FileUnique: image.TGFileUnique, MessageID: image.TGMessageID,
			ChannelID: channelID, ContentType: image.TGMimeType,
		})
	}
	if err := rows.Err(); err != nil {
		return model.PublishPayload{}, err
	}
	if len(payload.Photos) != payload.Count {
		return model.PublishPayload{}, fmt.Errorf("gallery expects %d images but has %d uploaded", payload.Count, len(payload.Photos))
	}
	payload.Source = "veil"
	payload.Status = "ok"
	payload.Href = "/album/" + payload.ID
	if len(payload.Photos) > 0 {
		payload.Cover = payload.Photos[0].TGURL
	}
	return payload, nil
}

func (s *Store) PendingSnapshots(ctx context.Context, limit int) ([]SnapshotCandidate, error) {
	if limit < 1 {
		return nil, fmt.Errorf("snapshot limit must be positive")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT g.source_gallery_id, g.target_chat_id, g.updated_at
FROM galleries g
LEFT JOIN snapshot_exports e ON e.source_gallery_id = g.source_gallery_id
WHERE g.status IN ('ready', 'ok')
  AND (e.source_gallery_id IS NULL OR e.gallery_updated_at <> g.updated_at)
ORDER BY g.updated_at, g.source_gallery_id
LIMIT ?
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates []SnapshotCandidate
	for rows.Next() {
		var candidate SnapshotCandidate
		if err := rows.Scan(&candidate.GalleryID, &candidate.ChannelID, &candidate.UpdatedAt); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) MarkSnapshotsExported(ctx context.Context, candidates []SnapshotCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, candidate := range candidates {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO snapshot_exports (source_gallery_id, gallery_updated_at, exported_at)
VALUES (?, ?, ?)
ON CONFLICT(source_gallery_id) DO UPDATE SET
  gallery_updated_at = excluded.gallery_updated_at,
  exported_at = excluded.exported_at
`, candidate.GalleryID, candidate.UpdatedAt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ResetSnapshotExports(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM snapshot_exports`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	stats := Stats{}
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM galleries GROUP BY status`)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return stats, err
		}
		switch status {
		case "pending":
			stats.Pending = count
		case "processing":
			stats.Processing = count
		case "incomplete":
			stats.Incomplete = count
		case "waiting":
			stats.Waiting = count
		case "ready":
			stats.Ready = count
		case "ok":
			stats.OK = count
		case "failed":
			stats.Failed = count
		case "blocked":
			stats.Blocked = count
		}
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(CASE WHEN status = 'uploaded' THEN 1 ELSE 0 END), 0) FROM images
`).Scan(&stats.Images, &stats.Uploaded); err != nil {
		return stats, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT name, bytes FROM traffic_counters`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var bytes int64
		if err := rows.Scan(&name, &bytes); err != nil {
			return stats, err
		}
		switch name {
		case "source_image_bytes":
			stats.SourceImageBytes = bytes
		case "telegram_file_bytes":
			stats.TelegramFileBytes = bytes
		}
	}
	return stats, rows.Err()
}
