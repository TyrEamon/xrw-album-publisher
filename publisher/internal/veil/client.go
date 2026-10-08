package veil

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	xproxy "golang.org/x/net/proxy"
)

var ErrRateLimited = errors.New("veil API rate limited")

type Client struct {
	baseURL      string
	slots        []*slot
	next         atomic.Uint64
	cooldown     time.Duration
	maxImageSize int64
}

type slot struct {
	client   *http.Client
	interval time.Duration
	mu       sync.Mutex
	nextAt   time.Time
	blocked  time.Time
}

type listResponse struct {
	Items      []listItem `json:"items"`
	Total      int        `json:"total"`
	Limit      int        `json:"limit"`
	Offset     int        `json:"offset"`
	TotalPages int        `json:"total_pages"`
	HasNext    bool       `json:"has_next"`
}

type listItem struct {
	ID             int64  `json:"id"`
	Title          string `json:"title"`
	Category       string `json:"category"`
	ImageCount     int    `json:"image_count"`
	UploadedImages int    `json:"uploaded_images"`
	Status         string `json:"status"`
	UpdatedAt      string `json:"updated_at"`
	Cover          struct {
		ImageID int64 `json:"image_id"`
	} `json:"cover"`
}

type GalleryDetail struct {
	ID           int64      `json:"id"`
	Title        string     `json:"title"`
	Category     string     `json:"category"`
	ImageCount   int        `json:"image_count"`
	CoverImageID int64      `json:"cover_image_id"`
	Status       string     `json:"status"`
	UpdatedAt    string     `json:"updated_at"`
	Tags         []string   `json:"tags"`
	Images       []ImageRef `json:"images"`
}

type ImageRef struct {
	ID        int64 `json:"id"`
	SortOrder int   `json:"sort_order"`
	Width     int   `json:"width"`
	Height    int   `json:"height"`
}

type Download struct {
	Path        string
	ContentType string
	Width       int
	Height      int
	Bytes       int64
}

func New(baseURL string, proxies []string, requests int, window, cooldown, timeout time.Duration, maxImageSize int64) (*Client, error) {
	if len(proxies) == 0 {
		proxies = []string{""}
	}
	interval := window / time.Duration(requests)
	client := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		cooldown:     cooldown,
		maxImageSize: maxImageSize,
	}
	for _, rawProxy := range proxies {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		var connector xproxy.ContextDialer = dialer
		if rawProxy != "" {
			proxyURL, err := url.Parse(rawProxy)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %q: %w", rawProxy, err)
			}
			if proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h" {
				return nil, fmt.Errorf("proxy %q must use socks5:// or socks5h://", rawProxy)
			}
			var auth *xproxy.Auth
			if proxyURL.User != nil {
				password, _ := proxyURL.User.Password()
				auth = &xproxy.Auth{User: proxyURL.User.Username(), Password: password}
			}
			proxyDialer, err := xproxy.SOCKS5("tcp", proxyURL.Host, auth, dialer)
			if err != nil {
				return nil, fmt.Errorf("configure proxy %q: %w", rawProxy, err)
			}
			contextDialer, ok := proxyDialer.(xproxy.ContextDialer)
			if !ok {
				return nil, fmt.Errorf("proxy %q does not support context dialing", rawProxy)
			}
			connector = contextDialer
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.ForceAttemptHTTP2 = false
		tlsConfig := &tls.Config{}
		if transport.TLSClientConfig != nil {
			tlsConfig = transport.TLSClientConfig.Clone()
		}
		tlsConfig.NextProtos = []string{"http/1.1"}
		transport.TLSClientConfig = tlsConfig
		transport.ResponseHeaderTimeout = timeout
		transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
			return connector.DialContext(ctx, "tcp4", address)
		}
		client.slots = append(client.slots, &slot{
			client:   &http.Client{Transport: transport, Timeout: timeout},
			interval: interval,
		})
	}
	return client, nil
}

