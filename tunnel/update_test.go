package tunnel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serveReleases(t *testing.T, body string) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/name/releases" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	previous := releasesAPI
	releasesAPI = server.URL
	t.Cleanup(func() { releasesAPI = previous })
}

func TestCheckForUpdateFindsTheNewestAppRelease(t *testing.T) {
	current, _ := parseVersion(Version)
	newer := fmt.Sprintf("v%d.%d.%d", current[0], current[1]+1, 0)
	newest := fmt.Sprintf("v%d.%d.%d", current[0], current[1]+2, 0)
	serveReleases(t, `[
		{"tag_name":"node-1","html_url":"node"},
		{"tag_name":"`+newer+`","html_url":"newer"},
		{"tag_name":"`+newest+`","html_url":"draft","draft":true},
		{"tag_name":"`+newest+`-beta","html_url":"beta"},
		{"tag_name":"v99.0.0","html_url":"pre","prerelease":true},
		{"tag_name":"v`+Version+`","html_url":"same"}
	]`)
	update, err := CheckForUpdate(context.Background(), "owner/name")
	if err != nil {
		t.Fatal(err)
	}
	if update == nil || update.Page != "newer" || "v"+update.Version != newer {
		t.Fatalf("got %+v, want %s", update, newer)
	}
}

func TestCheckForUpdateWithNothingNewer(t *testing.T) {
	serveReleases(t, `[{"tag_name":"node-1"},{"tag_name":"v0.0.1"},{"tag_name":"v`+Version+`"}]`)
	update, err := CheckForUpdate(context.Background(), "owner/name")
	if err != nil || update != nil {
		t.Fatalf("got %+v, %v", update, err)
	}
}

func TestCheckForUpdateReportsHTTPErrors(t *testing.T) {
	serveReleases(t, `[]`)
	if _, err := CheckForUpdate(context.Background(), "other/repo"); err == nil {
		t.Fatal("a missing repository was not reported")
	}
}

func TestVersionOrder(t *testing.T) {
	for _, pair := range [][2]string{{"0.1.0", "0.1.1"}, {"0.9.9", "0.10.0"}, {"1.2.3", "2.0.0"}} {
		a, _ := parseVersion(pair[0])
		b, _ := parseVersion(pair[1])
		if !versionLess(a, b) || versionLess(b, a) {
			t.Errorf("%s should come before %s", pair[0], pair[1])
		}
	}
}
