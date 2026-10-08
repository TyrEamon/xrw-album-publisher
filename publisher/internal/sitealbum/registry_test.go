package sitealbum

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TyrEamon/xrw-album/publisher/internal/legacy"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// secondSite is a one album site, so two indexes can never be confused for one
// another in a test.
func secondSite(t *testing.T) *fakeSite {
	t.Helper()
	site := newFakeSite(t)
	site.posts = []fakePost{{
		ID: 300, Slug: "gamma", Link: "https://example.com/gamma/",
		Date: "2026-08-01T10:00:00", Modified: "2026-08-01T10:00:00",
		Title: rendered{Rendered: "Gamma cosplay 1 photos"}, FeaturedMedia: 3001,
	}}
	site.media[300] = []fakeMedia{
		{3001, "gamma-cosplay-1_result", site.server.URL + "/uploads/gamma-1.webp", "image/webp"},
	}
	return site
}

func TestRegistryKeepsOneIndexPerSite(t *testing.T) {
	alpha := sampleSite(t)
	beta := secondSite(t)
	directory := t.TempDir()
	registry, err := NewRegistry(directory, []Site{
		{ID: "alpha", Name: "Alpha", BaseURL: alpha.server.URL},
		{ID: "beta", BaseURL: beta.server.URL},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.RefreshAll(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	one, err := registry.Target("alpha")
	if err != nil {
		t.Fatal(err)
	}
	albums, total, err := one.Store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(albums) != 2 {
		t.Fatalf("site alpha should cache its own two albums, got %+v", albums)
	}

	two, err := registry.Target("beta")
	if err != nil {
		t.Fatal(err)
	}
	albums, total, err = two.Store.List(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(albums) != 1 || albums[0].ID != 300 {
		t.Fatalf("site beta should cache only its own album, got %+v", albums)
	}

	// Each site must own a separate file, or a refresh of one would erase the
	// other's index.
	if one.Store.path == two.Store.path {
		t.Fatalf("two sites share the index file %q", one.Store.path)
	}
	if base := filepath.Base(filepath.Dir(one.Store.path)); base != "alpha" {
		t.Fatalf("site alpha should cache under its own id, got %q", base)
	}
	if one.Site.BaseURL != alpha.server.URL || two.Site.BaseURL != beta.server.URL {
		t.Fatalf("targets carry the wrong sites: %+v %+v", one.Site, two.Site)
	}

	statuses := registry.Status()
	if len(statuses) != 2 {
		t.Fatalf("every site needs a status, got %+v", statuses)
	}
	if statuses[0].ID != "alpha" || statuses[0].Name != "Alpha" || statuses[0].Total != 2 {
		t.Fatalf("first status is wrong: %+v", statuses[0])
	}
	// A site without an explicit name falls back to its id.
	if statuses[1].ID != "beta" || statuses[1].Name != "beta" || statuses[1].Total != 1 {
		t.Fatalf("second status is wrong: %+v", statuses[1])
	}

	sites := registry.Sites()
	if len(sites) != 2 || sites[0].ID != "alpha" || sites[1].ID != "beta" {
		t.Fatalf("sites must keep configuration order: %+v", sites)
	}
}

func TestRegistryResolvesTheDefaultAndRejectsUnknownSites(t *testing.T) {
	alpha := sampleSite(t)
	beta := secondSite(t)
	registry, err := NewRegistry(t.TempDir(), []Site{
		{ID: "alpha", BaseURL: alpha.server.URL},
		{ID: "beta", BaseURL: beta.server.URL},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	target, err := registry.Target("")
	if err != nil {
		t.Fatal(err)
	}
	if target.Site.ID != "alpha" {
		t.Fatalf("an empty id must mean the first site, got %q", target.Site.ID)
	}
	if _, err := registry.Target("gamma"); err == nil {
		t.Fatalf("an unknown site must be an error, not a silent fallback")
	}
}

func TestRegistryRejectsDuplicateIDs(t *testing.T) {
	site := sampleSite(t)
	_, err := NewRegistry(t.TempDir(), []Site{
		{ID: "same", BaseURL: site.server.URL},
		{ID: "same", BaseURL: site.server.URL},
	}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "configured twice") {
		t.Fatalf("a duplicated id must be reported, got %v", err)
	}
}

// TestRegistryAdoptsTheLegacyIndex guards the upgrade path: before sites had ids
// the cache sat directly in the parent directory, and re-walking a 5000 album
// site on first boot would be a needless load on the origin.
func TestRegistryAdoptsTheLegacyIndex(t *testing.T) {
	site := sampleSite(t)
	directory := t.TempDir()
	if err := newTestStore(t, directory, site).Refresh(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, indexFileName)); err != nil {
		t.Fatalf("the legacy index was not written: %v", err)
	}

	registry, err := NewRegistry(directory, []Site{{ID: "alpha", BaseURL: site.server.URL}}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	// The origin is unreachable from here on, so the albums can only come from the
	// adopted file.
	site.broken = true
	target, err := registry.Target("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if total := target.Store.Status().Total; total != 2 {
		t.Fatalf("the previous index was not adopted, got %d albums", total)
	}
	if _, err := os.Stat(filepath.Join(directory, indexFileName)); err == nil {
		t.Fatalf("the flat index should have moved into the site directory")
	}
}

func TestRegistryRefreshAllNamesTheFailingSite(t *testing.T) {
	alpha := sampleSite(t)
	beta := secondSite(t)
	beta.broken = true
	registry, err := NewRegistry(t.TempDir(), []Site{
		{ID: "alpha", BaseURL: alpha.server.URL},
		{ID: "beta", BaseURL: beta.server.URL},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	err = registry.RefreshAll(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("the failure must name the site that broke, got %v", err)
	}
	// A broken site must not stop the healthy ones from building.
	target, err := registry.Target("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if total := target.Store.Status().Total; total != 2 {
		t.Fatalf("site alpha should still be indexed, got %d albums", total)
	}
}

// TestRegistryCarriesTheImageSource pins the per-site read strategy: it has to
// survive from the configuration into the source that fetches the galleries.
func TestRegistryCarriesTheImageSource(t *testing.T) {
	alpha := sampleSite(t)
	registry, err := NewRegistry(t.TempDir(), []Site{
		{ID: "alpha", Name: "Alpha", BaseURL: alpha.server.URL},
		{ID: "hotlink", Name: "Hotlink", BaseURL: alpha.server.URL, ImageSource: ImageSourceContent},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}

	attachments, err := registry.Target("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if source := wordpressSource(t, attachments); source.ImageSource == ImageSourceContent {
		t.Fatalf("a site that does not ask for it must keep reading the media endpoint")
	}

	body, err := registry.Target("hotlink")
	if err != nil {
		t.Fatal(err)
	}
	if source := wordpressSource(t, body); source.ImageSource != ImageSourceContent {
		t.Fatalf("the image source did not reach the source: %q", source.ImageSource)
	}
	if body.Site.ImageSource != ImageSourceContent {
		t.Fatalf("the site should report its image source: %q", body.Site.ImageSource)
	}
}

// wordpressSource unwraps a target's reading strategy, failing when it is not the
// WordPress one a test wired up.
func wordpressSource(t *testing.T, target Target) *legacy.WordPressSource {
	t.Helper()
	source, ok := target.Source.(*legacy.WordPressSource)
	if !ok {
		t.Fatalf("expected a WordPress source, got %T", target.Source)
	}
	return source
}

// TestRegistryPicksTheReadingStrategy pins the engine selector: a site that names
// a reader other than WordPress gets that one, and still gets its own cache.
func TestRegistryPicksTheReadingStrategy(t *testing.T) {
	alpha := sampleSite(t)
	registry, err := NewRegistry(t.TempDir(), []Site{
		{ID: "acgmhn", Name: "Acgmhn", BaseURL: alpha.server.URL, ImageSource: EngineAcgmhn},
	}, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	target, err := registry.Target("acgmhn")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target.Source.(*legacy.AcgmhnSource); !ok {
		t.Fatalf("the acgmhn engine should build the HTML reader, got %T", target.Source)
	}
	if target.Store == nil {
		t.Fatal("a non-WordPress site still needs its own index cache")
	}
	if target.Site.ImageSource != EngineAcgmhn {
		t.Fatalf("the site should report its engine: %q", target.Site.ImageSource)
	}
}

// TestImagesOutsideBase pins which strategies need the page to allow a bare
// https: image source: only the media endpoint keeps its covers on the site.
func TestImagesOutsideBase(t *testing.T) {
	for _, engine := range []string{"", ImageSourceMedia} {
		if ImagesOutsideBase(engine) {
			t.Fatalf("%q keeps its covers on the site itself", engine)
		}
	}
	for _, engine := range []string{ImageSourceContent, EngineAcgmhn} {
		if !ImagesOutsideBase(engine) {
			t.Fatalf("%q hotlinks its covers from another host", engine)
		}
	}
}
