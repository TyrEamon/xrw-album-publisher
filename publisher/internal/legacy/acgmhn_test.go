package legacy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// acgmhnListingEntry renders one gallery the way the site's 写真 listing does.
// badge is the pagenum slot, which holds "23P" for a gallery and a duration such
// as "00:07" for a video.
func acgmhnListingEntry(id int, title, date, badge, category string) string {
	return fmt.Sprintf(`<li>
  <a href="/cos/%d.html" title="%s" class="thumb"><img width="222" height="282" alt="%s" src="https://m.acgnfl.com/re/26/10/c7/%d_thumb_500_425_x.webp" /></a>
  <span class="title"><a href="/cos/%d.html">%s</a></span>
  <span class="time">%s</span>
  <span class="pagenum">%s</span>
  <span class="category">%s</span>
</li>`, id, title, title, id, id, title, date, badge, category)
}

// acgmhnListingPage renders a listing page with a pager that names the last page,
// which is the only place the site publishes that number.
func acgmhnListingPage(lastPage int, entries ...string) string {
	return `<!DOCTYPE html><html><body><ul class="list">` +
		strings.Join(entries, "\n") +
		fmt.Sprintf(`</ul><div class="pager"><a href="/cos/index-%d.html">尾页</a></div></body></html>`, lastPage)
}

func acgmhnServer(t *testing.T, listing string, pages map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/ajax_cos/") {
			body, ok := pages[request.URL.Path]
			if !ok {
				// Past the last page the site still answers, with an empty picture.
				response.Header().Set("Content-Type", "text/html")
				fmt.Fprint(response, `{"pic":"","pager":"","canajax":[1,0]}`)
				return
			}
			response.Header().Set("Content-Type", "text/html")
			fmt.Fprint(response, body)
			return
		}
		response.Header().Set("Content-Type", "text/html")
		fmt.Fprint(response, listing)
	}))
	t.Cleanup(server.Close)
	return server
}

// acgmhnAjaxPage builds one page of the lightweight gallery endpoint. canajax is
// [previous, next], and next drops to zero on the last page, which is what ends
// the walk.
func acgmhnAjaxPage(picture string, previous, next int) string {
	pager := ""
	if next > 1 {
		pager = fmt.Sprintf(`<a href="/cos/884182-%d.html">下一页</a>`, next)
	}
	return fmt.Sprintf(`{"pic":"\u003cimg src=\"%s\" class=\"lazy\" /\u003e","pager":"%s","canajax":[%d,%d]}`,
		picture, pager, previous, next)
}

func newAcgmhnSource(t *testing.T, baseURL string) *AcgmhnSource {
	t.Helper()
	source := NewAcgmhnSource(baseURL, &http.Client{})
	// Pace nothing: the real source waits a second between pages to stay under
	// the site's throttle, which would make every test here take minutes.
	source.Delay = 0
	return source
}

