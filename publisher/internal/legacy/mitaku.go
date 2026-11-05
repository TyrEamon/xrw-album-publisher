package legacy

import (
	"context"
	"fmt"
	"hash/fnv"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EngineMitaku names the reading strategy for mitaku.net.
const EngineMitaku = "mitaku"

// DefaultMitakuBase is the site used when no base URL is configured.
const DefaultMitakuBase = "https://mitaku.net"

const (
	// mitakuPostsPerPage is the site's own page size. It takes no per_page
	// parameter, so this only turns a page count into an item count.
	mitakuPostsPerPage = 9
	// mitakuMaxGalleryImages bounds one gallery so a page carrying a looping
	// slider cannot produce an endless list. The largest galleries on the site
	// hold well under a hundred pictures.
	mitakuMaxGalleryImages = 1500
	// mitakuMaxTerms bounds the taxonomies kept in the index. The site has tens of
	// thousands of galleries and one tag per cosplayer, character and game, so
	// without a cap the cached index would be mostly tag names nobody filters by.
	// mitakuMaxTags has to stay above the site's real tag count: an album stores its
	// tags as ids, and an id the table dropped leaves the gallery card and the
	// caption with nothing to name. The site carries about two thousand distinct
	// tags across eight thousand galleries today.
	mitakuMaxTags       = 6000
	mitakuMaxCategories = 40
	// mitakuPlanTTL keeps the page counts fresh enough for browsing without
	// making every page-one request re-read two listings.
	mitakuPlanTTL = 30 * time.Minute
	// mitakuAgent is what the site serves HTML to. It also passes the edge
	// without a challenge, which a bare Go user agent does not.
	mitakuAgent = acgmhnAgent
)

// mitakuSections are the two sections the index walks, in the order the site
// files them. Every gallery lands in exactly one of them — the counts add up to
// the site's whole post count — so both have to be walked to see everything.
var mitakuSections = []string{"ero-cosplay", "sexy-set"}

// MitakuSource reads mitaku.net, a WordPress site with its REST API switched off:
// /wp-json answers `403 rest_disabled`, so both WordPress strategies are
// unavailable and the galleries have to be read out of the rendered HTML.
//
// Two traits shape this file. A gallery is a lightbox slider on the post page, so
// a single request yields the whole image list — the opposite of acgmhn, which
// spends one request per picture. And roughly half of what the site publishes is
// video: those posts share the page template but carry an inline player and no
// gallery at all, so they are dropped while the listing is read instead of
// failing at import time.
type MitakuSource struct {
	BaseURL string
	Client  *http.Client
	// Delay paces the listing walk. The site throttles nothing visible, but a
	// full index is nine hundred pages, so a walk still reads one at a time.
	Delay time.Duration

	mu         sync.Mutex
	plan       *mitakuPlan
	planAt     time.Time
	done       map[string]bool
	heldKey    string
	heldBody   []byte
	covers     map[int]string
	links      map[int]string
	tags       map[string]int
	categories map[string]int
	// seen holds the galleries this walk already took. The archive overlaps
	// itself — neighbouring pages repeat up to half their entries — so without
	// it the same gallery is counted and indexed more than once.
	seen map[int]struct{}
}

// mitakuSection is one walked listing and how many pages it publishes.
type mitakuSection struct {
	Slug  string
	Pages int
}

// mitakuPlan maps a global listing page onto the section page it really is. The
// store walks one page counter, but this site spreads its galleries over two
// sections, so the two are laid end to end: pages 1..842 are ero-cosplay, 843
// and on are sexy-set.
type mitakuPlan struct {
	Sections []mitakuSection
	Total    int
}

// NewMitakuSource prepares a reader for one mitaku site. A nil client honours
// HTTPS_PROXY through the shared transport.
func NewMitakuSource(baseURL string, client *http.Client) *MitakuSource {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultMitakuBase
	}
	return &MitakuSource{
		BaseURL:    baseURL,
		Client:     client,
		Delay:      500 * time.Millisecond,
		done:       map[string]bool{},
		covers:     map[int]string{},
		links:      map[int]string{},
		tags:       map[string]int{},
		categories: map[string]int{},
		seen:       map[int]struct{}{},
	}
}

