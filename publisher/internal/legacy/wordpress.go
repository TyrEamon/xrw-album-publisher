package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// SourceCosplaytele marks albums discovered through the cosplaytele WordPress site.
	SourceCosplaytele = "cosplaytele"
	// DefaultWordPressBase is the site used when no base URL is configured.
	DefaultWordPressBase = "https://cosplaytele.com"
	// wordpressOrdinalBase keeps WordPress ordinals clear of the linuxdo-85w range
	// (0..14972) so both sources can share one database without tripping the UNIQUE index.
	wordpressOrdinalBase = 100000000
	wordpressPerPage     = 100
	wordpressUserAgent   = "xrw-legacy-wp/1.0"
	wordpressMaxAttempts = 5
	wordpressMaxBody     = 32 << 20

	// ImageSourceMedia reads a gallery from the media endpoint, which is how a
	// site that uploads every gallery image as an attachment publishes it.
	ImageSourceMedia = "media"
	// ImageSourceContent reads a gallery out of the rendered post body, which is
	// the only place the images exist for a site that hotlinks them from a CDN.
	ImageSourceContent = "content"
)

// WPPost is one gallery post as it appears in the site's list view.
type WPPost struct {
	ID            int
	Slug          string
	Link          string
	Date          string
	Modified      string
	Title         string
	FeaturedMedia int
	Categories    []int
	Tags          []int
	Photos        int
	Videos        int
}

// WPMedia is one attachment belonging to a post.
type WPMedia struct {
	ID       int
	Slug     string
	URL      string
	MimeType string
}

// WPTerm is a category or tag.
type WPTerm struct {
	ID    int
	Slug  string
	Name  string
	Count int
}

// WPPostMeta is the caption block parsed out of a post's rendered content.
type WPPostMeta struct {
	Cosplayer     string
	Character     string
	AppearIn      string
	UnzipPassword string
	Photos        int
	Videos        int
}

// WPPage reports the site-wide totals advertised in the pagination headers.
type WPPage struct {
	Total      int
	TotalPages int
	// More marks a page that returned no posts but is still inside the listing, so
	// a walk that reads page by page should keep going. A section that also
	// carries video entries can hold whole pages with nothing to import, and
	// stopping there would silently truncate the index.
	More bool
}

// WPPostsOptions selects a page of posts.
type WPPostsOptions struct {
	Page          int
	PerPage       int
	Categories    []int
	Search        string
	ModifiedAfter time.Time
	OrderBy       string
	Order         string
	// Known lets a source with no modification filter stop early. Such a site
	// lists newest first, so once a page holds nothing but albums that are
	// already indexed the rest of the site is indexed too. A source that filters
	// by ModifiedAfter ignores it; the walk passes nil on a full rebuild.
	Known map[int]struct{}
}

// WPWalkOptions selects which posts Walk discovers.
type WPWalkOptions struct {
	PerPage       int
	Categories    []int
	Search        string
	ModifiedAfter time.Time
	OrderBy       string
	Order         string
	MaxPages      int
}

// WordPressSource talks to the WordPress REST API of a gallery site.
type WordPressSource struct {
	BaseURL string
	PerPage int
	Client  *http.Client
	// ImageSource picks where a gallery's images come from: ImageSourceMedia
	// (the default) or ImageSourceContent. It is per site, because the two
	// publishing styles are indistinguishable from the API surface alone.
	ImageSource string
}

// NewWordPressSource returns a source for baseURL. An empty base URL falls back to
// DefaultWordPressBase. A nil client honours HTTPS_PROXY through the default transport.
func NewWordPressSource(baseURL string, client *http.Client) *WordPressSource {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultWordPressBase
	}
	return &WordPressSource{BaseURL: baseURL, PerPage: wordpressPerPage, Client: client}
}

// Base returns the site root. It exists so callers that hold a reading strategy
// as an interface can still log and allowlist the site.
func (s *WordPressSource) Base() string {
	if s == nil {
		return ""
	}
	return s.BaseURL
}

func (s *WordPressSource) perPage(requested int) int {
	if requested <= 0 || requested > wordpressPerPage {
		requested = s.PerPage
	}
	if requested <= 0 || requested > wordpressPerPage {
		requested = wordpressPerPage
	}
	return requested
}

