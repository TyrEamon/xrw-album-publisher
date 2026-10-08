package legacy

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

type fakeUploader struct {
	calls int
}

func (f *fakeUploader) UploadGroup(_ context.Context, _ string, items []telegram.UploadItem, _ string) ([]telegram.Result, error) {
	f.calls++
	results := make([]telegram.Result, len(items))
	for index, item := range items {
		results[index] = telegram.Result{
			URL: "https://img.example/file/" + item.PublicKey, FileID: "file-id",
			FileUniqueID: "unique-id", MessageID: int64(index + 1),
			PublicKey: item.PublicKey, ContentType: item.ContentType,
		}
	}
	return results, nil
}

func TestRunnerPublishesValidatedAlbumWithDimensions(t *testing.T) {
	imageBytes := testPNG(t, 7, 11)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "image/png")
		_, _ = response.Write(imageBytes)
	}))
	defer server.Close()

	store, source, work := testStoreAndSource(t, "album\n"+server.URL+"/image.png\n")
	uploader := &fakeUploader{}
	runner := NewRunner(store, uploader, work, 2<<20, 1, 1, 5*time.Second, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := runner.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Ready != 1 || stats.Uploaded != 1 || uploader.calls != 1 {
		t.Fatalf("stats=%+v calls=%d source=%s", stats, uploader.calls, source)
	}
	albums := []SourceAlbum{}
	if err := ParseSource(source, func(album SourceAlbum) error { albums = append(albums, album); return nil }); err != nil {
		t.Fatal(err)
	}
	items, err := store.Images(context.Background(), albums[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Width != 7 || items[0].Height != 11 {
		t.Fatalf("dimensions = %dx%d", items[0].Width, items[0].Height)
	}
	if _, err := os.Stat(filepath.Join(work, "outbox", albums[0].ID+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerDoesNotPublishPartiallyBrokenAlbum(t *testing.T) {
	imageBytes := testPNG(t, 5, 9)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/missing" {
			http.Error(response, "gone", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "image/png")
		_, _ = response.Write(imageBytes)
	}))
	defer server.Close()

	store, _, work := testStoreAndSource(t, "broken\n"+server.URL+"/ok\n"+server.URL+"/missing\n")
	uploader := &fakeUploader{}
	runner := NewRunner(store, uploader, work, 2<<20, 1, 1, 5*time.Second, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := runner.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Invalid != 1 || stats.Dead != 1 || uploader.calls != 0 {
		t.Fatalf("stats=%+v calls=%d", stats, uploader.calls)
	}
}

func testStoreAndSource(t *testing.T, contents string) (*Store, string, string) {
	t.Helper()
	directory := t.TempDir()
	source := filepath.Join(directory, "source.txt")
	if err := os.WriteFile(source, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(directory, "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, _, err := store.SyncSource(context.Background(), source, []string{"-1001"}); err != nil {
		t.Fatal(err)
	}
	return store, source, filepath.Join(directory, "work")
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	canvas.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
