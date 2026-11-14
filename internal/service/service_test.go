package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michaelckearney/harness/internal/host"
	"github.com/michaelckearney/harness/internal/store"
)

// bin is the harness, built once, for the fake bot to run.
var bin string

func TestMain(m *testing.M) {
	if os.Getenv("HARNESS_FAKE_OPENCODE") == "1" {
		fakeOpenCode()
		return
	}
	dir, err := os.MkdirTemp("", "harness-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "harness")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/harness").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeGh is the owner's gh login, which the service asks only for its token.
const fakeGh = `#!/bin/sh
echo "$@" >> "$FAKE_DIR/gh-calls"
[ "$1 $2" = "auth token" ] && echo owner-token
`

// fakeBot plays the bot, run by the fake opencode for each prompt: what it
// does on a turn is read from a file, so a test can change it between turns.
// $BS is the harness.
const fakeBot = `#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
	--in) dir=$2; shift ;;
	-z) prompt=$2; shift ;;
	esac
	shift
done
echo "GH_TOKEN=$GH_TOKEN" >> "$FAKE_DIR/bot-env"
printf '%s\n---\n' "$prompt" >> "$FAKE_DIR/prompts"
cd "$dir" || exit 1
out="$FAKE_DIR/out"
case "$(cat "$FAKE_DIR/scenario")" in
work)
	"$BS" status > "$FAKE_DIR/status" 2>&1
	"$BS" run >> "$out" 2>&1
	echo hello > projects/app/hello.txt
	cp .harness/pr.md "$FAKE_DIR/template"
	"$BS" commit "Add hello" >> "$out" 2>&1
	printf 'Add hello.txt\n\nAdds hello.txt.\n' > .harness/pr.md
	"$BS" commit "Add hello" >> "$out" 2>&1
	mkdir -p shots && echo png > shots/hello.png
	printf '<!-- the template -->\nfeat: add hello.txt\n\nAdds hello.txt.\n\n![hello.txt in the tree](shots/hello.png)\n' > .harness/pr.md
	"$BS" commit "Add hello" >> "$out" 2>&1
	git status --porcelain > "$FAKE_DIR/git-status"
	"$BS" done >> "$out" 2>&1
	echo "Added hello.txt." | "$BS" reply - >> "$out" 2>&1
	"$BS" done >> "$out" 2>&1
	"$BS" test >> "$FAKE_DIR/test-out" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
sync)
	"$BS" sync >> "$out" 2>&1
	echo "Merged main." | "$BS" reply - >> "$out" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
scoped)
	printf 'feat: add hi\n\nAdds hi.\n' > .harness/pr.md
	echo hi > projects/app/hi.txt
	echo nope > README.md
	"$BS" commit "Add hi" >> "$out" 2>&1
	git checkout -q README.md
	"$BS" commit "Add hi" >> "$out" 2>&1
	"$BS" test -unit >> "$FAKE_DIR/test-out" 2>&1
	"$BS" done >> "$out" 2>&1
	"$BS" test >> "$FAKE_DIR/test-out" 2>&1
	echo "Added hi." | "$BS" reply - >> "$out" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
more)
	echo world >> projects/app/hello.txt
	"$BS" commit "Say hello world" >> "$out" 2>&1
	echo "It says hello world now." | "$BS" reply - >> "$out" 2>&1
	printf 'feat: say hello world\n\nAdds hello.txt, which says hello world.\n' > .harness/pr.md
	"$BS" done >> "$out" 2>&1
	"$BS" commit "Say hello world" >> "$out" 2>&1
	"$BS" status > "$FAKE_DIR/status" 2>&1
	"$BS" test >> "$FAKE_DIR/test-out" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
leak)
	printf '![key](/etc/hosts)\n![up](../../outside.png)\n' | "$BS" reply - >> "$out" 2>&1
	echo "No." | "$BS" reply - >> "$out" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
question)
	echo "Which test failed?" | "$BS" reply - >> "$out" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
quiet)
	"$BS" status > "$FAKE_DIR/status" 2>&1
	"$BS" done >> "$out" 2>&1
	;;
silent)
	;;
esac
`

// fakeFramework is a framework by the contract: test passes unless
// $FAKE_DIR/fail is there, and is full with no arguments.
const fakeFramework = `#!/bin/sh
verb=$1; shift
echo "$verb ${HARNESS_PROJECT_NAME:-framework} turn=$HARNESS_TURN $*" >> "$FAKE_DIR/framework-calls"
case "$verb" in
test)
	if [ $# -eq 0 ]; then echo '{"full":true}' > "$HARNESS_RESULT"; fi
	[ -e "$FAKE_DIR/fail" ] && { echo "a test failed"; exit 1; }
	echo ok
	;;
*) echo "no $verb" >&2; exit 2 ;;
esac
`

type harness struct {
	t      *testing.T
	s      *Service
	gh     *fakeGitHub
	origin string
	fake   string // the fake bot's directory
	work   string // a clone for pushing to origin as a person would
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=me", "GIT_AUTHOR_EMAIL=me@x",
		"GIT_COMMITTER_NAME=me", "GIT_COMMITTER_EMAIL=me@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *harness {
	// A short path: a Unix socket's path is limited to about 100 bytes.
	root, err := os.MkdirTemp("/tmp", "bs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	h := &harness{t: t, origin: filepath.Join(root, "origin.git"), fake: filepath.Join(root, "fake"), work: filepath.Join(root, "work")}

	gitIn(t, root, "init", "--quiet", "--bare", "--initial-branch=main", h.origin)
	gitIn(t, root, "clone", "--quiet", h.origin, h.work)
	write(t, filepath.Join(h.work, "README.md"), "demo\n")
	// Where the fake bot keeps its screenshots, as harness keeps recordings
	// in a folder git ignores.
	write(t, filepath.Join(h.work, ".gitignore"), "shots/\n")
	write(t, filepath.Join(h.work, "harness.yaml"), "schema: 1\nrepo: o/r\ntriggers: [owner]\n"+
		"agents: { default: opencode, allowed: [opencode, openai-codex] }\n"+
		"prepare: echo \"$(pwd) GH_TOKEN=$GH_TOKEN\" >> \"$FAKE_DIR/prepared\"\n"+
		"projects: { app: projects/app }\n")
	write(t, filepath.Join(h.work, "framework/framework.yaml"), "command: [sh, ./fw.sh]\nverbs: { test: its tests, run: start it, look: { help: look at it, turn: true } }\n")
	write(t, filepath.Join(h.work, "framework/fw.sh"), fakeFramework)
	write(t, filepath.Join(h.work, "framework/AGENTS.md"), "Framework fw: keep it small.\n")
	write(t, filepath.Join(h.work, "projects/app/AGENTS.md"), "Project app: be kind.\n")
	gitIn(t, h.work, "add", "-A")
	gitIn(t, h.work, "commit", "--quiet", "-m", "Start")
	gitIn(t, h.work, "push", "--quiet", "origin", "main")

	write(t, filepath.Join(h.fake, "bot"), fakeBot)
	write(t, filepath.Join(h.fake, "gh"), fakeGh)
	write(t, filepath.Join(h.fake, "scenario"), "work")
	write(t, filepath.Join(root, "token"), "secret\n")
	t.Setenv("FAKE_DIR", h.fake)
	t.Setenv("BS", bin)
	t.Setenv("GH_TOKEN", "host-token")

	var srvURL string
	h.gh, srvURL = func() (*fakeGitHub, string) { f, s := newFakeGitHub(t, "o/r", h.origin); return f, s.URL }()
	self, _ := os.Executable()
	t.Setenv("HARNESS_FAKE_OPENCODE", "1")
	write(t, filepath.Join(root, "host.yaml"), fmt.Sprintf(`schema: 1
repo: o/r
git_url: %s
agents: [opencode]
commands: { opencode: %s }
model: { id: test-model, base_url: "http://127.0.0.1:1/v1", vision: true }
env: { HARNESS_TEST_ENV: from-the-host }
state_dir: %s
gh: %s
github:
  token_file: %s
  api_url: %s
`, h.origin, self, filepath.Join(root, "state"), filepath.Join(h.fake, "gh"),
		filepath.Join(root, "token"), srvURL))
	cfg, err := host.Load(filepath.Join(root, "host.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	if testing.Verbose() {
		logger = log.New(os.Stderr, "service: ", 0)
	}
	h.s, err = New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := h.s.listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.s.stopAgents(); h.s.unlisten(ln); h.s.st.Close() })
	go h.s.serve(ln)
	return h
}

// step is one go round the service's loop, waiting for any turn it starts.
func (h *harness) step() {
	h.t.Helper()
	for _, p := range h.s.projects {
		if err := h.s.poll(context.Background(), p); err != nil {
			h.t.Fatal(err)
		}
	}
	h.s.schedule(context.Background())
	deadline := time.Now().Add(30 * time.Second)
	for {
		h.s.mu.Lock()
		busy := len(h.s.turns) > 0
		h.s.mu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("a turn did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) thread(n int) *store.Thread {
	h.t.Helper()
	th, err := h.s.st.Thread("o/r", n)
	if err != nil || th == nil {
		h.t.Fatalf("thread %d: %v", n, err)
	}
	return th
}

func (h *harness) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.fake, name))
	return string(b)
}

