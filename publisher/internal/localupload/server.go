package localupload

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/TyrEamon/xrw-album/publisher/internal/sitealbum"
)

//go:embed web/*
var webFiles embed.FS

type ServerOptions struct {
	ChatIDs     []string
	SnapshotDir string
	Imports     *TelegramImportStore
	ImportToken string
	Sites       *sitealbum.Registry
	SiteImport  *SiteImporter
}

type Server struct {
	store       *Store
	service     *Service
	logger      *slog.Logger
	chatIDs     []string
	snapshotDir string
	imports     *TelegramImportStore
	importToken string
	sites       *sitealbum.Registry
	siteImport  *SiteImporter
	imgSources  string
	static      http.Handler
}

func NewServer(store *Store, service *Service, options ServerOptions, logger *slog.Logger) (*Server, error) {
	root, err := fs.Sub(webFiles, "web")
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	// Cover images are hotlinked from every configured gallery site, so each
	// origin has to be allowed explicitly: the base policy only permits 'self'.
	// A site whose galleries live on a CDN gets `https:` on top, since its images
	// sit on hosts its base URL never names.
	imgSources := "'self' data:"
	if options.Sites != nil {
		seen := map[string]bool{}
		hotlinked := false
		for _, site := range options.Sites.Sites() {
			if sitealbum.ImagesOutsideBase(site.ImageSource) {
				hotlinked = true
			}
			origin := siteOrigin(site.BaseURL)
			if origin == "" || seen[origin] {
				continue
			}
			seen[origin] = true
			imgSources += " " + origin
			if wildcard := siteWildcard(site.BaseURL); wildcard != "" {
				imgSources += " " + wildcard
			}
		}
		if hotlinked {
			imgSources += " https:"
		}
	}
	return &Server{
		store: store, service: service, logger: logger,
		chatIDs: append([]string(nil), options.ChatIDs...), snapshotDir: options.SnapshotDir,
		imports: options.Imports, importToken: options.ImportToken,
		sites: options.Sites, siteImport: options.SiteImport, imgSources: imgSources,
		static: http.FileServer(http.FS(root)),
	}, nil
}

// siteOrigin reduces a base URL to the `scheme://host` form a CSP source needs.
func siteOrigin(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// siteHost is the bare host of a base URL, port included.
func siteHost(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return parsed.Host
}

// siteWildcard is the `https://*.host` source covering the subdomain a site keeps
// its media on. A bare IP or a single label has no subdomain to cover, so it
// yields nothing rather than an unusable source.
func siteWildcard(baseURL string) string {
	host := siteHost(baseURL)
	if at := strings.LastIndex(host, ":"); at >= 0 {
		host = host[:at]
	}
	if !strings.Contains(host, ".") {
		return ""
	}
	for _, letter := range host {
		if letter >= 'a' && letter <= 'z' || letter >= 'A' && letter <= 'Z' {
			return "https://*." + host
		}
	}
	return ""
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/jobs", s.createJob)
	mux.HandleFunc("GET /api/jobs/{id}/thumbnail/{position}", s.thumbnail)
	mux.HandleFunc("POST /api/jobs/{id}/retry", s.retryJob)
	mux.HandleFunc("POST /api/folders/pick", s.pickFolder)
	mux.HandleFunc("POST /api/snapshots/open", s.openSnapshotFolder)
	mux.HandleFunc("GET /api/telegram-imports/{id}", s.telegramImport)
	mux.HandleFunc("DELETE /api/telegram-imports/{id}", s.deleteTelegramImport)
	mux.HandleFunc("GET /api/telegram-imports/{id}/files/{file}", s.telegramImportFile)
	mux.HandleFunc("POST /api/telegram-imports", s.createTelegramImport)
	mux.HandleFunc("POST /api/telegram-imports/{id}/files", s.addTelegramImportFile)
	mux.HandleFunc("POST /api/telegram-imports/{id}/commit", s.commitTelegramImport)
	mux.HandleFunc("GET /api/site-albums", s.siteAlbums)
	mux.HandleFunc("POST /api/site-albums/refresh", s.refreshSiteAlbums)
	mux.HandleFunc("POST /api/site-albums/verify", s.verifySiteAlbums)
	mux.HandleFunc("POST /api/site-albums/import", s.importSiteAlbum)
	mux.HandleFunc("/", s.staticFile)
	return s.securityHeaders(mux)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("Content-Security-Policy", "default-src 'self'; img-src "+s.imgSources+"; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		if s.isTelegramIngest(request) && !s.validImportToken(request.Header.Get("X-XRW-Import-Token")) {
			writeError(response, http.StatusForbidden, errors.New("Telegram import token is invalid"))
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead &&
			!s.isTelegramIngest(request) && !sameOrigin(request) {
			writeError(response, http.StatusForbidden, errors.New("cross-origin request rejected"))
			return
		}
		next.ServeHTTP(response, request)
	})
}

