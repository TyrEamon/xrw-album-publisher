package legacy

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func walkAlbums(albums ...SourceAlbum) func(func(SourceAlbum) error) error {
	return func(visit func(SourceAlbum) error) error {
		for _, album := range albums {
			if err := visit(album); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestSyncAlbumsParksWithoutQueueing(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	staged := SourceAlbum{ID: "wp-1", Ordinal: 0, Title: "staged", Cover: "https://example.test/cover.webp", Source: "cosplaytele"}
	queued := SourceAlbum{ID: "wp-2", Ordinal: 1, Title: "live", Source: "cosplaytele"}
	if albums, images, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusParked, walkAlbums(staged)); err != nil {
		t.Fatal(err)
	} else if albums != 1 || images != 0 {
		t.Fatalf("albums=%d images=%d", albums, images)
	}
	if _, _, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusPending, walkAlbums(queued)); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Parked != 1 || stats.Pending != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	album, found, err := store.ClaimNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !found || album.ID != "wp-2" {
		t.Fatalf("claimed %+v found=%v", album, found)
	}
	if album.Source != "cosplaytele" {
		t.Fatalf("source = %q", album.Source)
	}
	albums, total, err := store.ListAlbums(ctx, AlbumFilter{Status: SyncStatusParked})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(albums) != 1 || albums[0].CoverURL != "https://example.test/cover.webp" {
		t.Fatalf("total=%d albums=%+v", total, albums)
	}
}

func TestSyncAlbumsRejectsUnknownStatus(t *testing.T) {
	store := newTestStore(t)
	if _, _, err := store.SyncAlbums(context.Background(), []string{"-1001"}, "queued", walkAlbums()); err == nil {
		t.Fatal("expected an error for an unsupported status")
	}
	if _, _, err := store.SyncAlbums(context.Background(), nil, SyncStatusParked, walkAlbums()); err == nil {
		t.Fatal("expected an error when no chat id is configured")
	}
}

func TestActivateAlbumsRequiresImagesAndParkedState(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, _, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusParked, walkAlbums(SourceAlbum{ID: "wp-1", Ordinal: 0, Title: "no images"})); err != nil {
		t.Fatal(err)
	}
	activated, err := store.ActivateAlbums(ctx, []string{"wp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if activated != 0 {
		t.Fatalf("activated an album with no images: %d", activated)
	}
	if err := store.SetAlbumImages(ctx, "wp-1", []string{"https://example.test/1.webp", "https://example.test/2.webp"}); err != nil {
		t.Fatal(err)
	}
	activated, err = store.ActivateAlbums(ctx, []string{"wp-1"})
	if err != nil {
		t.Fatal(err)
	}
	if activated != 1 {
		t.Fatalf("activated = %d", activated)
	}
	album, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if album.Expected != 2 {
		t.Fatalf("expected = %d", album.Expected)
	}
	images, err := store.Images(ctx, "wp-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 2 || images[0].SourceURL != "https://example.test/1.webp" {
		t.Fatalf("images=%+v", images)
	}
	// A second click must not pull the running album back into the queue.
	if activated, err := store.ActivateAlbums(ctx, []string{"wp-1"}); err != nil || activated != 0 {
		t.Fatalf("second activate = %d err=%v", activated, err)
	}
}

func TestSetAlbumImagesRefusesNonParkedAlbum(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, _, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusPending,
		walkAlbums(SourceAlbum{ID: "wp-1", Ordinal: 0, Title: "live", URLs: []string{"https://example.test/1.webp"}})); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlbumImages(ctx, "wp-1", []string{"https://example.test/other.webp"}); err == nil {
		t.Fatal("expected an error when rewriting a queued album")
	}
	if err := store.SetAlbumImages(ctx, "wp-missing", []string{"https://example.test/1.webp"}); err == nil {
		t.Fatal("expected an error for an unknown album")
	}
	if err := store.SetAlbumImages(ctx, "wp-1", nil); err == nil {
		t.Fatal("expected an error for an empty image list")
	}
}

