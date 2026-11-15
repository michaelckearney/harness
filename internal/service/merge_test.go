package service

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michaelckearney/harness/internal/github"
)

func TestAnApprovalMerges(t *testing.T) {
	h := setup(t)
	h.step()
	n := h.gh.openScoped("owner", "Say hello", "Add hello.txt, please.")
	h.step()
	b := "harness/" + fmt.Sprint(n)
	pr := h.gh.pr(b)
	if pr == nil || pr.Draft {
		t.Fatalf("no pull request ready for review: %+v\n%s", pr, h.read("out"))
	}
	notYet := func(why string) {
		t.Helper()
		h.step()
		if got := h.gh.merged(); len(got) != 0 {
			t.Fatalf("merged %s: %q", why, got)
		}
	}

	h.gh.pass(b)
	notYet("with nobody's approval")
	h.gh.review(pr.Number, "stranger", "APPROVED", "")
	notYet("on a stranger's approval")

	// The owner approves, saying nothing: that is for the merge, and the
	// bot has no turn. Main moved first, so the branch is behind it.
	before := h.read("prompts")
	write(t, filepath.Join(h.fake, "scenario"), "sync")
	write(t, filepath.Join(h.work, "CHANGELOG.md"), "news\n")
	gitIn(t, h.work, "add", "-A")
	gitIn(t, h.work, "commit", "--quiet", "-m", "News")
	gitIn(t, h.work, "push", "--quiet", "origin", "main")
	h.gh.review(pr.Number, "owner", "APPROVED", "")
	notYet("behind main")
	if p := h.read("prompts"); strings.Contains(strings.TrimPrefix(p, before), "reviewed the pull request") {
		t.Errorf("an empty approval gave the bot a turn:\n%s", p)
	}
	if got := gitIn(t, h.work, "--git-dir", h.origin, "show", b+":CHANGELOG.md"); got != "news" {
		t.Fatalf("the bot did not sync main:\n%s", h.read("out"))
	}

	// The bot's sync is a new head, which CI has not checked yet.
	notYet("before CI ran on the new head")
	h.gh.pass(b)
	head := h.gh.sha(b)
	h.step()
	if got, want := h.gh.merged(), []string{fmt.Sprintf("%d %s feat: add hello.txt (#%d)", pr.Number, head, pr.Number)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("merges: %q, want %q", got, want)
	}
	h.step()
	if th := h.thread(n); th.State != "closed" {
		t.Errorf("after the merge the thread is %s", th.State)
	}
}

func TestApprovals(t *testing.T) {
	trusted := func(login string) bool { return login != "stranger" }
	r := func(user, state, sha string) github.Review {
		return github.Review{User: github.User{Login: user}, State: state, CommitID: sha}
	}
	for _, c := range []struct {
		name    string
		reviews []github.Review
		want    string
	}{
		{"none", nil, "[]"},
		{"approved", []github.Review{r("owner", "APPROVED", "a")}, "[a]"},
		{"a stranger's", []github.Review{r("stranger", "APPROVED", "a")}, "[]"},
		{"a comment after is not a last word", []github.Review{r("owner", "APPROVED", "a"), r("owner", "COMMENTED", "b")}, "[a]"},
		{"changes asked for after", []github.Review{r("owner", "APPROVED", "a"), r("owner", "CHANGES_REQUESTED", "b")}, "[]"},
		{"approved again after changes", []github.Review{r("owner", "CHANGES_REQUESTED", "a"), r("owner", "APPROVED", "b")}, "[b]"},
		{"another asks for changes", []github.Review{r("owner", "APPROVED", "a"), r("other", "CHANGES_REQUESTED", "a")}, "[]"},
	} {
		if got := fmt.Sprint(approvals(c.reviews, trusted)); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestAGuardedChangeNeedsItsHeadApproved(t *testing.T) {
	for file, want := range map[string]bool{"harness.yaml": true, ".github/workflows/pr.yml": true, "projects/app/harness.yaml": false, "README.md": false} {
		if guarded(file) != want {
			t.Errorf("guarded(%q) = %v", file, !want)
		}
	}
}
