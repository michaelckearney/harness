package cache

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPruneGHCR(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/orgs/owner/packages/container/repo/versions":
			http.NotFound(w, r) // a user's package, not an org's
		case r.Method == http.MethodGet && r.URL.Path == "/users/owner/packages/container/repo/versions":
			fmt.Fprint(w, `[{"id": 1, "name": "sha256:new"}, {"id": 2, "name": "sha256:old"}, {"id": 3, "name": "sha256:other"}]`)
		case r.Method == http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = "https://api.github.com" })

	if err := pruneGHCR(context.Background(), "ghcr.io/owner/repo:ci-cache-test", "sha256:old", "tok"); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "/users/owner/packages/container/repo/versions/2" {
		t.Fatalf("deleted %v, want only the old version", deleted)
	}

	// Another registry, or no token: nothing to do.
	deleted = nil
	for _, c := range []struct{ ref, token string }{
		{"registry.example.com/owner/repo:ci-cache-test", "tok"},
		{"ghcr.io/owner/repo:ci-cache-test", ""},
	} {
		if err := pruneGHCR(context.Background(), c.ref, "sha256:old", c.token); err != nil {
			t.Fatal(err)
		}
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted %v", deleted)
	}
}