func TestSyncAlbumsPreservesExistingStatus(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	album := SourceAlbum{ID: "wp-1", Ordinal: 0, Title: "first", URLs: []string{"https://example.test/1.webp"}}
	if _, _, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusPending, walkAlbums(album)); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNext(ctx)
	if err != nil || !found {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	if err := store.MarkReady(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	album.Title = "renamed"
	if _, _, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusParked, walkAlbums(album)); err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Ready != 1 || stats.Pending != 0 || stats.Parked != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	albums, _, err := store.ListAlbums(ctx, AlbumFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Title != "renamed" {
		t.Fatalf("albums=%+v", albums)
	}
}

func TestListAlbumsFiltersPagesAndTotals(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	albums := []SourceAlbum{
		{ID: "wp-1", Ordinal: 0, Title: "Genshin Impact pack"},
		{ID: "wp-2", Ordinal: 1, Title: "Azur Lane pack"},
		{ID: "wp-3", Ordinal: 2, Title: "NIKKE pack"},
	}
	if _, _, err := store.SyncAlbums(ctx, []string{"-1001"}, SyncStatusParked, walkAlbums(albums...)); err != nil {
		t.Fatal(err)
	}
	page, total, err := store.ListAlbums(ctx, AlbumFilter{Status: SyncStatusParked, Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page) != 2 || page[0].ID != "wp-2" {
		t.Fatalf("total=%d page=%+v", total, page)
	}
	found, total, err := store.ListAlbums(ctx, AlbumFilter{Query: "azur"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(found) != 1 || found[0].ID != "wp-2" {
		t.Fatalf("total=%d found=%+v", total, found)
	}
	// A literal % must stay a literal substring instead of acting like a LIKE wildcard.
	if _, total, err := store.ListAlbums(ctx, AlbumFilter{Query: "%"}); err != nil || total != 0 {
		t.Fatalf("wildcard query total=%d err=%v", total, err)
	}
}

func TestOpenBackfillsColumnsForExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE legacy_albums (
  album_id TEXT PRIMARY KEY,
  ordinal INTEGER NOT NULL UNIQUE,
  title TEXT NOT NULL,
  expected_count INTEGER NOT NULL,
  target_chat_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  retry_count INTEGER NOT NULL DEFAULT 0,
  next_retry_at INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE legacy_images (
  album_id TEXT NOT NULL,
  sort_order INTEGER NOT NULL,
  source_url TEXT NOT NULL,
  width INTEGER NOT NULL DEFAULT 0,
  height INTEGER NOT NULL DEFAULT 0,
  bytes INTEGER NOT NULL DEFAULT 0,
  content_type TEXT NOT NULL DEFAULT '',
  local_path TEXT NOT NULL DEFAULT '',
  tg_url TEXT NOT NULL DEFAULT '',
  tg_file_id TEXT NOT NULL DEFAULT '',
  tg_file_unique_id TEXT NOT NULL DEFAULT '',
  tg_message_id INTEGER NOT NULL DEFAULT 0,
  tg_public_key TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  retry_count INTEGER NOT NULL DEFAULT 0,
  http_status INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (album_id, sort_order)
);
INSERT INTO legacy_albums (album_id, ordinal, title, expected_count, target_chat_id, status, created_at, updated_at)
VALUES ('0000-abc', 0, 'legacy row', 1, '-1001', 'pending', 0, 0);`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	albums, total, err := store.ListAlbums(ctx, AlbumFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(albums) != 1 {
		t.Fatalf("total=%d albums=%+v", total, albums)
	}
	if albums[0].Source != SourceLinuxDO85W || albums[0].CoverURL != "" {
		t.Fatalf("backfilled album = %+v", albums[0])
	}
	// Reopening must stay idempotent rather than failing on a duplicate column.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, total, err := again.ListAlbums(ctx, AlbumFilter{}); err != nil || total != 1 {
		t.Fatalf("reopen total=%d err=%v", total, err)
	}
}
