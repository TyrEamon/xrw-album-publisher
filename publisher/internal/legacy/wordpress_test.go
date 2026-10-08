package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParsePostTitle(t *testing.T) {
	cases := []struct {
		rendered string
		title    string
		photos   int
		videos   int
	}{
		{
			rendered: `Byoru (ビョル) cosplay Hsin &#8211; Wuthering Waves &#8220;80 photos and 33 videos&#8221;`,
			title:    `Byoru (ビョル) cosplay Hsin – Wuthering Waves`,
			photos:   80, videos: 33,
		},
		{
			rendered: `ChuChu Magic cosplay Kuromi Serika Swimsuit - Blue Archive &#8220;173 photos&#8221;`,
			title:    `ChuChu Magic cosplay Kuromi Serika Swimsuit - Blue Archive`,
			photos:   173, videos: 0,
		},
		{
			rendered: `流年不停_w (Liunian bu ting) cosplay Unicorn Pajamas - Azur Lane &#8220;47 photos and 1 video&#8221;`,
			title:    `流年不停_w (Liunian bu ting) cosplay Unicorn Pajamas - Azur Lane`,
			photos:   47, videos: 1,
		},
		{
			rendered: `嫩小兔 (nenxiaotu) cosplay Jingliu - Honkai:Star Rail &#8220;17 photos and 1 video&#8221;`,
			title:    `嫩小兔 (nenxiaotu) cosplay Jingliu - Honkai:Star Rail`,
			photos:   17, videos: 1,
		},
		{
			rendered: `Some album without a counter`,
			title:    `Some album without a counter`,
			photos:   0, videos: 0,
		},
		{
			rendered: ``,
			title:    ``,
			photos:   0, videos: 0,
		},
	}
	for _, item := range cases {
		title, photos, videos := ParsePostTitle(item.rendered)
		if title != item.title || photos != item.photos || videos != item.videos {
			t.Errorf("ParsePostTitle(%q) = (%q, %d, %d), want (%q, %d, %d)",
				item.rendered, title, photos, videos, item.title, item.photos, item.videos)
		}
	}
}

func TestParsePostMeta(t *testing.T) {
	content := `<blockquote>
<p class="gt-block; color:black;"><strong>Cosplayer: <a href="https://cosplaytele.com/category/byoru/" target="_blank" rel="noopener">Byoru (ビョル)</a></strong><br />
<strong>Character: <a href="https://cosplaytele.com/tag/hsin/" target="_blank" rel="noopener">Hsin</a></strong><br />
<strong>Appear In: <a href="https://cosplaytele.com/tag/wuthering-waves/" target="_blank" rel="noopener">Wuthering Waves</a></strong></p>
<p class="gt-block; color:black;"><strong>Photos: 80 photos and 33 videos</strong><br />
<strong>File Size: </strong><br />
<strong>Unzip Password:<input readonly="readonly" size="11" type="text" value="cosplaytele" /></strong></p>
</blockquote>
<p>Enjoy better photo with large size</p>`
	meta := ParsePostMeta(content)
	if meta.Cosplayer != "Byoru (ビョル)" {
		t.Errorf("cosplayer = %q", meta.Cosplayer)
	}
	if meta.Character != "Hsin" {
		t.Errorf("character = %q", meta.Character)
	}
	if meta.AppearIn != "Wuthering Waves" {
		t.Errorf("appear in = %q", meta.AppearIn)
	}
	if meta.UnzipPassword != "cosplaytele" {
		t.Errorf("unzip = %q", meta.UnzipPassword)
	}
	if meta.Photos != 80 || meta.Videos != 33 {
		t.Errorf("photos=%d videos=%d", meta.Photos, meta.Videos)
	}
	// The trailing prose must not leak into the counts.
	if strings.Contains(meta.AppearIn, "Enjoy") {
		t.Errorf("appear in leaked body text: %q", meta.AppearIn)
	}
}

