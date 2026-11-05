package legacy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mitakuListingEntry renders one listing entry the way the theme does: the
// article carries the WordPress id, the title holds the permalink, and the
// taxonomies are link lists inside spans.
func mitakuListingEntry(id int, title, link, date, updated string, categories, tags []string) string {
	categoryLinks := ""
	for _, name := range categories {
		categoryLinks += fmt.Sprintf(`<a href="https://mitaku.net/category/%s/" rel="category tag">%s</a>&nbsp;`,
			strings.ToLower(strings.ReplaceAll(name, " ", "-")), name)
	}
	tagLinks := ""
	for _, name := range tags {
		tagLinks += fmt.Sprintf(`<a href="https://mitaku.net/tag/%s/" rel="tag">%s</a>, `,
			strings.ToLower(strings.ReplaceAll(name, " ", "-")), name)
	}
	updatedSlot := ""
	if updated != "" {
		updatedSlot = fmt.Sprintf(`<time class="updated" datetime="%s">%s</time>`, updated, updated)
	}
	return fmt.Sprintf(`<article id="post-%d" class="post-%d post type-post status-publish hentry">
  <div class="featured-image">
    <a href="%s" title="%s"><img width="800" height="400" src="https://mitaku.net/wp-content/uploads/2026/11/cover-%d.jpg" class="attachment-colormag-featured-image" srcset="https://mitaku.net/wp-content/uploads/2026/11/cover-%d-300x150.jpg 300w" /></a>
  </div>
  <div class="article-content clearfix">
    <div class="above-entry-meta"><span class="cat-links">%s</span></div>
    <header class="entry-header"><h2 class="entry-title"><a href="%s" title="%s">%s</a></h2></header>
    <div class="below-entry-meta">
      <span class="posted-on"><a href="%s" rel="bookmark"><time class="entry-date published" datetime="%s">%s</time>%s</a></span>
      <span class="tag-links"><i class="fa fa-tags"></i>%s</span>
    </div>
  </div>
</article>`, id, id, link, title, id, id, categoryLinks, link, title, title, link, date, date, updatedSlot, tagLinks)
}

// mitakuListingPage wraps entries the way the archive template does, with the
// pager that names the section's last page.
func mitakuListingPage(lastPage int, entries ...string) string {
	return `<!DOCTYPE html><html><body>` + strings.Join(entries, "\n") +
		fmt.Sprintf(`<div class='wp-pagenavi' role='navigation'>
  <span class='pages'>Page 1 of %d</span>
  <a class="last" aria-label="Last page" href="https://mitaku.net/category/ero-cosplay/page/%d/">Last</a>
</div></body></html>`, lastPage, lastPage)
}

// mitakuGalleryPage renders a post page: the lightbox slider plus the metadata
// paragraphs the site writes under it.
func mitakuGalleryPage(links ...string) string {
	slides := ""
	for index, link := range links {
		slides += fmt.Sprintf(`<div class="msacwl-slide" data-item-index="%d">
  <div class="msacwl-img-wrap">
    <a class="msacwl-img-link" href="javascript:void(0);" data-mfp-src="%s"></a>
    <img decoding="async" class="msacwl-img" data-lazy="%s" src="https://mitaku.net/wp-content/plugins/meta-slider-and-carousel-with-lightbox-pro/assets/images/spacer.gif" alt="spacer" />
  </div>
  <div class="msacwl-gallery-container"><div class="msacwl-image-title">Title (%d)</div></div>
</div>`, index+1, link, link, index+1)
	}
	return `<html><body><div class="entry-content clearfix">
<div class="msacwl-slider-wrap msacwl-design-1"><div id="msacwl-slider-1" class="msacwl-slider">` + slides + `</div></div>
<p style="text-align: center;">Cosplayer: Tokar 浵卡</p>
<p style="text-align: center;">Image: 3 Pics (Nude)</p>
<p style="text-align: center;">Ouo: <a href="https://ouo.io/abc">Download Mediafire</a></p>
</div></body></html>`
}

