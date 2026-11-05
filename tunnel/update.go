package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// releasesAPI is GitHub's API root; tests point it at a local server.
var releasesAPI = "https://api.github.com"

// Update is a newer release of the apps than the running one.
type Update struct {
	Version string `json:"version"`
	Page    string `json:"page"`
}

// CheckForUpdate looks at the releases of repo ("owner/name") and returns the newest app release when it is newer than Version.
// App releases are tagged like v1.2.3; other tags in the same repository, such as the node's, are skipped.
func CheckForUpdate(ctx context.Context, repo string) (*Update, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI+"/repos/"+repo+"/releases?per_page=30", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("releases: HTTP %d", response.StatusCode)
	}
	var releases []struct {
		Tag        string `json:"tag_name"`
		Page       string `json:"html_url"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(response.Body).Decode(&releases); err != nil {
		return nil, err
	}
	current, _ := parseVersion(Version)
	var newest *Update
	newestParts := current
	for _, release := range releases {
		parts, ok := parseVersion(release.Tag)
		if !ok || release.Draft || release.Prerelease || !versionLess(newestParts, parts) {
			continue
		}
		newest, newestParts = &Update{Version: strings.TrimPrefix(release.Tag, "v"), Page: release.Page}, parts
	}
	return newest, nil
}

// parseVersion reads "v1.2.3" or "1.2.3".
func parseVersion(text string) ([3]int, bool) {
	var parts [3]int
	fields := strings.Split(strings.TrimPrefix(text, "v"), ".")
	if len(fields) != 3 {
		return parts, false
	}
	for i, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil || number < 0 {
			return parts, false
		}
		parts[i] = number
	}
	return parts, true
}

func versionLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