// Base returns the site root, so callers that hold a reading strategy as an
// interface can still log and allowlist the site.
func (s *MitakuSource) Base() string { return s.BaseURL }

func (s *MitakuSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return defaultClient
}

// pace waits out one request slot before every attempt, so a retry slows the
// walk down instead of hammering the site.
func (s *MitakuSource) pace(ctx context.Context) error {
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

// get fetches one page with the headers the site expects, retrying the throttling
// and transient edge errors that come back under load.
func (s *MitakuSource) get(ctx context.Context, address string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= wordpressMaxAttempts; attempt++ {
		if err := s.pace(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", mitakuAgent)
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		request.Header.Set("Accept-Language", "en-US,en;q=0.9")
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
				lastErr = fmt.Errorf("%s: HTTP 403 (edge challenge)", address)
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

// listingURL names one page of one section. The first page has no index suffix,
// which is the site's own convention; /page/1/ would be a redirect.
func (s *MitakuSource) listingURL(section string, page int) string {
	if page <= 1 {
		return s.BaseURL + "/category/" + section + "/"
	}
	return fmt.Sprintf("%s/category/%s/page/%d/", s.BaseURL, section, page)
}

// Posts reads one page of the listing. A page past the last one answers with
// nothing rather than an error, which is how a walk learns the site shrank.
func (s *MitakuSource) Posts(ctx context.Context, options WPPostsOptions) ([]WPPost, WPPage, error) {
	page := options.Page
	if page <= 0 {
		page = 1
	}
	plan, err := s.planFor(ctx, page == 1)
	if err != nil {
		return nil, WPPage{}, err
	}
	info := WPPage{Total: plan.Total * mitakuPostsPerPage, TotalPages: plan.Total, More: true}
	index, local := plan.locate(page)
	if index < 0 {
		return nil, info, nil
	}
	section := plan.Sections[index]
	if s.sectionSettled(section.Slug) {
		// The section's first page held nothing new, so the rest of it cannot
		// either. Answering without a request is what keeps a periodic refresh
		// from reading nine hundred pages for nothing.
		return nil, info, nil
	}
	body, err := s.listing(ctx, section.Slug, local)
	if err != nil {
		return nil, info, err
	}
	posts := s.parseListing(string(body))
	if options.Known != nil && local == 1 && len(posts) > 0 && mitakuAllKnown(posts, options.Known) {
		s.settle(section.Slug)
		return nil, info, nil
	}
	return posts, info, nil
}

// listing reads one section page, reusing the copy the plan already fetched for
// page one.
func (s *MitakuSource) listing(ctx context.Context, section string, page int) ([]byte, error) {
	key := mitakuPageKey(section, page)
	s.mu.Lock()
	if s.heldKey == key && s.heldBody != nil {
		body := s.heldBody
		s.heldKey, s.heldBody = "", nil
		s.mu.Unlock()
		return body, nil
	}
	s.mu.Unlock()
	return s.get(ctx, s.listingURL(section, page))
}

// planFor returns the layout of the two sections, rebuilding it on a full walk
// (page one) or once it went stale so grow-with-the-site page counts keep up.
func (s *MitakuSource) planFor(ctx context.Context, refresh bool) (mitakuPlan, error) {
	s.mu.Lock()
	cached, cachedAt := s.plan, s.planAt
	s.mu.Unlock()
	if cached != nil && !refresh && time.Since(cachedAt) < mitakuPlanTTL {
		return *cached, nil
	}

	next := mitakuPlan{}
	for index, section := range mitakuSections {
		body, err := s.get(ctx, s.listingURL(section, 1))
		if err != nil {
			return mitakuPlan{}, err
		}
		pages := mitakuTotalPages(string(body))
		if pages < 1 {
			pages = 1
		}
		next.Sections = append(next.Sections, mitakuSection{Slug: section, Pages: pages})
		next.Total += pages
		if index == 0 {
			// This is the page the caller is about to ask for, so hand it over
			// instead of reading it twice.
			s.mu.Lock()
			s.heldKey, s.heldBody = mitakuPageKey(section, 1), body
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	s.plan, s.planAt = &next, time.Now()
	s.done = map[string]bool{}
	// A rebuilt plan starts a walk from page one, so the running tallies start
	// over: they are counted page by page, and a walk that was interrupted and
	// resumed would otherwise report every tag and category twice. The store
	// keeps the previous counts of its own accord when it walks incrementally.
	s.tags = map[string]int{}
	s.categories = map[string]int{}
	s.seen = map[int]struct{}{}
	s.mu.Unlock()
	return next, nil
}

// locate turns a global page number into a section and its own page number.
func (p mitakuPlan) locate(page int) (int, int) {
	remaining := page
	for index, section := range p.Sections {
		if remaining <= section.Pages {
			return index, remaining
		}
		remaining -= section.Pages
	}
	return -1, 0
}

func mitakuPageKey(section string, page int) string {
	return section + "/" + strconv.Itoa(page)
}

func (s *MitakuSource) sectionSettled(section string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done[section]
}

func (s *MitakuSource) settle(section string) {
	s.mu.Lock()
	s.done[section] = true
	s.mu.Unlock()
}

// mitakuAllKnown reports whether every gallery on a page is already indexed.
func mitakuAllKnown(posts []WPPost, known map[int]struct{}) bool {
	if len(posts) == 0 {
		return false
	}
	for _, post := range posts {
		if _, seen := known[post.ID]; !seen {
			return false
		}
	}
	return true
}

// Covers answers from the thumbnails the last listing read carried, so showing a
// page of albums costs no extra request.
func (s *MitakuSource) Covers(_ context.Context, ids []int) (map[int]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	covers := make(map[int]string, len(ids))
	for _, id := range ids {
		if cover, ok := s.covers[id]; ok {
			covers[id] = cover
		}
	}
	return covers, nil
}

// TermsAreLocal reports that Terms reads counters the walk has already filled
// in, so the album store can ask for the filter list while a long walk is still
// running instead of leaving it empty for ten minutes.
func (s *MitakuSource) TermsAreLocal() bool { return true }

// Terms reports the taxonomies collected while walking. The site publishes them
// only as links on each gallery, so one request per gallery would be needed to
// ask; counting them as the listing goes by is free.
func (s *MitakuSource) Terms(_ context.Context, taxonomy, _ string) ([]WPTerm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts, limit := s.tags, mitakuMaxTags
	if strings.EqualFold(strings.TrimSpace(taxonomy), "categories") {
		counts, limit = s.categories, mitakuMaxCategories
	}
	terms := make([]WPTerm, 0, len(counts))
	for name, count := range counts {
		terms = append(terms, WPTerm{
			ID: mitakuTermID(name), Slug: mitakuTermSlug(name), Name: name, Count: count,
		})
	}
	sort.Slice(terms, func(left, right int) bool {
		if terms[left].Count != terms[right].Count {
			return terms[left].Count > terms[right].Count
		}
		return terms[left].Name < terms[right].Name
	})
	if len(terms) > limit {
		terms = terms[:limit]
	}
	return terms, nil
}

// AlbumImages reads one gallery. Every picture sits in the post's lightbox
// slider, so the whole list arrives with the one request a gallery page costs.
// The `?p=` form is used so a gallery can be read from its id alone, without
// remembering the permalink the listing carried: WordPress redirects it to the
// pretty URL.
func (s *MitakuSource) AlbumImages(ctx context.Context, postID int) ([]WPMedia, error) {
	address := fmt.Sprintf("%s/?p=%d", s.BaseURL, postID)
	body, err := s.get(ctx, address)
	if err != nil {
		return nil, err
	}
	images := parseMitakuGallery(string(body))
	if len(images) == 0 {
		return nil, fmt.Errorf("图包 %d 在源站没有可下载的图片（可能是只有视频的帖子）", postID)
	}
	return images, nil
}

var (
	// mitakuArticleBlock splits the listing into its entries. Reading each one on
	// its own keeps the title and thumbnail patterns from leaking across entries.
	mitakuArticleBlock = regexp.MustCompile(`(?is)<article([^>]*)>(.*?)</article>`)
	// mitakuPostID reads the WordPress id off the article, which is the only
	// stable name a gallery has: the permalink is a slug that can be edited.
	mitakuPostID      = regexp.MustCompile(`(?i)\bid=["']post-(\d+)["']`)
	mitakuPostIDClass = regexp.MustCompile(`(?i)\bclass=["'][^"']*\bpost-(\d+)\b`)
	// mitakuEntryTitle carries both the permalink and the display title.
	mitakuEntryTitle = regexp.MustCompile(`(?is)<h2[^>]*class=["'][^"']*entry-title[^"']*["'][^>]*>\s*<a[^>]*href=["']([^"']+)["'][^>]*>(.*?)</a>`)
	mitakuPublished  = regexp.MustCompile(`(?is)<time[^>]*class=["'][^"']*entry-date\b[^"']*published[^"']*["'][^>]*datetime=["']([^"']+)["']`)
	mitakuUpdated    = regexp.MustCompile(`(?is)<time[^>]*class=["'][^"']*\bupdated\b[^"']*["'][^>]*datetime=["']([^"']+)["']`)
	// mitakuFeatured is the listing thumbnail, which doubles as the album cover.
	// It is a cropped 800x400 copy, not the gallery's first picture.
	mitakuFeatured = regexp.MustCompile(`(?is)<div[^>]*class=["'][^"']*featured-image[^"']*["'][^>]*>.*?<img[^>]*\bsrc=["']([^"']+)["']`)
	mitakuTagSpan  = regexp.MustCompile(`(?is)<span[^>]*class=["'][^"']*tag-links[^"']*["'][^>]*>(.*?)</span>`)
	mitakuCatSpan  = regexp.MustCompile(`(?is)<span[^>]*class=["'][^"']*cat-links[^"']*["'][^>]*>(.*?)</span>`)
	mitakuTermLink = regexp.MustCompile(`(?is)<a[^>]*>([^<]+)</a>`)
	// mitakuPager reads "Page 2 of 69", the only place the site publishes a
	// section's page count. The attributes are single-quoted, so both quote
	// styles have to be accepted.
	mitakuPager = regexp.MustCompile(`(?is)class=["']pages["'][^>]*>\s*Page\s+\d+\s+of\s+(\d+)`)
	// mitakuLastPage is the fallback: the pager's "Last" link names the last page.
	mitakuLastPage = regexp.MustCompile(`(?is)class=["']last["'][^>]*href=["'][^"']*/page/(\d+)/`)
	// mitakuVideoTitle marks a post that holds only a video. Such a post has no
	// gallery at all, so it would only fail at import time.
	mitakuVideoTitle = regexp.MustCompile(`(?i)\(\s*video\s*\)\s*$`)
)

// parseMitakuListing reads one listing page: the galleries it holds, plus the
// covers, permalinks and taxonomies that belong to them. Video-only posts are
// dropped here, which is the only place that knows about them cheaply.
func (s *MitakuSource) parseListing(body string) []WPPost {
	posts := make([]WPPost, 0, mitakuPostsPerPage)
	for _, block := range mitakuArticleBlock.FindAllStringSubmatch(body, -1) {
		attributes, item := block[1], block[2]
		id := 0
		if match := mitakuPostID.FindStringSubmatch(item); match != nil {
			id, _ = strconv.Atoi(match[1])
		}
		if id == 0 {
			if match := mitakuPostID.FindStringSubmatch(attributes); match != nil {
				id, _ = strconv.Atoi(match[1])
			}
		}
		if id == 0 {
			if match := mitakuPostIDClass.FindStringSubmatch(attributes); match != nil {
				id, _ = strconv.Atoi(match[1])
			}
		}
		if id <= 0 {
			continue
		}
		titleMatch := mitakuEntryTitle.FindStringSubmatch(item)
		if titleMatch == nil {
			continue
		}
		link := html.UnescapeString(titleMatch[1])
		title := cleanMitakuTitle(titleMatch[2])
		if mitakuVideoTitle.MatchString(title) {
			// A video post: the site puts the player where the gallery would be,
			// so there is nothing to import.
			continue
		}
		date, updated := "", ""
		if match := mitakuPublished.FindStringSubmatch(item); match != nil {
			date = match[1]
		}
		if match := mitakuUpdated.FindStringSubmatch(item); match != nil {
			updated = match[1]
		}
		if updated == "" {
			updated = date
		}
		cover := ""
		if match := mitakuFeatured.FindStringSubmatch(item); match != nil {
			cover = html.UnescapeString(match[1])
		}

		if !s.claim(id) {
			continue
		}

		post := WPPost{
			ID: id, Slug: mitakuSlug(link), Link: link,
			Date: date, Modified: updated, Title: title,
			FeaturedMedia: id,
			Categories:    s.recordTerms("categories", item),
			Tags:          s.recordTerms("tags", item),
		}

		s.mu.Lock()
		s.covers[id] = cover
		s.links[id] = link
		s.mu.Unlock()
		posts = append(posts, post)
	}
	return posts
}

// claim takes a gallery for this walk, reporting false when an earlier page
// already took it. The archive pages overlap — one page shared four of its nine
// entries with its neighbour — and the store keys albums by post id, so a repeat
// would be dropped there anyway; skipping it here keeps the term counts as
// truthful as the index.
func (s *MitakuSource) claim(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.seen[id]; taken {
		return false
	}
	s.seen[id] = struct{}{}
	return true
}

// recordTerms reads one taxonomy off a listing entry and counts it, returning the
// term ids the album carries. The site has no term ids to hand out over HTML, so
// a stable hash of the name stands in for one: it survives a restart, which a
// counter assigned in walk order would not.
func (s *MitakuSource) recordTerms(taxonomy, item string) []int {
	target := mitakuTagSpan
	if taxonomy == "categories" {
		target = mitakuCatSpan
	}
	block := target.FindStringSubmatch(item)
	if block == nil {
		return nil
	}
	ids := make([]int, 0, 4)
	seen := map[int]struct{}{}
	for _, link := range mitakuTermLink.FindAllStringSubmatch(block[1], -1) {
		name := strings.Join(strings.Fields(html.UnescapeString(stripTags(link[1]))), " ")
		if name == "" {
			continue
		}
		id := mitakuTermID(name)
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
		s.mu.Lock()
		if taxonomy == "categories" {
			s.categories[name]++
		} else {
			s.tags[name]++
		}
		s.mu.Unlock()
	}
	return ids
}

// cleanMitakuTitle strips the markup and the entity escapes a listing title
// carries, and folds the padding the theme leaves around it.
func cleanMitakuTitle(raw string) string {
	return strings.Join(strings.Fields(html.UnescapeString(stripTags(raw))), " ")
}

// mitakuSlug is the permalink's last path segment, which is the slug the site
// shows and the one the site's own URLs use.
func mitakuSlug(link string) string {
	parsed, err := url.Parse(link)
	if err != nil {
		return ""
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return ""
	}
	if at := strings.LastIndex(path, "/"); at >= 0 {
		return path[at+1:]
	}
	return path
}

// mitakuTotalPages reads a section's page count off the pager it renders.
func mitakuTotalPages(body string) int {
	if match := mitakuPager.FindStringSubmatch(body); match != nil {
		if pages, err := strconv.Atoi(match[1]); err == nil && pages > 0 {
			return pages
		}
	}
	if match := mitakuLastPage.FindStringSubmatch(body); match != nil {
		if pages, err := strconv.Atoi(match[1]); err == nil && pages > 0 {
			return pages
		}
	}
	return 0
}

var (
	// mitakuSlideAnchor is one picture of the lightbox. The picture's address is
	// on the anchor; the img beside it only ever holds a 1x1 spacer, because the
	// slider loads the real file on demand.
	mitakuSlideAnchor = regexp.MustCompile(`(?is)<a[^>]*class=["'][^"']*msacwl-img-link[^"']*["'][^>]*>`)
	mitakuSlideSource = regexp.MustCompile(`(?i)data-mfp-src=["']([^"']+)["']`)
	// mitakuContentImage covers the older posts that put the pictures straight
	// into the post body instead of a slider.
	mitakuContentImage = regexp.MustCompile(`(?is)<img[^>]*\bsrc=["']([^"']+)["']`)
	// mitakuImageIndex is the picture's position in the gallery, which the site
	// appends to every filename. Sorting on it keeps the order the gallery was
	// published in, whatever order the markup happens to list them.
	mitakuImageIndex = regexp.MustCompile(`(?i)-(\d+)\.(?:jpe?g|png|webp|gif)(?:[?#].*)?$`)
)

// parseMitakuGallery reads every picture of one gallery page.
func parseMitakuGallery(body string) []WPMedia {
	seen := map[string]struct{}{}
	urls := make([]string, 0, 48)
	for _, anchor := range mitakuSlideAnchor.FindAllString(body, -1) {
		if match := mitakuSlideSource.FindStringSubmatch(anchor); match != nil {
			urls = appendMitakuImage(urls, seen, match[1])
		}
	}
	if len(urls) == 0 {
		for _, match := range mitakuContentImage.FindAllStringSubmatch(body, -1) {
			urls = appendMitakuImage(urls, seen, match[1])
		}
	}
	urls = mitakuSortImages(urls)
	images := make([]WPMedia, 0, len(urls))
	for index, link := range urls {
		images = append(images, WPMedia{ID: index + 1, URL: link, MimeType: imageMimeForURL(link)})
	}
	return images
}

// appendMitakuImage keeps one copy of a picture, dropping the placeholders and
// the theme's own assets the page markup is padded with.
func appendMitakuImage(urls []string, seen map[string]struct{}, raw string) []string {
	link := strings.TrimSpace(html.UnescapeString(raw))
	if link == "" || strings.HasPrefix(link, "data:") {
		return urls
	}
	lower := strings.ToLower(link)
	if strings.Contains(lower, "spacer.gif") || strings.Contains(lower, "/wp-content/plugins/") ||
		strings.Contains(lower, "/wp-content/themes/") {
		return urls
	}
	if _, duplicate := seen[link]; duplicate {
		return urls
	}
	seen[link] = struct{}{}
	if len(urls) >= mitakuMaxGalleryImages {
		return urls
	}
	return append(urls, link)
}

// mitakuSortImages orders a gallery by the index the site appends to each
// filename. A gallery whose names do not all carry one keeps the document order,
// which is the order the slider shows.
func mitakuSortImages(urls []string) []string {
	if len(urls) < 2 {
		return urls
	}
	indices := make([]int, len(urls))
	for position, link := range urls {
		match := mitakuImageIndex.FindStringSubmatch(link)
		if match == nil {
			return urls
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			return urls
		}
		indices[position] = index
	}
	order := make([]int, len(urls))
	for position := range order {
		order[position] = position
	}
	sort.SliceStable(order, func(left, right int) bool {
		return indices[order[left]] < indices[order[right]]
	})
	sorted := make([]string, 0, len(urls))
	for _, position := range order {
		sorted = append(sorted, urls[position])
	}
	return sorted
}

// mitakuTermID turns a term name into a stable id. The site hands out no ids over
// HTML, so the name is hashed instead: the browser filters by id, and a hash
// keeps the same tag pointing at the same id across restarts and rebuilds.
func mitakuTermID(name string) int {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(strings.ToLower(strings.TrimSpace(name))))
	id := int(hash.Sum32() & 0x7fffffff)
	if id == 0 {
		return 1
	}
	return id
}

// mitakuTermSlug is the term name in the shape the site's own URLs use.
func mitakuTermSlug(name string) string {
	// The site's own tag slugs drop punctuation, so "Honkai: Star Rail" is filed
	// as "honkai-star-rail"; the page only filters by id, but a slug that matches
	// the source is what makes a cached index readable.
	var builder strings.Builder
	lastDash := false
	for _, letter := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case letter >= 'a' && letter <= 'z', letter >= '0' && letter <= '9', letter > 127:
			builder.WriteRune(letter)
			lastDash = false
		default:
			if !lastDash && builder.Len() > 0 {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(builder.String(), "-")
}