// mitakuServer serves the section listings and the post pages a test needs. The
// requests it answered are kept so a test can prove what was (not) fetched.
func mitakuServer(t *testing.T, pages map[string]string, posts map[int]string) (*httptest.Server, *[]string) {
	t.Helper()
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requested = append(requested, request.URL.RequestURI())
		if id := request.URL.Query().Get("p"); id != "" {
			body, ok := posts[atoiOrZero(id)]
			if !ok {
				response.WriteHeader(http.StatusNotFound)
				return
			}
			fmt.Fprint(response, body)
			return
		}
		body, ok := pages[request.URL.Path]
		if !ok {
			response.WriteHeader(http.StatusNotFound)
			return
		}
		// The site writes these pages as text/html with a charset the reader
		// does not care about; the body is all that matters here.
		response.Header().Set("Content-Type", "text/html; charset=UTF-8")
		fmt.Fprint(response, body)
	}))
	t.Cleanup(server.Close)
	return server, &requested
}

func atoiOrZero(value string) int {
	number := 0
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0
		}
		number = number*10 + int(digit-'0')
	}
	return number
}

func newMitakuSource(t *testing.T, baseURL string) *MitakuSource {
	t.Helper()
	source := NewMitakuSource(baseURL, &http.Client{})
	// The real reader keeps half a second between pages; a test would spend its
	// whole budget waiting.
	source.Delay = 0
	return source
}

// mitakuTwoSections is the listing layout every test starts from: two pages of
// ero-cosplay and one of sexy-set, which makes page three the first sexy-set page.
func mitakuTwoSections(extra map[string]string) map[string]string {
	pages := map[string]string{
		"/category/ero-cosplay/": mitakuListingPage(2,
			mitakuListingEntry(369128, "Tokar 浵卡 &#8211; Kisaki (Blue Archive)",
				"https://mitaku.net/ero-cosplay/tokar-kisaki-blue-archive/",
				"2026-11-01T14:30:00-03:00", "2026-10-29T03:08:44-03:00",
				[]string{"Ero Cosplay", "Nude", "Online Video"},
				[]string{"Blue Archive", "Kisaki", "Tokar 浵卡"}),
			mitakuListingEntry(368847, "SushiBunniii - Citlali (Video)",
				"https://mitaku.net/ero-cosplay/sushibunniii-citlali/",
				"2026-11-01T14:30:00-03:00", "",
				[]string{"Ero Cosplay", "Nude", "Online Video"},
				[]string{"Citlali", "Genshin Impact", "SushiBunniii"}),
		),
		"/category/ero-cosplay/page/2/": mitakuListingPage(2,
			mitakuListingEntry(316, "Old Gallery", "https://mitaku.net/ero-cosplay/old-gallery/",
				"2016-05-01T10:00:00-03:00", "",
				[]string{"Ero Cosplay"}, []string{"Herta"}),
		),
		"/category/sexy-set/": mitakuListingPage(1,
			mitakuListingEntry(107360, "Hana Bunny - Daring Mommy",
				"https://mitaku.net/sexy-set/hana-bunny-daring-mommy/",
				"2026-10-25T10:00:00-03:00", "",
				[]string{"Sexy Set", "Online Video"}, []string{"Hana Bunny"}),
		),
	}
	for path, body := range extra {
		pages[path] = body
	}
	return pages
}

