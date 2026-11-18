package cache

import (
	"context"
	"crypto/rand"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/michaelckearney/harness/internal/retry"
)

func TestRef(t *testing.T) {
	if got := Ref("ghcr.io/owner/repo:ci-1a2b3c", "test"); got != "ghcr.io/owner/repo:ci-cache-test" {
		t.Fatalf("Ref = %s", got)
	}
}

func TestSaveRestore(t *testing.T) {
	srv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/owner/repo:ci-cache-test"
	ctx := context.Background()

	// Nothing there yet: a cold start, not an error.
	if err := Restore(ctx, ref, t.TempDir(), io.Discard); err != nil {
		t.Fatalf("Restore with no cache: %v", err)
	}

	src := t.TempDir()
	mod := filepath.Join(src, "home%ci%go%pkg%mod", "example.com", "m@v1")
	must(t, os.MkdirAll(mod, 0o755))
	must(t, os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module m\n"), 0o444))
	must(t, os.Symlink("go.mod", filepath.Join(mod, "link")))
	// Go keeps its module cache read-only.
	must(t, os.Chmod(mod, 0o555))
	t.Cleanup(func() { os.Chmod(mod, 0o755) })
	if err := Save(ctx, ref, src, io.Discard); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dst := t.TempDir()
	if err := Restore(ctx, ref, dst, io.Discard); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := filepath.Join(dst, "home%ci%go%pkg%mod", "example.com", "m@v1")
	b, err := os.ReadFile(filepath.Join(got, "link"))
	if err != nil || string(b) != "module m\n" {
		t.Fatalf("restored link reads %q, %v", b, err)
	}
	// A later run can add to a restored folder.
	must(t, os.WriteFile(filepath.Join(got, "new"), nil, 0o644))
	// And restore over what is there.
	if err := Restore(ctx, ref, dst, io.Discard); err != nil {
		t.Fatalf("Restore again: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// flaky is a registry whose blob downloads are cut off part way, the first
// cut times, and whose uploads are denied, while deny is set.
type flaky struct {
	h    http.Handler
	cut  atomic.Int32
	deny atomic.Bool
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.deny.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"errors":[{"code":"DENIED","message":"denied"}]}`)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") && f.cut.Add(-1) >= 0 {
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, r)
		body := rec.Body.Bytes()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(rec.Code)
		w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // the connection reset part way
	}
	f.h.ServeHTTP(w, r)
}

func TestRestoreCutOff(t *testing.T) {
	retry.Wait = time.Millisecond
	f := &flaky{h: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/owner/repo:ci-cache-test"
	ctx := context.Background()

	src := t.TempDir()
	// Big enough that half of it is cut off mid-file.
	big := make([]byte, 1<<20)
	rand.Read(big)
	must(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	must(t, os.WriteFile(filepath.Join(src, "a", "b", "big"), big, 0o644))
	must(t, Save(ctx, ref, src, io.Discard))

	// Cut off once: tried again from the start, into an empty folder.
	f.cut.Store(1)
	dst := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dst, "stale"), nil, 0o644))
	var out strings.Builder
	if err := Restore(ctx, ref, dst, &out); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "a", "b", "big")); err != nil || len(b) != len(big) {
		t.Fatalf("restored %d bytes, %v\n%s", len(b), err, out.String())
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); err == nil {
		t.Errorf("the restore kept what was there before it")
	}
	if !strings.Contains(out.String(), "harness: retrying restoring the cache") {
		t.Errorf("no retry said:\n%s", out.String())
	}

	// Cut off every time: a cold start, in an empty folder, with a warning.
	f.cut.Store(100)
	dst = t.TempDir()
	out.Reset()
	if err := Restore(ctx, ref, dst, &out); err != nil {
		t.Fatalf("Restore cut off every time: %v", err)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Errorf("a cold start left %d entries", len(entries))
	}
	if !strings.Contains(out.String(), "::warning::harness: unpacking the cache") || !strings.Contains(out.String(), "starting cold") {
		t.Errorf("no warning:\n%s", out.String())
	}
}

func TestSaveDenied(t *testing.T) {
	retry.Wait = time.Millisecond
	f := &flaky{h: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	f.deny.Store(true)
	srv := httptest.NewServer(f)
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/owner/repo:ci-cache-test"
	var out strings.Builder
	err := Save(context.Background(), ref, t.TempDir(), &out)
	if err == nil || !strings.Contains(err.Error(), "publishing the cache") {
		t.Fatalf("Save denied = %v", err)
	}
	if n := strings.Count(out.String(), "harness: retrying publishing the cache"); n != retry.Attempts-1 {
		t.Errorf("%d retries:\n%s", n, out.String())
	}
}
