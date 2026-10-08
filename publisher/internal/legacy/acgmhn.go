package legacy

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AcgmhnSource reads the 写真 section of acgmhn.com, which publishes plain HTML
// instead of a REST API. Two of its traits shape this file: the edge refuses any
// request that does not look like a browser, and a gallery is spread over one
// page per image whose filenames follow the site's own ordering, so the image
// list cannot be derived from the first one.
type AcgmhnSource struct {
	BaseURL string
	Client  *http.Client
	// Delay spaces out the page reads. After roughly twenty requests in a row the
	// site starts answering 429 and refills slowly, so a walk has to be paced.
	Delay time.Duration

	mu sync.Mutex
	// last holds the thumbnails the most recent Posts call carried. The store
	// asks for covers right after reading a page, so the ids always describe the
	// page it just read; anything else would cost one request per gallery.
	last map[int]string
}

// DefaultAcgmhnBase is the site used when no base URL is configured.
const DefaultAcgmhnBase = "https://www.acgmhn.com"

// EngineAcgmhn names the reading strategy for sites that are not WordPress and
// expose their galleries as paginated HTML.
const EngineAcgmhn = "acgmhn"

const (
	// acgmhnSection is the 写真 listing, the only section this source reads.
	acgmhnSection = "/cos/"
	// acgmhnPerPage is the site's own page size. It takes no per_page parameter,
	// so this is only used to turn a page count into an item count.
	acgmhnPerPage = 36
	// acgmhnMaxImages bounds one gallery so a misbehaving pager cannot loop
	// forever; the largest gallery seen on the site is well under a hundred.
	acgmhnMaxImages = 1000
	// acgmhnAgent is the user agent the edge accepts. Without it every request
	// comes back as a Cloudflare interstitial instead of the page.
	acgmhnAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
)

// NewAcgmhnSource prepares a reader for one acgmhn site. A nil client honours
// HTTPS_PROXY through the shared transport.
func NewAcgmhnSource(baseURL string, client *http.Client) *AcgmhnSource {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultAcgmhnBase
	}
	return &AcgmhnSource{
		BaseURL: baseURL,
		Client:  client,
		Delay:   time.Second,
		last:    map[int]string{},
	}
}

// Base returns the site root, so callers that hold a source interface can still
// log and allowlist the site.
func (s *AcgmhnSource) Base() string { return s.BaseURL }

func (s *AcgmhnSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient
}

// pace waits out one request slot. Every attempt pays it, so a retry slows the
// walk instead of hammering the site.
func (s *AcgmhnSource) pace(ctx context.Context) error {
	if s.Delay <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.Delay):
		return nil
	}
}

// get fetches one page with browser headers, retrying the throttling and the
// transient edge errors the site hands out under load.
func (s *AcgmhnSource) get(ctx context.Context, address string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= wordpressMaxAttempts; attempt++ {
		if err := s.pace(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", acgmhnAgent)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		response, err := s.client().Do(request)
		if err != nil {
			lastErr = err
		} else {
			body, readErr := readLimited(response)
			switch {
			case readErr != nil:
				lastErr = readErr
			case response.StatusCode == http.StatusOK:
				return body, nil
			case response.StatusCode == http.StatusForbidden:
				// The edge answers a client it distrusts with a challenge page
				// rather than the site, so this reads as an auth-like failure.
				lastErr = fmt.Errorf("%s: HTTP 403 (Cloudflare challenge)", address)
			case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
				lastErr = fmt.Errorf("%s: HTTP %d", address, response.StatusCode)
			default:
				return nil, fmt.Errorf("%s: HTTP %d", address, response.StatusCode)
			}
		}
		if attempt < wordpressMaxAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s: gave up after %d attempts", address, wordpressMaxAttempts)
	}
	return nil, lastErr
}

func readLimited(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, wordpressMaxBody))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// Posts reads one page of the 写真 listing. The site has no per_page parameter,
// so PerPage is ignored and each page carries its own thirty-six items. A page
// past the end answers with an empty listing rather than an error, which is how
// a walk learns to stop.
func (s *AcgmhnSource) Posts(ctx context.Context, options WPPostsOptions) ([]WPPost, WPPage, error) {
	page := options.Page
	if page <= 0 {
		page = 1
	}
	body, err := s.get(ctx, s.listingURL(page))
	if err != nil {
		return nil, WPPage{}, err
	}
	posts, covers := parseAcgmhnListing(s.BaseURL, string(body))
	s.mu.Lock()
	s.last = covers
	s.mu.Unlock()
	totalPages := acgmhnTotalPages(string(body))
	info := WPPage{Total: totalPages * acgmhnPerPage, TotalPages: totalPages}
	if acgmhnNothingNew(posts, options.Known) {
		// The listing is newest first, so a page of albums that are all already
		// indexed means the rest of the site is indexed too. Reporting nothing
		// here is what keeps a periodic refresh from reading a thousand pages at
		// one request per second.
		return nil, info, nil
	}
	if len(posts) == 0 && (totalPages == 0 || page < totalPages) {
		// The 写真 listing also carries animated entries, and the site posts them
		// in runs: several pages in a row can hold nothing importable while the
		// pages after them are full of galleries. Those pages are not the end of
		// the listing.
		info.More = true
	}
	return posts, info, nil
}