func TestMitakuPostsReadsTheListing(t *testing.T) {
	server, _ := mitakuServer(t, mitakuTwoSections(nil), nil)
	source := newMitakuSource(t, server.URL)

	posts, info, err := source.Posts(context.Background(), WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The second entry is a video post: the site puts a player where the gallery
	// would be, so there is nothing to import and it must not become an album.
	if len(posts) != 1 {
		t.Fatalf("expected only the gallery with pictures, got %d", len(posts))
	}
	post := posts[0]
	if post.ID != 369128 {
		t.Fatalf("the WordPress id should name the gallery: %+v", post)
	}
	if post.Title != "Tokar 浵卡 – Kisaki (Blue Archive)" {
		t.Fatalf("the title should arrive decoded and without padding: %q", post.Title)
	}
	if post.Link != "https://mitaku.net/ero-cosplay/tokar-kisaki-blue-archive/" {
		t.Fatalf("unexpected link: %q", post.Link)
	}
	if post.Slug != "tokar-kisaki-blue-archive" {
		t.Fatalf("the slug should be the permalink's last segment: %q", post.Slug)
	}
	if post.Date != "2026-11-01T14:30:00-03:00" || post.Modified != "2026-10-29T03:08:44-03:00" {
		t.Fatalf("dates should come from the time elements: %q / %q", post.Date, post.Modified)
	}
	if post.FeaturedMedia != post.ID {
		t.Fatalf("the cover is keyed by post id: %+v", post)
	}
	// Two sections of two and one page: the walk has to see the whole site.
	if info.TotalPages != 3 {
		t.Fatalf("the plan should add both sections' pages: %d", info.TotalPages)
	}
	if !info.More {
		t.Fatal("a page inside the plan always has a following page")
	}
}

func TestMitakuPostsWalksBothSections(t *testing.T) {
	server, requested := mitakuServer(t, mitakuTwoSections(nil), nil)
	source := newMitakuSource(t, server.URL)

	// The store walks one counter. Page three is the first page of the second
	// section, which is what makes the last galleries on the site reachable.
	posts, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].ID != 107360 {
		t.Fatalf("page three should be the sexy-set listing, got %+v", posts)
	}
	wanted := "/category/sexy-set/"
	if last := (*requested)[len(*requested)-1]; last != wanted {
		t.Fatalf("expected the sexy-set listing to be read, got %q", last)
	}

	// Page two still belongs to the first section.
	posts, _, err = source.Posts(context.Background(), WPPostsOptions{Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].ID != 316 {
		t.Fatalf("page two should be the second ero-cosplay page, got %+v", posts)
	}
}

func TestMitakuCoversAnswerFromTheListing(t *testing.T) {
	server, _ := mitakuServer(t, mitakuTwoSections(nil), nil)
	source := newMitakuSource(t, server.URL)

	posts, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	covers, err := source.Covers(context.Background(), FeaturedIDs(posts))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://mitaku.net/wp-content/uploads/2026/11/cover-369128.jpg"
	if covers[369128] != want {
		t.Fatalf("the listing thumbnail should serve as the cover, got %q", covers[369128])
	}
}

func TestMitakuTermsCountWhileWalking(t *testing.T) {
	server, _ := mitakuServer(t, mitakuTwoSections(nil), nil)
	source := newMitakuSource(t, server.URL)

	for page := 1; page <= 3; page++ {
		if _, _, err := source.Posts(context.Background(), WPPostsOptions{Page: page}); err != nil {
			t.Fatal(err)
		}
	}
	tags, err := source.Terms(context.Background(), "tags", "count")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, term := range tags {
		names[term.Name] = term.Count
	}
	// The video post is dropped, so its tags must not be counted either.
	if _, counted := names["Citlali"]; counted {
		t.Fatalf("a dropped video post must not contribute tags: %v", names)
	}
	for _, name := range []string{"Blue Archive", "Kisaki", "Tokar 浵卡", "Herta", "Hana Bunny"} {
		if names[name] != 1 {
			t.Fatalf("tag %q should be counted once, got %d", name, names[name])
		}
	}

	categories, err := source.Terms(context.Background(), "categories", "count")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, term := range categories {
		counts[term.Name] = term.Count
	}
	if counts["Ero Cosplay"] != 2 || counts["Sexy Set"] != 1 {
		t.Fatalf("categories should be counted per gallery: %v", counts)
	}
	// The id the browser filters by has to be stable across runs, which is why it
	// is the hash of the name rather than a counter.
	for _, term := range categories {
		if term.ID != mitakuTermID(term.Name) {
			t.Fatalf("term ids must be stable: %+v", term)
		}
		if term.Slug == "" {
			t.Fatalf("term should carry a slug: %+v", term)
		}
	}
}

// TestMitakuTermsRestartWithANewWalk pins the tallies to one walk. The section
// page counts drift, so the reader rebuilds its plan now and then, and a walk
// that restarted would otherwise count every tag and category twice — which is
// how an index ends up claiming more galleries than it actually holds.
func TestMitakuTermsRestartWithANewWalk(t *testing.T) {
	server, _ := mitakuServer(t, mitakuTwoSections(nil), nil)
	source := newMitakuSource(t, server.URL)

	for round := 1; round <= 2; round++ {
		for page := 1; page <= 3; page++ {
			if _, _, err := source.Posts(context.Background(), WPPostsOptions{Page: page}); err != nil {
				t.Fatalf("round %d page %d: %v", round, page, err)
			}
		}
	}

	categories, err := source.Terms(context.Background(), "categories", "count")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, term := range categories {
		counts[term.Name] = term.Count
	}
	if counts["Ero Cosplay"] != 2 || counts["Sexy Set"] != 1 {
		t.Fatalf("a second walk must count the galleries once, not twice: %v", counts)
	}

	tags, err := source.Terms(context.Background(), "tags", "count")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, term := range tags {
		names[term.Name] = term.Count
	}
	if names["Blue Archive"] != 1 || names["Herta"] != 1 {
		t.Fatalf("tags must not double up after a restart: %v", names)
	}
}

// TestMitakuOverlappingPagesCountOnce covers the archive's own overlap: page two
// repeats the last entries of page one, and both the album and its tags must be
// taken from the first sighting only.
func TestMitakuOverlappingPagesCountOnce(t *testing.T) {
	repeated := mitakuListingEntry(369128, "Tokar 浵卡 &#8211; Kisaki (Blue Archive)",
		"https://mitaku.net/ero-cosplay/tokar-kisaki-blue-archive/",
		"2026-11-01T14:30:00-03:00", "",
		[]string{"Ero Cosplay", "Nude"}, []string{"Blue Archive", "Kisaki"})
	pages := mitakuTwoSections(map[string]string{
		"/category/ero-cosplay/page/2/": mitakuListingPage(2,
			repeated,
			mitakuListingEntry(316, "Old Gallery", "https://mitaku.net/ero-cosplay/old-gallery/",
				"2016-05-01T10:00:00-03:00", "",
				[]string{"Ero Cosplay"}, []string{"Herta"}),
		),
	})
	server, _ := mitakuServer(t, pages, nil)
	source := newMitakuSource(t, server.URL)

	first, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("the repeat must not be handed to the store twice: %d then %d", len(first), len(second))
	}
	if second[0].ID != 316 {
		t.Fatalf("the page should keep only the gallery the walk had not seen: %+v", second[0])
	}

	tags, err := source.Terms(context.Background(), "tags", "count")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range tags {
		if term.Name == "Kisaki" && term.Count != 1 {
			t.Fatalf("a repeated entry must not count its tags twice: %+v", term)
		}
	}
	categories, err := source.Terms(context.Background(), "categories", "count")
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range categories {
		if term.Name == "Ero Cosplay" && term.Count != 2 {
			t.Fatalf("two galleries carry the section, got %+v", term)
		}
	}
}

