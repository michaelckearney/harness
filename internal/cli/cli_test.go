package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "harness-cli")
	bin = filepath.Join(dir, "harness")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/harness").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func write(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=me", "GIT_AUTHOR_EMAIL=me@x",
		"GIT_COMMITTER_NAME=me", "GIT_COMMITTER_EMAIL=me@x")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=me", "GIT_AUTHOR_EMAIL=me@x",
		"GIT_COMMITTER_NAME=me", "GIT_COMMITTER_EMAIL=me@x")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// monorepo is a framework and three projects, whose framework command says
// what it was asked.
func monorepo(t *testing.T) string {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write(t, filepath.Join(root, "harness.yaml"), `schema: 1
repo: o/r
triggers: [owner]
projects:
  harbor: projects/harbor
  slipway: projects/slipway
  site: projects/site
`)
	fw := `#!/bin/sh
verb=$1; shift
echo "$verb [${HARNESS_PROJECT_NAME:-self}] cwd=$(pwd | sed "s|$HARNESS_ROOT/||") args=$* version=$HARNESS_VERSION"
[ $# -eq 0 ] && [ "$verb" = test ] && echo '{"full":true}' > "$HARNESS_RESULT"
[ "$verb" = build ] && echo '{"artifacts":["build/app.zip"]}' > "$HARNESS_RESULT"
[ -e "$HARNESS_ROOT/fail-$HARNESS_PROJECT_NAME" ] && exit 3
exit 0
`
	write(t, filepath.Join(root, "framework/framework.yaml"), `command: [sh, ./fw.sh]
verbs:
  test: lint and tests
  build: the release artifacts
  run: start the app
  modes: { help: list the modes, turn: true }
`)
	write(t, filepath.Join(root, "framework/fw.sh"), fw)
	write(t, filepath.Join(root, "framework/ci/Dockerfile"), "FROM scratch\n")
	for _, p := range []string{"harbor", "slipway", "site"} {
		write(t, filepath.Join(root, "projects", p, "README.md"), p+"\n")
	}
	return root
}

