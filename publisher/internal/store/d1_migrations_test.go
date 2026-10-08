package store_test

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	_ "modernc.org/sqlite"
)

// The D1 schema (migrations/) and the seed exporter (scripts/export-d1-sql.js)
// live in the xrw-album website repository, not in this uploader repository.
// These tests therefore need a website checkout: point XRW_ALBUM_REPO at one, or
// keep this repository nested inside a checkout as it was before the split.
func albumRepository(t *testing.T) string {
	t.Helper()
	if dir := os.Getenv("XRW_ALBUM_REPO"); dir != "" {
		return dir
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test location")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
}

func requireAlbumRepository(t *testing.T) string {
	t.Helper()
	repositoryRoot := albumRepository(t)
	if _, err := os.Stat(filepath.Join(repositoryRoot, "migrations", "0001_init.sql")); err != nil {
		t.Skipf("xrw-album website checkout not found at %s; set XRW_ALBUM_REPO to run this test", repositoryRoot)
	}
	return repositoryRoot
}

func TestD1MigrationsApplyInOrder(t *testing.T) {
	repositoryRoot := requireAlbumRepository(t)
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "d1.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	for _, name := range []string{"0001_init.sql", "0002_publishing.sql"} {
		contents, err := os.ReadFile(filepath.Join(repositoryRoot, "migrations", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := database.Exec(string(contents)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	for _, object := range []struct {
		name string
		kind string
	}{
		{"album_sources", "table"},
		{"tg_files", "table"},
		{"tags", "table"},
		{"album_tags", "table"},
		{"trg_album_tags_insert", "trigger"},
		{"trg_album_tags_delete", "trigger"},
	} {
		var found int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE name = ? AND type = ?",
			object.name, object.kind,
		).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 1 {
			t.Fatalf("missing %s %s", object.kind, object.name)
		}
	}
}

func TestD1SeedExportPreservesPublishedRows(t *testing.T) {
	repositoryRoot := requireAlbumRepository(t)
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required to run scripts/export-d1-sql.js")
	}
	databasePath := filepath.Join(t.TempDir(), "seed.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, name := range []string{"0001_init.sql", "0002_publishing.sql"} {
		contents, err := os.ReadFile(filepath.Join(repositoryRoot, "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(string(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`
INSERT INTO albums (id, title, title_lc, count, cover, href, album_order, start_offset, end_offset,
  source, source_gallery_id, publish_status, storage_provider, mirror_status)
VALUES ('veil-1', 'dynamic', 'dynamic', 1, 'https://example.test/file/1',
  'https://example.test/gallery/1', 99, 100, 101, 'veil', '1', 'ok', 'telegram', 'ok')
`); err != nil {
		t.Fatal(err)
	}

	seedPath := filepath.Join(t.TempDir(), "seed.sql")
	command := exec.Command("node", filepath.Join(repositoryRoot, "scripts", "export-d1-sql.js"),
		"--limit=2", "--out="+seedPath)
	command.Dir = repositoryRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("export seed: %v\n%s", err, output)
	}
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(string(seed)); err != nil {
		t.Fatalf("apply generated seed: %v", err)
	}

	var albumCount, manifestCount, photoCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM albums").Scan(&albumCount); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`
SELECT json_extract(value, '$.albumCount'), json_extract(value, '$.photoCount')
FROM meta WHERE key = 'manifest'
`).Scan(&manifestCount, &photoCount); err != nil {
		t.Fatal(err)
	}
	if albumCount != 3 || manifestCount != 3 || photoCount != 65 {
		t.Fatalf("unexpected counts: albums=%d manifest=%d photos=%d", albumCount, manifestCount, photoCount)
	}
}