func TestMitakuKnownGalleriesStopTheSection(t *testing.T) {
	server, requested := mitakuServer(t, mitakuTwoSections(nil), nil)
	source := newMitakuSource(t, server.URL)

	known := map[int]struct{}{369128: {}, 316: {}}
	posts, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 1, Known: known})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 0 {
		t.Fatalf("a page of already indexed galleries has nothing new: %+v", posts)
	}
	before := len(*requested)
	// The rest of that section is settled: reading it must cost no request, or a
	// periodic refresh would walk nine hundred pages for nothing.
	if _, _, err := source.Posts(context.Background(), WPPostsOptions{Page: 2, Known: known}); err != nil {
		t.Fatal(err)
	}
	if len(*requested) != before {
		t.Fatalf("a settled section must not be read again, got %v", (*requested)[before:])
	}
}

func TestMitakuAlbumImagesReadsTheSlider(t *testing.T) {
	first := "https://mitaku.net/wp-content/uploads/2026/11/Tokar-浵卡-Kisaki-Blue-Archive-1.jpg"
	second := "https://mitaku.net/wp-content/uploads/2026/11/Tokar-浵卡-Kisaki-Blue-Archive-2.jpg"
	third := "https://mitaku.net/wp-content/uploads/2026/11/Tokar-浵卡-Kisaki-Blue-Archive-3.jpg"
	// The rendered slider clones its first and last slide, so the markup carries
	// more anchors than the gallery has pictures.
	page := mitakuGalleryPage(third, first, second, first, third)
	server, requested := mitakuServer(t, nil, map[int]string{369128: page})
	source := newMitakuSource(t, server.URL)

	images, err := source.AlbumImages(context.Background(), 369128)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 3 {
		t.Fatalf("duplicates must be dropped, got %d images", len(images))
	}
	// The filenames carry the gallery's own order, which is what the pictures are
	// sorted on.
	for index, want := range []string{first, second, third} {
		if images[index].URL != want {
			t.Fatalf("image %d should be %q, got %q", index, want, images[index].URL)
		}
		if images[index].ID != index+1 {
			t.Fatalf("images should be numbered in order: %+v", images[index])
		}
		if images[index].MimeType != "image/jpeg" {
			t.Fatalf("unexpected mime type: %+v", images[index])
		}
	}
	// A gallery is read from its id alone, so the permalink never has to be
	// remembered between a rebuild and a click.
	if got := (*requested)[0]; got != "/?p=369128" {
		t.Fatalf("expected the gallery to be read through ?p=, got %q", got)
	}
}