// acgmhnNothingNew reports whether every gallery on the page is already indexed.
func acgmhnNothingNew(posts []WPPost, known map[int]struct{}) bool {
	if len(known) == 0 || len(posts) == 0 {
		return false
	}
	for _, post := range posts {
		if _, seen := known[post.ID]; !seen {
			return false
		}
	}
	return true
}

// listingURL names one page. The first page has no index suffix, which is the
// site's own convention.
func (s *AcgmhnSource) listingURL(page int) string {
	if page <= 1 {
		return s.BaseURL + acgmhnSection
	}
	return fmt.Sprintf("%s%sindex-%d.html", s.BaseURL, acgmhnSection, page)
}

// Covers answers from the thumbnails the last listing read carried, so reading a
// page costs no extra request.
func (s *AcgmhnSource) Covers(_ context.Context, ids []int) (map[int]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	covers := make(map[int]string, len(ids))
	for _, id := range ids {
		if url, ok := s.last[id]; ok {
			covers[id] = url
		}
	}
	return covers, nil
}

// Terms reports no taxonomies. The listing carries none, and the tags a gallery
// page shows would cost one request each to collect.
func (s *AcgmhnSource) Terms(context.Context, string, string) ([]WPTerm, error) {
	return nil, nil
}

// AlbumImages reads every page of one gallery. The site shows a single image per
// page and names the files in a dictionary order of its own, so there is no way
// around one request per image.
func (s *AcgmhnSource) AlbumImages(ctx context.Context, postID int) ([]WPMedia, error) {
	images := make([]WPMedia, 0, 32)
	seen := map[string]struct{}{}
	for page := 1; page <= acgmhnMaxImages; page++ {
		payload, err := s.ajaxPage(ctx, postID, page)
		if err != nil {
			return nil, err
		}
		link := contentImageLink(payload.Pic)
		if link == "" {
			break
		}
		if _, duplicate := seen[link]; !duplicate {
			seen[link] = struct{}{}
			images = append(images, WPMedia{ID: len(images) + 1, URL: link, MimeType: imageMimeForURL(link)})
		}
		if len(payload.CanAjax) < 2 || payload.CanAjax[1] == 0 {
			break
		}
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("图包 %d 在源站没有可下载的图片", postID)
	}
	return images, nil
}

// acgmhnAjaxPayload is the fragment the site returns for one gallery page. The
// picture arrives as ready-made HTML, and canajax is [previous, next]: the next
// slot drops to zero on the last page.
type acgmhnAjaxPayload struct {
	Pic     string `json:"pic"`
	Pager   string `json:"pager"`
	CanAjax []int  `json:"canajax"`
}

// ajaxPage reads one page of a gallery through the site's own lightweight
// endpoint: roughly 240 bytes instead of the 15 KB of the rendered page.
func (s *AcgmhnSource) ajaxPage(ctx context.Context, postID, page int) (acgmhnAjaxPayload, error) {
	name := strconv.Itoa(postID)
	if page > 1 {
		name += "-" + strconv.Itoa(page)
	}
	address := fmt.Sprintf("%s/ajax_cos/%s.html?ajax=1", s.BaseURL, name)
	body, err := s.get(ctx, address)
	if err != nil {
		return acgmhnAjaxPayload{}, err
	}
	var payload acgmhnAjaxPayload
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(body), "\ufeff")), &payload); err != nil {
		return acgmhnAjaxPayload{}, fmt.Errorf("%s: 响应不是预期的 JSON: %w", address, err)
	}
	return payload, nil
}

