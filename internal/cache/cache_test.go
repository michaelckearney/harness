package cache

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
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
