package app

import (
	"testing"

	"github.com/TyrEamon/xrw-album/publisher/internal/model"
	"github.com/TyrEamon/xrw-album/publisher/internal/veil"
)

func TestNormalizeDetailUsesFirstOrderedImageAsMissingCover(t *testing.T) {
	detail := veil.GalleryDetail{
		ID:         10496,
		ImageCount: 2,
		Images: []veil.ImageRef{
			{ID: 20, SortOrder: 2},
			{ID: 19, SortOrder: 1},
		},
	}

	normalizeDetail(&detail)

	if detail.CoverImageID != 19 {
		t.Fatalf("cover image = %d, want 19", detail.CoverImageID)
	}
}

func TestNormalizeDetailPreservesExplicitCover(t *testing.T) {
	detail := veil.GalleryDetail{
		CoverImageID: 7,
		Images:       []veil.ImageRef{{ID: 9, SortOrder: 1}},
	}

	normalizeDetail(&detail)

	if detail.CoverImageID != 7 {
		t.Fatalf("cover image = %d, want 7", detail.CoverImageID)
	}
}

func TestUnsupportedVideoUsesCategory(t *testing.T) {
	if !isUnsupportedVideo(model.Gallery{Category: " Video "}) {
		t.Fatal("video category should be unsupported")
	}
	if isUnsupportedVideo(model.Gallery{Title: "[Video] label only", Category: "Cosplay"}) {
		t.Fatal("non-video category should remain eligible")
	}
}