// defaultClient is built rather than taken from http.DefaultClient so that a
// walk over a slow, proxied link gets a patient handshake and reuses its
// connections: indexing a big site opens hundreds of them in a row.
var defaultClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 45 * time.Second,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
	},
}

func (s *WordPressSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient
}

// get performs a REST request, retrying transport errors, 429 and 5xx responses.
// Indexing a big site takes hundreds of requests over many minutes, so the
// retries have to outlast a short Cloudflare blip (522 and friends): one failed
// page would otherwise throw away the whole walk.
func (s *WordPressSource) get(ctx context.Context, path string, query url.Values, target any) (http.Header, error) {
	address := s.BaseURL + "/wp-json/wp/v2" + path
	if encoded := query.Encode(); encoded != "" {
		address += "?" + encoded
	}
	var lastErr error
	for attempt := 0; attempt < wordpressMaxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", wordpressUserAgent)
		response, err := s.client().Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		header := response.Header.Clone()
		body, readErr := io.ReadAll(io.LimitReader(response.Body, wordpressMaxBody))
		_ = response.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", response.StatusCode, summarizeBody(body))
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return header, fmt.Errorf("%s: HTTP %d: %s", path, response.StatusCode, summarizeBody(body))
		}
		if target != nil {
			if err := json.Unmarshal(body, target); err != nil {
				return header, fmt.Errorf("%s: decode response: %w", path, err)
			}
		}
		return header, nil
	}
	return nil, fmt.Errorf("%s: %w", path, lastErr)
}

func summarizeBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return text
}

// readWPPage reads the pagination headers. TotalPages stays 0 when the header is
// absent, which callers treat as "unknown" rather than "one page".
func readWPPage(header http.Header, fallback int) WPPage {
	page := WPPage{Total: fallback}
	if value, err := strconv.Atoi(header.Get("X-WP-Total")); err == nil && value > 0 {
		page.Total = value
	}
	if value, err := strconv.Atoi(header.Get("X-WP-TotalPages")); err == nil && value > 0 {
		page.TotalPages = value
	}
	return page
}

type wpRendered struct {
	Rendered string `json:"rendered"`
}

type wpPostJSON struct {
	ID            int        `json:"id"`
	Slug          string     `json:"slug"`
	Link          string     `json:"link"`
	Date          string     `json:"date"`
	Modified      string     `json:"modified"`
	Title         wpRendered `json:"title"`
	FeaturedMedia int        `json:"featured_media"`
	Categories    []int      `json:"categories"`
	Tags          []int      `json:"tags"`
	Content       wpRendered `json:"content"`
}

type wpMediaJSON struct {
	ID     int    `json:"id"`
	Slug   string `json:"slug"`
	Source string `json:"source_url"`
	Mime   string `json:"mime_type"`
}