func TestAcgmhnPostsReadsTheListing(t *testing.T) {
	listing := acgmhnListingPage(1005,
		acgmhnListingEntry(884182, "DJAWA Photo: Doodoong", "2026-10-02", "23P", "图片"),
		acgmhnListingEntry(884180, "Cosplayer cheese cubes", "2026-10-01", "46P", "图片"),
		acgmhnListingEntry(884179, "Some clip", "2026-09-30", "00:07", "视频"),
	)
	server := acgmhnServer(t, listing, nil)
	source := newAcgmhnSource(t, server.URL)

	posts, info, err := source.Posts(context.Background(), WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The video entry carries a duration instead of a photo count, so it has
	// nothing to import and must not appear as an album.
	if len(posts) != 2 {
		t.Fatalf("expected the two photo galleries, got %d", len(posts))
	}
	first := posts[0]
	if first.ID != 884182 || first.Photos != 23 {
		t.Fatalf("unexpected first gallery: %+v", first)
	}
	if first.Title != "DJAWA Photo: Doodoong" {
		t.Fatalf("title should arrive without markup: %q", first.Title)
	}
	if first.Date != "2026-10-02" {
		t.Fatalf("unexpected date: %q", first.Date)
	}
	if want := server.URL + "/cos/884182.html"; first.Link != want {
		t.Fatalf("unexpected link: %q", first.Link)
	}
	if info.TotalPages != 1005 {
		t.Fatalf("the pager should name the last page: %d", info.TotalPages)
	}
}

func TestAcgmhnCoversAnswerFromTheListing(t *testing.T) {
	listing := acgmhnListingPage(2, acgmhnListingEntry(884182, "DJAWA", "2026-10-02", "23P", "图片"))
	server := acgmhnServer(t, listing, nil)
	source := newAcgmhnSource(t, server.URL)

	posts, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	covers, err := source.Covers(context.Background(), FeaturedIDs(posts))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://m.acgnfl.com/re/26/10/c7/884182_thumb_500_425_x.webp"
	if covers[884182] != want {
		t.Fatalf("the listing thumbnail should serve as the cover, got %q", covers[884182])
	}
}

func TestAcgmhnAlbumImagesWalksEveryPage(t *testing.T) {
	pages := map[string]string{
		"/ajax_cos/884182.html":   acgmhnAjaxPage("https://m.acgnfl.com/re/26/10/c7/884182_7_0021.webp", 0, 1),
		"/ajax_cos/884182-2.html": acgmhnAjaxPage("https://m.acgnfl.com/re/26/10/c7/884182_7_00210.webp", 1, 1),
		"/ajax_cos/884182-3.html": acgmhnAjaxPage("https://m.acgnfl.com/re/26/10/c7/884182_7_0029.webp", 1, 0),
	}
	server := acgmhnServer(t, "", pages)
	source := newAcgmhnSource(t, server.URL)

	images, err := source.AlbumImages(context.Background(), 884182)
	if err != nil {
		t.Fatal(err)
	}
	// Page three says there is no next page, so the walk stops after it even
	// though page four would answer too.
	if len(images) != 3 {
		t.Fatalf("expected three images, got %d: %+v", len(images), images)
	}
	for index, image := range images {
		if image.ID != index+1 {
			t.Fatalf("image %d should be numbered %d, got %d", index, index+1, image.ID)
		}
		if image.MimeType != "image/webp" {
			t.Fatalf("image %d should be a webp, got %q", index, image.MimeType)
		}
	}
	if !strings.HasSuffix(images[2].URL, "884182_7_0029.webp") {
		t.Fatalf("the last page image is wrong: %q", images[2].URL)
	}
}

func TestAcgmhnAlbumImagesStopsOnAnEmptyPage(t *testing.T) {
	// A gallery whose first page is already empty: the site still answers, with a
	// well-formed but pictureless payload.
	server := acgmhnServer(t, "", nil)
	source := newAcgmhnSource(t, server.URL)

	if _, err := source.AlbumImages(context.Background(), 884182); err == nil {
		t.Fatal("a gallery with no pictures should report an error")
	}
}

func TestAcgmhnPostsStopsWhenNothingIsNew(t *testing.T) {
	listing := acgmhnListingPage(1005,
		acgmhnListingEntry(884182, "DJAWA", "2026-10-02", "23P", "图片"),
		acgmhnListingEntry(884180, "cheese cubes", "2026-10-01", "46P", "图片"),
	)
	server := acgmhnServer(t, listing, nil)
	source := newAcgmhnSource(t, server.URL)

	known := map[int]struct{}{884182: {}, 884180: {}}
	posts, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1, Known: known})
	if err != nil {
		t.Fatal(err)
	}
	// An empty page is how the walk learns to stop, so a periodic refresh reads
	// one page instead of a thousand.
	if len(posts) != 0 {
		t.Fatalf("a page of already indexed galleries should end the walk, got %d", len(posts))
	}

	fresh := map[int]struct{}{884182: {}}
	posts, _, err = source.Posts(context.Background(), WPPostsOptions{Page: 1, Known: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 {
		t.Fatalf("one unindexed gallery should keep the page, got %d", len(posts))
	}
}

func TestAcgmhnSendsBrowserHeaders(t *testing.T) {
	// The edge answers anything that does not look like a browser with a
	// challenge page, so the user agent is load bearing.
	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seen = request.Header.Clone()
		response.Header().Set("Content-Type", "text/html")
		fmt.Fprint(response, acgmhnListingPage(1, acgmhnListingEntry(1, "x", "2026-01-01", "1P", "图片")))
	}))
	defer server.Close()
	source := newAcgmhnSource(t, server.URL)

	if _, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen.Get("User-Agent"), "Mozilla") {
		t.Fatalf("the request should look like a browser, got %q", seen.Get("User-Agent"))
	}
	if seen.Get("Accept") == "" {
		t.Fatal("the request should send an Accept header")
	}
}