func TestAlbumFromMediaKeepsCoverInsideGallery(t *testing.T) {
	// Shape A, taken from post 527293: 43 attachments, the title claims 43 photos,
	// and the featured image is one of them.
	items := make([]WPMedia, 0, 43)
	for index := 1; index <= 43; index++ {
		items = append(items, WPMedia{ID: 527870 + index, Slug: "album-" + strconv.Itoa(index) + "_result", MimeType: "image/webp"})
	}
	got := AlbumFromMedia(items, 527890, 43)
	if len(got) != 43 {
		t.Fatalf("shape A kept %d images, want 43", len(got))
	}
	if got[19].ID != 527890 {
		t.Fatalf("shape A dropped the in-gallery cover, got %v", ids(got))
	}
}

func TestAlbumFromMediaDropsSurplusCover(t *testing.T) {
	// Shape B, taken from post 527286: 81 attachments, the title claims 80 photos,
	// and the featured image is the surplus attachment.
	items := make([]WPMedia, 0, 81)
	for index := 1; index <= 81; index++ {
		items = append(items, WPMedia{ID: 527294 + index, Slug: "album-" + strconv.Itoa(index) + "_result", MimeType: "image/webp"})
	}
	got := AlbumFromMedia(items, 527375, 80)
	if len(got) != 80 {
		t.Fatalf("shape B kept %d images, want 80", len(got))
	}
	for _, item := range got {
		if item.ID == 527375 {
			t.Fatalf("shape B kept the surplus cover: %v", ids(got))
		}
	}
}

func TestAlbumFromMediaDropsStrayAttachment(t *testing.T) {
	// Post 526721 really carries a 25th attachment named after a different
	// gallery (nenxiaotu-cosplay-jingliu-...-24_result) alongside its own 24.
	items := make([]WPMedia, 0, 25)
	for index := 1; index <= 23; index++ {
		items = append(items, WPMedia{
			ID: 526827 + index, MimeType: "image/webp",
			Slug: fmt.Sprintf("unknown-cosplayer-cosplay-alya-masturbation-alya-sometimes-hides-her-feelings-in-russian-%d_result", index),
		})
	}
	items = append(items, WPMedia{ID: 526871, MimeType: "image/webp", Slug: "nenxiaotu-cosplay-jingliu-honkaistar-rail-24_result"})
	items = append(items, WPMedia{
		ID: 526872, MimeType: "image/webp",
		Slug: "unknown-cosplayer-cosplay-alya-masturbation-alya-sometimes-hides-her-feelings-in-russian-24_result",
	})

	got := AlbumFromMedia(items, 526872, 23)
	if len(got) != 23 {
		t.Fatalf("kept %d images, want the album's own 23", len(got))
	}
	for _, item := range got {
		if item.ID == 526871 || item.ID == 526872 {
			t.Fatalf("stray attachment or cover survived: %v", ids(got))
		}
	}
}