func TestAnIssueBecomesAPullRequest(t *testing.T) {
	h := setup(t)
	h.step() // starts watching, and reads harness.yaml
	write(t, filepath.Join(h.fake, "fail"), "")

	// Someone not trusted is ignored; the owner is not.
	h.gh.openIssue("stranger", "Delete everything", "")
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()

	// prepare runs in the worktree as the host; the bot gets no token.
	if got, want := h.read("prepared"), h.s.worktree(h.s.projects[0], n)+" GH_TOKEN=host-token\n"; got != want {
		t.Errorf("prepare ran as %q, want %q", got, want)
	}
	if got := h.read("bot-env"); !strings.HasSuffix(got, "GH_TOKEN=\n") || strings.Contains(got, "host-token") {
		t.Errorf("the bot saw %q", got)
	}

	if !strings.Contains(h.read("status"), "Add hello.txt, please.") {
		t.Errorf("status lacks the issue:\n%s", h.read("status"))
	}
	pr := h.gh.pr("harness/" + fmt.Sprint(n))
	if pr == nil {
		t.Fatalf("no pull request; the bot said:\n%s", h.read("out"))
	}
	// harness test failed: the framework said so. done took the failing
	// run, and left the pull request a draft.
	if !pr.Draft {
		t.Error("done marked a pull request ready whose tests failed")
	}
	if th := h.thread(n); th.TestedTree == "" || th.TestedPass {
		t.Errorf("the failing run was not recorded: %+v", th)
	}
	// The framework's issue changed one project, so its test was that
	// project's alone.
	if got := h.read("test-out"); !strings.Contains(got, "the changes are in project app only") {
		t.Errorf("the test was not narrowed to what changed:\n%s", got)
	}
	if got := h.read("framework-calls"); strings.Contains(got, "test framework") {
		t.Errorf("the framework's own tests ran for a project's change:\n%s", got)
	}
	if got := gitIn(t, h.work, "--git-dir", h.origin, "show", "harness/"+fmt.Sprint(n)+":projects/app/hello.txt"); got != "hello" {
		t.Errorf("hello.txt on GitHub: %q", got)
	}
	out := h.read("out")
	for _, want := range []string{"harness run is not for a bot's turn",
		"write your pull request's title and description in .harness/pr.md first",
		`the title in .harness/pr.md, "Add hello.txt", is not a conventional commit`,
		"Opened draft pull request", "harness reply first", "the whole of harness test has not run", "so the pull request stays a draft"} {
		if !strings.Contains(out, want) {
			t.Errorf("the bot was not told %q:\n%s", want, out)
		}
	}
	// The pull request is the file: git ignores it, and a new worktree
	// starts it from the template, which has no title.
	if got := h.read("template"); got != prTemplate {
		t.Errorf("the worktree's pull request file started as %q", got)
	}
	if got := h.read("git-status"); got != "" {
		t.Errorf("git sees the pull request file: %q", got)
	}
	if pr.Title != "feat: add hello.txt" {
		t.Errorf("the title is %q", pr.Title)
	}
	// The reply is in the issue, not on the pull request.
	if c := h.gh.commentsOn(n); len(c) != 1 || c[0] != "Added hello.txt." {
		t.Errorf("comments on the issue: %q", c)
	}
	if c := h.gh.commentsOn(pr.Number); len(c) != 0 {
		t.Errorf("comments on the pull request: %q", c)
	}
	// The image went up with the owner's login, and the description that
	// shows it went to the App's API.
	if got := h.gh.uploaded(); len(got) != 1 || got[0] != "token owner-token hello.png image/png" {
		t.Errorf("uploads: %q", got)
	}
	if got := h.read("gh-calls"); got != "auth token\n" {
		t.Errorf("gh was asked for %q", got)
	}
	for _, want := range []string{"![hello.txt in the tree](https://github.test/user-attachments/assets/1)", fmt.Sprintf("Closes #%d.", n)} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("the description lacks %q:\n%s", want, pr.Body)
		}
	}
	if th := h.thread(n); th.State != store.Idle || th.Session != "ses_1" || th.PR != pr.Number {
		t.Errorf("thread after the turn: %+v", th)
	}
	if _, err := h.s.st.Thread("o/r", n-1); err != nil {
		t.Fatal(err)
	}
	if th, _ := h.s.st.Thread("o/r", n-1); th != nil {
		t.Errorf("a stranger's issue started a thread: %+v", th)
	}

	// Nothing new: no turn.
	before := h.read("prompts")
	h.step()
	if h.read("prompts") != before {
		t.Error("a turn ran with nothing new")
	}

	// Someone else's edit of the title or description is put back, and
	// the bot is not told: no turn.
	h.gh.mu.Lock()
	published := pr.Body
	pr.Title, pr.Body = "Whatever I like", "Mine now."
	h.gh.mu.Unlock()
	h.step()
	if got := h.gh.pr("harness/" + fmt.Sprint(n)); got.Title != "feat: add hello.txt" || got.Body != published {
		t.Errorf("the edit was not put back: %q %q", got.Title, got.Body)
	}
	if h.read("prompts") != before {
		t.Error("an edit of the pull request started a turn")
	}

	// Only a check's latest run counts: a failure a later run replaced is
	// not an event.
	head := h.gh.sha("harness/" + fmt.Sprint(n))
	h.gh.mu.Lock()
	h.gh.checks[head] = []map[string]any{
		{"id": 1, "name": "unit", "status": "completed", "conclusion": "failure", "html_url": "https://ci/1"},
		{"id": 2, "name": "unit", "status": "completed", "conclusion": "success", "html_url": "https://ci/2"},
	}
	h.gh.mu.Unlock()
	h.step()
	if h.read("prompts") != before {
		t.Error("a failure a later run replaced started a turn")
	}

	// CI failing on the head the bot pushed is an event. Nobody asked
	// anything, so the bot may end the turn without a reply.
	write(t, filepath.Join(h.fake, "scenario"), "quiet")
	h.gh.mu.Lock()
	h.gh.checks[head] = []map[string]any{
		{"id": 3, "name": "unit", "status": "completed", "conclusion": "success", "html_url": "https://ci/3"},
		{"id": 4, "name": "unit", "status": "completed", "conclusion": "failure", "html_url": "https://ci"},
		{"id": 5, "name": "lint", "status": "completed", "conclusion": "failure", "html_url": "https://ci/5"},
	}
	h.gh.mu.Unlock()
	h.step()
	if p := h.read("prompts"); !strings.Contains(p, "CI failed on "+head[:7]) {
		t.Errorf("no CI event in the prompt:\n%s", p)
	}
	// status shows the end of the failed job's log, up to its error.
	if st := h.read("status"); !strings.Contains(st, "## Why CI failed") || !strings.Contains(st, "--- FAIL: TestHello") ||
		!strings.Contains(st, "##[error]Process completed") || strings.Contains(st, "Post job cleanup") ||
		strings.Contains(st, "2026-11-11T") || strings.Contains(st, "\x1b") || !strings.Contains(st, "### lint: failure\n\nIts log cannot be read: the GitHub App needs read access to Actions") {
		t.Errorf("status of a CI failure:\n%s", st)
	}
	if c := h.gh.commentsOn(n); len(c) != 1 {
		t.Errorf("a turn on CI alone commented: %q", c)
	}
	if th := h.thread(n); th.State != store.Idle {
		t.Errorf("after the CI turn the thread is %s; the bot said:\n%s", th.State, h.read("out"))
	}
	h.gh.mu.Lock()
	h.gh.checks[head] = nil
	h.gh.mu.Unlock()

	// A bot that never replies is resumed once, then blocked.
	write(t, filepath.Join(h.fake, "scenario"), "silent")
	h.gh.comment(pr.Number, "owner", "Make it say hello world.")
	h.step()
	if th := h.thread(n); th.State != store.Blocked {
		t.Errorf("a silent bot left the thread %s", th.State)
	}
	if p := h.read("prompts"); !strings.Contains(p, "ended without") || !strings.Contains(p, "Make it say hello world.") {
		t.Errorf("prompts:\n%s", p)
	}
	if c := h.gh.commentsOn(n); len(c) == 0 || !strings.Contains(c[len(c)-1], "blocked") {
		t.Errorf("no word on the issue that it is blocked: %q", c)
	}
	if !h.gh.pr("harness/" + fmt.Sprint(n)).Draft {
		t.Error("the pull request is not back to draft while the bot works")
	}

	// The default branch moving on is an event, and sync merges it. What
	// you say on the pull request is answered in the issue, linked back.
	write(t, filepath.Join(h.fake, "scenario"), "sync")
	write(t, filepath.Join(h.work, "CHANGELOG.md"), "news\n")
	gitIn(t, h.work, "add", "-A")
	gitIn(t, h.work, "commit", "--quiet", "-m", "News")
	gitIn(t, h.work, "push", "--quiet", "origin", "main")
	h.gh.comment(pr.Number, "owner", "Mind the changelog.")
	h.step()
	if th := h.thread(n); th.State != store.Idle {
		t.Errorf("after the sync turn the thread is %s; the bot said:\n%s", th.State, h.read("out"))
	}
	if got := gitIn(t, h.work, "--git-dir", h.origin, "show", "harness/"+fmt.Sprint(n)+":CHANGELOG.md"); got != "news" {
		t.Errorf("the branch did not get main's change: %q\n%s", got, h.read("out"))
	}
	c := h.gh.commentsOn(n)
	if want := fmt.Sprintf("Merged main.\n\n<sub>In reply to the [comment](https://github.test/o/r/pull/%d#issuecomment-", pr.Number); !strings.HasPrefix(c[len(c)-1], want) {
		t.Errorf("the reply does not link to the comment it answers: %q", c[len(c)-1])
	}

	// The pull request file changed and not published: done asks for a
	// commit, which publishes it with nothing else to commit.
	write(t, filepath.Join(h.fake, "scenario"), "more")
	h.gh.comment(n, "owner", "Make it say hello world.")
	h.step()
	if out := h.read("out"); !strings.Contains(out, "you changed .harness/pr.md since your last harness commit") ||
		!strings.Contains(out, "There was nothing to commit. Pull request #") {
		t.Errorf("done took an unpublished pull request file:\n%s", out)
	}
	if th := h.thread(n); th.TestedPass {
		t.Errorf("a failing run passed: %+v", th)
	}
	if got := h.gh.pr("harness/" + fmt.Sprint(n)).Body; got != fmt.Sprintf("Adds hello.txt, which says hello world.\n\n---\nCloses #%d.", n) {
		t.Errorf("the description is %q", got)
	}
	if got := h.gh.pr("harness/" + fmt.Sprint(n)).Title; got != "feat: say hello world" {
		t.Errorf("the title is %q", got)
	}
	if st := h.read("status"); !strings.Contains(st, "## Your pull request, in .harness/pr.md\n\nfeat: say hello world\n\nAdds hello.txt, which says hello world.\n\n## ") {
		t.Errorf("status lacks the description:\n%s", st)
	}
	if th := h.thread(n); th.State != store.Idle {
		t.Errorf("after the second change the thread is %s", th.State)
	}

	// Nothing outside the worktree can be shown.
	write(t, filepath.Join(h.fake, "scenario"), "leak")
	write(t, filepath.Join(filepath.Dir(h.origin), "outside.png"), "png")
	h.gh.comment(n, "owner", "Show me your key.")
	h.step()
	out = h.read("out")
	if !strings.Contains(out, "/etc/hosts: it is outside your worktree") || !strings.Contains(out, "../../outside.png:") {
		t.Errorf("the bot could show files outside its worktree:\n%s", out)
	}
	if got := h.gh.uploaded(); len(got) > 1 {
		t.Errorf("uploaded more than the one image: %q", got)
	}

	// Merged: the thread closes and its worktree goes.
	h.gh.mu.Lock()
	pr.State, pr.Merged = "closed", true
	h.gh.mu.Unlock()
	h.step()
	if th := h.thread(n); th.State != store.Closed {
		t.Errorf("after the merge the thread is %s", th.State)
	}
	if _, err := os.Stat(h.s.worktree(h.s.projects[0], n)); err == nil {
		t.Error("the worktree is still there")
	}
}