var (
	// acgmhnItem splits the listing into its list items. Reading each item on its
	// own keeps the title and thumbnail patterns from leaking across entries.
	acgmhnItem = regexp.MustCompile(`(?is)<li[^>]*>(.*?)</li>`)
	// acgmhnLink finds the gallery behind an item and captures its id.
	acgmhnLink = regexp.MustCompile(`(?i)href="(?:https?://[^"]+)?/cos/(\d+)\.html"`)
	// acgmhnTitle pulls the display title out of the item's second link.
	acgmhnTitle = regexp.MustCompile(`(?is)<span class="title">(.*?)</span>`)
	acgmhnTime  = regexp.MustCompile(`(?is)<span class="time">([^<]*)</span>`)
	// acgmhnCount is the "23P" badge, which holds the real photo count. Video
	// entries put a duration there instead, which is what separates the two.
	acgmhnCount  = regexp.MustCompile(`(?is)<span class="pagenum">\s*([^<]*?)\s*</span>`)
	acgmhnPhotos = regexp.MustCompile(`^(\d+)P$`)
	// acgmhnThumb is the listing thumbnail, which doubles as the album cover.
	acgmhnThumb = regexp.MustCompile(`(?is)<img[^>]*\bsrc="([^"]+)"`)
	// acgmhnIndexPage counts the numbered listing pages, the only page count the
	// site publishes.
	acgmhnIndexPage = regexp.MustCompile(`(?i)/cos/index-(\d+)\.html`)
)

// parseAcgmhnListing reads one listing page: the galleries it holds and their
// thumbnails. The thumbnails come back keyed by gallery id so Covers can answer
// from memory.
func parseAcgmhnListing(baseURL, body string) ([]WPPost, map[int]string) {
	posts := make([]WPPost, 0, acgmhnPerPage)
	covers := make(map[int]string, acgmhnPerPage)
	for _, block := range acgmhnItem.FindAllStringSubmatch(body, -1) {
		item := block[1]
		link := acgmhnLink.FindStringSubmatch(item)
		if link == nil {
			continue
		}
		id, err := strconv.Atoi(link[1])
		if err != nil || id <= 0 {
			continue
		}
		count := acgmhnCount.FindStringSubmatch(item)
		if count == nil {
			continue
		}
		photos := acgmhnPhotos.FindStringSubmatch(count[1])
		if photos == nil {
			// A video entry: nothing to import.
			continue
		}
		pictures, err := strconv.Atoi(photos[1])
		if err != nil || pictures <= 0 {
			continue
		}
		title := ""
		if match := acgmhnTitle.FindStringSubmatch(item); match != nil {
			title = strings.TrimSpace(html.UnescapeString(stripTags(match[1])))
		}
		date := ""
		if match := acgmhnTime.FindStringSubmatch(item); match != nil {
			date = strings.TrimSpace(match[1])
		}
		if cover := acgmhnThumb.FindStringSubmatch(item); cover != nil {
			covers[id] = html.UnescapeString(cover[1])
		}
		posts = append(posts, WPPost{
			ID:            id,
			Slug:          strconv.Itoa(id),
			Link:          fmt.Sprintf("%s%s%d.html", baseURL, acgmhnSection, id),
			Date:          date,
			Modified:      date,
			Title:         title,
			FeaturedMedia: id,
			Photos:        pictures,
		})
	}
	return posts, covers
}

// acgmhnTotalPages reports how many listing pages the site publishes, learned
// from the pager links on the page in hand.
func acgmhnTotalPages(body string) int {
	total := 0
	for _, match := range acgmhnIndexPage.FindAllStringSubmatch(body, -1) {
		if page, err := strconv.Atoi(match[1]); err == nil && page > total {
			total = page
		}
	}
	if total == 0 && strings.Contains(body, "/cos/") {
		total = 1
	}
	return total
}

// stripTags removes the inline markup a listing title carries.
func stripTags(value string) string {
	return stripTagPattern.ReplaceAllString(value, "")
}

var stripTagPattern = regexp.MustCompile(`(?s)<[^>]*>`)
