package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	"github.com/TyrEamon/xrw-album/publisher/internal/store"
)

func TestExportWritesSanitizedIncrementalBatch(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	summary := model.GallerySummary{ID: 40, Title: "snapshot", ImageCount: 1, UploadedImages: 1, CoverImageID: 400}
	if err := database.UpsertGallery(ctx, summary); err != nil {
		t.Fatal(err)
	}
	gallery, found, err := database.ClaimNext(ctx, false)
	if err != nil || !found {
		t.Fatalf("claim failed: found=%v err=%v", found, err)
	}
	if err := database.ApplyDetail(ctx, gallery, map[int]model.Image{1: {SourceImageID: 400, Width: 1200, Height: 1800}}); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkImageUploaded(ctx, 400, "https://album.example/file/veil-400", "secret-file-id", "unique", "veil-400", "image/jpeg", 1, 1200, 1800); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGallery(ctx, summary.ID, "ready", "", 0); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(t.TempDir(), "batches")
	secret := strings.Repeat("s", 32)
	file, count, err := Export(ctx, database, outDir, 100, Options{
		ImageBase: "https://gimg.example.com", SigningSecret: secret,
	})
	if err != nil || count != 1 {
		t.Fatalf("export count=%d file=%q err=%v", count, file, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "secret-file-id") || strings.Contains(text, "tg_files") {
		t.Fatalf("snapshot leaked Telegram private mapping: %s", text)
	}
	if !strings.Contains(text, "veil-40") || !strings.Contains(text, "https://gimg.example.com/tg/") {
		t.Fatalf("snapshot is missing public gallery data: %s", text)
	}
	if strings.Contains(text, "https://album.example/file/veil-400") {
		t.Fatalf("snapshot retained the D1-backed image URL: %s", text)
	}

	file, count, err = Export(ctx, database, outDir, 100, Options{
		ImageBase: "https://gimg.example.com", SigningSecret: secret,
	})
	if err != nil || count != 0 || file != "" {
		t.Fatalf("second export was not empty: count=%d file=%q err=%v", count, file, err)
	}
}

func TestExportRejectsIncompleteImageWorkerConfiguration(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, _, err = Export(context.Background(), database, t.TempDir(), 1, Options{ImageBase: "https://gimg.example.com"})
	if err == nil {
		t.Fatal("expected incomplete gimg configuration to fail")
	}
}
