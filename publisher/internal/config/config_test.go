package config

import (
	"strings"
	"testing"
)

func TestChatForGalleryIsStable(t *testing.T) {
	cfg := Config{TGChatIDs: []string{"channel-0", "channel-1", "channel-2"}}
	if got := cfg.ChatForGallery(7); got != "channel-1" {
		t.Fatalf("gallery 7 assigned to %q", got)
	}
	if got := cfg.ChatForGallery(7); got != "channel-1" {
		t.Fatalf("gallery assignment changed to %q", got)
	}
}

func TestChatForGalleryWithoutChannels(t *testing.T) {
	if got := (Config{}).ChatForGallery(7); got != "" {
		t.Fatalf("unexpected channel %q", got)
	}
}

func TestWordPressSitesParsesSeveralSites(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", strings.Join([]string{
		"https://www.a.example.com/",
		"second|https://b.example.com/some/path",
		"third|Third Site|https://c.example.com",
	}, "\n"))

	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 3 {
		t.Fatalf("expected three sites, got %+v", sites)
	}
	// A bare URL derives both its id and its display name from the host, and the
	// path is dropped so the REST client cannot be pointed somewhere else.
	if sites[0].ID != "a-example-com" || sites[0].Name != "a-example-com" || sites[0].BaseURL != "https://www.a.example.com" {
		t.Fatalf("bare entry resolved wrong: %+v", sites[0])
	}
	if sites[1].ID != "second" || sites[1].Name != "second" || sites[1].BaseURL != "https://b.example.com" {
		t.Fatalf("id|url entry resolved wrong: %+v", sites[1])
	}
	if sites[2].ID != "third" || sites[2].Name != "Third Site" || sites[2].BaseURL != "https://c.example.com" {
		t.Fatalf("id|name|url entry resolved wrong: %+v", sites[2])
	}
	if !(Config{WordPressSites: sites}).WordPressEnabled() {
		t.Fatalf("configured sites must enable the region")
	}
}

func TestWordPressSitesAcceptsACommaSeparatedList(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "one|https://a.example, two|https://b.example")
	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 || sites[0].ID != "one" || sites[1].ID != "two" {
		t.Fatalf("comma separated entries were not split: %+v", sites)
	}
}

func TestWordPressSitesFallsBackToTheSingleBase(t *testing.T) {
	t.Setenv("XRW_WP_SITES", "")
	t.Setenv("XRW_WP_BASE", "https://solo.example")
	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0].ID != "solo-example" || sites[0].BaseURL != "https://solo.example" {
		t.Fatalf("XRW_WP_BASE must keep working on its own: %+v", sites)
	}
}

func TestWordPressSitesDefaultsToTheBundledSite(t *testing.T) {
	t.Setenv("XRW_WP_SITES", "")
	t.Setenv("XRW_WP_BASE", "")
	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 1 || sites[0].BaseURL != defaultWordPressBase {
		t.Fatalf("expected the default site, got %+v", sites)
	}
}

func TestWordPressSitesDisabled(t *testing.T) {
	for _, value := range []string{"off", "none", "-"} {
		t.Setenv("XRW_WP_SITES", "")
		t.Setenv("XRW_WP_BASE", value)
		sites, err := wordpressSites()
		if err != nil || len(sites) != 0 {
			t.Fatalf("%q should disable the region, got %+v err=%v", value, sites, err)
		}
	}
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "off")
	if sites, err := wordpressSites(); err != nil || len(sites) != 0 {
		t.Fatalf("XRW_WP_SITES=off should disable the region, got %+v err=%v", sites, err)
	}
}

func TestWordPressSitesRejectsAMissingURL(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "not-a-url")
	if _, err := wordpressSites(); err == nil {
		t.Fatalf("a malformed entry must be reported instead of silently dropped")
	}
	t.Setenv("XRW_WP_SITES", "ftp://a.example")
	if _, err := wordpressSites(); err == nil {
		t.Fatalf("a non-http scheme must be reported")
	}
}

func TestWordPressSitesDisambiguatesDuplicateIDs(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "same|https://a.example\nsame|https://b.example")
	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	// Two tenants on one host are legitimate, so the second gets a suffix rather
	// than losing its entry to a directory-name collision.
	if len(sites) != 2 || sites[0].ID != "same" || sites[1].ID != "same-2" {
		t.Fatalf("duplicate ids were not disambiguated: %+v", sites)
	}
}

func TestWordPressSitesReadsTheImageSource(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "misskon|MissKon|https://misskon.com|content,plain|https://plain.example")
	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("expected two sites, got %+v", sites)
	}
	// A site that publishes its gallery inside the post body has to say so;
	// every other site keeps reading the media endpoint.
	if sites[0].ImageSource != "content" {
		t.Fatalf("the image source was not read: %+v", sites[0])
	}
	if sites[0].ID != "misskon" || sites[0].Name != "MissKon" || sites[0].BaseURL != "https://misskon.com" {
		t.Fatalf("the other fields shifted: %+v", sites[0])
	}
	if sites[1].ImageSource != "media" {
		t.Fatalf("the default image source should be media: %+v", sites[1])
	}
}

func TestWordPressSitesReadsTheAcgmhnEngine(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "acgmhn|ACG漫画|https://www.acgmhn.com|acgmhn,cosplaytele-com|Cosplaytele|https://cosplaytele.com")
	sites, err := wordpressSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("expected two sites, got %+v", sites)
	}
	// A site that publishes no API at all names its reader the same way a
	// WordPress site names its image source.
	if sites[0].ImageSource != "acgmhn" {
		t.Fatalf("the acgmhn engine was not read: %+v", sites[0])
	}
	if sites[0].BaseURL != "https://www.acgmhn.com" {
		t.Fatalf("the other fields shifted: %+v", sites[0])
	}
	if sites[1].ImageSource != "media" {
		t.Fatalf("an entry without an engine should stay on media: %+v", sites[1])
	}
}

func TestWordPressSitesRejectsAnUnknownImageSource(t *testing.T) {
	t.Setenv("XRW_WP_BASE", "")
	t.Setenv("XRW_WP_SITES", "misskon|https://misskon.com|gallery")
	if _, err := wordpressSites(); err == nil {
		t.Fatalf("a typo in the image source must be reported, not silently ignored")
	}
	t.Setenv("XRW_WP_SITES", "misskon|MissKon|https://misskon.com|content|extra")
	if _, err := wordpressSites(); err == nil {
		t.Fatalf("a fifth field must be reported")
	}
}
