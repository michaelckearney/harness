package gitversion

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	for subject, want := range map[string]Bump{
		"feat: add a thing":              Feature,
		"feat(app): add a thing":         Feature,
		"feat!: drop the old thing":      Breaking,
		"fix(serve)!: change the socket": Breaking,
		"fix: mend it":                   Patch,
		"perf: faster":                   Patch,
		"docs: explain":                  None,
		"chore(deps): slipway v0.5.0":    None,
	} {
		if _, got, ok := classify(subject, ""); !ok || got != want {
			t.Errorf("%q: got %v, %v; want %v", subject, got, ok, want)
		}
	}
	if _, got, _ := classify("feat: x", "Some text.\n\nBREAKING CHANGE: it moved"); got != Breaking {
		t.Errorf("a BREAKING CHANGE footer: got %v", got)
	}
	for _, subject := range []string{"Keep the bots' database the service's own", "feature: x", "fix:x", "Fix: x"} {
		if _, _, ok := classify(subject, ""); ok {
			t.Errorf("%q read as a conventional commit", subject)
		}
	}
}

func TestApply(t *testing.T) {
	for _, c := range []struct {
		from Version
		bump Bump
		want string
	}{
		{Version{0, 4, 0}, Breaking, "0.5.0"},
		{Version{0, 4, 1}, Feature, "0.4.2"},
		{Version{0, 4, 1}, Patch, "0.4.2"},
		{Version{0, 4, 1}, None, "0.4.1"},
		{Version{1, 2, 3}, Breaking, "2.0.0"},
		{Version{1, 2, 3}, Feature, "1.3.0"},
		{Version{1, 2, 3}, Patch, "1.2.4"},
	} {
		if got := c.from.apply(c.bump).String(); got != c.want {
			t.Errorf("%s with %v: got %s, want %s", c.from, c.bump, got, c.want)
		}
	}
}

// repo makes a git repository with a commit for each subject, tagging the
// commits that tags names by index.
func repo(t *testing.T, subjects []string, tags map[int]string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for i, s := range subjects {
		if err := os.WriteFile(filepath.Join(dir, "f"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", "f")
		run("commit", "-q", "-m", s)
		if tag, ok := tags[i]; ok {
			run("tag", tag)
		}
	}
	return dir
}

func TestCalculate(t *testing.T) {
	ctx := context.Background()

	p, err := Calculate(ctx, repo(t, []string{"Start", "Not conventional"}, nil), Series{})
	if err != nil || p.Last != nil || p.Next.String() != "0.1.0" || !p.Releases() {
		t.Errorf("no tag: %+v, %v; want a first release of 0.1.0", p, err)
	}

	dir := repo(t, []string{"Before", "fix: one", "docs: two", "feat!: three"}, map[int]string{0: "v0.4.0"})
	p, err = Calculate(ctx, dir, Series{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Last.String() != "0.4.0" || p.Next.String() != "0.5.0" || p.Bump != Breaking || len(p.Changes) != 3 {
		t.Errorf("got last %v, next %v, bump %v, %d changes", p.Last, p.Next, p.Bump, len(p.Changes))
	}
	if err := p.Strict(); err != nil {
		t.Errorf("Strict: %v", err)
	}

	p, err = Calculate(ctx, repo(t, []string{"Before", "docs: only"}, map[int]string{0: "v0.4.0"}), Series{})
	if err != nil || p.Releases() || p.Next.String() != "0.4.0" {
		t.Errorf("docs only: %+v, %v; want nothing to release", p, err)
	}

	p, err = Calculate(ctx, repo(t, []string{"Before", "Prose title"}, map[int]string{0: "v0.4.0"}), Series{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Strict(); err == nil || !strings.Contains(err.Error(), "Prose title") {
		t.Errorf("Strict with a prose title: %v", err)
	}

	dir = repo(t, []string{"Before", "Merge pull request #2 from o/harness/1\n\nfeat: a merged pull request"}, map[int]string{0: "v0.4.0"})
	p, err = Calculate(ctx, dir, Series{})
	if err != nil || p.Next.String() != "0.4.1" || len(p.Changes) != 1 || p.Changes[0].Subject != "feat: a merged pull request" || p.Strict() != nil {
		t.Errorf("a merge commit: %+v, %v; want its pull request's title read", p, err)
	}

	if _, err := Calculate(ctx, t.TempDir(), Series{}); err == nil {
		t.Error("Calculate outside git: no error")
	}
}

func TestBuild(t *testing.T) {
	ctx := context.Background()
	dir := repo(t, []string{"Before", "fix: one"}, map[int]string{0: "v0.4.0", 1: "v0.4.1"})
	if v, err := Build(ctx, dir, Series{}); err != nil || v != "0.4.1" {
		t.Errorf("at a tag: %q, %v", v, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := Build(ctx, dir, Series{})
	if err != nil || !strings.HasPrefix(v, "0.4.2-dev.") || !strings.HasSuffix(v, ".dirty") {
		t.Errorf("at a tag, with changes: %q, %v", v, err)
	}

	dir = repo(t, []string{"Before", "A branch's own prose commit"}, map[int]string{0: "v0.4.0"})
	if v, err := Build(ctx, dir, Series{}); err != nil || !strings.HasPrefix(v, "0.4.1-dev.") || strings.HasSuffix(v, ".dirty") {
		t.Errorf("on a branch: %q, %v", v, err)
	}
	if Core("0.4.1-dev.abc1234.dirty") != "0.4.1" {
		t.Error("Core")
	}
}
