package sitealbum

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/legacy"
)

type rendered struct {
	Rendered string `json:"rendered"`
}

type fakePost struct {
	ID            int      `json:"id"`
	Slug          string   `json:"slug"`
	Link          string   `json:"link"`
	Date          string   `json:"date"`
	Modified      string   `json:"modified"`
	Title         rendered `json:"title"`
	Content       rendered `json:"content"`
	FeaturedMedia int      `json:"featured_media"`
	Categories    []int    `json:"categories"`
	Tags          []int    `json:"tags"`
}

type fakeMedia struct {
	ID     int    `json:"id"`
	Slug   string `json:"slug"`
	Source string `json:"source_url"`
	Mime   string `json:"mime_type"`
}

type fakeTerm struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// fakeSite serves just enough of the WordPress REST API to exercise the index.
type fakeSite struct {
	server     *httptest.Server
	posts      []fakePost
	changed    []fakePost
	media      map[int][]fakeMedia
	categories []fakeTerm
	tags       []fakeTerm
	broken     bool
	// gate, when set, parks every page request until the test closes it, so a
	// walk can be inspected while it is halfway through.
	gate chan struct{}
	// pages counts the page requests that reached the handler.
	pages int32
	// failAfter makes the site die once that many pages have been served, which
	// is how a walk that breaks after it already published part of itself is
	// reproduced.
	failAfter int32
	// imageSource selects the strategy the store is configured with, so a fixture
	// can serve galleries that only exist in the post body.
	imageSource string
}

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	site := &fakeSite{media: map[int][]fakeMedia{}}
	site.server = httptest.NewServer(http.HandlerFunc(site.handle))
	t.Cleanup(site.server.Close)
	return site
}

func (s *fakeSite) handle(response http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, "/wp-json/wp/v2")
	query := request.URL.Query()

	if s.broken && path == "/posts" {
		// 404 again: not retried, so a broken site fails the walk immediately.
		http.Error(response, "no such page", http.StatusNotFound)
		return
	}
	if path == "/posts" {
		served := atomic.AddInt32(&s.pages, 1)
		// A 404 rather than a 5xx: client errors are not retried, so the tests
		// that expect a failed walk stay fast.
		if s.failAfter > 0 && served > s.failAfter {
			http.Error(response, "no such page", http.StatusNotFound)
			return
		}
		if s.gate != nil {
			<-s.gate
		}
	}

	switch {
	case path == "/posts":
		if include := query.Get("include"); include != "" {
			s.writeList(response, selectPosts(s.posts, include))
			return
		}
		if query.Get("modified_after") != "" {
			s.writePage(response, s.changed, query)
			return
		}
		s.writePage(response, s.posts, query)
	case strings.HasPrefix(path, "/posts/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/posts/"))
		for _, post := range s.posts {
			if post.ID == id {
				writeJSON(response, post)
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
		var items []fakeMedia
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
	case path == "/categories":
		s.writeList(response, s.categories)
	case path == "/tags":
		s.writeList(response, s.tags)
	default:
		http.NotFound(response, request)
	}
}

// selectPosts answers an include= request, which is how several post bodies are
// read at once when the browser wants to know the real image counts.
func selectPosts(posts []fakePost, include string) []fakePost {
	wanted := map[int]bool{}
	for _, raw := range strings.Split(include, ",") {
		id, _ := strconv.Atoi(raw)
		if id > 0 {
			wanted[id] = true
		}
	}
	selected := []fakePost{}
	for _, post := range posts {
		if wanted[post.ID] {
			selected = append(selected, post)
		}
	}
	return selected
}

func (s *fakeSite) writeList(response http.ResponseWriter, items any) {
	count := sliceLen(items)
	response.Header().Set("X-WP-Total", strconv.Itoa(count))
	pages := 0
	if count > 0 {
		pages = 1
	}
	response.Header().Set("X-WP-TotalPages", strconv.Itoa(pages))
	writeJSON(response, items)
}

// writePage answers one page of a list, which is what the store walks. Without a
// page parameter it returns everything, so the small fixtures behave as before.
func (s *fakeSite) writePage(response http.ResponseWriter, items any, query url.Values) {
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	page, _ := strconv.Atoi(query.Get("page"))
	total := sliceLen(items)
	if perPage <= 0 || page <= 0 {
		s.writeList(response, items)
		return
	}
	pages := (total + perPage - 1) / perPage
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	response.Header().Set("X-WP-Total", strconv.Itoa(total))
	response.Header().Set("X-WP-TotalPages", strconv.Itoa(pages))
	writeJSON(response, reflect.ValueOf(items).Slice(start, end).Interface())
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		http.Error(response, err.Error(), http.StatusInternalServerError)
	}
}