func TestAlbumFromMediaKeepsInconsistentlyNamedGallery(t *testing.T) {
	// Post 527291 spreads 63 attachments over three different stems, so no stem
	// holds a majority. The gallery must survive intact and only lose its cover.
	items := []WPMedia{
		{ID: 1, MimeType: "image/webp", Slug: "riria-arcana-1_result"},
		{ID: 2, MimeType: "image/webp", Slug: "riria-blanc-2_result"},
		{ID: 3, MimeType: "image/webp", Slug: "riria-red-hood-3_result"},
		{ID: 4, MimeType: "image/webp", Slug: "riria-arcana-4_result"},
	}
	got := AlbumFromMedia(items, 4, 3)
	if len(got) != 3 {
		t.Fatalf("kept %d images, want 3", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Fatalf("unexpected images %v", ids(got))
	}
}

func TestAlbumFromMediaFiltersNonImagesAndUnclaimedPosts(t *testing.T) {
	mixed := []WPMedia{
		{ID: 1, Slug: "album-1_result", MimeType: "image/webp"},
		{ID: 2, Slug: "album-2_result", MimeType: "video/mp4"},
		{ID: 3, Slug: "album-3_result", MimeType: "image/webp"},
	}
	if got := AlbumFromMedia(mixed, 0, 2); len(got) != 2 || got[1].ID != 3 {
		t.Fatalf("non-image attachments must be skipped, got %v", ids(got))
	}
	// A post whose title carries no photo count keeps every image.
	if got := AlbumFromMedia(mixed, 1, 0); len(got) != 2 {
		t.Fatalf("claim=0 kept %d images, want 2", len(got))
	}
	// A surplus list whose cover is not in it must not lose a real photo.
	items := []WPMedia{{ID: 1, MimeType: "image/webp"}, {ID: 2, MimeType: "image/webp"}}
	if got := AlbumFromMedia(items, 999, 1); len(got) != 2 {
		t.Fatalf("absent cover dropped a real photo, got %v", ids(got))
	}
	// A wildly miscounted post keeps the full attachment list.
	if got := AlbumFromMedia(items, 2, 5); len(got) != 2 {
		t.Fatalf("miscounted post kept %d images, want 2", len(got))
	}
}

func ids(items []WPMedia) []int {
	result := make([]int, len(items))
	for index, item := range items {
		result[index] = item.ID
	}
	return result
}

func TestSourceAlbumFromPost(t *testing.T) {
	album := SourceAlbumFromPost(WPPost{ID: 527286, Title: "Byoru"}, "https://example.test/cover.webp")
	if album.ID != "wp-527286" {
		t.Errorf("id = %q", album.ID)
	}
	if album.Ordinal != wordpressOrdinalBase+527286 {
		t.Errorf("ordinal = %d", album.Ordinal)
	}
	if album.Source != SourceCosplaytele {
		t.Errorf("source = %q", album.Source)
	}
	if album.Cover != "https://example.test/cover.webp" {
		t.Errorf("cover = %q", album.Cover)
	}
	// The linuxdo-85w migration owns 0..14972, so WordPress ordinals must clear it.
	if album.Ordinal <= 14972 {
		t.Errorf("ordinal %d collides with the linuxdo-85w range", album.Ordinal)
	}
}

type fakeSite struct {
	server  *httptest.Server
	posts   map[int][]map[string]any
	media   map[int][]map[string]any
	total   int
	pages   int
	failing *atomic.Int32
}

func newFakeSite() *fakeSite {
	site := &fakeSite{posts: map[int][]map[string]any{}, media: map[int][]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/wp-json/wp/v2/posts", func(writer http.ResponseWriter, request *http.Request) {
		if site.failing != nil && site.failing.Add(-1) >= 0 {
			http.Error(writer, `{"code":"server_error"}`, http.StatusInternalServerError)
			return
		}
		page, _ := strconv.Atoi(request.URL.Query().Get("page"))
		writer.Header().Set("X-WP-Total", strconv.Itoa(site.total))
		writer.Header().Set("X-WP-TotalPages", strconv.Itoa(site.pages))
		writeJSON(writer, site.posts[page])
	})
	mux.HandleFunc("/wp-json/wp/v2/posts/", func(writer http.ResponseWriter, request *http.Request) {
		id, _ := strconv.Atoi(strings.TrimPrefix(request.URL.Path, "/wp-json/wp/v2/posts/"))
		for _, page := range site.posts {
			for _, post := range page {
				if number, _ := post["id"].(int); number == id {
					writeJSON(writer, post)
					return
				}
			}
		}
		http.Error(writer, `{"code":"rest_post_invalid_id"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/wp-json/wp/v2/media", func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if include := query.Get("include"); include != "" {
			var result []map[string]any
			for _, value := range strings.Split(include, ",") {
				id, _ := strconv.Atoi(value)
				result = append(result, map[string]any{
					"id": id, "source_url": fmt.Sprintf("https://cdn.test/%d.webp", id),
				})
			}
			writeJSON(writer, result)
			return
		}
		parent, _ := strconv.Atoi(query.Get("parent"))
		writer.Header().Set("X-WP-Total", strconv.Itoa(len(site.media[parent])))
		writer.Header().Set("X-WP-TotalPages", "1")
		writeJSON(writer, site.media[parent])
	})
	mux.HandleFunc("/wp-json/wp/v2/categories", func(writer http.ResponseWriter, request *http.Request) {
		var result []map[string]any
		for _, value := range strings.Split(request.URL.Query().Get("include"), ",") {
			id, _ := strconv.Atoi(value)
			if id == 0 {
				continue
			}
			result = append(result, map[string]any{
				"id": id, "slug": fmt.Sprintf("cat-%d", id),
				"name": fmt.Sprintf("Cosplay %d", id), "count": id * 2,
			})
		}
		writeJSON(writer, result)
	})
	site.server = httptest.NewServer(mux)
	return site
}

func writeJSON(writer http.ResponseWriter, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(encoded)
}

func TestWordPressWalkPagesAndCovers(t *testing.T) {
	site := newFakeSite()
	defer site.server.Close()
	site.total, site.pages = 3, 2
	site.posts[1] = []map[string]any{
		{"id": 101, "slug": "a", "title": map[string]any{"rendered": "Alpha &#8220;10 photos&#8221;"}, "featured_media": 9001},
		{"id": 102, "slug": "b", "title": map[string]any{"rendered": "Beta &#8220;20 photos and 2 videos&#8221;"}, "featured_media": 9002},
	}
	site.posts[2] = []map[string]any{
		{"id": 103, "slug": "c", "title": map[string]any{"rendered": "Gamma"}, "featured_media": 0},
	}

	source := NewWordPressSource(site.server.URL, site.server.Client())
	var albums []SourceAlbum
	err := source.Walk(context.Background(), WPWalkOptions{}, func(album SourceAlbum) error {
		albums = append(albums, album)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 3 {
		t.Fatalf("walked %d albums", len(albums))
	}
	if albums[0].ID != "wp-101" || albums[0].Title != "Alpha" || albums[0].Cover != "https://cdn.test/9001.webp" {
		t.Errorf("album[0] = %+v", albums[0])
	}
	if albums[1].Title != "Beta" || albums[1].Source != SourceCosplaytele {
		t.Errorf("album[1] = %+v", albums[1])
	}
	if albums[2].Cover != "" {
		t.Errorf("album[2] cover = %q, want empty for featured_media 0", albums[2].Cover)
	}
	if albums[2].Ordinal != wordpressOrdinalBase+103 {
		t.Errorf("album[2] ordinal = %d", albums[2].Ordinal)
	}
}

func TestWordPressAlbumImagesDropsSurplusCover(t *testing.T) {
	// Shape B from post 527286: the title claims 80 photos, the post carries 81
	// attachments, and the featured image is the surplus one.
	site := newFakeSite()
	defer site.server.Close()
	site.posts[1] = []map[string]any{
		{"id": 527286, "slug": "hsin-2", "title": map[string]any{"rendered": "Byoru cosplay Hsin &#8211; Wuthering Waves &#8220;80 photos and 33 videos&#8221;"}, "featured_media": 527375},
	}
	media := make([]map[string]any, 0, 81)
	for index := 1; index <= 81; index++ {
		media = append(media, map[string]any{
			"id": 527294 + index, "slug": fmt.Sprintf("waves-%d_result", index),
			"source_url": fmt.Sprintf("https://cdn.test/%d.webp", index), "mime_type": "image/webp",
		})
	}
	site.media[527286] = media

	source := NewWordPressSource(site.server.URL, site.server.Client())
	images, err := source.AlbumImages(context.Background(), 527286)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 80 {
		t.Fatalf("got %d images, want 80 after dropping the surplus cover", len(images))
	}
	for index, item := range images {
		want := fmt.Sprintf("https://cdn.test/%d.webp", index+1)
		if item.URL != want {
			t.Fatalf("images[%d] = %q, want %q", index, item.URL, want)
		}
	}
}

func TestWordPressAlbumImagesKeepsInGalleryCover(t *testing.T) {
	// Shape A from post 527293: the title claims 43 photos, the post carries 43
	// attachments, and the featured image is one of them.
	site := newFakeSite()
	defer site.server.Close()
	site.posts[1] = []map[string]any{
		{"id": 527293, "slug": "taihou", "title": map[string]any{"rendered": "Meroko cosplay Taihou &#8220;43 photos&#8221;"}, "featured_media": 527890},
	}
	media := make([]map[string]any, 0, 43)
	for index := 1; index <= 43; index++ {
		media = append(media, map[string]any{
			"id": 527870 + index, "slug": fmt.Sprintf("taihou-%d_result", index),
			"source_url": fmt.Sprintf("https://cdn.test/%d.webp", index), "mime_type": "image/webp",
		})
	}
	site.media[527293] = media

	source := NewWordPressSource(site.server.URL, site.server.Client())
	images, err := source.AlbumImages(context.Background(), 527293)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 43 {
		t.Fatalf("got %d images, want all 43 kept", len(images))
	}
	if images[19].ID != 527890 {
		t.Fatalf("the in-gallery cover must survive, got %v", ids(images))
	}
}

func TestWordPressRetriesServerErrors(t *testing.T) {
	site := newFakeSite()
	defer site.server.Close()
	failing := &atomic.Int32{}
	failing.Store(2)
	site.failing = failing
	site.total, site.pages = 1, 1
	site.posts[1] = []map[string]any{
		{"id": 7, "slug": "x", "title": map[string]any{"rendered": "Retried"}, "featured_media": 0},
	}
	source := NewWordPressSource(site.server.URL, site.server.Client())
	source.Client = &http.Client{Timeout: 10 * time.Second, Transport: site.server.Client().Transport}
	posts, _, err := source.Posts(context.Background(), WPPostsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Title != "Retried" {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestWordPressMetaAndCategories(t *testing.T) {
	site := newFakeSite()
	defer site.server.Close()
	site.posts[1] = []map[string]any{
		{
			"id": 527286, "slug": "hsin-2", "title": map[string]any{"rendered": "Byoru"},
			"featured_media": 527921,
			"content":        map[string]any{"rendered": `<blockquote><strong>Cosplayer: <a href="/x/">Byoru (ビョル)</a></strong><br /><strong>Character: <a href="/y/">Hsin</a></strong><br /><strong>Appear In: <a href="/z/">Wuthering Waves</a></strong><br /><strong>Photos: 80 photos and 33 videos</strong><br /><strong>Unzip Password:<input value="cosplaytele" /></strong></blockquote>`},
		},
	}
	source := NewWordPressSource(site.server.URL, site.server.Client())
	meta, err := source.Meta(context.Background(), 527286)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Cosplayer != "Byoru (ビョル)" || meta.AppearIn != "Wuthering Waves" || meta.Photos != 80 || meta.Videos != 33 {
		t.Fatalf("meta = %+v", meta)
	}
	terms, err := source.CategoryNames(context.Background(), []int{104, 194})
	if err != nil {
		t.Fatal(err)
	}
	if len(terms) != 2 || terms[104].Name != "Cosplay 104" || terms[104].Count != 208 {
		t.Fatalf("terms = %+v", terms)
	}
}

// TestWordPressLive is the only test that touches the real site. It stays skipped
// unless XRW_WP_LIVE=1, and honours HTTPS_PROXY like the shipped binary does.
func TestWordPressLive(t *testing.T) {
	if os.Getenv("XRW_WP_LIVE") != "1" {
		t.Skip("set XRW_WP_LIVE=1 to probe the live site")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	source := NewWordPressSource(os.Getenv("XRW_WP_BASE"), nil)

	posts, page, err := source.Posts(ctx, WPPostsOptions{PerPage: 24, OrderBy: "id", Order: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 24 || page.Total < 5000 {
		t.Fatalf("posts=%d page=%+v", len(posts), page)
	}
	t.Logf("site total=%d pages=%d first=%q photos=%d videos=%d",
		page.Total, page.TotalPages, posts[0].Title, posts[0].Photos, posts[0].Videos)

	target := posts[0]
	if target.Photos == 0 {
		t.Skip("first post has no photo counter")
	}
	images, err := source.AlbumImages(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != target.Photos {
		t.Fatalf("post %d: media returned %d images, title claims %d", target.ID, len(images), target.Photos)
	}
	t.Logf("post %d %q -> %d images, first=%s", target.ID, target.Title, len(images), images[0].URL)

	meta, err := source.Meta(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("meta cosplayer=%q character=%q appearIn=%q unzip=%q", meta.Cosplayer, meta.Character, meta.AppearIn, meta.UnzipPassword)
	if meta.Cosplayer == "" {
		t.Errorf("post %d: no Cosplayer parsed", target.ID)
	}

	covers, err := source.Covers(ctx, []int{posts[0].FeaturedMedia, posts[1].FeaturedMedia})
	if err != nil {
		t.Fatal(err)
	}
	if len(covers) != 2 {
		t.Fatalf("covers = %v", covers)
	}
	t.Logf("covers = %v", covers)

	// Sweep a full page of real galleries: the image set must match the photo
	// count the site prints in the title, for both cover shapes and for albums
	// whose attachment list carries a stray upload from another gallery.
	checked := 0
	for _, post := range posts {
		if post.Photos == 0 {
			continue
		}
		images, err := source.AlbumImages(ctx, post.ID)
		if err != nil {
			t.Errorf("post %d: %v", post.ID, err)
			continue
		}
		checked++
		if len(images) != post.Photos {
			t.Errorf("post %d %q: got %d images, title claims %d",
				post.ID, post.Title, len(images), post.Photos)
			continue
		}
		t.Logf("post %d ok: %d images", post.ID, len(images))
	}
	if checked < 20 {
		t.Fatalf("only %d posts carried a photo counter", checked)
	}
}

// TestImagesFromContentReadsTheGalleryInOrder covers the sites that publish a
// gallery inside the post body: the media endpoint only carries the cover there,
// so the images have to come out of the markup, in order and without the
// decorative or advertising pictures around them.
func TestImagesFromContentReadsTheGalleryInOrder(t *testing.T) {
	rendered := `<p>intro</p>` +
		`<script src="//ads.example.com/banner.js"></script>` +
		`<img class="aligncenter lazy" src="data:image/gif;base64,R0lGODlh" data-src="https://pok.misskon.com/imghost/uploads/2026/10/06/DJAWA-001.QytZ5vQe.webp" />` +
		`<img decoding="async" src="https://wes.misskon.com/images/2026/03/08/Cosplayer-001c72f1.webp" />` +
		`<a href="https://ouo.io/0PL1LiX"><img src="https://1.bp.blogspot.com/-abc/s1600/preview.jpg" /></a>` +
		`<img src="https://pok.misskon.com/imghost/uploads/2026/10/06/DJAWA-001.QytZ5vQe.webp" />` +
		`<img src="https://misskon.com/wp-content/themes/misskon/logo.svg" />` +
		`<img src="/relative/cover.webp" />`

	images := ImagesFromContent(rendered)
	want := []WPMedia{
		{ID: 1, URL: "https://pok.misskon.com/imghost/uploads/2026/10/06/DJAWA-001.QytZ5vQe.webp", MimeType: "image/webp"},
		{ID: 2, URL: "https://wes.misskon.com/images/2026/03/08/Cosplayer-001c72f1.webp", MimeType: "image/webp"},
		{ID: 3, URL: "https://1.bp.blogspot.com/-abc/s1600/preview.jpg", MimeType: "image/jpeg"},
	}
	if len(images) != len(want) {
		t.Fatalf("expected %d body images, got %d: %+v", len(want), len(images), images)
	}
	for index, expected := range want {
		if images[index] != expected {
			t.Errorf("image %d = %+v, want %+v", index, images[index], expected)
		}
	}
	if got := ImagesFromContent(""); len(got) != 0 {
		t.Fatalf("an empty body has no gallery, got %+v", got)
	}
}

// TestAlbumImagesPicksTheConfiguredStrategy pins the per-site switch: a site
// whose gallery lives in the body must not be read through the media endpoint.
func TestAlbumImagesPicksTheConfiguredStrategy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/wp-json/wp/v2/posts/77":
			fmt.Fprint(response, `{"id":77,"title":{"rendered":"Hot cosplay 2 photos"},`+
				`"content":{"rendered":"<img src=\"https://cdn.example.com/a.webp\" /><img src=\"https://cdn.example.com/b.webp\" />"}}`)
		case "/wp-json/wp/v2/media":
			fmt.Fprint(response, `[{"id":7701,"slug":"hot-cover","source_url":"https://cdn.example.com/cover.webp","mime_type":"image/webp"}]`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	source := NewWordPressSource(server.URL, server.Client())
	source.ImageSource = ImageSourceContent
	images, err := source.AlbumImages(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].URL != "https://cdn.example.com/a.webp" {
		t.Fatalf("the content strategy should read the body: %+v", images)
	}

	source.ImageSource = ""
	attachments, err := source.AlbumImages(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if len(attachments) != 1 || attachments[0].URL != "https://cdn.example.com/cover.webp" {
		t.Fatalf("the default strategy should read the media endpoint: %+v", attachments)
	}
}

// TestPostsRetriesATransientServerError covers the retry that keeps one bad page
// from throwing away a walk over hundreds of them.
func TestPostsRetriesATransientServerError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(response, "origin timed out", http.StatusBadGateway)
			return
		}
		response.Header().Set("X-WP-Total", "1")
		response.Header().Set("X-WP-TotalPages", "1")
		_, _ = response.Write([]byte(`[{"id":1,"slug":"alpha","link":"https://example.com/alpha/","title":{"rendered":"Alpha 2 photos"}}]`))
	}))
	defer server.Close()

	source := NewWordPressSource(server.URL, server.Client())
	posts, _, err := source.Posts(context.Background(), WPPostsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("the retry should have recovered the page, got %d posts", len(posts))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected one retry, got %d requests", got)
	}
}

// TestPostsDoesNotRetryAClientError keeps a wrong request from being repeated: a
// 4xx means the request itself is at fault, not the upstream.
func TestPostsDoesNotRetryAClientError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(response, "no such page", http.StatusNotFound)
	}))
	defer server.Close()

	source := NewWordPressSource(server.URL, server.Client())
	if _, _, err := source.Posts(context.Background(), WPPostsOptions{}); err == nil {
		t.Fatalf("expected the failing request to surface an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a client error must not be retried, got %d requests", got)
	}
}

// TestContentsMeasuresSeveralPostsAtOnce covers the single request that answers
// how many images a whole page of albums really carries. The list endpoint
// renders the same body as the single one, so measuring a page of galleries does
// not cost one request each.
func TestContentsMeasuresSeveralPostsAtOnce(t *testing.T) {
	var calls int32
	var path, include, fields string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&calls, 1)
		path = request.URL.Path
		include = request.URL.Query().Get("include")
		fields = request.URL.Query().Get("_fields")
		_, _ = response.Write([]byte(`[
			{"id":41,"content":{"rendered":"<img data-src=\"https://cdn.example.com/a-001.webp\"><img src=\"https://cdn.example.com/a-002.webp\">"}},
			{"id":42,"content":{"rendered":"<img src=\"https://cdn.example.com/b-001.webp\">"}}
		]`))
	}))
	defer server.Close()

	source := NewWordPressSource(server.URL, server.Client())
	images, err := source.Contents(context.Background(), []int{41, 42, 43})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("a page of albums should be measured in one request, got %d", got)
	}
	if include != "41,42,43" {
		t.Fatalf("unexpected include parameter %q", include)
	}
	if path != "/wp-json/wp/v2/posts" {
		t.Fatalf("unexpected path %q", path)
	}
	if fields != "id,content" {
		t.Fatalf("unexpected _fields %q", fields)
	}
	if len(images[41]) != 2 || len(images[42]) != 1 {
		t.Fatalf("unexpected counts: %d and %d", len(images[41]), len(images[42]))
	}
	if _, ok := images[43]; ok {
		t.Fatalf("a post the site did not answer must not appear in the result")
	}
}
