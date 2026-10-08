package localupload

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestTelegramImportDraftDeduplicatesAndPreparesSelection(t *testing.T) {
	store, err := NewTelegramImportStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.Create(CreateTelegramImportRequest{
		SourceURL: "https://web.telegram.org/k/#test", Title: "Album",
		Tags: []string{"tag", "#tag"},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstData := telegramTestPNG(t, 12, 8)
	first, duplicate, err := store.AddFile(draft.ID, "first.png", "image/png", "message-1", bytes.NewReader(firstData))
	if err != nil || duplicate {
		t.Fatalf("first file duplicate=%v err=%v", duplicate, err)
	}
	duplicateFile, duplicate, err := store.AddFile(draft.ID, "copy.png", "image/png", "message-2", bytes.NewReader(firstData))
	if err != nil || !duplicate || duplicateFile.ID != first.ID {
		t.Fatalf("exact duplicate was not reused: file=%+v duplicate=%v err=%v", duplicateFile, duplicate, err)
	}
	second, duplicate, err := store.AddFile(draft.ID, "second.png", "image/png", "message-3", bytes.NewReader(telegramTestPNG(t, 9, 15)))
	if err != nil || duplicate {
		t.Fatalf("second file duplicate=%v err=%v", duplicate, err)
	}
	loaded, err := store.Get(draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Files) != 2 || len(loaded.Tags) != 1 || loaded.Files[0].Width != 12 {
		t.Fatalf("unexpected draft: %+v", loaded)
	}
	folder, _, err := store.Prepare(draft.ID, []string{second.ID, first.ID})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != "000001.png" || entries[1].Name() != "000002.png" {
		t.Fatalf("unexpected prepared files in %s: %+v", filepath.Base(folder), entries)
	}
}

func TestSafeImportComponentRejectsTraversal(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../draft", `..\\draft`, "draft/file"} {
		if safeImportComponent(value) {
			t.Fatalf("unsafe component accepted: %q", value)
		}
	}
	if !safeImportComponent("tg-20260918-abcd") {
		t.Fatal("valid import id was rejected")
	}
}

func TestTelegramImportDraftDeletionAndCompletedCleanup(t *testing.T) {
	store, err := NewTelegramImportStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.Create(CreateTelegramImportRequest{Title: "Discard me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(draft.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted draft still exists: %v", err)
	}

	committed, err := store.Create(CreateTelegramImportRequest{Title: "Committed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCommitted(committed.ID, "job-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(committed.ID); err == nil {
		t.Fatal("committed draft was deleted as an ordinary draft")
	}
	if err := store.RemoveCommitted(committed.ID, "wrong-job"); err == nil {
		t.Fatal("completed cleanup accepted the wrong job id")
	}
	if err := store.RemoveCommitted(committed.ID, "job-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(committed.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed draft still exists: %v", err)
	}
	hooked, err := store.Create(CreateTelegramImportRequest{Title: "Ready hook"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCommitted(hooked.ID, "job-2"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveByJob("job-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(hooked.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ready hook draft still exists: %v", err)
	}
}

func telegramTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	picture.Set(0, 0, color.RGBA{R: uint8(width), G: uint8(height), A: 255})
	var data bytes.Buffer
	if err := png.Encode(&data, picture); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
