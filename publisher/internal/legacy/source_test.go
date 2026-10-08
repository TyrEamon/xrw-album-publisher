package legacy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSourceMatchesExistingAlbumIDs(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "source.txt")
	data := "PIA-피아-LEEHEE-EXPRESS-LERB-216B-Set01-04-06\n" +
		"https://telegra.ph/file/46dfd9b00f6efebb85b04.jpg\n" +
		"https://telegra.ph/file/c81d8eeae6734ab4d37d5.jpg\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	var albums []SourceAlbum
	if err := ParseSource(path, func(album SourceAlbum) error {
		albums = append(albums, album)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 {
		t.Fatalf("albums = %d", len(albums))
	}
	if albums[0].ID != "0000-c74f7797eb" {
		t.Fatalf("id = %q", albums[0].ID)
	}
	if len(albums[0].URLs) != 2 {
		t.Fatalf("urls = %d", len(albums[0].URLs))
	}
}

func TestParseSourceHashesRawCoverAndNormalizesStoredURL(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "source.txt")
	raw := "https://telegra.phhttps://legra.ph/file/a.jpg"
	if err := os.WriteFile(path, []byte("album\n"+raw+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got SourceAlbum
	if err := ParseSource(path, func(album SourceAlbum) error { got = album; return nil }); err != nil {
		t.Fatal(err)
	}
	if got.ID != sourceAlbumID(0, "album", raw) {
		t.Fatalf("id = %q", got.ID)
	}
	if got.URLs[0] != "https://telegra.ph/file/a.jpg" {
		t.Fatalf("url = %q", got.URLs[0])
	}
}
