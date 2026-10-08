package localupload

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/sitealbum"
	"github.com/TyrEamon/xrw-album/publisher/internal/snapshot"
	"github.com/TyrEamon/xrw-album/publisher/internal/telegram"
)

func TestRootServesEmbeddedUploaderWithoutRedirect(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	imports, err := NewTelegramImportStore(filepath.Join(t.TempDir(), "imports"), 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: imports, ImportToken: "test-import-token",
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "" {
		t.Fatalf("root unexpectedly redirected to %q", location)
	}
	if !strings.Contains(response.Body.String(), "发布一套写真") {
		t.Fatalf("root did not serve the embedded uploader")
	}
}

func TestTelegramImportRequiresPairingToken(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	imports, err := NewTelegramImportStore(filepath.Join(t.TempDir(), "imports"), 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: imports, ImportToken: "pairing-token",
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/api/telegram-imports",
		strings.NewReader(`{"title":"Imported album"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://web.telegram.org")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected missing token to be rejected, got %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/api/telegram-imports",
		strings.NewReader(`{"title":"Imported album"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://web.telegram.org")
	request.Header.Set("X-XRW-Import-Token", "pairing-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("expected paired import to be accepted, got %d: %s", response.Code, response.Body.String())
	}
	var draft TelegramImportDraft
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil || draft.Title != "Imported album" {
		t.Fatalf("unexpected draft response: %+v err=%v", draft, err)
	}
}

func TestTelegramDraftCanBeDeletedFromLocalUI(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	imports, err := NewTelegramImportStore(filepath.Join(t.TempDir(), "imports"), 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := imports.Create(CreateTelegramImportRequest{Title: "Temporary"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: imports, ImportToken: "pairing-token",
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodDelete,
		"http://127.0.0.1:8765/api/telegram-imports/"+draft.ID, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected draft deletion to succeed, got %d: %s", response.Code, response.Body.String())
	}
	remaining, err := imports.List()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("draft still listed after deletion: %+v err=%v", remaining, err)
	}
}

func TestSiteAlbumEndpointsBrowseAndQueueGalleries(t *testing.T) {
	fixture := newSiteFixture(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: fixture.drafts, ImportToken: "pairing-token",
		Sites: fixture.registry, SiteImport: fixture.importer,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	response := siteRequest(t, handler, http.MethodGet, "/api/site-albums", "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	var listing struct {
		Albums     []siteAlbumView  `json:"albums"`
		Total      int              `json:"total"`
		Categories []sitealbum.Term `json:"categories"`
		Statuses   []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Total    int    `json:"total"`
			BaseURL  string `json:"base_url"`
			Building bool   `json:"building"`
		} `json:"status"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Total != 1 || len(listing.Albums) != 1 {
		t.Fatalf("expected the cached gallery, got %+v", listing)
	}
	if listing.Albums[0].ID != 100 || listing.Albums[0].CoverURL == "" {
		t.Fatalf("gallery view is incomplete: %+v", listing.Albums[0])
	}
	if listing.Albums[0].DraftID != "" || listing.Albums[0].Importing {
		t.Fatalf("a fresh gallery should not look imported: %+v", listing.Albums[0])
	}
	if len(listing.Categories) != 1 || listing.Categories[0].Name != "Cosplay" {
		t.Fatalf("categories were not exposed: %+v", listing.Categories)
	}
	if len(listing.Statuses) != 1 || listing.Statuses[0].ID != siteTestID ||
		listing.Statuses[0].Total != 1 || listing.Statuses[0].BaseURL == "" {
		t.Fatalf("status was not exposed: %+v", listing.Statuses)
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, fixture.site.server.URL) {
		t.Fatalf("covers must be allowed by the policy, got %q", csp)
	}

	// A request may name its site, and an unknown one has to be reported rather
	// than silently answered with the default site's galleries.
	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums?site="+siteTestID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for a named site, got %d", response.Code)
	}
	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums?site=nope", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("an unknown site should be a 404, got %d", response.Code)
	}

	response = siteRequest(t, handler, http.MethodPost, "/api/site-albums/import",
		`{"site":"`+siteTestID+`","post_ids":[100]}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202, got %d: %s", response.Code, response.Body.String())
	}
	var queued struct {
		Accepted int `json:"accepted"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &queued); err != nil || queued.Accepted != 1 {
		t.Fatalf("queue response is wrong: %+v err=%v", queued, err)
	}

	response = siteRequest(t, handler, http.MethodPost, "/api/site-albums/import", `{"post_ids":[]}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("an empty selection should conflict, got %d", response.Code)
	}

	waitForSiteImport(t, fixture.importer, siteTestID, 100)

	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums", "")
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Albums[0].DraftID == "" {
		t.Fatalf("the imported gallery should link to its draft: %+v", listing.Albums[0])
	}

	response = siteRequest(t, handler, http.MethodGet, "/api/state", "")
	var state map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["site_albums"] == nil {
		t.Fatalf("the uploader page needs site_albums in its state")
	}
}

// TestSiteAlbumEndpointsSwitchBetweenSites covers the second gallery site: the
// page builds its picker from the state payload, and every site answers with its
// own index while the cover policy still allows both origins.
func TestSiteAlbumEndpointsSwitchBetweenSites(t *testing.T) {
	beta := betaGallery(t)
	fixture := newSiteFixtureWith(t, []sitealbum.Site{
		{ID: "beta", Name: "Beta", BaseURL: beta.server.URL},
	})

	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: fixture.drafts, ImportToken: "pairing-token",
		Sites: fixture.registry, SiteImport: fixture.importer,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	response := siteRequest(t, handler, http.MethodGet, "/api/state", "")
	var state struct {
		SiteAlbums []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Total int    `json:"total"`
		} `json:"site_albums"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.SiteAlbums) != 2 || state.SiteAlbums[0].ID != siteTestID || state.SiteAlbums[1].ID != "beta" {
		t.Fatalf("the page needs every site to build its picker: %+v", state.SiteAlbums)
	}
	if state.SiteAlbums[1].Name != "Beta" || state.SiteAlbums[1].Total != 1 {
		t.Fatalf("the second site's status is wrong: %+v", state.SiteAlbums[1])
	}

	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums?site=beta", "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	var listing struct {
		Site   string          `json:"site"`
		Albums []siteAlbumView `json:"albums"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.Site != "beta" || len(listing.Albums) != 1 || listing.Albums[0].ID != 200 {
		t.Fatalf("the named site should serve its own gallery: %+v", listing)
	}
	csp := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, beta.server.URL) || !strings.Contains(csp, fixture.site.server.URL) {
		t.Fatalf("every cover origin must be allowed, got %q", csp)
	}

	// Importing names its site too, so a pick on the second site cannot be
	// resolved against the first one's post ids.
	response = siteRequest(t, handler, http.MethodPost, "/api/site-albums/import",
		`{"site":"beta","post_ids":[200]}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202, got %d: %s", response.Code, response.Body.String())
	}
	waitForSiteImport(t, fixture.importer, "beta", 200)

	drafts, err := fixture.drafts.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].SourceURL != "https://example.com/beta/" || drafts[0].FileCount != 1 {
		t.Fatalf("the second site's gallery did not become a draft: %+v", drafts)
	}

	response = siteRequest(t, handler, http.MethodPost, "/api/site-albums/refresh", `{"site":"beta"}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 for a named refresh, got %d: %s", response.Code, response.Body.String())
	}
}

// TestSiteAlbumImportFeedsThePublishPipeline walks the whole objective: browse
// the site index, queue a gallery, commit the draft, then run the existing
// worker so the job really reaches "ready" with a signed snapshot.
func TestSiteAlbumImportFeedsThePublishPipeline(t *testing.T) {
	ctx := context.Background()
	fixture := newSiteFixture(t)

	// The fake Telegram API answers one sendMediaGroup with one document per photo.
	telegramServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse upload: %v", err)
			return
		}
		if chat := request.FormValue("chat_id"); chat != "-100123" {
			t.Errorf("unexpected Telegram channel %q", chat)
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"ok":true,"result":[`+
			`{"message_id":11,"document":{"file_id":"file-1","file_unique_id":"unique-1","mime_type":"image/png"}},`+
			`{"message_id":12,"document":{"file_id":"file-2","file_unique_id":"unique-2","mime_type":"image/png"}},`+
			`{"message_id":13,"document":{"file_id":"file-3","file_unique_id":"unique-3","mime_type":"image/png"}}]}`)
	}))
	defer telegramServer.Close()

	directory := t.TempDir()
	database, err := OpenStore(filepath.Join(directory, "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	uploader := telegram.New(telegramServer.URL, "secret", "https://gimg.example", 0, 0, 1, time.Second)
	service := NewService(database, uploader, Options{
		SnapshotDir: filepath.Join(directory, "batches"), ImageBase: "https://gimg.example",
		SigningSecret: strings.Repeat("s", 32), MaxImageBytes: 20 << 20, ChatIDs: []string{"-100123"},
	}, logger)
	service.SetSnapshotPublisher(func(context.Context, string) error { return nil })
	server, err := NewServer(database, service, ServerOptions{
		SnapshotDir: filepath.Join(directory, "batches"), Imports: fixture.drafts, ImportToken: "pairing-token",
		Sites: fixture.registry, SiteImport: fixture.importer,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	response := siteRequest(t, handler, http.MethodPost, "/api/site-albums/import",
		`{"site":"`+siteTestID+`","post_ids":[100]}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202, got %d: %s", response.Code, response.Body.String())
	}
	waitForSiteImport(t, fixture.importer, siteTestID, 100)

	drafts, err := fixture.drafts.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].FileCount != 3 {
		t.Fatalf("the gallery did not become a draft: %+v", drafts)
	}
	draft, err := fixture.drafts.readDraft(drafts[0].ID)
	if err != nil {
		t.Fatal(err)
	}

	// Commit exactly what the browser sends: every file, in gallery order.
	fileIDs := make([]string, 0, len(draft.Files))
	for _, file := range draft.Files {
		fileIDs = append(fileIDs, file.ID)
	}
	body, err := json.Marshal(map[string]any{
		"title": "Alpha cosplay", "category": "Cosplay", "tags": []string{"Byoru"},
		"channel_id": "-100123", "file_ids": fileIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	response = siteRequest(t, handler, http.MethodPost, "/api/telegram-imports/"+draft.ID+"/commit", string(body))
	if response.Code != http.StatusCreated {
		t.Fatalf("expected HTTP 201, got %d: %s", response.Code, response.Body.String())
	}
	var job Job
	if err := json.Unmarshal(response.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.ID == "" || job.TotalFiles != 3 || job.ChannelID != "-100123" {
		t.Fatalf("unexpected job from the site draft: %+v", job)
	}
	if job.Title != "Alpha cosplay" || job.Category != "Cosplay" {
		t.Fatalf("draft metadata did not reach the job: %+v", job)
	}

	claimed, found, err := database.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if claimed.ID != job.ID {
		t.Fatalf("claimed job %q instead of %q", claimed.ID, job.ID)
	}
	if err := service.process(ctx, claimed); err != nil {
		t.Fatal(err)
	}
	completed, err := database.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "ready" || completed.UploadedFiles != 3 {
		t.Fatalf("the site gallery did not publish: %+v", completed)
	}

	data, err := os.ReadFile(completed.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	var batch snapshot.Batch
	if err := json.Unmarshal(data, &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.Galleries) != 1 || batch.Galleries[0].ID != job.ID || len(batch.Galleries[0].Photos) != 3 {
		t.Fatalf("unexpected snapshot for the site gallery: %+v", batch)
	}
	if !strings.HasPrefix(batch.Galleries[0].Photos[0].TGURL, "https://gimg.example/tg/") {
		t.Fatalf("snapshot photos must be signed, got %q", batch.Galleries[0].Photos[0].TGURL)
	}
	if strings.Contains(string(data), "file-1") {
		t.Fatalf("public snapshot leaked Telegram file ids: %s", data)
	}

	reopened, err := fixture.drafts.readDraft(draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.CommittedJobID != job.ID {
		t.Fatalf("draft was not marked committed: %+v", reopened)
	}
}

// TestServerAllowsHotlinkedCoverHosts covers the cover policy for a site whose
// galleries live on a CDN: the site's own origin, the wildcard subdomain its
// media sits on, and `https:` because those hosts cannot be listed up front.
func TestServerAllowsHotlinkedCoverHosts(t *testing.T) {
	fixture := newSiteFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The policy is built from the configured sites alone, so this registry needs
	// no index and can name real domains without reaching for the network.
	registry, err := sitealbum.NewRegistry(t.TempDir(), []sitealbum.Site{
		{ID: "cosplaytele-com", Name: "Cosplaytele", BaseURL: "https://cosplaytele.com"},
		{ID: "misskon", Name: "MissKon", BaseURL: "https://misskon.com", ImageSource: sitealbum.ImageSourceContent},
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := NewService(store, nil, Options{}, logger)
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: fixture.drafts, ImportToken: "pairing-token",
		Sites: registry, SiteImport: fixture.importer,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	csp := siteRequest(t, server.Handler(), http.MethodGet, "/api/state", "").Header().Get("Content-Security-Policy")

	for _, want := range []string{
		"https://cosplaytele.com",
		"https://*.cosplaytele.com",
		"https://misskon.com",
		"https://*.misskon.com",
		" https:",
		"script-src 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("the policy is missing %q: %q", want, csp)
		}
	}
}

// TestSiteAlbumVerifyEndpointReportsRealCounts covers the measurement the page
// asks for on a site whose titles overstate the gallery: only reading the posts
// tells a complete album from a truncated one, and the answer is kept in the
// index so the listing can show both numbers.
func TestSiteAlbumVerifyEndpointReportsRealCounts(t *testing.T) {
	site := hotlinkGallery(t)
	// The title advertises nine photos; the post carries two.
	site.posts[0].Title = siteRendered{Rendered: "Hotlink cosplay 9 photos"}
	fixture := newSiteFixtureWith(t, []sitealbum.Site{
		{ID: "hotlink", Name: "Hotlink", BaseURL: site.server.URL, ImageSource: sitealbum.ImageSourceContent},
	})

	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: fixture.drafts, ImportToken: "pairing-token",
		Sites: fixture.registry, SiteImport: fixture.importer,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	response := siteRequest(t, handler, http.MethodPost, "/api/site-albums/verify",
		`{"site":"hotlink","post_ids":[900]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	var measured struct {
		Site   string         `json:"site"`
		Actual map[string]int `json:"actual"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &measured); err != nil {
		t.Fatal(err)
	}
	if measured.Site != "hotlink" || measured.Actual["900"] != 2 {
		t.Fatalf("the endpoint should report what the post really carries: %+v", measured)
	}

	response = siteRequest(t, handler, http.MethodGet, "/api/site-albums?site=hotlink", "")
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	var listing struct {
		Albums []struct {
			ID     int `json:"id"`
			Photos int `json:"photos"`
			Actual int `json:"actual"`
		} `json:"albums"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Albums) != 1 {
		t.Fatalf("expected one album, got %+v", listing.Albums)
	}
	if album := listing.Albums[0]; album.Photos != 9 || album.Actual != 2 {
		t.Fatalf("the listing should carry both the advertised and the real count: %+v", album)
	}

	// A site that keeps its gallery in attachments has nothing to measure: the
	// title is already the attachment count.
	response = siteRequest(t, handler, http.MethodPost, "/api/site-albums/verify",
		`{"site":"alpha","post_ids":[100]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", response.Code, response.Body.String())
	}
	measured.Actual = nil
	if err := json.Unmarshal(response.Body.Bytes(), &measured); err != nil {
		t.Fatal(err)
	}
	if len(measured.Actual) != 0 {
		t.Fatalf("a media site has nothing to measure: %+v", measured)
	}
}

// TestStateReportsADraftThatIsStillBeingFilled covers the "抓取中" hint in the
// draft list: a site import creates its draft before the first image lands, so the
// page has to tell an import in progress apart from an empty result.
func TestStateReportsADraftThatIsStillBeingFilled(t *testing.T) {
	fixture := newSiteFixture(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: fixture.drafts, ImportToken: "pairing-token",
		Sites: fixture.registry, SiteImport: fixture.importer,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}

	draft, err := fixture.drafts.Create(CreateTelegramImportRequest{
		Kind: KindWordPress, SourceURL: "https://example.com/alpha/", Title: "Alpha cosplay",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.importer.markFilling(draft.ID, 45)

	filling := stateDrafts(t, server)[draft.ID]
	if !filling.Importing || filling.TargetCount != 45 {
		t.Fatalf("a draft mid-import should report its target: %+v", filling)
	}
	if filling.FileCount != 0 {
		t.Fatalf("the draft is listed before its images land, got %d files", filling.FileCount)
	}

	fixture.importer.unmarkFilling(draft.ID)
	settled := stateDrafts(t, server)[draft.ID]
	if settled.Importing || settled.TargetCount != 0 {
		t.Fatalf("a finished import should not claim its draft: %+v", settled)
	}
}

type stateDraft struct {
	ID          string `json:"id"`
	FileCount   int    `json:"file_count"`
	Importing   bool   `json:"importing"`
	TargetCount int    `json:"target_count"`
}

func stateDrafts(t *testing.T, server *Server) map[string]stateDraft {
	t.Helper()
	response := siteRequest(t, server.Handler(), http.MethodGet, "/api/state", "")
	if response.Code != http.StatusOK {
		t.Fatalf("state should answer 200, got %d", response.Code)
	}
	var state struct {
		Imports []stateDraft `json:"telegram_imports"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	found := map[string]stateDraft{}
	for _, draft := range state.Imports {
		found[draft.ID] = draft
	}
	return found
}

func siteRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
