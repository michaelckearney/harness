package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/michaelckearney/harness/internal/retry"
)

// A cache the registry will not take is a warning, not the run's result.
func TestCISaveOnlyWarns(t *testing.T) {
	retry.Wait = time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"errors":[{"code":"DENIED","message":"denied"}]}`)
	}))
	defer srv.Close()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	ref := strings.TrimPrefix(srv.URL, "http://") + "/owner/repo:ci-cache-test"
	var out strings.Builder
	ciSave(context.Background(), ref, t.TempDir(), &out)
	if !strings.Contains(out.String(), "::warning::harness: publishing the cache "+ref) {
		t.Errorf("no warning:\n%s", out.String())
	}
}
