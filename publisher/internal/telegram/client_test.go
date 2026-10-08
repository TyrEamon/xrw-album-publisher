package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type failingTransport struct{ err error }

func (transport failingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request.Body.Close()
	return nil, transport.err
}

func TestUploadNetworkErrorRedactsTokenAndPreservesCause(t *testing.T) {
	const token = "123456789:unit_test_token_ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	path := filepath.Join(t.TempDir(), "one.jpg")
	if err := os.WriteFile(path, []byte("document"), 0o600); err != nil {
		t.Fatal(err)
	}
	cause := &net.DNSError{Err: "connection timed out", Name: "api.telegram.org", IsTimeout: true}
	client := New("https://api.telegram.org", token, "https://img.example", 0, 0, 1, time.Second)
	client.client.Transport = failingTransport{err: cause}
	_, err := client.UploadGroup(context.Background(), "-100123", []UploadItem{{Path: path}}, "")
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") || !strings.Contains(err.Error(), "HTTPS_PROXY") {
		t.Fatal("network error must redact the token and explain proxy configuration")
	}
	var dns *net.DNSError
	if !errors.As(err, &dns) || !dns.Timeout() {
		t.Fatal("redaction must preserve the underlying error type")
	}
	retry := &RetryAfterError{Duration: 3 * time.Second, Description: "retry " + token}
	var rateLimit *RetryAfterError
	if !errors.As(redactError(retry, token), &rateLimit) || rateLimit.Duration != 3*time.Second {
		t.Fatal("redaction must preserve retry_after handling")
	}
}

func TestRedactHistoricalAndEncodedTokens(t *testing.T) {
	for _, message := range []string{
		`Post "https://api.telegram.org/bot123456789:old_token_ABCDEFGHIJKLMNOPQRSTUVWXYZ/sendMediaGroup": timeout`,
		`123456789%3Aold_token_ABCDEFGHIJKLMNOPQRSTUVWXYZ`,
		`https://api.telegram.org/botshort-test-secret/getMe`,
	} {
		redacted := RedactSecrets(message)
		if redacted == message || strings.Contains(redacted, "old_token") || strings.Contains(redacted, "short-test-secret") {
			t.Fatal("historical diagnostic was not redacted")
		}
		if RedactSecrets(redacted) != redacted {
			t.Fatal("redaction must be idempotent")
		}
	}
	if RedactSecrets("ordinary timeout") != "ordinary timeout" {
		t.Fatal("ordinary errors must be preserved")
	}
}

func TestUploadDocumentGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/botsecret/sendMediaGroup" {
			http.NotFound(response, request)
			return
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("chat_id") != "-100123" {
			t.Fatalf("unexpected chat_id %q", request.FormValue("chat_id"))
		}
		var media []inputMedia
		if err := json.Unmarshal([]byte(request.FormValue("media")), &media); err != nil {
			t.Fatal(err)
		}
		if len(media) != 2 || media[0].Type != "document" ||
			media[0].Caption != "#tag\n<blockquote>album</blockquote>" || media[1].Caption != "" {
			t.Fatalf("unexpected media: %+v", media)
		}
		for index := range media {
			file, _, err := request.FormFile(fmt.Sprintf("document%d", index))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(file)
			_ = file.Close()
			if string(data) != fmt.Sprintf("image-%d", index) {
				t.Fatalf("unexpected body %q", data)
			}
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, "{\"ok\":true,\"result\":[{\"message_id\":41,\"document\":{\"file_id\":\"file-1\",\"file_unique_id\":\"unique-1\",\"mime_type\":\"image/png\"}},{\"message_id\":42,\"document\":{\"file_id\":\"file-2\",\"file_unique_id\":\"unique-2\",\"mime_type\":\"image/png\"}}]}")
	}))
	defer server.Close()

	directory := t.TempDir()
	items := make([]UploadItem, 2)
	for index := range items {
		path := filepath.Join(directory, fmt.Sprintf("test-%d.png", index))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("image-%d", index)), 0o644); err != nil {
			t.Fatal(err)
		}
		items[index] = UploadItem{Path: path, ContentType: "image/png", PublicKey: fmt.Sprintf("veil-%d", index)}
	}
	client := New(server.URL, "secret", "https://img.example", 0, 0, 1, time.Second)
	results, err := client.UploadGroup(context.Background(), "-100123", items, "#tag\n<blockquote>album</blockquote>")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].URL != "https://img.example/file/veil-0" ||
		results[0].FileID != "file-1" || results[1].MessageID != 42 || results[0].ContentType != "image/png" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestUploadSingleDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/botsecret/sendDocument" {
			http.NotFound(response, request)
			return
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("parse_mode") != "HTML" ||
			request.FormValue("caption") != "<blockquote>one</blockquote>" {
			t.Fatalf("unexpected caption fields")
		}
		if _, _, err := request.FormFile("document"); err != nil {
			t.Fatal(err)
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, "{\"ok\":true,\"result\":{\"message_id\":7,\"document\":{\"file_id\":\"file-7\",\"file_unique_id\":\"unique-7\",\"mime_type\":\"image/jpeg\"}}}")
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "one.jpg")
	if err := os.WriteFile(path, []byte("document"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := New(server.URL, "secret", "https://img.example", 0, 0, 1, time.Second)
	results, err := client.UploadGroup(context.Background(), "-100456", []UploadItem{{
		Path: path, ContentType: "image/jpeg", PublicKey: "veil-1",
	}}, "<blockquote>one</blockquote>")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].FileID != "file-7" ||
		results[0].FileUniqueID != "unique-7" || results[0].ContentType != "image/jpeg" {
		t.Fatalf("unexpected result: %+v", results)
	}
}

func TestUploadReportsTelegramRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(response, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":3}}`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "one.jpg")
	if err := os.WriteFile(path, []byte("document"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := New(server.URL, "secret", "https://img.example", 0, 0, 1, time.Second)
	_, err := client.UploadGroup(context.Background(), "-100456", []UploadItem{{
		Path: path, ContentType: "image/jpeg", PublicKey: "manual-1",
	}}, "")
	var retry *RetryAfterError
	if !errors.As(err, &retry) || retry.Duration != 3*time.Second {
		t.Fatalf("expected 3 second RetryAfterError, got %v", err)
	}
}

func TestIsWebPMatchesTelegramStickerDetection(t *testing.T) {
	cases := []struct {
		item UploadItem
		want bool
	}{
		{UploadItem{Path: `C:\photos\000001.webp`, ContentType: "image/webp"}, true},
		{UploadItem{Path: "/tmp/000001.WEBP", ContentType: "application/octet-stream"}, true},
		{UploadItem{Path: "/tmp/000001.bin", ContentType: "image/webp"}, true},
		{UploadItem{Path: "/tmp/000001.jpg", ContentType: "image/jpeg"}, false},
		{UploadItem{Path: "/tmp/000001.png", ContentType: "image/png"}, false},
	}
	for _, test := range cases {
		if got := isWebP(test.item); got != test.want {
			t.Fatalf("isWebP(%+v) = %v, want %v", test.item, got, test.want)
		}
	}
}

func TestUploadSendsWebPOneDocumentPerRequest(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	captions := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/botsecret/sendDocument" {
			t.Errorf("webp uploads must not use %q", request.URL.Path)
			http.NotFound(response, request)
			return
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		if request.FormValue("disable_content_type_detection") != "true" {
			t.Errorf("webp uploads must disable content type detection")
		}
		file, _, err := request.FormFile("document")
		if err != nil {
			t.Errorf("missing document part: %v", err)
			return
		}
		data, _ := io.ReadAll(file)
		_ = file.Close()
		mu.Lock()
		index := requests
		requests++
		captions = append(captions, request.FormValue("caption"))
		mu.Unlock()
		if string(data) != fmt.Sprintf("webp-%d", index) {
			t.Errorf("unexpected body %q", data)
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(response, `{"ok":true,"result":{"message_id":%d,"document":{"file_id":"file-%d","file_unique_id":"unique-%d","mime_type":"image/webp"}}}`, 61+index, index+1, index+1)
	}))
	defer server.Close()

	directory := t.TempDir()
	items := make([]UploadItem, 2)
	for index := range items {
		path := filepath.Join(directory, fmt.Sprintf("%06d.webp", index+1))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("webp-%d", index)), 0o644); err != nil {
			t.Fatal(err)
		}
		items[index] = UploadItem{Path: path, ContentType: "image/webp", PublicKey: fmt.Sprintf("wp-%d", index)}
	}
	client := New(server.URL, "secret", "https://img.example", 0, 0, 1, time.Second)
	results, err := client.UploadGroup(context.Background(), "-100789", items, "#tag album")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expected one request per document, got %d", requests)
	}
	if len(results) != 2 || results[0].FileID != "file-1" || results[0].MessageID != 61 ||
		results[1].FileID != "file-2" || results[1].ContentType != "image/webp" {
		t.Fatalf("unexpected results: %+v", results)
	}
	if captions[0] != "#tag album" || captions[1] != "" {
		t.Fatalf("caption must stay on the first document only: %q", captions)
	}
}

func TestUploadSplitsMixedBatchWhenAnyFileIsWebP(t *testing.T) {
	var mu sync.Mutex
	endpoints := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		endpoints = append(endpoints, request.URL.Path)
		mu.Unlock()
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		fmt.Fprint(response, `{"ok":true,"result":{"message_id":1,"document":{"file_id":"file","file_unique_id":"unique","mime_type":"image/jpeg"}}}`)
	}))
	defer server.Close()

	directory := t.TempDir()
	items := make([]UploadItem, 2)
	for index, name := range []string{"one.jpg", "two.webp"} {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		contentType := "image/jpeg"
		if strings.HasSuffix(name, ".webp") {
			contentType = "image/webp"
		}
		items[index] = UploadItem{Path: path, ContentType: contentType, PublicKey: fmt.Sprintf("mix-%d", index)}
	}
	client := New(server.URL, "secret", "https://img.example", 0, 0, 1, time.Second)
	if _, err := client.UploadGroup(context.Background(), "-100789", items, ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, endpoint := range endpoints {
		if !strings.HasSuffix(endpoint, "/sendDocument") {
			t.Fatalf("a webp in the batch must split it, saw %q", endpoint)
		}
	}
	if len(endpoints) != 2 {
		t.Fatalf("expected two single document requests, got %d", len(endpoints))
	}
}