func sliceLen(value any) int {
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Slice {
		return 0
	}
	return reflected.Len()
}

func newTestStore(t *testing.T, directory string, site *fakeSite) *Store {
	t.Helper()
	source := legacy.NewWordPressSource(site.server.URL, site.server.Client())
	source.ImageSource = site.imageSource
	store, err := NewStore(directory, source, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func sampleSite(t *testing.T) *fakeSite {
	t.Helper()
	site := newFakeSite(t)
	site.posts = []fakePost{
		{
			ID: 100, Slug: "alpha", Link: "https://example.com/alpha/",
			Date: "2026-10-01T10:00:00", Modified: "2026-10-01T10:00:00",
			Title: rendered{Rendered: "Alpha cosplay 3 photos"}, FeaturedMedia: 1004,
			Categories: []int{7}, Tags: []int{11},
		},
		{
			ID: 200, Slug: "beta", Link: "https://example.com/beta/",
			Date: "2026-09-20T10:00:00", Modified: "2026-09-21T10:00:00",
			Title: rendered{Rendered: "Beta cosplay 2 photos and 1 videos"}, FeaturedMedia: 2002,
			Categories: []int{9}, Tags: []int{11, 12},
		},
	}
	site.media[100] = []fakeMedia{
		{1001, "alpha-cosplay-1_result", site.server.URL + "/uploads/alpha-1.webp", "image/webp"},
		{1002, "alpha-cosplay-2_result", site.server.URL + "/uploads/alpha-2.webp", "image/webp"},
		{1003, "alpha-cosplay-3_result", site.server.URL + "/uploads/alpha-3.webp", "image/webp"},
		{1004, "alpha-cover_result", site.server.URL + "/uploads/alpha-cover.webp", "image/webp"},
	}
	site.media[200] = []fakeMedia{
		{2001, "beta-cosplay-1_result", site.server.URL + "/uploads/beta-1.webp", "image/webp"},
		{2002, "beta-cosplay-2_result", site.server.URL + "/uploads/beta-2.webp", "image/webp"},
	}
	site.categories = []fakeTerm{{ID: 7, Slug: "cosplay-nude", Name: "Cosplay Nude", Count: 42}}
	site.tags = []fakeTerm{{ID: 11, Slug: "genshin-impact", Name: "Genshin Impact", Count: 9}}
	return site
}

// contentSite serves galleries the way misskon.com does: the media endpoint only
// holds the cover, the gallery lives in the post body, and the title advertises
// more photos than the post actually carries.
func contentSite(t *testing.T) *fakeSite {
	t.Helper()
	site := newFakeSite(t)
	site.imageSource = legacy.ImageSourceContent
	site.posts = []fakePost{
		{
			ID: 300, Slug: "gamma", Link: "https://example.com/gamma/",
			Date: "2026-10-02T10:00:00", Modified: "2026-10-02T10:00:00",
			Title: rendered{Rendered: "Gamma cosplay 9 photos"}, FeaturedMedia: 3001,
			Content: rendered{Rendered: `<img src="` + site.server.URL + `/uploads/gamma-1.webp">` +
				`<img data-src="` + site.server.URL + `/uploads/gamma-2.webp">`},
		},
		{
			ID: 400, Slug: "delta", Link: "https://example.com/delta/",
			Date: "2026-10-03T10:00:00", Modified: "2026-10-03T10:00:00",
			Title: rendered{Rendered: "Delta cosplay 4 photos"}, FeaturedMedia: 4001,
			Content: rendered{Rendered: `<img src="` + site.server.URL + `/uploads/delta-1.webp">`},
		},
	}
	site.media[300] = []fakeMedia{{3001, "gamma-cover", site.server.URL + "/uploads/gamma-cover.webp", "image/webp"}}
	site.media[400] = []fakeMedia{{4001, "delta-cover", site.server.URL + "/uploads/delta-cover.webp", "image/webp"}}
	return site
}

func TestRefreshBuildsIndexWithCoversAndTerms(t *testing.T) {
	site := sampleSite(t)
	store := newTestStore(t, t.TempDir(), site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	status := store.Status()
	if status.Total != 2 {
		t.Fatalf("expected 2 albums, got %d", status.Total)
	}
	if status.BuiltAt == 0 || status.RefreshedAt == 0 {
		t.Fatalf("expected timestamps to be recorded: %+v", status)
	}
	if status.Error != "" {
		t.Fatalf("unexpected refresh error: %s", status.Error)
	}
	if status.Building {
		t.Fatalf("refresh should have finished")
	}

	albums, total, err := store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(albums) != 2 {
		t.Fatalf("expected 2 albums, got %d of %d", len(albums), total)
	}
	// Newest gallery id first, matching the site's own listing order.
	if albums[0].ID != 200 || albums[1].ID != 100 {
		t.Fatalf("unexpected order: %d, %d", albums[0].ID, albums[1].ID)
	}
	if albums[0].Photos != 2 || albums[0].Videos != 1 {
		t.Fatalf("title counters were not parsed: %+v", albums[0])
	}
	if albums[0].CoverURL != site.server.URL+"/uploads/beta-2.webp" {
		t.Fatalf("cover was not resolved: %q", albums[0].CoverURL)
	}
	if albums[0].Title != "Beta cosplay" {
		t.Fatalf("title should be the cleaned prefix, got %q", albums[0].Title)
	}

	album, err := store.Get(100)
	if err != nil {
		t.Fatal(err)
	}
	if album.Slug != "alpha" || album.CoverID != 1004 {
		t.Fatalf("unexpected album: %+v", album)
	}

	categories, tags := store.Terms()
	if len(categories) != 1 || categories[0].Name != "Cosplay Nude" {
		t.Fatalf("unexpected categories: %+v", categories)
	}
	if len(tags) != 1 || tags[0].Slug != "genshin-impact" {
		t.Fatalf("unexpected tags: %+v", tags)
	}
}

func TestListFiltersAndPaginates(t *testing.T) {
	site := sampleSite(t)
	store := newTestStore(t, t.TempDir(), site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	matched, total, err := store.List(Filter{Query: "ALPHA"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(matched) != 1 || matched[0].ID != 100 {
		t.Fatalf("query filter failed: %d of %d", len(matched), total)
	}

	matched, total, err = store.List(Filter{Query: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || matched[0].ID != 200 {
		t.Fatalf("slug search failed: %d of %d", len(matched), total)
	}

	matched, total, err = store.List(Filter{CategoryID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || matched[0].ID != 100 {
		t.Fatalf("category filter failed: %d of %d", len(matched), total)
	}

	matched, total, err = store.List(Filter{CategoryID: 404})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(matched) != 0 {
		t.Fatalf("expected no matches for an unknown category, got %d", total)
	}

	first, total, err := store.List(Filter{PerPage: 1, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(first) != 1 || first[0].ID != 200 {
		t.Fatalf("page 1 wrong: %+v total=%d", first, total)
	}
	second, _, err := store.List(Filter{PerPage: 1, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != 100 {
		t.Fatalf("page 2 wrong: %+v", second)
	}
	beyond, _, err := store.List(Filter{PerPage: 1, Page: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(beyond) != 0 {
		t.Fatalf("expected an empty page past the end, got %+v", beyond)
	}

	// A wildcard typed into the search box must stay a literal character.
	if _, total, err := store.List(Filter{Query: "%"}); err != nil || total != 0 {
		t.Fatalf("query should be literal, got total=%d err=%v", total, err)
	}
}

func TestIncrementalRefreshKeepsTheOriginalWindow(t *testing.T) {
	site := sampleSite(t)
	store := newTestStore(t, t.TempDir(), site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	builtAt := store.Status().BuiltAt

	site.changed = []fakePost{{
		ID: 300, Slug: "gamma", Link: "https://example.com/gamma/",
		Date: "2026-10-05T10:00:00", Modified: "2026-10-05T10:00:00",
		Title: rendered{Rendered: "Gamma cosplay 5 photos"}, FeaturedMedia: 3001,
	}}
	site.media[300] = []fakeMedia{
		{3001, "gamma-cosplay-1_result", site.server.URL + "/uploads/gamma-1.webp", "image/webp"},
	}
	if err := store.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	status := store.Status()
	if status.Total != 3 {
		t.Fatalf("incremental refresh should merge into the cache, got %d", status.Total)
	}
	if status.BuiltAt != builtAt {
		t.Fatalf("incremental refresh must not slide the window: %d -> %d", builtAt, status.BuiltAt)
	}
	if status.RefreshedAt < builtAt {
		t.Fatalf("refreshed_at should move forward: %d < %d", status.RefreshedAt, builtAt)
	}
}

func TestStoreReloadsTheCachedIndex(t *testing.T) {
	site := sampleSite(t)
	directory := t.TempDir()
	store := newTestStore(t, directory, site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	// A second process must see the same listing without walking the site again.
	site.broken = true
	reloaded := newTestStore(t, directory, site)
	albums, total, err := reloaded.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(albums) != 2 {
		t.Fatalf("cached index was not reused: %d of %d", len(albums), total)
	}
	if reloaded.Status().Total != 2 {
		t.Fatalf("cached status was not restored")
	}
}

func TestRefreshRecordsFailureAndKeepsServingTheOldIndex(t *testing.T) {
	site := sampleSite(t)
	store := newTestStore(t, t.TempDir(), site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	site.broken = true
	if err := store.Refresh(context.Background(), true); err == nil {
		t.Fatalf("expected the broken site to surface an error")
	}
	status := store.Status()
	if status.Error == "" {
		t.Fatalf("expected the failure to be recorded")
	}
	if status.Building {
		t.Fatalf("a failed refresh must release the single flight slot")
	}
	if _, total, err := store.List(Filter{}); err != nil || total != 2 {
		t.Fatalf("a failed refresh must not drop the cached albums: %d err=%v", total, err)
	}
}

// bigSite is a site with enough pages to make the walk publish along the way.
func bigSite(t *testing.T) *fakeSite {
	t.Helper()
	site := newFakeSite(t)
	for index := 0; index < publishEveryPages*refreshPerPage; index++ {
		id := 100000 - index
		site.posts = append(site.posts, fakePost{
			ID: id, Slug: fmt.Sprintf("post-%d", id), Link: fmt.Sprintf("https://example.com/%d/", id),
			Date: "2026-08-01T10:00:00", Modified: "2026-08-01T10:00:00",
			Title: rendered{Rendered: "Post 1 photos"},
		})
	}
	return site
}

// TestRefreshPublishesWhatItHasSoFar covers a site too large to read in one
// breath: the albums already read have to be browsable while the rest is still
// coming in, instead of the list staying empty for minutes.
func TestRefreshPublishesWhatItHasSoFar(t *testing.T) {
	site := bigSite(t)
	total := publishEveryPages * refreshPerPage
	// Buffered, so releasing a page never blocks the test itself.
	site.gate = make(chan struct{}, 64)
	store := newTestStore(t, t.TempDir(), site)

	done := make(chan error, 1)
	go func() { done <- store.Refresh(context.Background(), true) }()

	site.gate <- struct{}{} // let page one through
	deadline := time.Now().Add(10 * time.Second)
	for atomic.LoadInt32(&site.pages) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// The walk is parked on page two, so page one is published and the rest is
	// still pending: exactly the moment a user opens the page.
	if got := store.Status().Total; got != refreshPerPage {
		t.Fatalf("the first page should already be listed, got %d albums", got)
	}
	if albums, count, err := store.List(Filter{PerPage: 1}); err != nil || count != refreshPerPage || len(albums) != 1 {
		t.Fatalf("a partial index must be served whole: count=%d err=%v", count, err)
	}

	for page := 2; page <= publishEveryPages+2; page++ {
		site.gate <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := store.Status().Total; got != total {
		t.Fatalf("the walk should finish with every album, got %d of %d", got, total)
	}
}

// TestFailedWalkKeepsWhatItRead covers a rebuild that dies after it has already
// published part of itself. Every publish is a union with the previous index, so
// the albums read before the failure stay browsable, and the partial mark makes
// the next refresh walk the whole site again instead of topping it up.
func TestFailedWalkKeepsWhatItRead(t *testing.T) {
	site := bigSite(t)
	total := publishEveryPages * refreshPerPage
	directory := t.TempDir()
	store := newTestStore(t, directory, site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := store.Status().Total; got != total {
		t.Fatalf("the first walk should index everything, got %d of %d", got, total)
	}

	// The rebuild publishes its first page and then dies.
	atomic.StoreInt32(&site.pages, 0)
	site.failAfter = 1
	if err := store.Refresh(context.Background(), true); err == nil {
		t.Fatalf("expected the failing walk to surface an error")
	}
	if got := store.Status().Total; got != total {
		t.Fatalf("a failed rebuild must keep the albums it already had, got %d of %d", got, total)
	}

	// The cache on disk says it is partial, so the next refresh cannot settle for
	// an incremental walk: everything the failed one never reached would stay
	// stale for good.
	site.failAfter = 0
	reloaded := newTestStore(t, directory, site)
	if got := reloaded.Status().Total; got != total {
		t.Fatalf("the failed rebuild should have kept the albums on disk, got %d of %d", got, total)
	}
	if !reloaded.index.Partial {
		t.Fatalf("the interrupted walk should have marked the index partial")
	}
	if err := reloaded.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Status().Total; got != total {
		t.Fatalf("the recovery walk should rebuild everything, got %d of %d", got, total)
	}
	if reloaded.index.Partial {
		t.Fatalf("a finished walk must clear the partial mark")
	}
}

// TestPartialIndexIsRebuiltWhole covers a process that was killed while a walk
// was publishing: the index on disk says "I am part of a list". Topping it up by
// asking only for newer posts would leave every album the walk never reached
// missing forever, so the next refresh has to walk the site again.
func TestPartialIndexIsRebuiltWhole(t *testing.T) {
	site := sampleSite(t)
	directory := t.TempDir()
	store := newTestStore(t, directory, site)
	if err := store.publish(
		map[int]Album{1: {ID: 1, Slug: "old", Title: "Old"}},
		Index{BuiltAt: time.Now().Add(-time.Hour)},
	); err != nil {
		t.Fatal(err)
	}

	reloaded := newTestStore(t, directory, site)
	if err := reloaded.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Status().Total; got != 2 {
		t.Fatalf("a partial index must be rebuilt whole, got %d albums", got)
	}
	if reloaded.index.Partial {
		t.Fatalf("a finished walk must clear the partial mark")
	}
}

// TestVerifyRecordsActualCounts covers the measurement the browser asks for on a
// site whose titles overstate the gallery: the walk only knows what the title
// claims, and reading the post bodies is the only way to tell a complete album
// from a truncated one.
func TestVerifyRecordsActualCounts(t *testing.T) {
	site := contentSite(t)
	store := newTestStore(t, t.TempDir(), site)
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	albums, _, err := store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 2 {
		t.Fatalf("expected two albums, got %d", len(albums))
	}
	for _, album := range albums {
		if album.Actual != 0 {
			t.Fatalf("a walk cannot know the real counts yet: %+v", album)
		}
	}

	counts, err := store.Verify(context.Background(), []int{300, 400, 999})
	if err != nil {
		t.Fatal(err)
	}
	if counts[300] != 2 || counts[400] != 1 {
		t.Fatalf("unexpected measured counts: %v", counts)
	}
	if _, ok := counts[999]; ok {
		t.Fatalf("an album the site did not answer must not be measured: %v", counts)
	}

	albums, _, err = store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if albums[0].Actual != 1 || albums[1].Actual != 2 {
		t.Fatalf("the measured counts did not reach the cache: %+v", albums)
	}
	if albums[1].Photos != 9 {
		t.Fatalf("measuring must not overwrite the advertised count: %+v", albums[1])
	}

	// A walk rebuilds every album from the list view, so the numbers have to
	// survive it, or the browser would measure the same page on every visit.
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	albums, _, err = store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if albums[0].Actual != 1 || albums[1].Actual != 2 {
		t.Fatalf("a walk dropped the measured counts: %+v", albums)
	}

	// The numbers belong to one album revision: once the title changes, the old
	// measurement says nothing about the new gallery.
	site.posts[1].Title = rendered{Rendered: "Delta cosplay 7 photos"}
	site.posts[1].Modified = "2026-10-04T10:00:00"
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	albums, _, err = store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if albums[0].Actual != 0 {
		t.Fatalf("a changed album must be measured again: %+v", albums[0])
	}
	if albums[1].Actual != 2 {
		t.Fatalf("an unchanged album keeps its measurement: %+v", albums[1])
	}
}

// TestRefreshWalksPastAPageWithNothingToImport guards a walk over a site whose
// listing mixes in entries that cannot be imported. Stopping at such a page would
// silently truncate the index to whatever came before it.
func TestRefreshWalksPastAPageWithNothingToImport(t *testing.T) {
	entry := func(id int, badge string) string {
		return fmt.Sprintf(`<li><a href="/cos/%d.html" class="thumb"><img src="https://cdn.example/%d.webp" /></a>`+
			`<span class="title">Gallery %d</span><span class="time">2026-10-01</span>`+
			`<span class="pagenum">%s</span></li>`, id, id, id, badge)
	}
	page := func(entries ...string) string {
		return `<ul>` + strings.Join(entries, "") +
			`</ul><div class="pager"><a href="/cos/index-3.html">尾页</a></div>`
	}
	pages := map[string]string{
		"/cos/":             page(entry(11, "3P"), entry(12, "4P")),
		"/cos/index-2.html": page(entry(21, "00:07"), entry(22, "00:12")),
		"/cos/index-3.html": page(entry(31, "5P")),
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, ok := pages[request.URL.Path]
		if !ok {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(response, body)
	}))
	defer server.Close()

	source := legacy.NewAcgmhnSource(server.URL, server.Client())
	source.Delay = 0
	store, err := NewStore(t.TempDir(), source, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// Two galleries before the video page, one after it.
	if got := store.Status().Total; got != 3 {
		t.Fatalf("a page with nothing to import must not end the walk, got %d albums", got)
	}
}
