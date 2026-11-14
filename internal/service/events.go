package service

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/michaelckearney/harness/internal/github"
	"github.com/michaelckearney/harness/internal/repo"
	"github.com/michaelckearney/harness/internal/store"
)

// failed are the check-run conclusions that need the bot.
var failed = []string{"failure", "timed_out", "cancelled", "action_required", "startup_failure"}

// events are everything in a thread a turn answers, answered or not: what
// the trigger users wrote after harness serve started watching, a failed CI run
// on the branch's head, and the default branch moving past the branch.
//
// CI and the default branch are left until no turn is running: during one,
// the head is still moving.
func (s *Service) events(p *proj, t *store.Thread, issue *github.Issue, pr *github.PR, start time.Time) ([]store.Event, error) {
	trusted := func(u github.User, at time.Time) bool { return p.repo.Trusted(u.Login) && !at.Before(start) }
	var out []store.Event

	if trusted(issue.User, issue.CreatedAt) {
		out = append(out, store.Event{Key: "issue", Kind: "opened", At: issue.CreatedAt,
			Text: fmt.Sprintf("@%s opened the issue: %s\n\n%s", issue.User.Login, issue.Title, s.attachedFiles(p, t.Number, issue.Body, issue.BodyHTML))})
	}
	comments, err := s.gh.Comments(t.Repo, t.Number)
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		if trusted(c.User, c.CreatedAt) {
			out = append(out, store.Event{Key: fmt.Sprintf("comment:%d", c.ID), Kind: "comment", At: c.CreatedAt,
				Text: fmt.Sprintf("@%s commented: %s", c.User.Login, s.attachedFiles(p, t.Number, c.Body, c.BodyHTML))})
		}
	}
	if pr == nil {
		return sorted(out), nil
	}

	// What is said on the pull request is answered in the issue, so each
	// event links back to it; see reply.
	prComments, err := s.gh.Comments(t.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	for _, c := range prComments {
		if trusted(c.User, c.CreatedAt) {
			out = append(out, store.Event{Key: fmt.Sprintf("comment:%d", c.ID), Kind: "comment", At: c.CreatedAt,
				Text: fmt.Sprintf("@%s commented on the pull request: %s", c.User.Login, s.attachedFiles(p, t.Number, c.Body, c.BodyHTML)),
				Link: fmt.Sprintf("[comment](%s)", c.HTMLURL)})
		}
	}

	reviews, err := s.gh.Reviews(t.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	for _, r := range reviews {
		if r.State != "PENDING" && trusted(r.User, r.SubmittedAt) {
			text := fmt.Sprintf("@%s reviewed the pull request: %s", r.User.Login, strings.ToLower(strings.ReplaceAll(r.State, "_", " ")))
			if r.Body != "" {
				text += "\n\n" + s.attachedFiles(p, t.Number, r.Body, r.BodyHTML)
			}
			out = append(out, store.Event{Key: fmt.Sprintf("review:%d", r.ID), Kind: "review", At: r.SubmittedAt, Text: text,
				Link: fmt.Sprintf("[review](%s)", r.HTMLURL)})
		}
	}
	rcs, err := s.gh.ReviewComments(t.Repo, pr.Number)
	if err != nil {
		return nil, err
	}
	for _, c := range rcs {
		if trusted(c.User, c.CreatedAt) {
			out = append(out, store.Event{Key: fmt.Sprintf("review-comment:%d", c.ID), Kind: "review-comment", At: c.CreatedAt,
				Text: fmt.Sprintf("@%s commented on %s: %s", c.User.Login, where(c), s.attachedFiles(p, t.Number, c.Body, c.BodyHTML)),
				Link: fmt.Sprintf("[comment on `%s`](%s)", where(c), c.HTMLURL)})
		}
	}

	s.mu.Lock()
	running := s.turns[key(t.Repo, t.Number)] != nil
	base := p.base
	s.mu.Unlock()
	if running {
		return sorted(out), nil
	}
	g, err := s.git(p, "")
	if err != nil {
		return nil, err
	}
	head, err := g.Run("rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch(t.Number))
	if err != nil || head == "" {
		return sorted(out), nil
	}

	// Only the head the bot last pushed: a failure on an older commit is
	// already behind it.
	if head == pr.Head.SHA {
		runs, err := s.gh.CheckRuns(t.Repo, head)
		if err != nil {
			return nil, err
		}
		var bad []string
		done := len(runs) > 0
		for _, r := range runs {
			if r.Status != "completed" {
				done = false
			} else if slices.Contains(failed, r.Conclusion) {
				bad = append(bad, fmt.Sprintf("%s (%s) %s", r.Name, r.Conclusion, r.HTMLURL))
			}
		}
		if done && len(bad) > 0 {
			out = append(out, store.Event{Key: "ci:" + head, Kind: "ci-failed", At: time.Now(),
				Text: "CI failed on " + short(head) + ":\n- " + strings.Join(bad, "\n- ")})
		}
	}

	// The default branch moving on is the bot's to judge: a clean merge can
	// still break the change. One event for wherever it is now.
	baseSHA, err := g.Run("rev-parse", "refs/remotes/origin/"+base)
	if err != nil {
		return nil, err
	}
	if _, err := g.Run("merge-base", "--is-ancestor", baseSHA, head); err != nil {
		log, _ := g.Run("log", "--format=- %h %s", "--max-count=20", head+".."+baseSHA)
		files, _ := g.Run("diff", "--name-only", head+"..."+baseSHA)
		conflict := "It merges without conflicts."
		if _, err := g.Run("merge-tree", "--write-tree", head, baseSHA); err != nil {
			conflict = "It has merge conflicts with your branch."
		}
		// What the default branch changed cannot touch a project's work:
		// the harness merges it, and the bot is not asked.
		if conflict == "It merges without conflicts." && unrelated(p.repo, t.Scope, files) {
			if err := s.mergeBase(p, t, head, baseSHA); err == nil {
				s.log.Printf("%s: merged %s, which changes nothing %s depends on", key(t.Repo, t.Number), base, t.Scope)
				return sorted(out), nil
			} else {
				s.log.Printf("%s: could not merge %s for the bot, so it is the bot's: %v", key(t.Repo, t.Number), base, err)
			}
		}
		text := fmt.Sprintf("%s has moved on since your branch was cut. %s\n\nNew commits:\n%s\n\nFiles they change:\n%s",
			base, conflict, log, indent(files))
		out = append(out, store.Event{Key: "base:" + baseSHA, Kind: "base-moved", At: time.Now(), Text: text})
	}
	return sorted(out), nil
}

// unrelated reports whether the files the default branch changed, one per
// line, leave a project's work as it was: none is in the project, and none is
// outside every project, in the framework or at the root, which every
// project depends on. The framework's work depends on everything.
func unrelated(cfg *repo.Config, label string, files string) bool {
	if cfg == nil || files == "" {
		return false
	}
	sc, err := cfg.ScopeNamed(label)
	if err != nil || sc.Kind != repo.ProjectKind {
		return false
	}
	for _, f := range strings.Split(files, "\n") {
		if f == "" {
			continue
		}
		if other := cfg.ScopeOf(f); other.Kind != repo.ProjectKind || other.Name == sc.Name {
			return false
		}
	}
	return true
}

// mergeBase merges the default branch, at baseSHA, into the thread's branch
// for the bot, between turns, as harness sync would. head is the branch on GitHub: the
// worktree must be there, clean, and nothing else. Anything that goes wrong
// leaves the worktree as it was.
func (s *Service) mergeBase(p *proj, t *store.Thread, head, baseSHA string) error {
	if err := s.ensureWorktree(p, t.Number); err != nil {
		return err
	}
	g, err := s.wgit(p, t)
	if err != nil {
		return err
	}
	if local, err := g.Run("rev-parse", "HEAD"); err != nil || local != head {
		return fmt.Errorf("the worktree is not at the branch's head on GitHub")
	}
	if _, err := s.sync(p, t, baseSHA); err != nil {
		g.Run("merge", "--abort")
		g.Run("reset", "--quiet", "--hard", head)
		return err
	}
	return nil
}

func sorted(es []store.Event) []store.Event {
	slices.SortStableFunc(es, func(a, b store.Event) int { return a.At.Compare(b.At) })
	return es
}

func where(c github.ReviewComment) string {
	if c.Line > 0 {
		return fmt.Sprintf("%s:%d", c.Path, c.Line)
	}
	return c.Path
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func indent(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			lines = append(lines, "- "+l)
		}
	}
	if len(lines) > 30 {
		lines = append(lines[:30], fmt.Sprintf("- and %d more", len(lines)-30))
	}
	return strings.Join(lines, "\n")
}