func TestMitakuAlbumImagesRejectsAVideoPost(t *testing.T) {
	// A video post has the same template but no slider: only the player and the
	// theme's spacer image are there.
	page := `<html><body><div class="entry-content"><iframe src="https://example.invalid/e/x"></iframe>
<img src="https://mitaku.net/wp-content/plugins/meta-slider-and-carousel-with-lightbox-pro/assets/images/spacer.gif" /></div></body></html>`
	server, _ := mitakuServer(t, nil, map[int]string{368847: page})
	source := newMitakuSource(t, server.URL)

	if _, err := source.AlbumImages(context.Background(), 368847); err == nil {
		t.Fatal("a post with no pictures must report that instead of an empty gallery")
	}
}

func TestMitakuAlbumImagesReadsAPlainPostBody(t *testing.T) {
	// The oldest posts put the pictures straight into the body.
	page := `<html><body><div class="entry-content">
<p><img src="https://mitaku.net/wp-content/uploads/2016/05/old-gallery-2.jpg" /></p>
<p><img src="https://mitaku.net/wp-content/uploads/2016/05/old-gallery-1.jpg" /></p>
</div></body></html>`
	server, _ := mitakuServer(t, nil, map[int]string{316: page})
	source := newMitakuSource(t, server.URL)

	images, err := source.AlbumImages(context.Background(), 316)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || !strings.HasSuffix(images[0].URL, "old-gallery-1.jpg") {
		t.Fatalf("a plain body should still yield an ordered gallery: %+v", images)
	}
}

func TestMitakuTotalPages(t *testing.T) {
	for name, testCase := range map[string]struct {
		body string
		want int
	}{
		"pager": {
			body: `<div class='wp-pagenavi'><span class='pages'>Page 2 of 842</span></div>`,
			want: 842,
		},
		"last link": {
			body: `<div class='wp-pagenavi'><a class='last' aria-label='Last page' href='https://mitaku.net/category/sexy-set/page/69/'>Last</a></div>`,
			want: 69,
		},
		"first page only": {
			body: `<div class='wp-pagenavi'><span class='pages'>Page 1 of 1</span></div>`,
			want: 1,
		},
		"no pager": {body: `<html><body>nothing</body></html>`, want: 0},
	} {
		if got := mitakuTotalPages(testCase.body); got != testCase.want {
			t.Fatalf("%s: expected %d pages, got %d", name, testCase.want, got)
		}
	}
}

func TestMitakuPlanLocatesPages(t *testing.T) {
	plan := mitakuPlan{
		Sections: []mitakuSection{{Slug: "ero-cosplay", Pages: 842}, {Slug: "sexy-set", Pages: 69}},
		Total:    911,
	}
	for _, testCase := range []struct{ page, section, local int }{
		{1, 0, 1}, {842, 0, 842}, {843, 1, 1}, {911, 1, 69},
	} {
		section, local := plan.locate(testCase.page)
		if section != testCase.section || local != testCase.local {
			t.Fatalf("page %d should be section %d page %d, got %d/%d",
				testCase.page, testCase.section, testCase.local, section, local)
		}
	}
	if section, _ := plan.locate(912); section != -1 {
		t.Fatalf("a page past the plan should be out of range, got section %d", section)
	}
}
