package localupload

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestParseFeaturesDisablesNamedParts(t *testing.T) {
	cases := []struct {
		value          string
		telegramImport bool
	}{
		{"", true},
		{"telegram-import", false},
		{" telegram-import ", false},
		{"TELEGRAM-IMPORT", false},
		{"nothing-known", true},
		{"nothing-known,telegram-import", false},
	}
	for _, testCase := range cases {
		features := ParseFeatures(testCase.value)
		if features.TelegramImport() != testCase.telegramImport {
			t.Errorf("ParseFeatures(%q).TelegramImport() = %v, want %v",
				testCase.value, features.TelegramImport(), testCase.telegramImport)
		}
	}
}

// A build that turns the userscript import off must not hand the script out, must
// refuse its ingest posts, and must say so on the page — while the drafts the
// gallery-site browser fills stay reachable.
func TestDisabledTelegramImportIsRefused(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := NewService(store, nil, Options{}, logger)
	imports, err := NewTelegramImportStore(filepath.Join(t.TempDir(), "imports"), 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := imports.Create(CreateTelegramImportRequest{Title: "From the gallery browser"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, service, ServerOptions{
		SnapshotDir: t.TempDir(), Imports: imports, ImportToken: "pairing-token",
		Features: Features{TelegramImportDisabled: true},
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765"+telegramImportScript, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("the userscript should not be served, got %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/api/telegram-imports", nil)
	request.Header.Set("X-XRW-Import-Token", "pairing-token")
	request.Header.Set("Origin", "http://127.0.0.1:8765")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("the ingest endpoint should refuse the post, got %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/api/state", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var state struct {
		TelegramImport bool `json:"telegram_import"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.TelegramImport {
		t.Fatalf("the page must be told the import is off: %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodDelete,
		"http://127.0.0.1:8765/api/telegram-imports/"+draft.ID, nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("the drafts stay usable without the userscript, got %d: %s", response.Code, response.Body.String())
	}
}
