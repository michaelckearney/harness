package service

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/michaelckearney/harness/internal/github"
	"github.com/michaelckearney/harness/internal/store"
)

// passed are the check-run conclusions a merge may go ahead on.
var passed = []string{"success", "neutral", "skipped"}

// guarded are the files that decide what the bots may do: harness.yaml, and
// the pipelines. An approval of a change to them covers only the head it
// reviewed, never what a bot pushes after it.
func guarded(file string) bool {
	return file == "harness.yaml" || strings.HasPrefix(file, ".github/")
}

// merge squash-merges one bot's pull request that is ready: a trigger user
// approved it, and nothing is left to do on it. One a poll, so the others
// are brought up to date with what it merged before they go in: the
// default branch moving on is an event, a turn, and new CI.
//
// It runs between polling and scheduling, in the loop that starts turns, so
// no turn starts while it merges; a bot's push between its look and the
// merge is refused by GitHub, since the merge names the head it checked.
func (s *Service) merge(p *proj) {
	threads, err := s.st.Threads()
	if err != nil {
		s.log.Printf("merge: %v", err)
		return
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].PR < threads[j].PR })
	for _, t := range threads {
		if t.Repo != p.cfg.Repo || t.PR == 0 || t.State != store.Idle {
			continue
		}
		pr, why, err := s.mergeable(p, t)
		if err != nil {
			s.log.Printf("%s: merge: %v", key(t.Repo, t.Number), err)
			continue
		}
		if why != "" {
			continue
		}
		if err := s.gh.Merge(t.Repo, pr.Number, pr.Head.SHA, pr.Title); err != nil {
			s.log.Printf("%s: merge: %v", key(t.Repo, t.Number), err)
			continue
		}
		s.log.Printf("%s: merged pull request #%d at %s", key(t.Repo, t.Number), pr.Number, short(pr.Head.SHA))
		return
	}
}

// mergeable is a thread's pull request, if it may be merged now, or why not.
func (s *Service) mergeable(p *proj, t *store.Thread) (*github.PR, string, error) {
	s.mu.Lock()
	running := s.turns[key(t.Repo, t.Number)] != nil
	pending, err := s.pending(p, t.Number)
	base := p.base
	s.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	if running || len(pending) > 0 {
		return nil, "the bot has a turn to take", nil
	}
	pr, err := s.gh.PR(t.Repo, t.PR)
	if err != nil {
		return nil, "", err
	}
	if pr.State != "open" || pr.Draft {
		return nil, "it is not ready for review", nil
	}
	reviews, err := s.gh.Reviews(t.Repo, pr.Number)
	if err != nil {
		return nil, "", err
	}
	approved := approvals(reviews, p.repo.Trusted)
	if len(approved) == 0 {
		return nil, "no trigger user has approved it, or one asked for changes", nil
	}

	g, err := s.git(p, "")
	if err != nil {
		return nil, "", err
	}
	head, err := g.Run("rev-parse", "--verify", "-q", "refs/remotes/origin/"+branch(t.Number))
	if err != nil || head != pr.Head.SHA {
		return nil, "the clone is behind the branch", nil
	}
	baseSHA, err := g.Run("rev-parse", "refs/remotes/origin/"+base)
	if err != nil {
		return nil, "", err
	}
	if _, err := g.Run("merge-base", "--is-ancestor", baseSHA, head); err != nil {
		return nil, "it is behind " + base, nil
	}
	files, err := g.Run("diff", "--name-only", baseSHA+"..."+head)
	if err != nil {
		return nil, "", err
	}
	if slices.ContainsFunc(strings.Split(files, "\n"), guarded) && !slices.Contains(approved, head) {
		return nil, "it changes harness.yaml or .github/, and nobody approved its head", nil
	}

	runs, err := s.gh.CheckRuns(t.Repo, head)
	if err != nil {
		return nil, "", err
	}
	if len(runs) == 0 {
		return nil, "CI has not run", nil
	}
	for _, r := range runs {
		if r.Status != "completed" {
			return nil, "CI is running", nil
		}
		if !slices.Contains(passed, r.Conclusion) {
			return nil, fmt.Sprintf("%s %s", r.Name, r.Conclusion), nil
		}
	}
	return pr, "", nil
}

// approvals are the heads trigger users approved, if they approve it as it
// stands: each one's last word is their approval or request for changes, and
// no trigger user's is a request for changes. A comment is not a last word,
// and neither is a dismissed review.
func approvals(reviews []github.Review, trusted func(string) bool) []string {
	last := map[string]github.Review{}
	for _, r := range reviews {
		if !trusted(r.User.Login) {
			continue
		}
		if r.State == "APPROVED" || r.State == "CHANGES_REQUESTED" {
			last[r.User.Login] = r
		}
	}
	var heads []string
	for _, r := range last {
		if r.State == "CHANGES_REQUESTED" {
			return nil
		}
		heads = append(heads, r.CommitID)
	}
	return heads
}