type wpTermJSON struct {
	ID    int    `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func (raw wpPostJSON) toPost() WPPost {
	title, photos, videos := ParsePostTitle(raw.Title.Rendered)
	return WPPost{
		ID: raw.ID, Slug: raw.Slug, Link: raw.Link,
		Date: raw.Date, Modified: raw.Modified, Title: title,
		FeaturedMedia: raw.FeaturedMedia,
		Categories:    append([]int(nil), raw.Categories...),
		Tags:          append([]int(nil), raw.Tags...),
		Photos:        photos, Videos: videos,
	}
}

const wpPostFields = "id,slug,link,date,modified,title,featured_media,categories,tags"

// Posts returns one page of gallery posts plus the site-wide totals.
func (s *WordPressSource) Posts(ctx context.Context, options WPPostsOptions) ([]WPPost, WPPage, error) {
	page := options.Page
	if page < 1 {
		page = 1
	}
	query := url.Values{}
	query.Set("per_page", strconv.Itoa(s.perPage(options.PerPage)))
	query.Set("page", strconv.Itoa(page))
	query.Set("_fields", wpPostFields)
	query.Set("orderby", firstNonEmpty(options.OrderBy, "id"))
	query.Set("order", firstNonEmpty(options.Order, "desc"))
	if len(options.Categories) > 0 {
		query.Set("categories", joinInts(options.Categories))
	}
	if search := strings.TrimSpace(options.Search); search != "" {
		query.Set("search", search)
	}
	if !options.ModifiedAfter.IsZero() {
		query.Set("modified_after", options.ModifiedAfter.UTC().Format("2006-01-02T15:04:05"))
	}
	var payload []wpPostJSON
	header, err := s.get(ctx, "/posts", query, &payload)
	if err != nil {
		return nil, WPPage{}, err
	}
	posts := make([]WPPost, 0, len(payload))
	for _, raw := range payload {
		posts = append(posts, raw.toPost())
	}
	return posts, readWPPage(header, len(posts)), nil
}

// Post returns a single post without its content body.
func (s *WordPressSource) Post(ctx context.Context, id int) (WPPost, error) {
	query := url.Values{}
	query.Set("_fields", wpPostFields)
	var raw wpPostJSON
	if _, err := s.get(ctx, "/posts/"+strconv.Itoa(id), query, &raw); err != nil {
		return WPPost{}, err
	}
	return raw.toPost(), nil
}

// Content returns the rendered content of a post.
func (s *WordPressSource) Content(ctx context.Context, id int) (string, error) {
	query := url.Values{}
	query.Set("_fields", "content")
	var raw wpPostJSON
	if _, err := s.get(ctx, "/posts/"+strconv.Itoa(id), query, &raw); err != nil {
		return "", err
	}
	return raw.Content.Rendered, nil
}

// Media returns every attachment of a post in upload order. The site's own
// featured image is still included here; use AlbumImages to drop it.
func (s *WordPressSource) Media(ctx context.Context, postID int) ([]WPMedia, error) {
	var result []WPMedia
	for page := 1; page <= 100; page++ {
		query := url.Values{}
		query.Set("parent", strconv.Itoa(postID))
		query.Set("per_page", strconv.Itoa(wordpressPerPage))
		query.Set("page", strconv.Itoa(page))
		query.Set("orderby", "id")
		query.Set("order", "asc")
		query.Set("_fields", "id,slug,source_url,mime_type")
		var payload []wpMediaJSON
		header, err := s.get(ctx, "/media", query, &payload)
		if err != nil {
			return nil, err
		}
		for _, item := range payload {
			if item.ID == 0 || strings.TrimSpace(item.Source) == "" {
				continue
			}
			result = append(result, WPMedia{ID: item.ID, Slug: item.Slug, URL: item.Source, MimeType: item.Mime})
		}
		if len(payload) < wordpressPerPage {
			break
		}
		if info := readWPPage(header, 0); info.TotalPages > 0 && page >= info.TotalPages {
			break
		}
	}
	return result, nil
}

// AlbumImages returns a post's gallery in display order. Which endpoint it reads
// depends on the site's ImageSource.
func (s *WordPressSource) AlbumImages(ctx context.Context, postID int) ([]WPMedia, error) {
	if s.imageSource() == ImageSourceContent {
		return s.ContentImages(ctx, postID)
	}
	post, err := s.Post(ctx, postID)
	if err != nil {
		return nil, err
	}
	items, err := s.Media(ctx, postID)
	if err != nil {
		return nil, err
	}
	return AlbumFromMedia(items, post.FeaturedMedia, post.Photos), nil
}

// imageSource normalises the configured strategy, falling back to the media
// endpoint so an unset or misspelled value keeps the original behaviour.
func (s *WordPressSource) imageSource() string {
	if strings.EqualFold(strings.TrimSpace(s.ImageSource), ImageSourceContent) {
		return ImageSourceContent
	}
	return ImageSourceMedia
}

// ContentImages returns the images a post embeds in its rendered body. One
// request is enough: WordPress renders every page of a paginated post into the
// same content field, so nothing has to be walked.
func (s *WordPressSource) ContentImages(ctx context.Context, postID int) ([]WPMedia, error) {
	content, err := s.Content(ctx, postID)
	if err != nil {
		return nil, err
	}
	return ImagesFromContent(content), nil
}

// maxContentBatch is the most posts one Contents call asks for at a time.
const maxContentBatch = 100

// Contents reads the bodies of several posts in one request, which is how a page
// of albums can be measured without one request per album. The list endpoint
// renders the same body as the single one, so the counts agree; a post that is
// missing from the answer is simply absent from the result.
func (s *WordPressSource) Contents(ctx context.Context, ids []int) (map[int][]WPMedia, error) {
	images := map[int][]WPMedia{}
	wanted := make([]int, 0, len(ids))
	for _, id := range ids {
		if id > 0 {
			wanted = append(wanted, id)
		}
	}
	if len(wanted) == 0 {
		return images, nil
	}
	for start := 0; start < len(wanted); start += maxContentBatch {
		end := start + maxContentBatch
		if end > len(wanted) {
			end = len(wanted)
		}
		query := url.Values{}
		query.Set("include", joinInts(wanted[start:end]))
		query.Set("per_page", strconv.Itoa(end-start))
		query.Set("_fields", "id,content")
		var raw []wpPostJSON
		if _, err := s.get(ctx, "/posts", query, &raw); err != nil {
			return nil, err
		}
		for _, post := range raw {
			images[post.ID] = ImagesFromContent(post.Content.Rendered)
		}
	}
	return images, nil
}

// Measure reports the real image count per post, which is what the browser shows
// next to the title a gallery advertises. Only the content strategy can do this
// cheaply: the media strategy answers one post at a time, so a page of albums
// would cost a request each and its titles are accurate anyway.
func (s *WordPressSource) Measure(ctx context.Context, ids []int) (map[int]int, error) {
	if s.imageSource() != ImageSourceContent {
		return map[int]int{}, nil
	}
	images, err := s.Contents(ctx, ids)
	if err != nil {
		return nil, err
	}
	counts := make(map[int]int, len(images))
	for id, items := range images {
		counts[id] = len(items)
	}
	return counts, nil
}

var (
	contentImageTag     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	contentImageDataSrc = regexp.MustCompile(`(?i)\bdata-src\s*=\s*["']([^"']+)["']`)
	contentImageSrc     = regexp.MustCompile(`(?i)\bsrc\s*=\s*["']([^"']+)["']`)
	contentImageExt     = regexp.MustCompile(`(?i)\.(?:webp|jpe?g|png|gif|avif)(?:[?#]\S*)?$`)
)

// ImagesFromContent extracts the gallery a post embeds in its body, in document
// order and without duplicates. Only <img> sources are read, so ad scripts and
// outbound links cannot leak into the gallery, and data-src wins over src
// because a lazy loader leaves a placeholder behind in src.
func ImagesFromContent(rendered string) []WPMedia {
	images := make([]WPMedia, 0, 64)
	seen := map[string]struct{}{}
	for _, tag := range contentImageTag.FindAllString(rendered, -1) {
		link := contentImageLink(tag)
		if link == "" {
			continue
		}
		if _, duplicate := seen[link]; duplicate {
			continue
		}
		seen[link] = struct{}{}
		images = append(images, WPMedia{
			ID:       len(images) + 1,
			URL:      link,
			MimeType: imageMimeForURL(link),
		})
	}
	return images
}

// contentImageLink returns the first usable image URL inside one <img> tag.
func contentImageLink(tag string) string {
	for _, pattern := range []*regexp.Regexp{contentImageDataSrc, contentImageSrc} {
		for _, match := range pattern.FindAllStringSubmatch(tag, -1) {
			link := strings.TrimSpace(html.UnescapeString(match[1]))
			if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
				continue
			}
			if contentImageExt.MatchString(link) {
				return link
			}
		}
	}
	return ""
}

// imageMimeForURL reports the type the extension promises, so a draft keeps a
// concrete image type even when the origin sends no Content-Type.
func imageMimeForURL(link string) string {
	// A CDN often appends a cache key, so the extension is not the last thing.
	if at := strings.IndexAny(link, "?#"); at >= 0 {
		link = link[:at]
	}
	lower := strings.ToLower(link)
	switch {
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".avif"):
		return "image/avif"
	default:
		return "image/jpeg"
	}
}

