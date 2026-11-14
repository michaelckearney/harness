package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/michaelckearney/harness/internal/deploy"
	"github.com/michaelckearney/harness/internal/version"
)

// ErrUpdated is what Run returns once the service has put the harness
// harness.yaml pins in place of its own binary. Started again, the service
// is the new one: the unit starts it again when it exits with ExitUpdated.
var ErrUpdated = errors.New("updated the harness; starting the new one")

// ExitUpdated is the service's exit status after it updated itself.
const ExitUpdated = 75

// SelfUpdating is the first harness that updates itself. Pinning one before
// it would leave the server on a harness that never moves again, so the
// service refuses: deploy such a version by hand.
const SelfUpdating = "v0.7.0"

// pinned is the harness harness.yaml pins on the default branch, or "": in
// a harness.yaml this harness cannot read, too.
func (s *Service) pinned() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.projects {
		if p.pin != "" {
			return p.pin
		}
	}
	return ""
}

// selfUpdate puts the harness harness.yaml pins in place of this one when they
// differ, up or down. It reports whether it did: the service then stops,
// once its running turns finish, for systemd to start the new one. A pin it
// cannot install it tries again an hour later, and one the rollback gave up
// on, never.
func (s *Service) selfUpdate(ctx context.Context) bool {
	want, have := s.pinned(), version.Get()
	if want == "" || want == have || have == "dev" || s.ConfigPath == "" {
		return false
	}
	if last, ok := s.tried[want]; ok && time.Since(last) < time.Hour {
		return false
	}
	if s.tried == nil {
		s.tried = map[string]time.Time{}
	}
	s.tried[want] = time.Now()
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		s.log.Printf("update: %v", err)
		return false
	}
	if err := install(ctx, exe, want, s.ConfigPath); err != nil {
		s.log.Printf("update: harness.yaml pins harness %s, and this is %s: %v; staying on %s", want, have, err, have)
		return false
	}
	s.log.Printf("update: harness.yaml pins harness %s: it is in place of %s, and starts once the running turns finish", want, have)
	return true
}

// install puts the release want in place of exe, once it has checked that
// the release is what it says and accepts the service's configuration. The
// binary it replaces stays beside it as .previous, for the rollback.
func install(ctx context.Context, exe, want, config string) error {
	if older(want, SelfUpdating) {
		return fmt.Errorf("it is older than %s, the first harness that updates itself, so the server would stay on it; deploy it by hand", SelfUpdating)
	}
	if skip, _ := os.ReadFile(exe + ".skip"); bytes.Contains(append([]byte("\n"), skip...), []byte("\n"+want+"\n")) {
		return fmt.Errorf("it could not stay up here before, so the previous harness was put back; pin another, or deploy it by hand")
	}
	data, err := deploy.Binary(ctx, want, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	next := exe + ".new"
	defer os.Remove(next)
	if err := os.WriteFile(next, data, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(next, 0o755); err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, next, "version").Output()
	if err != nil || strings.TrimSpace(string(out)) != want {
		return fmt.Errorf("the release says it is %q", strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, next, "serve", "-validate", "--config", config).CombinedOutput(); err != nil {
		return fmt.Errorf("it does not accept %s: %s", config, strings.TrimSpace(string(out)))
	}
	if err := copyExe(exe, exe+".previous"); err != nil {
		return err
	}
	return os.Rename(next, exe)
}

func copyExe(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := to + ".tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}

// older reports whether release a is before release b, both vX.Y.Z. A
// version that is not one, such as a pre-release, is not older.
func older(a, b string) bool {
	pa, okA := parts(a)
	pb, okB := parts(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parts(v string) ([3]int, bool) {
	var p [3]int
	f := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if !strings.HasPrefix(v, "v") || len(f) != 3 {
		return p, false
	}
	for i, x := range f {
		n, err := strconv.Atoi(x)
		if err != nil {
			return p, false
		}
		p[i] = n
	}
	return p, true
}
