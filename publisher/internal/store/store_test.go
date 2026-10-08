package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
)

func TestUpsertGalleriesBatch(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	items := []model.GallerySummary{
		{ID: 10, Title: "ten", ImageCount: 1, UploadedImages: 1, CoverImageID: 100},
		{ID: 11, Title: "eleven", ImageCount: 1, UploadedImages: 1, CoverImageID: 101},
	}
	if err := database.UpsertGalleries(ctx, items); err != nil {
		t.Fatal(err)
	}
	gallery, found, err := database.ClaimNext(ctx, false)
	if err != nil || !found || gallery.SourceGalleryID != 11 {
		t.Fatalf("batch was not committed: gallery=%+v found=%v err=%v", gallery, found, err)
	}
}

func TestUpsertGalleriesPreservesProcessingStatus(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	item := model.GallerySummary{
		ID: 12, Title: "processing", ImageCount: 10, UploadedImages: 10,
		CoverImageID: 120, UpdatedAt: "2026-01-01",
	}
	if err := database.UpsertGallery(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, found, err := database.ClaimNext(ctx, false); err != nil || !found {
		t.Fatalf("initial claim failed: found=%v err=%v", found, err)
	}

	item.Title = "rediscovered"
	item.UploadedImages = 9
	item.CoverImageID = 121
	item.UpdatedAt = "2026-01-02"
	if err := database.UpsertGalleries(ctx, []model.GallerySummary{item}); err != nil {
		t.Fatal(err)
	}

	var status string
	if err := database.db.QueryRowContext(ctx,
		"SELECT status FROM galleries WHERE source_gallery_id = ?", item.ID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "processing" {
		t.Fatalf("rediscovery changed processing status to %q", status)
	}
	if _, found, err := database.ClaimNext(ctx, false); err != nil || found {
		t.Fatalf("processing gallery became claimable: found=%v err=%v", found, err)
	}
}

func TestOpenDoesNotRecoverProcessing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "publisher.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	item := model.GallerySummary{ID: 13, Title: "claimed", ImageCount: 1, UploadedImages: 1}
	if err := database.UpsertGallery(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, found, err := database.ClaimNext(ctx, false); err != nil || !found {
		t.Fatalf("initial claim failed: found=%v err=%v", found, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var status string
	if err := database.db.QueryRowContext(ctx,
		"SELECT status FROM galleries WHERE source_gallery_id = ?", item.ID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "processing" {
		t.Fatalf("opening database changed processing status to %q", status)
	}
	if err := database.RecoverProcessing(ctx); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx,
		"SELECT status FROM galleries WHERE source_gallery_id = ?", item.ID,
	).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "incomplete" {
		t.Fatalf("explicit recovery left status as %q", status)
	}
}

func TestFailedGalleryIsQuarantinedAndCanBeRetried(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	item := model.GallerySummary{ID: 20, Title: "retry", ImageCount: 1, UploadedImages: 1, CoverImageID: 200}
	if err := database.UpsertGallery(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, found, err := database.ClaimNext(ctx, false); err != nil || !found {
		t.Fatalf("initial claim failed: found=%v err=%v", found, err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		quarantined, err := database.MarkGalleryFailure(ctx, 20, "broken", 0, 3)
		if err != nil {
			t.Fatal(err)
		}
		if quarantined != (attempt == 3) {
			t.Fatalf("attempt %d quarantined=%v", attempt, quarantined)
		}
		if attempt < 3 {
			if _, found, err := database.ClaimNext(ctx, false); err != nil || !found {
				t.Fatalf("retry claim %d failed: found=%v err=%v", attempt, found, err)
			}
		}
	}
	if _, found, err := database.ClaimNext(ctx, false); err != nil || found {
		t.Fatalf("quarantined gallery was claimable: found=%v err=%v", found, err)
	}
	count, err := database.RetryFailed(ctx)
	if err != nil || count != 1 {
		t.Fatalf("retry failed returned count=%d err=%v", count, err)
	}
	if _, found, err := database.ClaimNext(ctx, false); err != nil || !found {
		t.Fatalf("released gallery was not claimable: found=%v err=%v", found, err)
	}
}

func TestGalleryCheckpointAndPayload(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	summary := model.GallerySummary{
		ID: 21674, Title: "album", Category: "Cosplay", ImageCount: 2,
		UploadedImages: 2, CoverImageID: 1191867, UpdatedAt: "2026-01-01",
	}
	if err := database.UpsertGallery(ctx, summary); err != nil {
		t.Fatal(err)
	}
	gallery, found, err := database.ClaimNext(ctx, false)
	if err != nil || !found {
		t.Fatalf("claim failed: found=%v err=%v", found, err)
	}
	gallery.Tags = []string{"Cosplay", "model"}
	refs := map[int]model.Image{
		1: {SourceImageID: 1191867, SortOrder: 1, Width: 1200, Height: 1800},
		2: {SourceImageID: 1191868, SortOrder: 2, Width: 1200, Height: 1800},
	}
	if err := database.ApplyDetail(ctx, gallery, refs); err != nil {
		t.Fatal(err)
	}
	for order, imageID := range []int64{1191867, 1191868} {
		if err := database.MarkImageUploaded(ctx, imageID, "https://img.example/file/"+string(rune('a'+order)), "file", "unique", "key", "image/jpeg", int64(order+1), 1200, 1800); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.BackfillUploadedChannels(ctx, "-100123"); err != nil {
		t.Fatal(err)
	}
	assigned, err := database.AssignTargetChat(ctx, 21674, "-100999")
	if err != nil {
		t.Fatal(err)
	}
	if assigned != "-100123" {
		t.Fatalf("historical channel changed to %q", assigned)
	}
	payload, err := database.Payload(ctx, 21674, "-100123")
	if err != nil {
		t.Fatal(err)
	}
	if payload.ID != "veil-21674" || payload.Href != "/album/veil-21674" || payload.Count != 2 || len(payload.Photos) != 2 || len(payload.TGFiles) != 2 || len(payload.Tags) != 2 || payload.TGFiles[0].FileID != "file" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestSnapshotCandidatesTrackCompletedGalleryChanges(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	summary := model.GallerySummary{
		ID: 30, Title: "snapshot", ImageCount: 1, UploadedImages: 1,
		CoverImageID: 300, UpdatedAt: "2026-01-01",
	}
	if err := database.UpsertGallery(ctx, summary); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkGallery(ctx, summary.ID, "ready", "", 0); err != nil {
		t.Fatal(err)
	}

	candidates, err := database.PendingSnapshots(ctx, 10)
	if err != nil || len(candidates) != 1 || candidates[0].GalleryID != summary.ID {
		t.Fatalf("unexpected candidates: %+v err=%v", candidates, err)
	}
	if err := database.MarkSnapshotsExported(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	candidates, err = database.PendingSnapshots(ctx, 10)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("exported gallery returned again: %+v err=%v", candidates, err)
	}

	if _, err := database.db.ExecContext(ctx, `
UPDATE galleries SET title = 'snapshot updated', updated_at = updated_at + 1
WHERE source_gallery_id = ?
`, summary.ID); err != nil {
		t.Fatal(err)
	}
	candidates, err = database.PendingSnapshots(ctx, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("updated gallery was not exported again: %+v err=%v", candidates, err)
	}
}