func TestVerbs(t *testing.T) {
	root := monorepo(t)
	// At the root, test is everything: the framework, then each project.
	out, err := run(t, root, "test")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	order := []string{"test [self] cwd=framework", "test [harbor]", "test [site]", "test [slipway]", "the framework passed"}
	last := -1
	for _, want := range order {
		i := strings.Index(out, want)
		if i <= last {
			t.Fatalf("test at the root, %q out of order:\n%s", want, out)
		}
		last = i
	}

	// In a project, that project; in the framework, it and every project.
	if out, _ := run(t, filepath.Join(root, "projects/site"), "test", "-unit"); !strings.Contains(out, "test [site] cwd=framework args=-unit") || strings.Contains(out, "harbor") {
		t.Errorf("test in a project:\n%s", out)
	}
	if out, _ := run(t, filepath.Join(root, "framework"), "test"); !strings.Contains(out, "test [slipway]") || !strings.Contains(out, "test [site]") {
		t.Errorf("test in the framework:\n%s", out)
	}
	if out, _ := run(t, root, "test", "-p", "harbor"); !strings.Contains(out, "test [harbor]") || strings.Contains(out, "slipway") {
		t.Errorf("test -p harbor:\n%s", out)
	}
	if out, _ := run(t, root, "test", "-p", "framework"); !strings.Contains(out, "test [self]") || !strings.Contains(out, "test [site]") {
		t.Errorf("test -p framework:\n%s", out)
	}

	// -since tests what changed since a ref: a project's change, that
	// project; any other, everything.
	git(t, root, "init", "--quiet", "--initial-branch=main")
	git(t, root, "add", "-A")
	git(t, root, "commit", "--quiet", "-m", "start")
	git(t, root, "checkout", "--quiet", "-b", "change")
	write(t, filepath.Join(root, "projects/site/new.txt"), "x\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "--quiet", "-m", "site")
	if out, err := run(t, root, "test", "-since", "main"); err != nil || !strings.Contains(out, "test [site]") || strings.Contains(out, "harbor") || strings.Contains(out, "[self]") {
		t.Errorf("test -since main, with a change to site: %v\n%s", err, out)
	}
	write(t, filepath.Join(root, "framework/new.txt"), "x\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "--quiet", "-m", "framework")
	if out, err := run(t, root, "test", "-since=main"); err != nil || !strings.Contains(out, "test [self]") || !strings.Contains(out, "test [harbor]") {
		t.Errorf("test -since main, with a change to the framework: %v\n%s", err, out)
	}

	// A failure stops the run, with the framework's exit code.
	write(t, filepath.Join(root, "fail-harbor"), "")
	out, err = run(t, root, "test")
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 || strings.Contains(out, "test [slipway]") {
		t.Errorf("a failing project: %v\n%s", err, out)
	}
	os.Remove(filepath.Join(root, "fail-harbor"))

	// The framework's own verbs need one project; one it lacks is unknown.
	if out, err := run(t, root, "run"); err == nil || !strings.Contains(out, "name one with -p <project>") {
		t.Errorf("run at the root: %s", out)
	}
	if out, err := run(t, root, "run", "-p", "site", "--port", "3"); err != nil || !strings.Contains(out, "run [site] cwd=framework args=--port 3") {
		t.Errorf("run -p site: %v %s", err, out)
	}
	if out, err := run(t, root, "modes", "-p", "site"); err != nil || !strings.Contains(out, "modes [site]") {
		t.Errorf("a verb of the framework's own: %v %s", err, out)
	}
	if out, err := run(t, root, "record", "-p", "site"); err == nil || !strings.Contains(out, `unknown command "record"`) {
		t.Errorf("a verb the framework lacks: %s", out)
	}
	if out, err := run(t, root, "help"); err != nil || !strings.Contains(out, "harness modes [args]") || !strings.Contains(out, "list the modes") {
		t.Errorf("help lists the framework's verbs: %v\n%s", err, out)
	}
	if out, err := run(t, root, "check"); err != nil || !strings.Contains(out, "project   site         projects/site") || !strings.Contains(out, "verbs: test, build, modes, run") {
		t.Errorf("check: %v\n%s", err, out)
	}
	if out, err := run(t, t.TempDir(), "test"); err == nil || !strings.Contains(out, "not in a monorepo") {
		t.Errorf("outside a monorepo: %s", out)
	}

	// A framework may not take the harness's own commands.
	write(t, filepath.Join(root, "framework/framework.yaml"), "command: [sh, ./fw.sh]\nverbs: { test: t, commit: c }\n")
	if out, err := run(t, root, "check"); err == nil || !strings.Contains(out, `"commit" is the harness's own command`) {
		t.Errorf("a reserved verb: %s", out)
	}
}

func TestReleaseVersionsEachProject(t *testing.T) {
	root := monorepo(t)
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "chore: start")
	git(t, root, "tag", "harbor/v0.1.0")
	git(t, root, "tag", "site/v0.3.0")
	write(t, filepath.Join(root, "projects/harbor/x"), "x")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "feat: harbor does more")
	write(t, filepath.Join(root, "projects/site/y"), "y")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "fix: the site works")

	out, err := run(t, root, "release", "-plan")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// slipway has no tag yet: its first release.
	if out != "harbor 0.1.1\nsite 0.3.1\nslipway 0.1.0\n" {
		t.Errorf("plan:\n%s", out)
	}
	if out, _ := run(t, root, "release", "-p", "harbor", "-notes"); !strings.Contains(out, "# harbor/v0.1.1") || !strings.Contains(out, "- feat: harbor does more") || strings.Contains(out, "site") {
		t.Errorf("notes:\n%s", out)
	}
	// build gets the version git gives the project.
	if out, _ := run(t, root, "build", "-p", "harbor"); !strings.Contains(out, "version=0.1.1-dev.") || !strings.Contains(out, "built projects/harbor/build/app.zip") {
		t.Errorf("build:\n%s", out)
	}
}

func TestInit(t *testing.T) {
	root := monorepo(t)
	git(t, root, "init", "-q", "-b", "main")
	dir := t.TempDir()
	out, err := run(t, dir, "init", "-repo", "me/new", "-from", filepath.Join(root, "framework"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"harness.yaml", "AGENTS.md", ".github/workflows/pr.yml", ".github/workflows/main.yml", ".github/CODEOWNERS", "framework/fw.sh", "projects"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("init made no %s", f)
		}
	}
	if y, _ := os.ReadFile(filepath.Join(dir, "harness.yaml")); !strings.Contains(string(y), "repo: me/new") || !strings.Contains(string(y), "triggers: [me]") {
		t.Errorf("harness.yaml:\n%s", y)
	}
	// A second init keeps what is there.
	if out, _ := run(t, dir, "init", "-repo", "me/new"); !strings.Contains(out, "kept    harness.yaml") {
		t.Errorf("second init:\n%s", out)
	}
}