// Covers resolves featured-media ids to their image URLs, one request per 100 ids.
func (s *WordPressSource) Covers(ctx context.Context, ids []int) (map[int]string, error) {
	result := make(map[int]string, len(ids))
	for start := 0; start < len(ids); start += wordpressPerPage {
		end := start + wordpressPerPage
		if end > len(ids) {
			end = len(ids)
		}
		query := url.Values{}
		query.Set("include", joinInts(ids[start:end]))
		query.Set("per_page", strconv.Itoa(wordpressPerPage))
		query.Set("_fields", "id,source_url")
		var payload []wpMediaJSON
		if _, err := s.get(ctx, "/media", query, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload {
			if item.ID == 0 || strings.TrimSpace(item.Source) == "" {
				continue
			}
			result[item.ID] = item.Source
		}
	}
	return result, nil
}

// CategoryNames resolves category ids to their terms.
func (s *WordPressSource) CategoryNames(ctx context.Context, ids []int) (map[int]WPTerm, error) {
	result := make(map[int]WPTerm, len(ids))
	for start := 0; start < len(ids); start += wordpressPerPage {
		end := start + wordpressPerPage
		if end > len(ids) {
			end = len(ids)
		}
		query := url.Values{}
		query.Set("include", joinInts(ids[start:end]))
		query.Set("per_page", strconv.Itoa(wordpressPerPage))
		query.Set("_fields", "id,slug,name,count")
		var payload []wpTermJSON
		if _, err := s.get(ctx, "/categories", query, &payload); err != nil {
			return nil, err
		}
		for _, item := range payload {
			result[item.ID] = WPTerm{ID: item.ID, Slug: item.Slug, Name: item.Name, Count: item.Count}
		}
	}
	return result, nil
}

// Terms lists a whole taxonomy ("categories" or "tags"), most used first.
func (s *WordPressSource) Terms(ctx context.Context, taxonomy, orderBy string) ([]WPTerm, error) {
	path, err := taxonomyPath(taxonomy)
	if err != nil {
		return nil, err
	}
	orderBy = strings.TrimSpace(orderBy)
	var result []WPTerm
	for page := 1; page <= 200; page++ {
		query := url.Values{}
		query.Set("per_page", strconv.Itoa(wordpressPerPage))
		query.Set("page", strconv.Itoa(page))
		query.Set("_fields", "id,slug,name,count")
		if orderBy != "" {
			query.Set("orderby", orderBy)
			query.Set("order", "desc")
		}
		var payload []wpTermJSON
		header, err := s.get(ctx, path, query, &payload)
		if err != nil {
			return nil, err
		}
		for _, item := range payload {
			if item.ID == 0 {
				continue
			}
			result = append(result, WPTerm{ID: item.ID, Slug: item.Slug, Name: item.Name, Count: item.Count})
		}
		if len(payload) < wordpressPerPage {
			break
		}
		if info := readWPPage(header, 0); info.TotalPages > 0 && page >= info.TotalPages {
			break
		}
	}
	return result, nil
}

// taxonomyPath maps a taxonomy name onto its REST path, refusing anything else so
// a caller cannot steer the request at an arbitrary endpoint.
func taxonomyPath(taxonomy string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(taxonomy)) {
	case "categories", "category":
		return "/categories", nil
	case "tags", "tag":
		return "/tags", nil
	default:
		return "", fmt.Errorf("unsupported taxonomy %q", taxonomy)
	}
}

