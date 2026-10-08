package localupload

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS local_jobs (
  id TEXT PRIMARY KEY,
  numeric_id INTEGER NOT NULL UNIQUE,
  title TEXT NOT NULL,
  category TEXT NOT NULL DEFAULT '',
  tags_json TEXT NOT NULL DEFAULT '[]',
  folder TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  total_files INTEGER NOT NULL,
  uploaded_files INTEGER NOT NULL DEFAULT 0,
  total_bytes INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  snapshot_path TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS local_files (
  job_id TEXT NOT NULL,
  position INTEGER NOT NULL,
  path TEXT NOT NULL,
  name TEXT NOT NULL,
  content_type TEXT NOT NULL,
  size INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  public_key TEXT NOT NULL,
  file_id TEXT NOT NULL DEFAULT '',
  file_unique_id TEXT NOT NULL DEFAULT '',
  message_id INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (job_id, position),
  FOREIGN KEY (job_id) REFERENCES local_jobs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_local_jobs_queue
  ON local_jobs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_local_files_queue
  ON local_files(job_id, status, position);
`

type Store struct {
	db *sql.DB
}

type Job struct {
	ID            string   `json:"id"`
	NumericID     int64    `json:"-"`
	Title         string   `json:"title"`
	Category      string   `json:"category"`
	Tags          []string `json:"tags"`
	Folder        string   `json:"folder"`
	ChannelID     string   `json:"channel_id"`
	Status        string   `json:"status"`
	TotalFiles    int      `json:"total_files"`
	UploadedFiles int      `json:"uploaded_files"`
	TotalBytes    int64    `json:"total_bytes"`
	LastError     string   `json:"last_error,omitempty"`
	SnapshotPath  string   `json:"snapshot_path,omitempty"`
	CreatedAt     int64    `json:"created_at"`
	UpdatedAt     int64    `json:"updated_at"`
}

type File struct {
	JobID        string `json:"-"`
	Position     int    `json:"position"`
	Path         string `json:"-"`
	Name         string `json:"name"`
	ContentType  string `json:"content_type"`
	Size         int64  `json:"size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	PublicKey    string `json:"-"`
	FileID       string `json:"-"`
	FileUniqueID string `json:"-"`
	MessageID    int64  `json:"-"`
	Status       string `json:"status"`
	LastError    string `json:"last_error,omitempty"`
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate local uploader database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Recover(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE local_jobs SET status = 'pending', last_error = '程序上次运行时中断，已恢复任务', updated_at = ?
WHERE status = 'uploading'
`, time.Now().Unix())
	return err
}

// RedactErrors removes credentials from old diagnostics without changing jobs,
// upload checkpoints, or the retry queue.
func (s *Store) RedactErrors(ctx context.Context, secrets ...string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"local_jobs", "local_files"} {
		rows, err := tx.QueryContext(ctx, "SELECT rowid, last_error FROM "+table+" WHERE last_error <> ''")
		if err != nil {
			return err
		}
		updates := make(map[int64]string)
		for rows.Next() {
			var id int64
			var message string
			if err := rows.Scan(&id, &message); err != nil {
				rows.Close()
				return err
			}
			if redacted := telegram.RedactSecrets(message, secrets...); redacted != message {
				updates[id] = redacted
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for id, message := range updates {
			if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET last_error = ? WHERE rowid = ?", message, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) Create(ctx context.Context, job Job, files []File) error {
	if len(files) == 0 {
		return errors.New("photo folder contains no supported images")
	}
	tags, err := json.Marshal(job.Tags)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO local_jobs (
  id, numeric_id, title, category, tags_json, folder, channel_id, status,
  total_files, uploaded_files, total_bytes, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, 0, ?, ?, ?)
`, job.ID, job.NumericID, job.Title, job.Category, string(tags), job.Folder, job.ChannelID,
		len(files), job.TotalBytes, job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return err
	}
	for _, file := range files {
		_, err = tx.ExecContext(ctx, `
INSERT INTO local_files (
  job_id, position, path, name, content_type, size, width, height, public_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
`, job.ID, file.Position, file.Path, file.Name, file.ContentType, file.Size,
			file.Width, file.Height, file.PublicKey)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) List(ctx context.Context, limit int) ([]Job, error) {
	if limit < 1 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, numeric_id, title, category, tags_json, folder, channel_id, status,
       total_files, uploaded_files, total_bytes, last_error, snapshot_path, created_at, updated_at
FROM local_jobs ORDER BY created_at DESC LIMIT ?
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, numeric_id, title, category, tags_json, folder, channel_id, status,
       total_files, uploaded_files, total_bytes, last_error, snapshot_path, created_at, updated_at
FROM local_jobs WHERE id = ?
`, id)
	return scanJob(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var tags string
	err := row.Scan(&job.ID, &job.NumericID, &job.Title, &job.Category, &tags, &job.Folder,
		&job.ChannelID, &job.Status, &job.TotalFiles, &job.UploadedFiles, &job.TotalBytes,
		&job.LastError, &job.SnapshotPath, &job.CreatedAt, &job.UpdatedAt)
	if err != nil {
		return Job{}, err
	}
	_ = json.Unmarshal([]byte(tags), &job.Tags)
	job.LastError = telegram.RedactSecrets(job.LastError)
	return job, nil
}

func (s *Store) ClaimNext(ctx context.Context) (Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, err
	}
	defer tx.Rollback()
	job, err := scanJob(tx.QueryRowContext(ctx, `
SELECT id, numeric_id, title, category, tags_json, folder, channel_id, status,
       total_files, uploaded_files, total_bytes, last_error, snapshot_path, created_at, updated_at
FROM local_jobs WHERE status = 'pending' ORDER BY created_at LIMIT 1
`))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, nil
		}
		return Job{}, false, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE local_jobs SET status = 'uploading', last_error = '', updated_at = ?
WHERE id = ? AND status = 'pending'
`, time.Now().Unix(), job.ID)
	if err != nil {
		return Job{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, err
	}
	job.Status = "uploading"
	job.LastError = ""
	return job, true, nil
}

func (s *Store) PendingFiles(ctx context.Context, jobID string, limit int) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, position, path, name, content_type, size, width, height, public_key,
       file_id, file_unique_id, message_id, status, last_error
FROM local_files
WHERE job_id = ? AND status <> 'uploaded'
ORDER BY position LIMIT ?
`, jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		var file File
		if err := rows.Scan(&file.JobID, &file.Position, &file.Path, &file.Name, &file.ContentType,
			&file.Size, &file.Width, &file.Height, &file.PublicKey, &file.FileID,
			&file.FileUniqueID, &file.MessageID, &file.Status, &file.LastError); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Store) Files(ctx context.Context, jobID string) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT job_id, position, path, name, content_type, size, width, height, public_key,
       file_id, file_unique_id, message_id, status, last_error
FROM local_files WHERE job_id = ? ORDER BY position
`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []File
	for rows.Next() {
		var file File
		if err := rows.Scan(&file.JobID, &file.Position, &file.Path, &file.Name, &file.ContentType,
			&file.Size, &file.Width, &file.Height, &file.PublicKey, &file.FileID,
			&file.FileUniqueID, &file.MessageID, &file.Status, &file.LastError); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

func (s *Store) MarkUploaded(ctx context.Context, jobID string, files []File, results []telegram.Result) error {
	if len(files) != len(results) {
		return errors.New("Telegram result count does not match file count")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for index, file := range files {
		result := results[index]
		_, err := tx.ExecContext(ctx, `
UPDATE local_files SET file_id = ?, file_unique_id = ?, message_id = ?, content_type = ?,
  status = 'uploaded', last_error = ''
WHERE job_id = ? AND position = ?
`, result.FileID, result.FileUniqueID, result.MessageID, result.ContentType, jobID, file.Position)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
UPDATE local_jobs SET
  uploaded_files = (SELECT COUNT(*) FROM local_files WHERE job_id = ? AND status = 'uploaded'),
  updated_at = ?
WHERE id = ?
`, jobID, now, jobID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkFailed(ctx context.Context, id string, err error) error {
	message := "unknown error"
	if err != nil {
		message = telegram.RedactSecrets(err.Error())
	}
	_, updateErr := s.db.ExecContext(ctx, `
UPDATE local_jobs SET status = 'failed', last_error = ?, updated_at = ? WHERE id = ?
`, message, time.Now().Unix(), id)
	return updateErr
}

func (s *Store) MarkReady(ctx context.Context, id, snapshotPath string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE local_jobs SET status = 'ready', uploaded_files = total_files, snapshot_path = ?,
  last_error = '', updated_at = ? WHERE id = ?
`, snapshotPath, time.Now().Unix(), id)
	return err
}

func (s *Store) Retry(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE local_jobs SET status = 'pending', last_error = '', updated_at = ?
WHERE id = ? AND status = 'failed'
`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("only failed jobs can be retried")
	}
	return nil
}

func (s *Store) File(ctx context.Context, jobID string, position int) (File, error) {
	var file File
	err := s.db.QueryRowContext(ctx, `
SELECT job_id, position, path, name, content_type, size, width, height, public_key,
       file_id, file_unique_id, message_id, status, last_error
FROM local_files WHERE job_id = ? AND position = ?
`, jobID, position).Scan(&file.JobID, &file.Position, &file.Path, &file.Name, &file.ContentType,
		&file.Size, &file.Width, &file.Height, &file.PublicKey, &file.FileID,
		&file.FileUniqueID, &file.MessageID, &file.Status, &file.LastError)
	return file, err
}