func TestAcgmhnRetriesThrottling(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempts++
		if attempts == 1 {
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		response.Header().Set("Content-Type", "text/html")
		fmt.Fprint(response, acgmhnListingPage(1, acgmhnListingEntry(1, "x", "2026-01-01", "1P", "图片")))
	}))
	defer server.Close()
	source := newAcgmhnSource(t, server.URL)

	posts, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected one retry, saw %d attempts", attempts)
	}
	if len(posts) != 1 {
		t.Fatalf("expected the page after the retry, got %d", len(posts))
	}
}

func TestAcgmhnListingPageURL(t *testing.T) {
	source := NewAcgmhnSource("https://www.acgmhn.com/", nil)
	if got := source.listingURL(1); got != "https://www.acgmhn.com/cos/" {
		t.Fatalf("unexpected first page: %q", got)
	}
	if got := source.listingURL(67); got != "https://www.acgmhn.com/cos/index-67.html" {
		t.Fatalf("unexpected later page: %q", got)
	}
}

// TestAcgmhnLive is the only test that touches the real site. It stays skipped
// unless XRW_ACGMHN_LIVE=1, and honours HTTPS_PROXY like the shipped binary does.
// It also confirms the listing's photo count matches what the gallery really
// holds, which is what makes the "不全" badge unnecessary here.
func TestAcgmhnLive(t *testing.T) {
	if os.Getenv("XRW_ACGMHN_LIVE") != "1" {
		t.Skip("set XRW_ACGMHN_LIVE=1 to probe the live site")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	source := NewAcgmhnSource("", nil)

	posts, page, err := source.Posts(ctx, WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) < 20 || page.TotalPages < 100 {
		t.Fatalf("listing page 1 looks wrong: posts=%d page=%+v", len(posts), page)
	}
	t.Logf("listing totalPages=%d first=%q photos=%d date=%s link=%s",
		page.TotalPages, posts[0].Title, posts[0].Photos, posts[0].Date, posts[0].Link)

	covers, err := source.Covers(ctx, FeaturedIDs(posts[:3]))
	if err != nil {
		t.Fatal(err)
	}
	if covers[posts[0].ID] == "" {
		t.Fatal("the listing thumbnail should have become the cover")
	}
	t.Logf("cover=%s", covers[posts[0].ID])

	// Pick a small gallery so a full read stays quick even at one request a
	// second.
	target := posts[0]
	for _, post := range posts {
		if post.Photos > 0 && post.Photos <= 14 {
			target = post
			break
		}
	}
	images, err := source.AlbumImages(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != target.Photos {
		t.Fatalf("gallery %d advertises %d photos but yielded %d", target.ID, target.Photos, len(images))
	}
	t.Logf("gallery %d yielded %d images, first=%s last=%s",
		target.ID, len(images), images[0].URL, images[len(images)-1].URL)

	// A page deep into the listing exercises the index-N.html naming.
	_, deep, err := source.Posts(ctx, WPPostsOptions{Page: 67})
	if err != nil {
		t.Fatal(err)
	}
	if deep.TotalPages != page.TotalPages {
		t.Fatalf("page 67 reports a different site size: %d vs %d", deep.TotalPages, page.TotalPages)
	}

	// The walk reads page after page, so a page inside the listing must never look
	// like the end of it.
	for _, number := range []int{2, 3} {
		more, info, err := source.Posts(ctx, WPPostsOptions{Page: number})
		if err != nil {
			t.Fatalf("page %d: %v", number, err)
		}
		t.Logf("page %d: galleries=%d totalPages=%d more=%v", number, len(more), info.TotalPages, info.More)
		if len(more) == 0 && !info.More {
			t.Fatalf("page %d ended the listing", number)
		}
	}
}

// acgmhnPagedServer serves a different listing per path, which a walk that spans a
// run of pages with nothing importable needs.
func acgmhnPagedServer(t *testing.T, pages map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, ok := pages[request.URL.Path]
		if !ok {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(response, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestAcgmhnEmptyPageKeepsTheWalkGoing pins the rule that a page holding nothing
// importable is not the end of the listing: the 写真 listing also carries animated
// entries, and the site posts them in runs.
func TestAcgmhnEmptyPageKeepsTheWalkGoing(t *testing.T) {
	first := acgmhnListingPage(3,
		acgmhnListingEntry(11, "Alpha", "2026-10-02", "3P", "图片"),
		acgmhnListingEntry(12, "Beta", "2026-10-01", "4P", "图片"),
	)
	videos := acgmhnListingPage(3,
		acgmhnListingEntry(21, "Clip", "2026-09-30", "00:07", "视频"),
		acgmhnListingEntry(22, "Clip two", "2026-09-29", "00:12", "视频"),
	)
	last := acgmhnListingPage(3, acgmhnListingEntry(31, "Gamma", "2026-09-01", "5P", "图片"))
	server := acgmhnPagedServer(t, map[string]string{
		"/cos/":             first,
		"/cos/index-2.html": videos,
		"/cos/index-3.html": last,
		"/cos/index-4.html": acgmhnListingPage(3),
	})
	source := newAcgmhnSource(t, server.URL)

	if _, info, err := source.Posts(context.Background(), WPPostsOptions{Page: 2}); err != nil {
		t.Fatal(err)
	} else if !info.More {
		t.Fatal("a page of videos inside the listing has to keep the walk going")
	}

	if _, info, err := source.Posts(context.Background(), WPPostsOptions{Page: 4}); err != nil {
		t.Fatal(err)
	} else if info.More {
		t.Fatal("a page past the last one must report the end, not ask for more")
	}
}

// TestAcgmhnCatchUpStopsTheWalk pins the other half of the rule: once every
// gallery on the page is already indexed, the newest-first listing has nothing
// left to give, even though the page is inside the listing.
func TestAcgmhnCatchUpStopsTheWalk(t *testing.T) {
	listing := acgmhnListingPage(1005,
		acgmhnListingEntry(11, "Alpha", "2026-10-02", "3P", "图片"),
		acgmhnListingEntry(12, "Beta", "2026-10-01", "4P", "图片"),
	)
	server := acgmhnServer(t, listing, nil)
	source := newAcgmhnSource(t, server.URL)

	known := map[int]struct{}{11: {}, 12: {}}
	posts, info, err := source.Posts(context.Background(), WPPostsOptions{Page: 1, Known: known})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 0 || info.More {
		t.Fatalf("a page that is fully indexed ends the walk: posts=%d more=%v", len(posts), info.More)
	}

	posts, info, err = source.Posts(context.Background(), WPPostsOptions{Page: 1, Known: map[int]struct{}{11: {}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 2 || info.More {
		t.Fatalf("one new gallery keeps the page: posts=%d more=%v", len(posts), info.More)
	}
}
