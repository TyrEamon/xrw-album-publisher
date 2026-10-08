package localupload

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactStoredErrorsPreservesProgress(t *testing.T) {
	db, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	job := Job{ID: "manual-test", NumericID: 1, Title: "test", TotalFiles: 2}
	if err := db.Create(ctx, job, []File{{Position: 1}, {Position: 2}}); err != nil {
		t.Fatal(err)
	}
	const message = "https://api.telegram.org/bot123456789:old_token_ABCDEFGHIJKLMNOPQRSTUVWXYZ/sendDocument: timeout"
	if _, err := db.db.Exec("UPDATE local_jobs SET status='failed', uploaded_files=1, last_error=?", message); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE local_files SET status='uploaded', file_id='saved-file', message_id=41, last_error=? WHERE position=1", message); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.RedactErrors(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := db.Get(ctx, job.ID)
	if err != nil || stored.Status != "failed" || stored.UploadedFiles != 1 || stored.TotalFiles != 2 {
		t.Fatal("redaction changed task progress")
	}
	files, err := db.Files(ctx, job.ID)
	if err != nil || files[0].Status != "uploaded" || files[0].FileID != "saved-file" || files[0].MessageID != 41 || files[1].Status != "pending" {
		t.Fatal("redaction changed upload checkpoints")
	}
	for _, table := range []string{"local_jobs", "local_files"} {
		var count int
		if err := db.db.QueryRow("SELECT COUNT(*) FROM " + table + " WHERE last_error LIKE '%old_token%'").Scan(&count); err != nil || count != 0 {
			t.Fatal("secret remains in persistent diagnostics")
		}
	}
	if err := db.MarkFailed(ctx, job.ID, errors.New(message)); err != nil {
		t.Fatal(err)
	}
	var diagnostic string
	if err := db.db.QueryRow("SELECT last_error FROM local_jobs WHERE id=?", job.ID).Scan(&diagnostic); err != nil || strings.Contains(diagnostic, "old_token") {
		t.Fatal("new failure stored an unredacted token")
	}
}
