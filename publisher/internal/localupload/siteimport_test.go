package localupload

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/sitealbum"
)

type siteRendered struct {
	Rendered string `json:"rendered"`
}

type sitePost struct {
	ID            int          `json:"id"`
	Slug          string       `json:"slug"`
	Link          string       `json:"link"`
	Date          string       `json:"date"`
	Modified      string       `json:"modified"`
	Title         siteRendered `json:"title"`
	Content       siteRendered `json:"content"`
	FeaturedMedia int          `json:"featured_media"`
	Categories    []int        `json:"categories"`
	Tags          []int        `json:"tags"`
}

type siteMedia struct {
	ID     int    `json:"id"`
	Slug   string `json:"slug"`
	Source string `json:"source_url"`
	Mime   string `json:"mime_type"`
}

type siteTerm struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// fakeGallerySite serves the slice of the WordPress REST API the importer uses,
// plus the image bytes themselves.
type fakeGallerySite struct {
	server  *httptest.Server
	posts   []sitePost
	media   map[int][]siteMedia
	files   map[string][]byte
	missing map[string]bool
	// gate holds every image download until it is closed, so a test can look at
	// the importer while it is halfway through.
	gate    chan struct{}
	uploads int
}

func newFakeGallerySite(t *testing.T) *fakeGallerySite {
	t.Helper()
	site := &fakeGallerySite{
		media:   map[int][]siteMedia{},
		files:   map[string][]byte{},
		missing: map[string]bool{},
	}
	site.server = httptest.NewServer(http.HandlerFunc(site.handle))
	t.Cleanup(site.server.Close)
	return site
}

// addImage stores one image and returns its absolute URL and slug.
func (s *fakeGallerySite) addImage(t *testing.T, name string, width, height int) (string, string) {
	t.Helper()
	path := "/uploads/" + name + ".png"
	s.files[path] = sitePNG(t, width, height)
	return s.server.URL + path, name
}

func (s *fakeGallerySite) handle(response http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/uploads/") {
		if s.gate != nil {
			<-s.gate
		}
		if s.missing[request.URL.Path] {
			http.NotFound(response, request)
			return
		}
		body, ok := s.files[request.URL.Path]
		if !ok {
			http.NotFound(response, request)
			return
		}
		s.uploads++
		response.Header().Set("Content-Type", "image/png")
		response.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = response.Write(body)
		return
	}

	path := strings.TrimPrefix(request.URL.Path, "/wp-json/wp/v2")
	query := request.URL.Query()
	switch {
	case path == "/posts":
		if include := query.Get("include"); include != "" {
			s.writeList(response, selectSitePosts(s.posts, include))
			return
		}
		s.writeList(response, s.posts)
	case strings.HasPrefix(path, "/posts/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/posts/"))
		for _, post := range s.posts {
			if post.ID == id {
				siteJSON(response, post)
				return
			}
		}
		http.NotFound(response, request)
	case path == "/media":
		if parent := query.Get("parent"); parent != "" {
			id, _ := strconv.Atoi(parent)
			s.writeList(response, s.media[id])
			return
		}
		var items []siteMedia
		for _, raw := range strings.Split(query.Get("include"), ",") {
			id, _ := strconv.Atoi(raw)
			for _, list := range s.media {
				for _, item := range list {
					if item.ID == id {
						items = append(items, item)
					}
				}
			}
		}
		s.writeList(response, items)
	case path == "/categories", path == "/tags":
		s.writeList(response, []siteTerm{{ID: 7, Slug: "cosplay", Name: "Cosplay", Count: 3}})
	default:
		http.NotFound(response, request)
	}
}

// selectSitePosts answers an include= request, which is how a page of galleries
// is measured without one request per album.
func selectSitePosts(posts []sitePost, include string) []sitePost {
	wanted := map[int]bool{}
	for _, raw := range strings.Split(include, ",") {
		id, _ := strconv.Atoi(raw)
		if id > 0 {
			wanted[id] = true
		}
	}
	selected := []sitePost{}
	for _, post := range posts {
		if wanted[post.ID] {
			selected = append(selected, post)
		}
	}
	return selected
}

func (s *fakeGallerySite) writeList(response http.ResponseWriter, items any) {
	count := siteSliceLen(items)
	response.Header().Set("X-WP-Total", strconv.Itoa(count))
	pages := 0
	if count > 0 {
		pages = 1
	}
	response.Header().Set("X-WP-TotalPages", strconv.Itoa(pages))
	siteJSON(response, items)
}

func siteJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
	}
}

func siteSliceLen(value any) int {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Slice {
		return 0
	}
	return reflected.Len()
}

func sitePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// siteTestID is the registry id the fixture site is registered under. Real
// deployments have several, which is exactly why every call carries one.
const siteTestID = "alpha"

// siteFixture wires a fake site, a cached index and an importer together, which
// is exactly how the uploader process assembles them.
type siteFixture struct {
	site     *fakeGallerySite
	registry *sitealbum.Registry
	store    *sitealbum.Store
	drafts   *TelegramImportStore
	importer *SiteImporter
}

func newSiteFixture(t *testing.T) *siteFixture {
	t.Helper()
	return newSiteFixtureWith(t, nil)
}

// newSiteFixtureWith registers the standard alpha gallery plus any extra site, so
// one fixture covers both the single-site and the site-switching cases.
func newSiteFixtureWith(t *testing.T, extras []sitealbum.Site) *siteFixture {
	t.Helper()
	site := alphaGallery(t)
	configured := append([]sitealbum.Site{
		{ID: siteTestID, Name: "Alpha", BaseURL: site.server.URL},
	}, extras...)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := sitealbum.NewRegistry(t.TempDir(), configured, logger)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry.Sites() {
		target, err := registry.Target(entry.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := target.Store.Refresh(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	target, err := registry.Target(siteTestID)
	if err != nil {
		t.Fatal(err)
	}
	drafts, err := NewTelegramImportStore(t.TempDir(), 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	importer, err := NewSiteImporter(registry, drafts, t.TempDir(), 20<<20, logger)
	if err != nil {
		t.Fatal(err)
	}
	importer.Bind(context.Background())
	return &siteFixture{site: site, registry: registry, store: target.Store, drafts: drafts, importer: importer}
}

// alphaGallery fills a fake site with the three image album the tests import. The
// fourth attachment is the featured image, which the album rules drop.
func alphaGallery(t *testing.T) *fakeGallerySite {
	t.Helper()
	site := newFakeGallerySite(t)

	coverURL, coverSlug := site.addImage(t, "alpha-cover_result", 1200, 1600)
	firstURL, firstSlug := site.addImage(t, "alpha-cosplay-1_result", 800, 1200)
	secondURL, secondSlug := site.addImage(t, "alpha-cosplay-2_result", 801, 1201)
	thirdURL, thirdSlug := site.addImage(t, "alpha-cosplay-3_result", 802, 1202)

	site.posts = []sitePost{{
		ID: 100, Slug: "alpha", Link: "https://example.com/alpha/",
		Date: "2026-10-01T10:00:00", Modified: "2026-10-01T10:00:00",
		Title: siteRendered{Rendered: "Alpha cosplay 3 photos"}, FeaturedMedia: 1004,
		Categories: []int{7},
	}}
	site.media[100] = []siteMedia{
		{ID: 1001, Slug: firstSlug, Source: firstURL, Mime: "image/png"},
		{ID: 1002, Slug: secondSlug, Source: secondURL, Mime: "image/png"},
		{ID: 1003, Slug: thirdSlug, Source: thirdURL, Mime: "image/png"},
		{ID: 1004, Slug: coverSlug, Source: coverURL, Mime: "image/png"},
	}
	return site
}

// betaGallery is a one album site with a deliberately different post id, so a
// test can tell the two indexes apart at a glance.
func betaGallery(t *testing.T) *fakeGallerySite {
	t.Helper()
	site := newFakeGallerySite(t)
	url, slug := site.addImage(t, "beta-cosplay-1_result", 900, 1300)
	site.posts = []sitePost{{
		ID: 200, Slug: "beta", Link: "https://example.com/beta/",
		Date: "2026-09-20T10:00:00", Modified: "2026-09-20T10:00:00",
		Title: siteRendered{Rendered: "Beta cosplay 1 photos"}, FeaturedMedia: 2001,
		Categories: []int{8},
	}}
	site.media[200] = []siteMedia{{ID: 2001, Slug: slug, Source: url, Mime: "image/png"}}
	return site
}

// hotlinkGallery is a one album site that keeps its gallery inside the post body
// instead of in attachments, which is how misskon.com publishes: its media
// endpoint carries only the cover, and the images themselves sit on a CDN.
func hotlinkGallery(t *testing.T) *fakeGallerySite {
	t.Helper()
	site := newFakeGallerySite(t)
	coverURL, coverSlug := site.addImage(t, "hot-cover_result", 1200, 1600)
	firstURL, _ := site.addImage(t, "hot-cosplay-1_result", 800, 1200)
	secondURL, _ := site.addImage(t, "hot-cosplay-2_result", 801, 1201)

	site.posts = []sitePost{{
		ID: 900, Slug: "hot", Link: "https://example.com/hot/",
		Date: "2026-10-02T10:00:00", Modified: "2026-10-02T10:00:00",
		Title: siteRendered{Rendered: "Hotlink cosplay 2 photos"},
		Content: siteRendered{Rendered: `<p>intro</p>` +
			`<img class="aligncenter" src="` + firstURL + `" />` +
			`<script src="//ads.example.com/banner.js"></script>` +
			`<img class="lazy" src="data:image/gif;base64,R0lGODlh" data-src="` + secondURL + `" />`},
		FeaturedMedia: 9001,
		Categories:    []int{7},
	}}
	site.media[900] = []siteMedia{{ID: 9001, Slug: coverSlug, Source: coverURL, Mime: "image/png"}}
	return site
}

func TestSiteImportFillsADraftInGalleryOrder(t *testing.T) {
	fixture := newSiteFixture(t)
	draft, err := fixture.importer.Import(context.Background(), SiteImportRequest{
		SiteID: siteTestID, PostID: 100, Category: "Cosplay", Tags: []string{"Byoru"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Kind != KindWordPress {
		t.Fatalf("site drafts must be tagged, got %q", draft.Kind)
	}
	if draft.SourceURL != "https://example.com/alpha/" {
		t.Fatalf("draft should point at the gallery page, got %q", draft.SourceURL)
	}
	if draft.Category != "Cosplay" || len(draft.Tags) != 1 {
		t.Fatalf("draft metadata was lost: %+v", draft)
	}
	if draft.Title != "Alpha cosplay" {
		t.Fatalf("draft should reuse the gallery title, got %q", draft.Title)
	}
	if len(draft.Files) != 3 {
		t.Fatalf("expected the cover to be dropped, got %d files", len(draft.Files))
	}
	for index, file := range draft.Files {
		if file.Position != index+1 {
			t.Fatalf("file %d has position %d", index, file.Position)
		}
		if file.Width != 800+index || file.Height != 1200+index {
			t.Fatalf("real dimensions were not read: %+v", file)
		}
		if file.SHA256 == "" {
			t.Fatalf("file %d has no digest", index)
		}
	}
	if !strings.HasPrefix(draft.Files[0].Name, "alpha-cosplay-1_result") {
		t.Fatalf("files must keep the source order, got %q", draft.Files[0].Name)
	}
	if fixture.site.uploads != 3 {
		t.Fatalf("expected exactly 3 image downloads, got %d", fixture.site.uploads)
	}
}

func TestSiteImportRejectsUnknownGallery(t *testing.T) {
	fixture := newSiteFixture(t)
	if _, err := fixture.importer.Import(context.Background(), SiteImportRequest{SiteID: siteTestID, PostID: 999}); err == nil {
		t.Fatalf("expected an unknown gallery to be rejected")
	}
	if _, err := fixture.importer.Import(context.Background(), SiteImportRequest{}); err == nil {
		t.Fatalf("expected a missing gallery id to be rejected")
	}
	if drafts, err := fixture.drafts.List(); err != nil || len(drafts) != 0 {
		t.Fatalf("no draft should survive a rejected import: %+v err=%v", drafts, err)
	}
}

func TestSiteImportCleansUpWhenAnImageIsMissing(t *testing.T) {
	fixture := newSiteFixture(t)
	fixture.site.missing["/uploads/alpha-cosplay-2_result.png"] = true

	if _, err := fixture.importer.Import(context.Background(), SiteImportRequest{SiteID: siteTestID, PostID: 100}); err == nil {
		t.Fatalf("expected a missing image to fail the import")
	}
	drafts, err := fixture.drafts.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 0 {
		t.Fatalf("a failed import must not leave a half filled draft: %+v", drafts)
	}
	if fixture.importer.IsActive(siteTestID, 100) {
		t.Fatalf("a failed import must release the gallery claim")
	}
}

func TestQueueImportsInTheBackground(t *testing.T) {
	fixture := newSiteFixture(t)
	if accepted := fixture.importer.Queue(siteTestID, []int{100, 100, 0}); accepted != 1 {
		t.Fatalf("expected one accepted gallery, got %d", accepted)
	}
	waitForSiteImport(t, fixture.importer, siteTestID, 100)

	if fixture.importer.Failure(siteTestID, 100) != "" {
		t.Fatalf("queued import failed: %s", fixture.importer.Failure(siteTestID, 100))
	}
	drafts, err := fixture.drafts.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].Kind != KindWordPress || drafts[0].FileCount != 3 {
		t.Fatalf("queued import did not produce a draft: %+v", drafts)
	}
	if fixture.importer.Active() != 0 {
		t.Fatalf("queue should be idle once the import finished")
	}
}

func TestQueueRecordsFailures(t *testing.T) {
	fixture := newSiteFixture(t)
	if accepted := fixture.importer.Queue(siteTestID, []int{999}); accepted != 1 {
		t.Fatalf("expected the id to be accepted for a background attempt")
	}
	waitForSiteImport(t, fixture.importer, siteTestID, 999)
	if fixture.importer.Failure(siteTestID, 999) == "" {
		t.Fatalf("expected the failure to be reported for the browser")
	}
}

// TestSiteImportReadsAHotlinkedGallery covers a site whose gallery is only in the
// post body. The media endpoint there holds the cover alone, so a draft with both
// body images is the proof that the configured strategy was used.
func TestSiteImportReadsAHotlinkedGallery(t *testing.T) {
	site := hotlinkGallery(t)
	fixture := newSiteFixtureWith(t, []sitealbum.Site{
		{ID: "hotlink", Name: "Hotlink", BaseURL: site.server.URL, ImageSource: sitealbum.ImageSourceContent},
	})

	target, err := fixture.registry.Target("hotlink")
	if err != nil {
		t.Fatal(err)
	}
	if _, total, err := target.Store.List(sitealbum.Filter{}); err != nil || total != 1 {
		t.Fatalf("the hotlinking site was not indexed: total=%d err=%v", total, err)
	}

	draft, err := fixture.importer.Import(context.Background(), SiteImportRequest{
		SiteID: "hotlink", PostID: 900, Category: "Cosplay",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Files) != 2 {
		t.Fatalf("expected the two body images, got %d: %+v", len(draft.Files), draft.Files)
	}
	if draft.Files[0].Width != 800 || draft.Files[1].Width != 801 {
		t.Fatalf("the body order was not kept: %+v", draft.Files)
	}
	if site.uploads != 2 {
		t.Fatalf("expected exactly two image downloads, got %d", site.uploads)
	}
}

// TestImporterMarksItsDraftWhileItFills covers the window between creating the
// draft and its last image landing. An import stages every image before it writes
// the first one, so without the marker the page shows an empty draft and the
// import looks broken instead of busy.
func TestImporterMarksItsDraftWhileItFills(t *testing.T) {
	fixture := newSiteFixture(t)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	fixture.site.gate = gate
	t.Cleanup(release)

	if accepted := fixture.importer.Queue(siteTestID, []int{100}); accepted != 1 {
		t.Fatalf("expected the gallery to be accepted for a background import")
	}
	draftID, target := waitForFillingDraft(t, fixture.importer, fixture.drafts)
	if target != 3 {
		t.Fatalf("the alpha gallery advertises 3 images, got %d", target)
	}

	release()
	waitForSiteImport(t, fixture.importer, siteTestID, 100)

	if _, filling := fixture.importer.Filling(draftID); filling {
		t.Fatalf("a finished import should stop claiming its draft")
	}
	draft, err := fixture.drafts.Get(draftID)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Files) != 3 {
		t.Fatalf("expected the three images to land once the gate opened, got %d", len(draft.Files))
	}
}

// waitForFillingDraft returns the draft a site import is currently filling.
func waitForFillingDraft(t *testing.T, importer *SiteImporter, drafts *TelegramImportStore) (string, int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		listed, err := drafts.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, draft := range listed {
			if target, filling := importer.Filling(draft.ID); filling {
				return draft.ID, target
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no draft was marked as being filled")
	return "", 0
}

func waitForSiteImport(t *testing.T, importer *SiteImporter, siteID string, postID int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !importer.IsActive(siteID, postID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("import of gallery %d did not finish", postID)
}
