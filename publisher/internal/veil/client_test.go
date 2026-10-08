package veil

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestListGalleryAndDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/galleries":
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprint(response, `{"items":[{"id":21674,"title":"album","category":"Cosplay","image_count":2,"uploaded_images":2,"status":"done","updated_at":"2026-01-01","cover":{"image_id":1191867}}],"total":1,"has_next":false}`)
		case "/v1/gallery/21674":
			response.Header().Set("Content-Type", "application/json")
			fmt.Fprint(response, `{"id":21674,"title":"album","category":"Cosplay","image_count":2,"cover_image_id":1191867,"updated_at":"2026-01-01","images":[{"id":1191867,"sort_order":1,"width":1,"height":1},{"id":1191868,"sort_order":2,"width":1,"height":1}]}`)
		case "/v1/image/1191867":
			response.Header().Set("Content-Type", "image/jpeg")
			_, _ = response.Write([]byte("jpeg"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, nil, 100, time.Second, time.Minute, time.Second, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	items, total, hasNext, err := client.ListGalleries(context.Background(), 100, 0)
	if err != nil || total != 1 || hasNext || len(items) != 1 || items[0].AlbumID() != "veil-21674" {
		t.Fatalf("unexpected list result: items=%+v total=%d next=%v err=%v", items, total, hasNext, err)
	}
	detail, err := client.Gallery(context.Background(), 21674)
	if err != nil || detail.CoverImageID != 1191867 {
		t.Fatalf("unexpected detail: %+v err=%v", detail, err)
	}
	base := filepath.Join(t.TempDir(), "1191867")
	download, err := client.DownloadImage(context.Background(), 1191867, 21674, base)
	if err != nil {
		t.Fatal(err)
	}
	if download.Path != base+".jpg" {
		t.Fatalf("unexpected download path %q", download.Path)
	}
	data, err := os.ReadFile(download.Path)
	if err != nil || string(data) != "jpeg" {
		t.Fatalf("unexpected downloaded file %q err=%v", data, err)
	}
}