// Meta parses the caption block of a post.
func (s *WordPressSource) Meta(ctx context.Context, postID int) (WPPostMeta, error) {
	content, err := s.Content(ctx, postID)
	if err != nil {
		return WPPostMeta{}, err
	}
	return ParsePostMeta(content), nil
}

// Walk pages through the site and reports every gallery as a parked-ready SourceAlbum.
func (s *WordPressSource) Walk(ctx context.Context, options WPWalkOptions, visit func(SourceAlbum) error) error {
	perPage := s.perPage(options.PerPage)
	for page := 1; page <= 1000; page++ {
		posts, info, err := s.Posts(ctx, WPPostsOptions{
			Page: page, PerPage: perPage, Categories: options.Categories,
			Search: options.Search, ModifiedAfter: options.ModifiedAfter,
			OrderBy: options.OrderBy, Order: options.Order,
		})
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			return nil
		}
		covers, err := s.Covers(ctx, FeaturedIDs(posts))
		if err != nil {
			return err
		}
		for _, post := range posts {
			if err := visit(SourceAlbumFromPost(post, covers[post.FeaturedMedia])); err != nil {
				return err
			}
		}
		if info.TotalPages > 0 && page >= info.TotalPages {
			return nil
		}
		if options.MaxPages > 0 && page >= options.MaxPages {
			return nil
		}
	}
	return nil
}

