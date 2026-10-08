package localupload

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

func TestLocalUploadCreatesResumableSignedSnapshot(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	photos := filepath.Join(directory, "photos")
	if err := os.MkdirAll(photos, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(photos, "10.png"), 10, 20)
	writeTestPNG(t, filepath.Join(photos, "2.png"), 20, 10)

	telegramServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("chat_id") != "-100123" {
			t.Fatalf("unexpected Telegram channel %q", request.FormValue("chat_id"))
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"ok":true,"result":[{"message_id":1,"document":{"file_id":"file-2","file_unique_id":"unique-2","mime_type":"image/png"}},{"message_id":2,"document":{"file_id":"file-10","file_unique_id":"unique-10","mime_type":"image/png"}}]}`)
	}))
	defer telegramServer.Close()

	database, err := OpenStore(filepath.Join(directory, "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	secret := strings.Repeat("s", 32)
	uploader := telegram.New(telegramServer.URL, "secret", "https://gimg.example", 0, 0, 1, time.Second)
	service := NewService(database, uploader, Options{
		SnapshotDir: filepath.Join(directory, "batches"), ImageBase: "https://gimg.example",
		SigningSecret: secret, MaxImageBytes: 20 << 20, ChatIDs: []string{"-100123"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var publishedSnapshot string
	service.SetSnapshotPublisher(func(_ context.Context, path string) error {
		publishedSnapshot = path
		return nil
	})

	created, err := service.Create(ctx, CreateRequest{
		Folder: photos, Title: "Manual Album", Category: "Cosplay",
		Tags: []string{"tag one", "tag one"}, ChannelID: "-100123",
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := database.Files(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "2.png" || files[0].Width != 20 || files[0].Height != 10 {
		t.Fatalf("unexpected natural order or dimensions: %+v", files)
	}

	claimed, found, err := database.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := service.process(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	completed, err := database.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" || completed.UploadedFiles != 2 {
		t.Fatalf("unexpected completed job: %+v", completed)
	}
	if publishedSnapshot != completed.SnapshotPath {
		t.Fatalf("snapshot was not published before ready: published=%q ready=%q", publishedSnapshot, completed.SnapshotPath)
	}
	data, err := os.ReadFile(completed.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "file-2") || strings.Contains(string(data), "file-10") {
		t.Fatalf("public snapshot leaked Telegram file ids: %s", data)
	}
	var batch snapshot.Batch
	if err := json.Unmarshal(data, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Galleries) != 1 || batch.Galleries[0].ID != created.ID ||
		len(batch.Galleries[0].Photos) != 2 || !strings.HasPrefix(batch.Galleries[0].Photos[0].TGURL, "https://gimg.example/tg/") {
		t.Fatalf("unexpected snapshot: %+v", batch)
	}
}

func TestOptionsRequireSignedImageWorker(t *testing.T) {
	err := (Options{SnapshotDir: t.TempDir(), MaxImageBytes: 1, ChatIDs: []string{"-100"}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "GIMG_PUBLIC_BASE") {
		t.Fatalf("expected gimg configuration error, got %v", err)
	}
}

func writeTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	picture.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(file, picture); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
