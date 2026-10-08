package legacy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	pubsnapshot "github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
)

func TestExportSnapshotPublishesReadyAlbumAndInvalidTombstone(t *testing.T) {
	imageBytes := testPNG(t, 13, 17)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/missing" {
			http.Error(response, "gone", http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "image/png")
		_, _ = response.Write(imageBytes)
	}))
	defer server.Close()

	store, source, work := testStoreAndSource(t, "ready\n"+server.URL+"/ok\ninvalid\n"+server.URL+"/missing\n")
	uploader := &fakeUploader{}
	runner := NewRunner(store, uploader, work, 2<<20, 1, 1, 5*time.Second, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := runner.Run(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	var sourceAlbums []SourceAlbum
	if err := ParseSource(source, func(album SourceAlbum) error {
		sourceAlbums = append(sourceAlbums, album)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	file, galleries, removed, err := ExportSnapshot(context.Background(), store, t.TempDir(), 10, pubsnapshot.Options{
		ImageBase: "https://gimg.example.com", SigningSecret: strings.Repeat("s", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if galleries != 1 || removed != 1 {
		t.Fatalf("galleries=%d removed=%d", galleries, removed)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var batch pubsnapshot.Batch
	if err := json.Unmarshal(data, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Galleries) != 1 || batch.Galleries[0].ID != sourceAlbums[0].ID {
		t.Fatalf("unexpected galleries: %+v", batch.Galleries)
	}
	photo := batch.Galleries[0].Photos[0]
	if photo.Width != 13 || photo.Height != 17 || !strings.HasPrefix(photo.TGURL, "https://gimg.example.com/tg/") {
		t.Fatalf("unexpected photo: %+v", photo)
	}
	if len(batch.RemovedIDs) != 1 || batch.RemovedIDs[0] != sourceAlbums[1].ID {
		t.Fatalf("unexpected removals: %+v", batch.RemovedIDs)
	}

	file, galleries, removed, err = ExportSnapshot(context.Background(), store, t.TempDir(), 10, pubsnapshot.Options{
		ImageBase: "https://gimg.example.com", SigningSecret: strings.Repeat("s", 32),
	})
	if err != nil || file != "" || galleries != 0 || removed != 0 {
		t.Fatalf("second export file=%q galleries=%d removed=%d err=%v", file, galleries, removed, err)
	}
}
