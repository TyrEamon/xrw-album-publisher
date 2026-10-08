package localupload

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/TyrEamon/xrw-album/publisher/internal/sitealbum"
)

// A site whose index has never been built still has to be browsable: the page
// reads one listing page straight from the site instead of showing an empty grid
// with a "not built yet" note. This is what a freshly installed phone sees.
func TestSiteAlbumsFallBackToTheLiveListingWithoutAnIndex(t *testing.T) {
	site := alphaGallery(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := sitealbum.NewRegistry(t.TempDir(), []sitealbum.Site{
		{ID: siteTestID, Name: "Alpha", BaseURL: site.server.URL},
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	imports, err := NewTelegramImportStore(filepath.Join(t.TempDir(), "imports"), 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, Options{}, logger)
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: imports, Sites: registry,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	response := siteRequest(t, handler, http.MethodGet, "/api/state", "")
	var state struct {
		SiteAlbums []struct {
			Total   int   `json:"total"`
			BuiltAt int64 `json:"built_at"`
		} `json:"site_albums"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.SiteAlbums) != 1 || state.SiteAlbums[0].BuiltAt != 0 || state.SiteAlbums[0].Total != 0 {
		t.Fatalf("the fixture should start with an empty index: %+v", state.SiteAlbums)
	}

	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums", "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	var listing struct {
		Albums []siteAlbumView `json:"albums"`
		Total  int             `json:"total"`
		Live   bool            `json:"live"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if !listing.Live {
		t.Fatalf("an empty index should be served live: %s", response.Body.String())
	}
	if len(listing.Albums) != 1 || listing.Albums[0].ID != 100 || listing.Albums[0].CoverURL == "" {
		t.Fatalf("the live page is incomplete: %+v", listing.Albums)
	}
	if listing.Total < 1 {
		t.Fatalf("the live page should report what the site holds, got %d", listing.Total)
	}

	// A search belongs to the index, and the cache is honest about being empty
	// rather than showing galleries the query never matched.
	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums?q=alpha", "")
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Live || len(listing.Albums) != 0 {
		t.Fatalf("a filtered request belongs to the cache: %+v", listing)
	}
}
