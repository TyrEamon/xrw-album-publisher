package app

import (
	"strings"
	"testing"
)

func TestGalleryCaption(t *testing.T) {
	caption := galleryCaption(
		"Sword Maiden < Goblin & Slaver",
		[]string{"COS", "水淼 Aqua", "#情趣", "COS", ""},
	)
	expected := "#COS #水淼_Aqua #情趣\n<blockquote>Sword Maiden &lt; Goblin &amp; Slaver</blockquote>"
	if caption != expected {
		t.Fatalf("unexpected caption:\n%s", caption)
	}
}

func TestGalleryCaptionWithoutTags(t *testing.T) {
	caption := galleryCaption("Album", nil)
	if caption != "<blockquote>Album</blockquote>" {
		t.Fatalf("unexpected caption %q", caption)
	}
}

func TestGalleryCaptionIsBounded(t *testing.T) {
	caption := galleryCaption(strings.Repeat("长标题", 1000), []string{strings.Repeat("标签", 1000)})
	if len([]rune(caption)) > 930 {
		t.Fatalf("caption is too long: %d runes", len([]rune(caption)))
	}
}