// SourceAlbumFromPost maps a site post onto the album identity used by the store.
func SourceAlbumFromPost(post WPPost, cover string) SourceAlbum {
	return SourceAlbum{
		ID:      fmt.Sprintf("wp-%d", post.ID),
		Ordinal: wordpressOrdinalBase + post.ID,
		Title:   post.Title,
		Cover:   cover,
		Source:  SourceCosplaytele,
	}
}

// mediaSlugCounter splits a file name like "album-12_result" into its stem and
// its position counter.
var mediaSlugCounter = regexp.MustCompile(`^(.*?)-(\d+)(?:_result)?$`)

// mediaPrefix returns the file-name stem an attachment shares with its
// siblings, or "" when the name carries no position counter.
func mediaPrefix(slug string) string {
	match := mediaSlugCounter.FindStringSubmatch(slug)
	if match == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(match[1]))
}

// AlbumFromMedia narrows a post's attachments to the images that make up the
// gallery, in upload order.
//
// The site stores a gallery as attachments of its post and then reuses one of
// them - or uploads one extra image - as the post's featured image. Both shapes
// occur in the wild, so the featured id alone cannot decide: the photo count
// printed in the post title is the tie-breaker. When the attachment list holds
// exactly one image more than the title claims, that surplus image is the cover
// and is dropped; otherwise the cover is already part of the gallery and every
// image is kept. If dropping the cover does not land on the claimed count the
// original list wins, so a mislabelled post can never lose a real photo.
func AlbumFromMedia(items []WPMedia, coverID, claim int) []WPMedia {
	images := make([]WPMedia, 0, len(items))
	for _, item := range items {
		if item.MimeType != "" && !strings.HasPrefix(item.MimeType, "image/") {
			continue
		}
		images = append(images, item)
	}
	if prefix, ok := dominantMediaPrefix(images); ok {
		images = keepMediaPrefix(images, prefix)
	}
	if claim <= 0 || len(images) != claim+1 {
		return images
	}
	kept := make([]WPMedia, 0, len(images))
	for _, item := range images {
		if coverID > 0 && item.ID == coverID {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == claim {
		return kept
	}
	return images
}

// dominantMediaPrefix reports the slug stem shared by a strict majority of the
// attachments. A post's attachment list occasionally carries a stray upload
// that belongs to another gallery; those are always named after that other
// gallery, so the stem held by most attachments identifies the real album. A
// stem is only returned when it covers more than half of the list and more than
// one stem exists, which leaves inconsistently named galleries untouched.
func dominantMediaPrefix(items []WPMedia) (string, bool) {
	if len(items) < 2 {
		return "", false
	}
	counts := make(map[string]int)
	for _, item := range items {
		prefix := mediaPrefix(item.Slug)
		if prefix == "" {
			continue
		}
		counts[prefix]++
	}
	if len(counts) < 2 {
		return "", false
	}
	best, bestCount := "", 0
	for prefix, count := range counts {
		if count > bestCount {
			best, bestCount = prefix, count
		}
	}
	if bestCount*2 <= len(items) {
		return "", false
	}
	return best, true
}

// keepMediaPrefix drops the attachments that belong to another gallery. Names
// without a counter are kept: an attachment that cannot be classified must not
// be discarded.
func keepMediaPrefix(items []WPMedia, prefix string) []WPMedia {
	result := make([]WPMedia, 0, len(items))
	for _, item := range items {
		current := mediaPrefix(item.Slug)
		if current == "" || current == prefix {
			result = append(result, item)
		}
	}
	return result
}

var (
	titleCounter  = regexp.MustCompile(`(?i)(\d+)\s+photos?\b(?:\s+and\s+(\d+)\s+videos?)?`)
	metaCosplayer = regexp.MustCompile(`(?is)Cosplayer\s*:\s*(?:<a[^>]*>\s*)?([^<\r\n]+)`)
	metaCharacter = regexp.MustCompile(`(?is)Character\s*:\s*(?:<a[^>]*>\s*)?([^<\r\n]+)`)
	metaAppearIn  = regexp.MustCompile(`(?is)Appear\s*In\s*:\s*(?:<a[^>]*>\s*)?([^<\r\n]+)`)
	metaUnzip     = regexp.MustCompile(`(?is)Unzip\s*Password\s*:[^>]*value\s*=\s*["']([^"']*)["']`)
	metaPhotos    = regexp.MustCompile(`(?i)(\d+)\s+photos?\b`)
	metaVideos    = regexp.MustCompile(`(?i)(\d+)\s+videos?\b`)
)

// ParsePostTitle splits a rendered title into the album name and its photo/video counts.
// Site titles look like `Byoru (ビョル) cosplay Hsin – Wuthering Waves “80 photos and 33 videos”`.
func ParsePostTitle(rendered string) (string, int, int) {
	decoded := strings.TrimSpace(html.UnescapeString(rendered))
	if decoded == "" {
		return "", 0, 0
	}
	matches := titleCounter.FindAllStringSubmatchIndex(decoded, -1)
	if len(matches) == 0 {
		return decoded, 0, 0
	}
	last := matches[len(matches)-1]
	photos, _ := strconv.Atoi(decoded[last[2]:last[3]])
	videos := 0
	if last[4] >= 0 {
		videos, _ = strconv.Atoi(decoded[last[4]:last[5]])
	}
	prefix := strings.TrimRightFunc(decoded[:last[0]], func(r rune) bool {
		return r == ' ' || r == '\t' || strings.ContainsRune(`"“”'‘’「」『』`, r)
	})
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = decoded
	}
	return prefix, photos, videos
}

// ParsePostMeta extracts the caption block of a rendered post body.
func ParsePostMeta(content string) WPPostMeta {
	block := content
	if start := strings.Index(content, "<blockquote"); start >= 0 {
		if end := strings.Index(content[start:], "</blockquote>"); end >= 0 {
			block = content[start : start+end]
		}
	}
	var meta WPPostMeta
	meta.Cosplayer = metaValue(metaCosplayer, block)
	meta.Character = metaValue(metaCharacter, block)
	meta.AppearIn = metaValue(metaAppearIn, block)
	if match := metaUnzip.FindStringSubmatch(block); match != nil {
		meta.UnzipPassword = strings.TrimSpace(html.UnescapeString(match[1]))
	}
	if match := metaPhotos.FindStringSubmatch(block); match != nil {
		meta.Photos, _ = strconv.Atoi(match[1])
	}
	if match := metaVideos.FindStringSubmatch(block); match != nil {
		meta.Videos, _ = strconv.Atoi(match[1])
	}
	return meta
}

// metaValue reads a `Label: <a …>value</a>` pair out of the caption block.
func metaValue(pattern *regexp.Regexp, block string) string {
	match := pattern.FindStringSubmatch(block)
	if match == nil {
		return ""
	}
	value := strings.TrimSpace(html.UnescapeString(match[1]))
	return strings.TrimSpace(strings.Trim(value, `"'`))
}

// FeaturedIDs collects the distinct featured-media ids of a page of posts, which
// is the batch size Covers expects.
func FeaturedIDs(posts []WPPost) []int {
	seen := make(map[int]struct{}, len(posts))
	result := make([]int, 0, len(posts))
	for _, post := range posts {
		if post.FeaturedMedia <= 0 {
			continue
		}
		if _, exists := seen[post.FeaturedMedia]; exists {
			continue
		}
		seen[post.FeaturedMedia] = struct{}{}
		result = append(result, post.FeaturedMedia)
	}
	return result
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