func (s *Server) isTelegramIngest(request *http.Request) bool {
	if request.Method != http.MethodPost {
		return false
	}
	path := strings.TrimSuffix(request.URL.Path, "/")
	return path == "/api/telegram-imports" || strings.HasSuffix(path, "/files")
}

func (s *Server) validImportToken(value string) bool {
	if s.importToken == "" || len(value) != len(s.importToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), []byte(s.importToken)) == 1
}

func sameOrigin(request *http.Request) bool {
	origin := strings.TrimSpace(request.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, request.Host) &&
		(parsed.Scheme == "http" || parsed.Scheme == "https")
}

func (s *Server) staticFile(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		http.NotFound(response, request)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	s.static.ServeHTTP(response, request)
}

func (s *Server) state(response http.ResponseWriter, request *http.Request) {
	s.cleanupReadyTelegramImports(request.Context())
	jobs, err := s.store.List(request.Context(), 50)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	configurationError := ""
	if err := s.service.Ready(); err != nil {
		configurationError = err.Error()
	}
	imports, err := s.imports.List()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"configured":            configurationError == "",
		"configuration_error":   configurationError,
		"chat_ids":              s.chatIDs,
		"snapshot_dir":          s.snapshotDir,
		"jobs":                  jobs,
		"telegram_imports":      s.draftViews(imports),
		"telegram_import_token": s.importToken,
		"site_albums":           s.siteStatus(),
	})
}

// importView decorates a stored draft with the site import that is still filling
// it, so the page can show progress instead of a draft that looks empty.
type importView struct {
	TelegramImportSummary
	Importing   bool `json:"importing,omitempty"`
	TargetCount int  `json:"target_count,omitempty"`
}

func (s *Server) draftViews(imports []TelegramImportSummary) []importView {
	views := make([]importView, 0, len(imports))
	for _, draft := range imports {
		view := importView{TelegramImportSummary: draft}
		if s.siteImport != nil {
			if target, filling := s.siteImport.Filling(draft.ID); filling {
				view.Importing = true
				view.TargetCount = target
			}
		}
		views = append(views, view)
	}
	return views
}

// siteStatus reports every configured site's cache state. It stays nil when no
// gallery site is configured, which is how the page knows to hide the region.
func (s *Server) siteStatus() any {
	if s.sites == nil {
		return nil
	}
	statuses := s.sites.Status()
	if len(statuses) == 0 {
		return nil
	}
	views := make([]map[string]any, 0, len(statuses))
	for _, status := range statuses {
		importing := 0
		if s.siteImport != nil {
			importing = s.siteImport.ActiveFor(status.ID)
		}
		views = append(views, map[string]any{
			"id": status.ID, "name": status.Name, "base_url": status.BaseURL,
			"total":        status.Total,
			"built_at":     status.BuiltAt,
			"refreshed_at": status.RefreshedAt,
			"building":     status.Building,
			"progress":     status.Progress,
			"error":        status.Error,
			"importing":    importing,
		})
	}
	return views
}

