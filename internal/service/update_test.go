package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/michaelckearney/harness/internal/deploy"
)

// release serves harness releases: each version's binary is a script that
// says its version, and accepts any configuration unless it is v9.0.1.
func release(t *testing.T) {
	t.Helper()
	file := "harness-" + runtime.GOOS + "-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if strings.HasPrefix(v, "v9.3") {
			http.NotFound(w, r)
			return
		}
		validate := "exit 0"
		if v == "v9.0.1" {
			validate = "echo no such key >&2; exit 1"
		}
		bin := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in version) echo %s ;; serve) %s ;; esac\n", v, validate)
		sum := sha256.Sum256([]byte(bin))
		switch name {
		case "SHA256SUMS":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), file)
		case file:
			fmt.Fprint(w, bin)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := deploy.Releases
	deploy.Releases = srv.URL + "/"
	t.Cleanup(func() { deploy.Releases = old })
}

func TestInstall(t *testing.T) {
	release(t)
	dir := t.TempDir()
	exe := filepath.Join(dir, "harness")
	os.WriteFile(exe, []byte("#!/bin/sh\necho v9.0.0\n"), 0o755)
	ctx := context.Background()

	if err := install(ctx, exe, "v9.1.0", "host.yaml"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "echo v9.1.0") {
		t.Errorf("the binary is now:\n%s", got)
	}
	if got, _ := os.ReadFile(exe + ".previous"); !strings.Contains(string(got), "echo v9.0.0") {
		t.Errorf("the previous binary is:\n%s", got)
	}

	// A release that does not accept the configuration is not put in place.
	if err := install(ctx, exe, "v9.0.1", "host.yaml"); err == nil || !strings.Contains(err.Error(), "no such key") {
		t.Errorf("a release that refuses the configuration: %v", err)
	}
	// Nor one the rollback gave up on, nor one that cannot update itself,
	// nor one that is not there.
	os.WriteFile(exe+".skip", []byte("v9.2.0\n"), 0o644)
	for v, want := range map[string]string{"v9.2.0": "could not stay up", "v0.6.0": "older than", "v9.3.0-rc.1": "404"} {
		if err := install(ctx, exe, v, "host.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", v, err, want)
		}
	}
	if got, _ := os.ReadFile(exe); !strings.Contains(string(got), "echo v9.1.0") {
		t.Errorf("after the refusals the binary is:\n%s", got)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Errorf("a download was left behind: %v", err)
	}
}

func TestOlder(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"v0.6.0", "v0.7.0", true}, {"v0.7.0", "v0.7.0", false}, {"v0.10.0", "v0.7.0", false}, {"v1.0.0", "v0.9.9", false}, {"dev", "v0.7.0", false}} {
		if older(c.a, c.b) != c.want {
			t.Errorf("older(%s, %s) = %v", c.a, c.b, !c.want)
		}
	}
}

func TestPinOf(t *testing.T) {
	for data, want := range map[string]string{
		"schema: 1\nharness: v0.8.0\nrepo: o/r\n": "v0.8.0",
		// A key this harness does not know, which only v0.9.0 does.
		"schema: 1\nharness: v0.9.0\nrepo: o/r\nnew_key: { a: 1 }\n": "v0.9.0",
		"schema: 1\nrepo: o/r\n":                                     "",
		"harness: [":                                                 "",
	} {
		if got := pinOf([]byte(data)); got != want {
			t.Errorf("pinOf(%q) = %q, want %q", data, got, want)
		}
	}
}