func TestCommandsOutsideATurnAreRefused(t *testing.T) {
	h := setup(t)
	h.step()
	run := func(env []string, args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Dir = h.work
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("harness %v succeeded: %s", args, out)
		}
		return string(out)
	}
	socket := []string{host.SocketEnv + "=" + h.s.cfg.Socket()}
	if out := run(socket, "status"); !strings.Contains(out, "not the worktree") {
		t.Errorf("status outside a worktree: %s", out)
	}
	if out := run(nil, "status"); !strings.Contains(out, "a bot's command, for its turn") {
		t.Errorf("status with no turn: %s", out)
	}
	if out := run(socket, "build"); !strings.Contains(out, "harness build is not for a bot's turn") {
		t.Errorf("build during a turn: %s", out)
	}
}

func TestDoneTakesAPassingRun(t *testing.T) {
	h := setup(t)
	h.step()
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()
	p := h.s.projects[0]
	th := h.thread(n)
	g, err := h.s.wgit(p, th)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := g.Run("rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	// As at the end of a turn that committed: the head it started on is
	// the default branch's, and harness test passed on what it committed.
	th.TurnHead = gitIn(t, h.work, "rev-parse", "HEAD")
	th.TestedTree, th.TestedPass, th.Replied = tree, true, true
	if _, err := h.s.done(p, th); err != nil {
		t.Fatal(err)
	}
	if h.gh.pr("harness/" + fmt.Sprint(n)).Draft {
		t.Error("a passing run left the pull request a draft")
	}
}

func TestTheSocketIsKept(t *testing.T) {
	h := setup(t)
	if _, err := h.s.listen(); err == nil || !strings.Contains(err.Error(), "another service is answering") {
		t.Errorf("a second service took the socket: %v", err)
	}
	// Whatever removes the socket's file, the next poll listens again.
	os.Remove(h.s.cfg.Socket())
	h.s.mu.Lock()
	old := h.s.socket
	h.s.mu.Unlock()
	ln, _ := net.Dial("unix", h.s.cfg.Socket())
	if ln != nil {
		t.Fatal("the socket answered after its file was removed")
	}
	next := h.s.keepSocket(nil)
	if next == nil {
		t.Fatal("keepSocket did not listen again")
	}
	t.Cleanup(func() { h.s.unlisten(next) })
	if c, err := net.Dial("unix", h.s.cfg.Socket()); err != nil {
		t.Errorf("the socket does not answer again: %v", err)
	} else {
		c.Close()
	}
	h.s.mu.Lock()
	same := os.SameFile(old, h.s.socket)
	h.s.mu.Unlock()
	if same {
		t.Error("the service still thinks it has the old socket")
	}
}

func TestLabelsPickTheScopeAndTheAgent(t *testing.T) {
	h := setup(t)
	h.step()

	// A stranger's label says nothing; the owner's picks the scope.
	write(t, filepath.Join(h.fake, "scenario"), "scoped")
	n := h.gh.openIssue("owner", "Say hi", "Add hi.txt to the app.")
	h.gh.label(n, "stranger", "agent:openai-codex", true)
	h.gh.label(n, "owner", "project:app", true)
	h.step()

	th := h.thread(n)
	if th.Agent != "opencode" || th.Scope != "project:app" {
		t.Fatalf("thread: %+v", th)
	}
	out := h.read("out")
	for _, want := range []string{"this issue is for project app, so you may change only projects/app", "README.md",
		"Committed", "Turn ended."} {
		if !strings.Contains(out, want) {
			t.Errorf("the bot was not told %q:\n%s", want, out)
		}
	}
	if got := h.read("test-out"); !strings.Contains(got, "it does not count for done") {
		t.Errorf("a filtered test run was not said not to count:\n%s", got)
	}
	if got := gitIn(t, h.work, "--git-dir", h.origin, "show", "harness/"+fmt.Sprint(n)+":README.md"); got != "demo" {
		t.Errorf("a change outside the scope reached GitHub: %q", got)
	}
	// The project alone is tested, in the turn, by its framework: the
	// filtered run does not count, the full one does.
	if got := h.read("framework-calls"); got != "test app turn=1 -unit\ntest app turn=1 \n" {
		t.Errorf("the framework was called:\n%s", got)
	}
	if th := h.thread(n); !th.TestedPass || h.gh.pr("harness/"+fmt.Sprint(n)).Draft {
		t.Errorf("a passing full run did not count: %+v\n%s", th, out)
	}
	sys := h.read("system")
	for _, want := range []string{"You work on project app", "Framework fw: keep it small.", "Project app: be kind."} {
		if !strings.Contains(sys, want) {
			t.Errorf("the instructions lack %q:\n%s", want, sys)
		}
	}

	// An agent this server does not run is another server's: no turn here.
	before := h.read("prompts")
	m := h.gh.openScoped("owner", "Think hard", "")
	h.gh.label(m, "owner", "agent:openai-codex", true)
	h.step()
	if th := h.thread(m); th.Agent != "openai-codex" || th.State != store.Idle {
		t.Errorf("codex's thread: %+v", th)
	}
	if h.read("prompts") != before {
		t.Error("this server took a turn for an agent it does not run")
	}

	// A label harness.yaml does not know is the owner's to fix, said once.
	k := h.gh.openIssue("owner", "Elsewhere", "")
	h.gh.label(k, "owner", "project:nope", true)
	h.step()
	h.step()
	if c := h.gh.commentsOn(k); len(c) != 1 || !strings.Contains(c[0], `there is no project "nope"`) {
		t.Errorf("comments on a badly labelled issue: %q", c)
	}
}

func TestAnIssueWithNoScopeIsNoBots(t *testing.T) {
	h := setup(t)
	h.step()
	before := h.read("prompts")
	n := h.gh.openIssue("owner", "Just a note", "Nothing to do yet.")
	h.step()
	if th, _ := h.s.st.Thread("o/r", n); th != nil {
		t.Errorf("an issue with no scope label has a thread: %+v", th)
	}
	if h.read("prompts") != before || len(h.gh.commentsOn(n)) != 0 {
		t.Error("an issue with no scope label was worked or answered")
	}
	// Labelling it is what starts the bot.
	write(t, filepath.Join(h.fake, "scenario"), "quiet")
	h.gh.label(n, "owner", "project:app", true)
	h.step()
	if th := h.thread(n); th.Scope != "project:app" || h.read("prompts") == before {
		t.Errorf("the labelled issue was not worked: %+v", th)
	}
}

// A turn that cannot start blocks the thread and says why, rather than
// starting again every few seconds (monorepo#17).
func TestATurnThatCannotStartBlocks(t *testing.T) {
	h := setup(t)
	h.step()
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()
	if th := h.thread(n); th.State != store.Idle {
		t.Fatalf("after the first turn: %+v", th)
	}
	// .harness a file: the worktree cannot be set up.
	dir := filepath.Join(h.s.worktree(h.s.projects[0], n), ".harness")
	os.RemoveAll(dir)
	write(t, dir, "not a folder\n")
	h.gh.comment(n, "owner", "One more thing.")
	h.step()
	if th := h.thread(n); th.State != store.Blocked {
		t.Fatalf("a turn that could not start left the thread %s", th.State)
	}
	var said []string
	for _, c := range h.gh.commentsOn(n) {
		if strings.Contains(c, "the turn could not start") {
			said = append(said, c)
		}
	}
	h.step()
	if th := h.thread(n); th.State != store.Blocked || len(said) != 1 {
		t.Errorf("state %s, said %q", th.State, said)
	}
}