// resolveSite maps a request's site id onto a configured site, defaulting to the
// first one. It writes the error response itself, so callers just return.
func (s *Server) resolveSite(response http.ResponseWriter, id string) (sitealbum.Target, bool) {
	if s.sites == nil {
		writeError(response, http.StatusNotFound, errors.New("未配置站点图库"))
		return sitealbum.Target{}, false
	}
	target, err := s.sites.Target(id)
	if err != nil {
		writeError(response, http.StatusNotFound, err)
		return sitealbum.Target{}, false
	}
	return target, true
}

// siteAlbumView decorates a cached album with the local draft it produced, so the
// browser can offer "继续编辑" instead of importing the same gallery twice.
type siteAlbumView struct {
	sitealbum.Album
	DraftID        string `json:"draft_id,omitempty"`
	CommittedJobID string `json:"committed_job_id,omitempty"`
	Importing      bool   `json:"importing,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (s *Server) siteAlbums(response http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	target, ok := s.resolveSite(response, query.Get("site"))
	if !ok {
		return
	}
	page, _ := strconv.Atoi(query.Get("page"))
	category, _ := strconv.Atoi(query.Get("category"))
	perPage, _ := strconv.Atoi(query.Get("per_page"))
	albums, total, err := target.Store.List(sitealbum.Filter{
		Query: query.Get("q"), CategoryID: category, Page: page, PerPage: perPage,
	})
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	drafts, err := s.imports.List()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	byLink := make(map[string]TelegramImportSummary, len(drafts))
	for _, draft := range drafts {
		if draft.SourceURL == "" {
			continue
		}
		// An uncommitted draft is the more useful link: it is the one the user can
		// still open and edit.
		if existing, ok := byLink[draft.SourceURL]; !ok || existing.CommittedJobID != "" {
			byLink[draft.SourceURL] = draft
		}
	}
	views := make([]siteAlbumView, 0, len(albums))
	for _, album := range albums {
		view := siteAlbumView{Album: album}
		if draft, ok := byLink[album.Link]; ok {
			view.DraftID = draft.ID
			view.CommittedJobID = draft.CommittedJobID
		}
		if s.siteImport != nil {
			view.Importing = s.siteImport.IsActive(target.Site.ID, album.ID)
			view.Error = s.siteImport.Failure(target.Site.ID, album.ID)
		}
		views = append(views, view)
	}
	categories, tags := target.Store.Terms()
	writeJSON(response, http.StatusOK, map[string]any{
		"site": target.Site.ID, "albums": views, "total": total, "page": page, "per_page": perPage,
		"categories": categories, "tags": tags, "status": s.siteStatus(),
	})
}

type refreshSiteAlbumsRequest struct {
	Site string `json:"site"`
	Full bool   `json:"full"`
}

func (s *Server) refreshSiteAlbums(response http.ResponseWriter, request *http.Request) {
	var input refreshSiteAlbumsRequest
	if request.ContentLength > 0 {
		if err := decodeJSON(request, &input); err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
	}
	target, ok := s.resolveSite(response, input.Site)
	if !ok {
		return
	}
	// The first build has nothing to merge into, so it is always a full walk.
	if target.Store.Status().Total == 0 {
		input.Full = true
	}
	if !target.Store.StartRefresh(input.Full) {
		writeError(response, http.StatusConflict, errors.New("图包列表正在更新中"))
		return
	}
	writeJSON(response, http.StatusAccepted, s.siteStatus())
}

// verifySiteAlbumsRequest names the albums whose real image count is wanted.
type verifySiteAlbumsRequest struct {
	Site    string `json:"site"`
	PostIDs []int  `json:"post_ids"`
}

// maxVerifyAlbums bounds one measurement, so a stale tab cannot ask the server to
// read thousands of posts at once. It is comfortably above one page of albums.
const maxVerifyAlbums = 120

// verifySiteAlbums counts the images the listed galleries really carry. The
// index only knows the count the title advertises, and on some sites that is
// several times what the post holds, so the browser measures the page it is
// showing instead of trusting the title.
func (s *Server) verifySiteAlbums(response http.ResponseWriter, request *http.Request) {
	var input verifySiteAlbumsRequest
	if request.ContentLength > 0 {
		if err := decodeJSON(request, &input); err != nil {
			writeError(response, http.StatusBadRequest, err)
			return
		}
	}
	target, ok := s.resolveSite(response, input.Site)
	if !ok {
		return
	}
	ids := input.PostIDs
	if len(ids) > maxVerifyAlbums {
		ids = ids[:maxVerifyAlbums]
	}
	actual, err := target.Store.Verify(request.Context(), ids)
	if err != nil {
		writeError(response, http.StatusBadGateway, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"site": target.Site.ID, "actual": actual})
}

// siteImportRequest names the galleries to pull in.
type siteImportRequest struct {
	Site    string `json:"site"`
	PostIDs []int  `json:"post_ids"`
}

// importSiteAlbum queues the picked galleries and returns immediately. A 170
// image album takes a while to fetch, and the page already polls the album list,
// so progress shows up there instead of holding an HTTP request open.
func (s *Server) importSiteAlbum(response http.ResponseWriter, request *http.Request) {
	if s.sites == nil || s.siteImport == nil {
		writeError(response, http.StatusNotFound, errors.New("未配置站点图库"))
		return
	}
	var input siteImportRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	target, ok := s.resolveSite(response, input.Site)
	if !ok {
		return
	}
	accepted := s.siteImport.Queue(target.Site.ID, input.PostIDs)
	if accepted == 0 {
		writeError(response, http.StatusConflict, errors.New("所选图包都已在导入中"))
		return
	}
	writeJSON(response, http.StatusAccepted, map[string]any{"accepted": accepted})
}

func (s *Server) cleanupReadyTelegramImports(ctx context.Context) {
	imports, err := s.imports.List()
	if err != nil {
		s.logger.Error("list Telegram imports for cleanup", "error", err)
		return
	}
	for _, draft := range imports {
		if draft.CommittedJobID == "" {
			continue
		}
		job, err := s.store.Get(ctx, draft.CommittedJobID)
		if err != nil || job.Status != "ready" {
			continue
		}
		if err := s.imports.RemoveCommitted(draft.ID, job.ID); err != nil {
			s.logger.Error("remove completed Telegram draft", "draft", draft.ID, "job", job.ID, "error", err)
			continue
		}
		s.logger.Info("completed Telegram draft files removed", "draft", draft.ID, "job", job.ID)
	}
}

func (s *Server) createTelegramImport(response http.ResponseWriter, request *http.Request) {
	var input CreateTelegramImportRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	draft, err := s.imports.Create(input)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, draft)
}

func (s *Server) addTelegramImportFile(response http.ResponseWriter, request *http.Request) {
	name, err := url.QueryUnescape(strings.TrimSpace(request.Header.Get("X-XRW-File-Name")))
	if err != nil || name == "" {
		writeError(response, http.StatusBadRequest, errors.New("Telegram image filename is missing"))
		return
	}
	file, duplicate, err := s.imports.AddFile(request.PathValue("id"), name,
		request.Header.Get("Content-Type"), request.Header.Get("X-XRW-Source-Message"), request.Body)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"file": file, "duplicate": duplicate})
}

func (s *Server) telegramImport(response http.ResponseWriter, request *http.Request) {
	draft, err := s.imports.Get(request.PathValue("id"))
	if err != nil {
		writeError(response, http.StatusNotFound, errors.New("Telegram draft not found"))
		return
	}
	writeJSON(response, http.StatusOK, draft)
}

func (s *Server) deleteTelegramImport(response http.ResponseWriter, request *http.Request) {
	if err := s.imports.Delete(request.PathValue("id")); err != nil {
		if os.IsNotExist(err) {
			writeError(response, http.StatusNotFound, errors.New("Telegram draft not found"))
			return
		}
		writeError(response, http.StatusConflict, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) telegramImportFile(response http.ResponseWriter, request *http.Request) {
	path, file, err := s.imports.FilePath(request.PathValue("id"), request.PathValue("file"))
	if err != nil {
		writeError(response, http.StatusNotFound, errors.New("Telegram import image not found"))
		return
	}
	handle, err := os.Open(path)
	if err != nil {
		writeError(response, http.StatusNotFound, err)
		return
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Content-Type", file.ContentType)
	response.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeContent(response, request, file.Name, info.ModTime(), handle)
}

type commitTelegramImportRequest struct {
	Title     string   `json:"title"`
	Category  string   `json:"category"`
	Tags      []string `json:"tags"`
	ChannelID string   `json:"channel_id"`
	FileIDs   []string `json:"file_ids"`
}

func (s *Server) commitTelegramImport(response http.ResponseWriter, request *http.Request) {
	var input commitTelegramImportRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	folder, draft, err := s.imports.Prepare(request.PathValue("id"), input.FileIDs)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = draft.Title
	}
	category := strings.TrimSpace(input.Category)
	if category == "" {
		category = draft.Category
	}
	tags := input.Tags
	if len(tags) == 0 {
		tags = draft.Tags
	}
	job, err := s.service.Create(request.Context(), CreateRequest{
		Folder: folder, Title: title, Category: category,
		Tags: tags, ChannelID: input.ChannelID,
	})
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err := s.imports.MarkCommitted(draft.ID, job.ID); err != nil {
		s.logger.Error("mark Telegram import committed", "draft", draft.ID, "job", job.ID, "error", err)
	}
	writeJSON(response, http.StatusCreated, job)
}

func (s *Server) createJob(response http.ResponseWriter, request *http.Request) {
	var input CreateRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	job, err := s.service.Create(request.Context(), input)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusCreated, job)
}

func (s *Server) retryJob(response http.ResponseWriter, request *http.Request) {
	if err := s.service.Retry(request.Context(), request.PathValue("id")); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) thumbnail(response http.ResponseWriter, request *http.Request) {
	position, err := strconv.Atoi(request.PathValue("position"))
	if err != nil || position < 1 {
		writeError(response, http.StatusBadRequest, errors.New("invalid image position"))
		return
	}
	file, err := s.store.File(request.Context(), request.PathValue("id"), position)
	if err != nil {
		writeError(response, http.StatusNotFound, errors.New("image not found"))
		return
	}
	handle, err := os.Open(file.Path)
	if err != nil {
		if file.Status == "uploaded" && file.FileID != "" {
			imageURL, signErr := s.service.ImageURL(file.FileID)
			if signErr == nil {
				http.Redirect(response, request, imageURL, http.StatusTemporaryRedirect)
				return
			}
		}
		writeError(response, http.StatusNotFound, err)
		return
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	contentType := file.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(file.Path))
	}
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeContent(response, request, file.Name, info.ModTime(), handle)
}

func (s *Server) pickFolder(response http.ResponseWriter, request *http.Request) {
	path, err := chooseFolder(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"path": path})
}

func chooseFolder(ctx context.Context) (string, error) {
	if runtime.GOOS != "windows" {
		return "", errors.New("folder picker currently supports Windows; paste an absolute path instead")
	}
	script := `[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; Add-Type -AssemblyName System.Windows.Forms; $dialog=New-Object System.Windows.Forms.FolderBrowserDialog; $dialog.Description='选择写真文件夹'; $dialog.ShowNewFolderButton=$false; if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Write($dialog.SelectedPath) }`
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-Command", script)
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("open Windows folder picker: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (s *Server) openSnapshotFolder(response http.ResponseWriter, request *http.Request) {
	if err := os.MkdirAll(s.snapshotDir, 0o755); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := openFolder(request.Context(), s.snapshotDir); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, map[string]bool{"ok": true})
}

func openFolder(ctx context.Context, path string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.CommandContext(ctx, "explorer.exe", path)
	case "darwin":
		command = exec.CommandContext(ctx, "open", path)
	default:
		command = exec.CommandContext(ctx, "xdg-open", path)
	}
	return command.Start()
}

func decodeJSON(request *http.Request, target any) error {
	if value := request.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(value), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func writeError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]string{"error": err.Error()})
}
