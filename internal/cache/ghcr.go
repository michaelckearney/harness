package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// githubAPI is GitHub's REST API; tests point it elsewhere.
var githubAPI = "https://api.github.com"

// pruneGHCR deletes the package version old, which a save to ref has just
// replaced, when ref is on GHCR: each save to a tag leaves the last one
// untagged, and GHCR keeps untagged versions, and counts them, until they are
// deleted. Only that version goes: other untagged ones, such as the parts of
// the framework's multi-platform image, are not the cache's to delete. GHCR
// has no registry API for it, so this one step is GitHub's, with GH_TOKEN.
func pruneGHCR(ctx context.Context, ref, old, token string) error {
	rest, ok := strings.CutPrefix(ref, "ghcr.io/")
	if !ok || old == "" || token == "" {
		return nil
	}
	rest, _, _ = strings.Cut(rest, ":")
	owner, pkg, ok := strings.Cut(rest, "/")
	if !ok {
		return nil
	}
	pkg = url.PathEscape(pkg)
	for _, kind := range []string{"orgs", "users"} {
		base := fmt.Sprintf("%s/%s/%s/packages/container/%s/versions", githubAPI, kind, owner, pkg)
		id, found, err := findVersion(ctx, base, old, token)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		resp, err := githubDo(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", base, id), token)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("deleting the cache's last version: %s", resp.Status)
		}
		return nil
	}
	return nil
}

// findVersion is the id of the package version named digest, among the
// newest ones: the version a save has just replaced is one of them.
func findVersion(ctx context.Context, base, digest, token string) (int64, bool, error) {
	for page := 1; page <= 10; page++ {
		resp, err := githubDo(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", base, page), token)
		if err != nil {
			return 0, false, err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return 0, false, nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return 0, false, fmt.Errorf("listing the cache's versions: %s", resp.Status)
		}
		var versions []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		}
		err = json.NewDecoder(resp.Body).Decode(&versions)
		resp.Body.Close()
		if err != nil {
			return 0, false, err
		}
		for _, v := range versions {
			if v.Name == digest {
				return v.ID, true, nil
			}
		}
		if len(versions) < 100 {
			return 0, false, nil
		}
	}
	return 0, false, nil
}

func githubDo(ctx context.Context, method, url, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if method == http.MethodDelete {
		io.Copy(io.Discard, resp.Body)
	}
	return resp, nil
}