func (c *Client) ListGalleries(ctx context.Context, limit, offset int) ([]model.GallerySummary, int, bool, error) {
	var response listResponse
	path := fmt.Sprintf("/v1/galleries?limit=%d&offset=%d", limit, offset)
	if err := c.getJSON(ctx, path, &response); err != nil {
		return nil, 0, false, err
	}
	items := make([]model.GallerySummary, 0, len(response.Items))
	for _, item := range response.Items {
		items = append(items, model.GallerySummary{
			ID:             item.ID,
			Title:          item.Title,
			Category:       item.Category,
			ImageCount:     item.ImageCount,
			UploadedImages: item.UploadedImages,
			Status:         item.Status,
			UpdatedAt:      item.UpdatedAt,
			CoverImageID:   item.Cover.ImageID,
		})
	}
	return items, response.Total, response.HasNext, nil
}

func (c *Client) Gallery(ctx context.Context, id int64) (GalleryDetail, error) {
	var detail GalleryDetail
	err := c.getJSON(ctx, fmt.Sprintf("/v1/gallery/%d", id), &detail)
	return detail, err
}

func (c *Client) DownloadImage(ctx context.Context, imageID, expectedGalleryID int64, destination string) (Download, error) {
	response, err := c.do(ctx, fmt.Sprintf("/v1/image/%d", imageID), "image/*,*/*;q=0.8")
	if err != nil {
		return Download{}, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return Download{}, responseError(response)
	}
	contentType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if !strings.HasPrefix(contentType, "image/") {
		return Download{}, fmt.Errorf("image %d returned content type %q", imageID, contentType)
	}
	destination += extensionFor(contentType)
	galleryHeader := strings.TrimSpace(response.Header.Get("x-gallery-id"))
	if galleryHeader != "" {
		galleryID, err := strconv.ParseInt(galleryHeader, 10, 64)
		if err != nil || galleryID != expectedGalleryID {
			return Download{}, fmt.Errorf("image %d gallery header %q does not match %d", imageID, galleryHeader, expectedGalleryID)
		}
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return Download{}, err
	}
	temporary := destination + ".part"
	file, err := os.Create(temporary)
	if err != nil {
		return Download{}, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, c.maxImageSize+1))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(temporary)
		return Download{}, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return Download{}, closeErr
	}
	if written > c.maxImageSize {
		_ = os.Remove(temporary)
		return Download{}, fmt.Errorf("image %d exceeds configured size limit", imageID)
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Remove(temporary)
		return Download{}, err
	}
	return Download{Path: destination, ContentType: contentType, Bytes: written}, nil
}

func extensionFor(contentType string) string {
	switch contentType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

func (c *Client) getJSON(ctx context.Context, path string, target any) error {
	response, err := c.do(ctx, path, "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responseError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, path, accept string) (*http.Response, error) {
	var lastErr error
	for range min(len(c.slots), 3) {
		s := c.slots[int(c.next.Add(1)-1)%len(c.slots)]
		if err := s.wait(ctx); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", accept)
		request.Header.Set("User-Agent", "xrw-publisher/0.1")
		response, err := s.client.Do(request)
		if err != nil {
			s.client.CloseIdleConnections()
			lastErr = err
			continue
		}
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
			_ = response.Body.Close()
			s.block(c.cooldown)
			lastErr = fmt.Errorf("%w: HTTP %d: %s", ErrRateLimited, response.StatusCode, strings.TrimSpace(string(body)))
			continue
		}
		return response, nil
	}
	if lastErr == nil {
		lastErr = ErrRateLimited
	}
	return nil, lastErr
}

func (s *slot) wait(ctx context.Context) error {
	s.mu.Lock()
	now := time.Now()
	readyAt := s.nextAt
	if s.blocked.After(readyAt) {
		readyAt = s.blocked
	}
	if readyAt.Before(now) {
		readyAt = now
	}
	s.nextAt = readyAt.Add(s.interval)
	s.mu.Unlock()

	timer := time.NewTimer(time.Until(readyAt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *slot) block(duration time.Duration) {
	s.mu.Lock()
	s.blocked = time.Now().Add(duration)
	s.mu.Unlock()
}

func responseError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	return fmt.Errorf("veil API HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
}
